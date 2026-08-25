package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func (s *Store) AdoptRepositoryLifecycleBaseline(ctx context.Context, input application.RepositoryBaselineInput) error {
	if len(input.Profiles) > 256 {
		return errors.New("repository lifecycle baseline profiles are invalid")
	}
	profiles := append([]application.RepositoryProfileAuthority(nil), input.Profiles...)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Authority.Repository < profiles[j].Authority.Repository })
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if profile.Authority.Validate() != nil || profile.Profile.CanonicalRepository != profile.Authority.Repository || profile.Profile.ProfileID != profile.Authority.ProfileID || profile.Profile.RepositoryBindingDigest != profile.Authority.BindingDigest || !validRepositoryProfile(profile.Profile) {
			return errors.New("repository lifecycle baseline profile is invalid")
		}
		if _, exists := seen[profile.Authority.Repository]; exists {
			return errors.New("repository lifecycle baseline contains a duplicate repository")
		}
		seen[profile.Authority.Repository] = struct{}{}
	}
	if input.AdoptedAt.IsZero() {
		input.AdoptedAt = time.Now().UTC()
	}
	input.AdoptedAt = input.AdoptedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	configuration, err := repositoryConfigurationAuthorityTx(ctx, tx)
	if err != nil {
		return err
	}
	profilesDigest := repositoryProfilesDigest(profiles)
	var count int
	var persistedDigest string
	err = tx.QueryRowContext(ctx, `SELECT repository_count,profiles_digest FROM repository_lifecycle_baseline WHERE authority_id=1`).Scan(&count, &persistedDigest)
	if err == nil {
		_ = count
		_ = persistedDigest
		for _, profile := range profiles {
			var profileID, profileDigest, bindingDigest string
			lookupErr := tx.QueryRowContext(ctx, `SELECT profile_id,profile_digest,repository_binding_digest FROM repository_lifecycles WHERE repository=? AND retired_at=''`, profile.Authority.Repository).Scan(&profileID, &profileDigest, &bindingDigest)
			if errors.Is(lookupErr, sql.ErrNoRows) {
				var onboardingCount int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_onboardings o JOIN repository_onboarding_steps s ON s.onboarding_id=o.onboarding_id WHERE o.canonical_repository=? AND o.status IN ('accepted','running','waiting_for_operator') AND s.step_name='configuration_applied' AND s.status IN ('intended','observed')`, profile.Authority.Repository).Scan(&onboardingCount); err != nil || onboardingCount != 1 {
					return application.ErrRepositoryLifecycleConflict
				}
				// Post-baseline profiles are fenced until the accepted onboarding
				// observes exact worker convergence and creates a disabled
				// incarnation through CreateOnboardingRepositoryLifecycle.
				continue
			}
			if lookupErr != nil {
				return application.ErrRepositoryLifecycleMissing
			}
			if profileID != profile.Authority.ProfileID || profileDigest != profile.Profile.ProfileDigest || bindingDigest != profile.Authority.BindingDigest {
				return application.ErrRepositoryLifecycleConflict
			}
		}
		var unmatched int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_lifecycles WHERE retired_at='' AND removal_state='' AND repository NOT IN (`+repositoryPlaceholders(len(profiles))+`)`, repositoryNames(profiles)...).Scan(&unmatched); err != nil {
			return err
		}
		if unmatched != 0 {
			return application.ErrRepositoryLifecycleConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(profiles) == 0 {
		return errors.New("repository lifecycle baseline profiles are invalid")
	}
	for _, profile := range profiles {
		incarnationID, err := nextRepositoryIncarnationIDTx(ctx, tx, profile.Authority.Repository, profile.Authority.BindingDigest, input.AdoptedAt)
		if err != nil {
			return err
		}
		lifecycle := application.RepositoryLifecycle{IncarnationID: incarnationID, Repository: profile.Authority.Repository, ProfileID: profile.Authority.ProfileID, ProfileDigest: profile.Profile.ProfileDigest, RepositoryBindingDigest: profile.Authority.BindingDigest, Intent: application.RepositoryEnabled, Version: 1, UpdatedAt: input.AdoptedAt}
		if err := insertInitialRepositoryLifecycleTx(ctx, tx, lifecycle, configuration, input.AdoptedAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_lifecycle_baseline(authority_id,configuration_generation_id,configuration_digest,configuration_authority_version,repository_count,profiles_digest,adopted_at) VALUES(1,?,?,?,?,?,?)`, configuration.GenerationID, configuration.Digest, configuration.AuthorityVersion, len(profiles), profilesDigest, formatTime(input.AdoptedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RepositoryOperationAuthority(ctx context.Context, repository string) (application.RepositoryOperationAuthority, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.RepositoryOperationAuthority{}, err
	}
	defer tx.Rollback()
	authority, err := repositoryOperationAuthorityTx(ctx, tx, strings.ToLower(strings.TrimSpace(repository)))
	if err != nil {
		return application.RepositoryOperationAuthority{}, err
	}
	return authority, tx.Commit()
}

func (s *Store) CreateOnboardingRepositoryLifecycle(ctx context.Context, onboardingID string, profile application.LocalRepository, at time.Time) (application.RepositoryProjection, bool, error) {
	if onboardingID == "" || !validRepositoryProfile(profile) || at.IsZero() {
		return application.RepositoryProjection{}, false, errors.New("onboarding lifecycle input is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RepositoryProjection{}, false, err
	}
	defer tx.Rollback()
	var repository, profileID, profileDigest, bindingDigest, status string
	var stepIndex, generationID int64
	if err := tx.QueryRowContext(ctx, `SELECT canonical_repository,profile_id,profile_digest,repository_binding_digest,status,step_index,configuration_generation_id FROM repository_onboardings WHERE onboarding_id=?`, onboardingID).Scan(&repository, &profileID, &profileDigest, &bindingDigest, &status, &stepIndex, &generationID); err != nil {
		return application.RepositoryProjection{}, false, application.ErrOnboardingNotFound
	}
	if repository != profile.CanonicalRepository || profileID != profile.ProfileID || profileDigest != profile.ProfileDigest || bindingDigest != profile.RepositoryBindingDigest || status != string(domain.OnboardingRunning) || stepIndex < 4 || generationID < 1 {
		return application.RepositoryProjection{}, false, application.ErrRepositoryLifecycleConflict
	}
	var lifecycleIntent int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name='lifecycle_created' AND status='intended' AND outcome='pending')`, onboardingID).Scan(&lifecycleIntent); err != nil || lifecycleIntent != 1 {
		return application.RepositoryProjection{}, false, application.ErrRepositoryLifecycleConflict
	}
	configurationAuthority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || configurationAuthority.Incomplete != nil || configurationAuthority.IncompleteRecovery != nil || configurationAuthority.Desired.GenerationID != generationID || configurationAuthority.EffectiveID != generationID || configurationAuthority.Desired.State != application.ConfigurationGenerationEffective {
		return application.RepositoryProjection{}, false, application.ErrConfigurationAuthorityConflict
	}
	if projection, err := repositoryProjectionTx(ctx, tx, repository); err == nil {
		if !profileMatchesLifecycle(profile, projection.Lifecycle) || projection.Lifecycle.Intent != application.RepositoryDisabled {
			return application.RepositoryProjection{}, false, application.ErrRepositoryLifecycleConflict
		}
		return projection, false, tx.Commit()
	} else if !errors.Is(err, application.ErrRepositoryLifecycleMissing) {
		return application.RepositoryProjection{}, false, err
	}
	incarnationID, err := nextRepositoryIncarnationIDTx(ctx, tx, repository, bindingDigest, at.UTC())
	if err != nil {
		return application.RepositoryProjection{}, false, err
	}
	lifecycle := application.RepositoryLifecycle{IncarnationID: incarnationID, Repository: repository, ProfileID: profileID, ProfileDigest: profileDigest, RepositoryBindingDigest: bindingDigest, Intent: application.RepositoryDisabled, Version: 1, UpdatedAt: at.UTC()}
	configuration := application.ConfigurationAdmissionAuthority{GenerationID: configurationAuthority.Desired.GenerationID, Digest: configurationAuthority.Desired.Digest, AuthorityVersion: configurationAuthority.Version, ValidThrough: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := insertInitialRepositoryLifecycleTx(ctx, tx, lifecycle, configuration, at.UTC()); err != nil {
		return application.RepositoryProjection{}, false, err
	}
	projection, err := repositoryProjectionTx(ctx, tx, repository)
	if err != nil {
		return application.RepositoryProjection{}, false, err
	}
	return projection, true, tx.Commit()
}

func (s *Store) RepositoryConfigurationAuthority(ctx context.Context) (application.ConfigurationAdmissionAuthority, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.ConfigurationAdmissionAuthority{}, err
	}
	defer tx.Rollback()
	authority, err := repositoryConfigurationAuthorityTx(ctx, tx)
	if err != nil {
		return application.ConfigurationAdmissionAuthority{}, err
	}
	return authority, tx.Commit()
}

func (s *Store) ListAuthorizedRepositories(ctx context.Context, scopes application.AuthorizedScopeSet, limit int, cursor string) (application.RepositoryListPage, error) {
	if scopes.Empty() || limit < 1 || limit > 100 {
		return application.RepositoryListPage{}, errors.New("authorized repository collection is invalid")
	}
	last, err := decodeRepositoryCursor(cursor, scopes.Digest())
	if err != nil {
		return application.RepositoryListPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.RepositoryListPage{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT repository,repository_binding_digest FROM repository_lifecycles WHERE retired_at='' ORDER BY repository`)
	if err != nil {
		return application.RepositoryListPage{}, err
	}
	var authorized []string
	for rows.Next() {
		var repository, binding string
		if err := rows.Scan(&repository, &binding); err != nil {
			rows.Close()
			return application.RepositoryListPage{}, err
		}
		if scopes.AllowsRepositoryBinding(binding) {
			authorized = append(authorized, repository)
		}
	}
	if err := rows.Close(); err != nil {
		return application.RepositoryListPage{}, err
	}
	page := application.RepositoryListPage{Total: len(authorized)}
	start := sort.SearchStrings(authorized, last)
	if last != "" && start < len(authorized) && authorized[start] == last {
		start++
	}
	end := min(start+limit, len(authorized))
	for _, repository := range authorized[start:end] {
		projection, err := repositoryProjectionTx(ctx, tx, repository)
		if err != nil {
			return application.RepositoryListPage{}, err
		}
		page.Repositories = append(page.Repositories, projection)
	}
	page.HasMore = end < len(authorized)
	if page.HasMore && len(page.Repositories) != 0 {
		page.NextCursor = encodeRepositoryCursor(scopes.Digest(), page.Repositories[len(page.Repositories)-1].Lifecycle.Repository)
	}
	return page, tx.Commit()
}

func (s *Store) GetAuthorizedRepository(ctx context.Context, repository string, scopes application.AuthorizedScopeSet) (application.RepositoryProjection, error) {
	if scopes.Empty() {
		return application.RepositoryProjection{}, application.ErrRepositoryLifecycleMissing
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.RepositoryProjection{}, err
	}
	defer tx.Rollback()
	projection, err := repositoryProjectionTx(ctx, tx, strings.ToLower(strings.TrimSpace(repository)))
	if err != nil || !scopes.AllowsRepositoryBinding(projection.Lifecycle.RepositoryBindingDigest) {
		return application.RepositoryProjection{}, application.ErrRepositoryLifecycleMissing
	}
	return projection, tx.Commit()
}

func (s *Store) BeginRepositoryRecheck(ctx context.Context, input application.RepositoryRecheckStart) (application.RepositoryRecheckState, bool, error) {
	if strings.TrimSpace(input.AttemptID) == "" || strings.TrimSpace(input.OperationID) == "" || input.StartedAt.IsZero() || !validRepositoryProfile(input.Profile) {
		return application.RepositoryRecheckState{}, false, errors.New("repository recheck attempt is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RepositoryRecheckState{}, false, err
	}
	defer tx.Rollback()
	current, err := repositoryOperationAuthorityTx(ctx, tx, input.Expected.Lifecycle.Repository)
	if err != nil || !sameRepositoryOperationAuthority(current, input.Expected) || !profileMatchesLifecycle(input.Profile, current.Lifecycle) {
		return application.RepositoryRecheckState{}, false, application.ErrRepositoryLifecycleConflict
	}
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, input.OperationID)
	if err != nil || !found || receipt.OperationType != application.OperationRecheckRepository || receipt.Scope != application.ScopeRepository || receipt.TargetID != current.Lifecycle.Repository || receipt.TargetBindingDigest != current.Lifecycle.RepositoryBindingDigest {
		return application.RepositoryRecheckState{}, false, application.ErrOperationReceiptConflict
	}
	var existingOperation, status, started string
	err = tx.QueryRowContext(ctx, `SELECT operation_id,status,started_at FROM repository_recheck_attempts WHERE attempt_id=?`, input.AttemptID).Scan(&existingOperation, &status, &started)
	if err == nil {
		if existingOperation != input.OperationID || status != "in_progress" {
			return application.RepositoryRecheckState{}, false, application.ErrRepositoryLifecycleConflict
		}
		state := application.RepositoryRecheckState{AttemptID: input.AttemptID, OperationID: input.OperationID, Refreshing: true, StartedAt: parseTime(started)}
		return state, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return application.RepositoryRecheckState{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO repository_recheck_attempts(attempt_id,operation_id,incarnation_id,repository,expected_profile_digest,expected_repository_binding_digest,expected_lifecycle_version,expected_configuration_generation_id,expected_configuration_digest,expected_configuration_authority_version,status,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,'in_progress',?)`, input.AttemptID, input.OperationID, current.Lifecycle.IncarnationID, current.Lifecycle.Repository, current.Lifecycle.ProfileDigest, current.Lifecycle.RepositoryBindingDigest, current.Lifecycle.Version, current.ConfigurationAuthority.GenerationID, current.ConfigurationAuthority.Digest, current.ConfigurationAuthority.AuthorityVersion, formatTime(input.StartedAt))
	if err != nil {
		return application.RepositoryRecheckState{}, false, application.ErrRepositoryLifecycleConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='applied',outcome='pending',applied_at=? WHERE operation_id=? AND phase='accepted' AND outcome='pending'`, formatTime(input.StartedAt), input.OperationID)
	if err != nil {
		return application.RepositoryRecheckState{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRecheckState{}, false, application.ErrOperationReceiptConflict
	}
	if err := tx.Commit(); err != nil {
		return application.RepositoryRecheckState{}, false, err
	}
	return application.RepositoryRecheckState{AttemptID: input.AttemptID, OperationID: input.OperationID, Refreshing: true, StartedAt: input.StartedAt.UTC()}, true, nil
}

func (s *Store) SaveRepositoryRecheckObservation(ctx context.Context, attemptID string, result domain.RepositoryDimensionResult) error {
	if strings.TrimSpace(attemptID) == "" || result.Validate() != nil {
		return errors.New("repository recheck observation is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM repository_recheck_attempts WHERE attempt_id=?`, attemptID).Scan(&status); err != nil || status != "in_progress" {
		return application.ErrRepositoryLifecycleConflict
	}
	var persistedStatus, reason, digest, observed string
	var identity string
	err = tx.QueryRowContext(ctx, `SELECT status,reason_code,identity_id,evidence_digest,observed_at FROM repository_recheck_observations WHERE attempt_id=? AND dimension=?`, attemptID, string(result.Dimension)).Scan(&persistedStatus, &reason, &identity, &digest, &observed)
	if err == nil {
		if persistedStatus != string(result.Status) || reason != result.ReasonCode || identity != result.Identity || digest != result.EvidenceDigest || !parseTime(observed).Equal(result.ObservedAt.UTC()) {
			return application.ErrRepositoryLifecycleConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_recheck_observations(attempt_id,dimension,status,reason_code,identity_id,evidence_digest,observed_at) VALUES(?,?,?,?,?,?,?)`, attemptID, string(result.Dimension), string(result.Status), result.ReasonCode, result.Identity, result.EvidenceDigest, formatTime(result.ObservedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PublishRepositoryRecheck(ctx context.Context, input application.RepositoryRecheckPublication) (application.RepositoryProjection, application.OperationReceipt, error) {
	if input.PublishedAt.IsZero() || domain.ValidateCompleteRepositoryReadiness(input.Results) != nil || !validRepositoryProfile(input.Profile) {
		return application.RepositoryProjection{}, application.OperationReceipt{}, errors.New("repository recheck publication is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	defer tx.Rollback()
	current, err := repositoryOperationAuthorityTx(ctx, tx, input.Expected.Lifecycle.Repository)
	if err != nil || !sameRepositoryBaseAuthority(current, input.Expected) || current.Recheck == nil || current.Recheck.AttemptID != input.AttemptID || !profileMatchesLifecycle(input.Profile, current.Lifecycle) {
		return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
	}
	var operationID, status string
	if err := tx.QueryRowContext(ctx, `SELECT operation_id,status FROM repository_recheck_attempts WHERE attempt_id=?`, input.AttemptID).Scan(&operationID, &status); err != nil || operationID != input.OperationID || status != "in_progress" {
		return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
	}
	persisted, err := repositoryRecheckObservationsTx(ctx, tx, input.AttemptID)
	if err != nil || !sameDimensionResults(persisted, input.Results) {
		return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
	}
	overall, _ := domain.AggregateRepositoryReadiness(input.Results)
	snapshot, err := buildRepositorySnapshot(current.Lifecycle, current.ConfigurationAuthority, input.Results, input.PublishedAt.UTC(), repositorySnapshotReason(input.Results, overall))
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	snapshot.SnapshotID = "repository-snapshot-" + strings.TrimPrefix(input.AttemptID, "repository-recheck-")
	if err := insertRepositorySnapshotTx(ctx, tx, snapshot); err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE repository_lifecycles SET current_snapshot_id=?,updated_at=? WHERE incarnation_id=? AND retired_at='' AND removal_state='' AND lifecycle_version=? AND profile_digest=? AND repository_binding_digest=?`, snapshot.SnapshotID, formatTime(input.PublishedAt), current.Lifecycle.IncarnationID, current.Lifecycle.Version, current.Lifecycle.ProfileDigest, current.Lifecycle.RepositoryBindingDigest)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE repository_recheck_attempts SET status='published',result_snapshot_id=?,settled_at=?,reason_code='snapshot_published' WHERE attempt_id=? AND status='in_progress'`, snapshot.SnapshotID, formatTime(input.PublishedAt), input.AttemptID)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
	}
	receipt, err := settleRepositoryReceiptTx(ctx, tx, input.OperationID, string(overall), current.Lifecycle.Version, snapshot.SnapshotDigest, snapshot.SnapshotDigest, input.PublishedAt)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	projection, err := repositoryProjectionTx(ctx, tx, current.Lifecycle.Repository)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	return projection, receipt, nil
}

func (s *Store) SettleRepositoryRecheckFailure(ctx context.Context, input application.RepositoryRecheckFailure) error {
	if strings.TrimSpace(input.AttemptID) == "" || strings.TrimSpace(input.OperationID) == "" || strings.TrimSpace(input.ReasonCode) == "" || input.SettledAt.IsZero() || input.Outcome != application.OperationOutcomeFailed && input.Outcome != application.OperationOutcomeConflict && input.Outcome != application.OperationOutcomeAmbiguous {
		return errors.New("repository recheck failure is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attemptOperation, status string
	err = tx.QueryRowContext(ctx, `SELECT operation_id,status FROM repository_recheck_attempts WHERE attempt_id=?`, input.AttemptID).Scan(&attemptOperation, &status)
	if err == nil {
		if attemptOperation != input.OperationID || status != "in_progress" {
			return application.ErrRepositoryLifecycleConflict
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE repository_recheck_attempts SET status=?,settled_at=?,reason_code=? WHERE attempt_id=? AND operation_id=? AND status='in_progress'`, string(input.Outcome), formatTime(input.SettledAt), input.ReasonCode, input.AttemptID, input.OperationID)
		if updateErr != nil {
			return updateErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.ErrRepositoryLifecycleConflict
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	digest := repositoryDigest("repository-recheck-failure-v1", input.AttemptID, input.OperationID, string(input.Outcome), input.ReasonCode)
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET outcome=?,evidence_digest=?,result_digest=?,settled_at=? WHERE operation_id=? AND operation_type=? AND outcome='pending'`, string(input.Outcome), digest, digest, formatTime(input.SettledAt), input.OperationID, string(application.OperationRecheckRepository))
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.ErrOperationReceiptConflict
	}
	return tx.Commit()
}

func (s *Store) ChangeRepositoryLifecycle(ctx context.Context, input application.RepositoryLifecycleChange) (application.RepositoryProjection, application.OperationReceipt, error) {
	if input.Intent != application.RepositoryEnabled && input.Intent != application.RepositoryDisabled || input.ChangedAt.IsZero() {
		return application.RepositoryProjection{}, application.OperationReceipt{}, errors.New("repository lifecycle change is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	defer tx.Rollback()
	current, err := repositoryOperationAuthorityTx(ctx, tx, input.Expected.Lifecycle.Repository)
	if err != nil || !sameRepositoryOperationAuthority(current, input.Expected) {
		return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
	}
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, input.OperationID)
	expectedOperation := application.OperationEnableRepository
	if input.Intent == application.RepositoryDisabled {
		expectedOperation = application.OperationDisableRepository
	}
	if err != nil || !found || receipt.OperationType != expectedOperation || receipt.TargetID != current.Lifecycle.Repository || receipt.TargetBindingDigest != current.Lifecycle.RepositoryBindingDigest || receipt.Outcome != application.OperationOutcomePending {
		return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrOperationReceiptConflict
	}
	if input.Intent == application.RepositoryEnabled {
		effective := effectiveRepositorySnapshot(current.Lifecycle, current.Snapshot, current.ConfigurationAuthority)
		if current.Recheck != nil || effective.Status != domain.RepositoryReady {
			return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
		}
	}
	version := current.Lifecycle.Version
	if current.Lifecycle.Intent != input.Intent {
		version++
		result, err := tx.ExecContext(ctx, `UPDATE repository_lifecycles SET intent=?,lifecycle_version=?,updated_at=? WHERE incarnation_id=? AND retired_at='' AND removal_state='' AND lifecycle_version=? AND intent=?`, string(input.Intent), version, formatTime(input.ChangedAt), current.Lifecycle.IncarnationID, current.Lifecycle.Version, string(current.Lifecycle.Intent))
		if err != nil {
			return application.RepositoryProjection{}, application.OperationReceipt{}, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.RepositoryProjection{}, application.OperationReceipt{}, application.ErrRepositoryLifecycleConflict
		}
	}
	resultDigest := repositoryDigest("repository-lifecycle-result-v1", current.Lifecycle.Repository, string(input.Intent), fmt.Sprint(version))
	receipt, err = settleRepositoryReceiptTx(ctx, tx, input.OperationID, string(input.Intent), version, resultDigest, resultDigest, input.ChangedAt)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	projection, err := repositoryProjectionTx(ctx, tx, current.Lifecycle.Repository)
	if err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return application.RepositoryProjection{}, application.OperationReceipt{}, err
	}
	return projection, receipt, nil
}

func (s *Store) SettleRepositoryLifecycleFailure(ctx context.Context, input application.RepositoryLifecycleFailure) error {
	if strings.TrimSpace(input.OperationID) == "" || strings.TrimSpace(input.ReasonCode) == "" || input.SettledAt.IsZero() || input.Outcome != application.OperationOutcomeFailed && input.Outcome != application.OperationOutcomeConflict && input.Outcome != application.OperationOutcomeAmbiguous {
		return errors.New("repository lifecycle failure is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, input.OperationID)
	if err != nil || !found || receipt.Scope != application.ScopeRepository || receipt.OperationType != application.OperationEnableRepository && receipt.OperationType != application.OperationDisableRepository || receipt.Outcome != application.OperationOutcomePending {
		return application.ErrOperationReceiptConflict
	}
	digest := repositoryDigest("repository-lifecycle-failure-v1", input.OperationID, string(input.Outcome), input.ReasonCode)
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET outcome=?,evidence_digest=?,result_digest=?,settled_at=? WHERE operation_id=? AND outcome='pending'`, string(input.Outcome), digest, digest, formatTime(input.SettledAt), input.OperationID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.ErrOperationReceiptConflict
	}
	return tx.Commit()
}

func (s *Store) CheckRepositoryAdmission(ctx context.Context, profile application.LocalRepository) (application.RepositoryAdmissionDecision, error) {
	if !validRepositoryProfile(profile) {
		return application.RepositoryAdmissionDecision{Reason: "profile_configuration_invalid"}, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.RepositoryAdmissionDecision{}, err
	}
	defer tx.Rollback()
	decision, err := repositoryAdmissionDecisionTx(ctx, tx, profile)
	if err != nil {
		return application.RepositoryAdmissionDecision{}, err
	}
	return decision, tx.Commit()
}

// ReconcileRepositoryRechecks classifies work that was in flight before the
// current managed composition. Partial observations remain private evidence;
// no snapshot is published and the same receipt settles ambiguous.
func (s *Store) ReconcileRepositoryRechecks(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT attempt_id,operation_id FROM repository_recheck_attempts WHERE status='in_progress' ORDER BY attempt_id`)
	if err != nil {
		return err
	}
	type interrupted struct{ attemptID, operationID string }
	var attempts []interrupted
	for rows.Next() {
		var attempt interrupted
		if err := rows.Scan(&attempt.attemptID, &attempt.operationID); err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, attempt := range attempts {
		result, err := tx.ExecContext(ctx, `UPDATE repository_recheck_attempts SET status='ambiguous',settled_at=?,reason_code='controller_restarted' WHERE attempt_id=? AND status='in_progress'`, formatTime(at), attempt.attemptID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.ErrRepositoryLifecycleConflict
		}
		digest := repositoryDigest("repository-recheck-restart-v1", attempt.attemptID, attempt.operationID)
		receiptResult, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase=CASE WHEN phase='accepted' THEN 'accepted' ELSE 'applied' END,outcome='ambiguous',evidence_digest=?,result_digest=?,settled_at=? WHERE operation_id=? AND outcome='pending'`, digest, digest, formatTime(at), attempt.operationID)
		if err != nil {
			return err
		}
		if changed, _ := receiptResult.RowsAffected(); changed != 1 {
			return application.ErrOperationReceiptConflict
		}
	}
	return tx.Commit()
}

func requireRepositoryAdmissionAuthorityTx(ctx context.Context, tx *sql.Tx, profile application.LocalRepository, expected application.RepositoryEligibilityToken) error {
	var lifecycleTable int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='repository_lifecycle_baseline'`).Scan(&lifecycleTable); err != nil {
		return err
	}
	if lifecycleTable == 0 {
		return nil
	}
	var baseline int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_lifecycle_baseline WHERE authority_id=1`).Scan(&baseline); err != nil {
		return err
	}
	// A raw, uncomposed Store remains usable by deterministic development
	// fixtures. Every managed production composition adopts the baseline before
	// constructing admission services, after which this gate is mandatory.
	if baseline == 0 {
		return nil
	}
	decision, err := repositoryAdmissionDecisionTx(ctx, tx, profile)
	if err != nil || !decision.Allowed || !expected.Valid() || decision.Token != expected {
		return application.ErrRepositoryAdmissionConflict
	}
	return nil
}

func repositoryAdmissionDecisionTx(ctx context.Context, tx *sql.Tx, profile application.LocalRepository) (application.RepositoryAdmissionDecision, error) {
	var baseline int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_lifecycle_baseline WHERE authority_id=1`).Scan(&baseline); err != nil {
		return application.RepositoryAdmissionDecision{}, err
	}
	if baseline != 1 {
		return application.RepositoryAdmissionDecision{Reason: "lifecycle_baseline_unavailable"}, nil
	}
	authority, err := repositoryOperationAuthorityTx(ctx, tx, profile.CanonicalRepository)
	if errors.Is(err, application.ErrRepositoryLifecycleMissing) {
		return application.RepositoryAdmissionDecision{Reason: "lifecycle_authority_missing"}, nil
	}
	if err != nil {
		return application.RepositoryAdmissionDecision{}, err
	}
	if !profileMatchesLifecycle(profile, authority.Lifecycle) {
		return application.RepositoryAdmissionDecision{Reason: "profile_authority_stale"}, nil
	}
	if authority.Lifecycle.Intent != application.RepositoryEnabled {
		return application.RepositoryAdmissionDecision{Reason: "repository_disabled"}, nil
	}
	if authority.Removal != nil {
		return application.RepositoryAdmissionDecision{Reason: authority.Removal.State}, nil
	}
	if authority.Recheck != nil {
		return application.RepositoryAdmissionDecision{Reason: "readiness_recheck_in_progress"}, nil
	}
	if err := requireConfigurationAdmissionAuthorityTx(ctx, tx, authority.ConfigurationAuthority); err != nil {
		return application.RepositoryAdmissionDecision{Reason: "configuration_authority_unavailable"}, nil
	}
	effective := effectiveRepositorySnapshot(authority.Lifecycle, authority.Snapshot, authority.ConfigurationAuthority)
	if effective.Status != domain.RepositoryReady {
		return application.RepositoryAdmissionDecision{Reason: effective.ReasonCode}, nil
	}
	configuration, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || configuration.Incomplete != nil || configuration.IncompleteRecovery != nil || configuration.EffectiveID != configuration.Desired.GenerationID || configuration.Desired.State != application.ConfigurationGenerationEffective {
		return application.RepositoryAdmissionDecision{Reason: "configuration_authority_unavailable"}, nil
	}
	token := application.RepositoryEligibilityToken{Repository: authority.Lifecycle.Repository, LifecycleVersion: authority.Lifecycle.Version, SnapshotID: authority.Snapshot.SnapshotID, SnapshotDigest: authority.Snapshot.SnapshotDigest, ProfileDigest: authority.Lifecycle.ProfileDigest, RepositoryBindingDigest: authority.Lifecycle.RepositoryBindingDigest, ConfigurationGenerationID: authority.ConfigurationAuthority.GenerationID, ConfigurationDigest: authority.ConfigurationAuthority.Digest, ConfigurationAuthorityVersion: authority.ConfigurationAuthority.AuthorityVersion}
	return application.RepositoryAdmissionDecision{Allowed: true, Reason: "ready", Token: token}, nil
}

func repositoryOperationAuthorityTx(ctx context.Context, tx *sql.Tx, repository string) (application.RepositoryOperationAuthority, error) {
	lifecycle, err := repositoryLifecycleTx(ctx, tx, repository)
	if err != nil {
		return application.RepositoryOperationAuthority{}, err
	}
	var snapshotID string
	if err := tx.QueryRowContext(ctx, `SELECT current_snapshot_id FROM repository_lifecycles WHERE repository=? AND retired_at=''`, repository).Scan(&snapshotID); err != nil || snapshotID == "" {
		return application.RepositoryOperationAuthority{}, application.ErrRepositoryLifecycleMissing
	}
	snapshot, err := repositorySnapshotTx(ctx, tx, snapshotID)
	if err != nil {
		return application.RepositoryOperationAuthority{}, err
	}
	configuration, err := repositoryConfigurationAuthorityTx(ctx, tx)
	if err != nil {
		return application.RepositoryOperationAuthority{}, err
	}
	recheck, err := repositoryActiveRecheckTx(ctx, tx, repository)
	if err != nil {
		return application.RepositoryOperationAuthority{}, err
	}
	removal, err := repositoryRemovalProjectionTx(ctx, tx, lifecycle.IncarnationID)
	if err != nil {
		return application.RepositoryOperationAuthority{}, err
	}
	return application.RepositoryOperationAuthority{Lifecycle: lifecycle, Snapshot: snapshot, Recheck: recheck, ConfigurationAuthority: configuration, Removal: removal}, nil
}

func repositoryLifecycleTx(ctx context.Context, tx *sql.Tx, repository string) (application.RepositoryLifecycle, error) {
	var lifecycle application.RepositoryLifecycle
	var intent, updated, retired string
	err := tx.QueryRowContext(ctx, `SELECT incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,intent,lifecycle_version,updated_at,retired_at,retirement_evidence_digest FROM repository_lifecycles WHERE repository=? AND retired_at=''`, repository).Scan(&lifecycle.IncarnationID, &lifecycle.Repository, &lifecycle.ProfileID, &lifecycle.ProfileDigest, &lifecycle.RepositoryBindingDigest, &intent, &lifecycle.Version, &updated, &retired, &lifecycle.RetirementEvidenceDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return application.RepositoryLifecycle{}, application.ErrRepositoryLifecycleMissing
	}
	if err != nil {
		return application.RepositoryLifecycle{}, err
	}
	lifecycle.Intent, lifecycle.UpdatedAt, lifecycle.RetiredAt = application.RepositoryLifecycleIntent(intent), parseTime(updated), parseTime(retired)
	if lifecycle.Validate() != nil {
		return application.RepositoryLifecycle{}, errors.New("repository lifecycle evidence is corrupt")
	}
	return lifecycle, nil
}

func repositorySnapshotTx(ctx context.Context, tx *sql.Tx, snapshotID string) (application.RepositoryReadinessSnapshot, error) {
	var snapshot application.RepositoryReadinessSnapshot
	var status, observed, published string
	err := tx.QueryRowContext(ctx, `SELECT snapshot_id,incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,lifecycle_version,configuration_generation_id,configuration_digest,configuration_authority_version,overall_status,reason_code,snapshot_digest,observed_at,published_at FROM repository_readiness_snapshots WHERE snapshot_id=?`, snapshotID).Scan(&snapshot.SnapshotID, &snapshot.IncarnationID, &snapshot.Repository, &snapshot.ProfileID, &snapshot.ProfileDigest, &snapshot.RepositoryBindingDigest, &snapshot.LifecycleVersion, &snapshot.ConfigurationGenerationID, &snapshot.ConfigurationDigest, &snapshot.ConfigurationAuthorityVersion, &status, &snapshot.ReasonCode, &snapshot.SnapshotDigest, &observed, &published)
	if err != nil {
		return application.RepositoryReadinessSnapshot{}, err
	}
	snapshot.Status, snapshot.ObservedAt, snapshot.PublishedAt = domain.RepositoryReadinessStatus(status), parseTime(observed), parseTime(published)
	rows, err := tx.QueryContext(ctx, `SELECT dimension,status,reason_code,identity_id,evidence_digest,observed_at FROM repository_readiness_dimensions WHERE snapshot_id=?`, snapshotID)
	if err != nil {
		return application.RepositoryReadinessSnapshot{}, err
	}
	for rows.Next() {
		var result domain.RepositoryDimensionResult
		var dimension, resultStatus, resultObserved string
		if err := rows.Scan(&dimension, &resultStatus, &result.ReasonCode, &result.Identity, &result.EvidenceDigest, &resultObserved); err != nil {
			rows.Close()
			return application.RepositoryReadinessSnapshot{}, err
		}
		result.Dimension, result.Status, result.ObservedAt = domain.RepositoryReadinessDimension(dimension), domain.RepositoryReadinessStatus(resultStatus), parseTime(resultObserved)
		snapshot.Dimensions = append(snapshot.Dimensions, result)
	}
	if err := rows.Close(); err != nil {
		return application.RepositoryReadinessSnapshot{}, err
	}
	if snapshot.Validate() != nil {
		return application.RepositoryReadinessSnapshot{}, errors.New("repository readiness snapshot is corrupt")
	}
	return snapshot, nil
}

func repositoryActiveRecheckTx(ctx context.Context, tx *sql.Tx, repository string) (*application.RepositoryRecheckState, error) {
	var state application.RepositoryRecheckState
	var started string
	err := tx.QueryRowContext(ctx, `SELECT attempt_id,operation_id,started_at FROM repository_recheck_attempts WHERE repository=? AND status='in_progress'`, repository).Scan(&state.AttemptID, &state.OperationID, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state.Refreshing, state.StartedAt = true, parseTime(started)
	return &state, nil
}

func repositoryConfigurationAuthorityTx(ctx context.Context, tx *sql.Tx) (application.ConfigurationAdmissionAuthority, error) {
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || authority.Desired.GenerationID < 1 || !validRepositoryDigest(authority.Desired.Digest) || authority.Version < 1 {
		if err == nil {
			err = application.ErrConfigurationAuthorityConflict
		}
		return application.ConfigurationAdmissionAuthority{}, err
	}
	return application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func repositoryProjectionTx(ctx context.Context, tx *sql.Tx, repository string) (application.RepositoryProjection, error) {
	authority, err := repositoryOperationAuthorityTx(ctx, tx, repository)
	if err != nil {
		return application.RepositoryProjection{}, err
	}
	projection := application.RepositoryProjection{Lifecycle: authority.Lifecycle, Readiness: effectiveRepositorySnapshot(authority.Lifecycle, authority.Snapshot, authority.ConfigurationAuthority), Recheck: authority.Recheck, Removal: authority.Removal}
	projection.Availability = application.RepositoryAvailability{Available: true, ReasonCode: "available"}
	if authority.Removal != nil {
		projection.Availability.Available, projection.Availability.ReasonCode = false, authority.Removal.State
	} else if authority.Recheck != nil {
		projection.Availability.Available, projection.Availability.ReasonCode = false, "readiness_recheck_in_progress"
	} else if authority.Lifecycle.Intent == application.RepositoryDisabled {
		projection.Availability.Available, projection.Availability.ReasonCode = false, "repository_disabled"
	} else if projection.Readiness.Status != domain.RepositoryReady {
		projection.Availability.Available, projection.Availability.ReasonCode = false, projection.Readiness.ReasonCode
	}
	var activeRun string
	_ = tx.QueryRowContext(ctx, `SELECT run_id FROM repository_slots WHERE repository_binding_digest=?`, authority.Lifecycle.RepositoryBindingDigest).Scan(&activeRun)
	if activeRun != "" {
		projection.Availability.Available, projection.Availability.ReasonCode, projection.Availability.ActiveRun = false, "repository_busy", activeRun
	}
	return projection, nil
}

func effectiveRepositorySnapshot(lifecycle application.RepositoryLifecycle, snapshot application.RepositoryReadinessSnapshot, configuration application.ConfigurationAdmissionAuthority) application.RepositoryReadinessSnapshot {
	result := snapshot
	result.Dimensions = append([]domain.RepositoryDimensionResult(nil), snapshot.Dimensions...)
	staleDimension := func(dimension domain.RepositoryReadinessDimension, reason string) {
		for index := range result.Dimensions {
			if result.Dimensions[index].Dimension == dimension {
				result.Dimensions[index].Status = domain.RepositoryUnknown
				result.Dimensions[index].ReasonCode = reason
			}
		}
		result.Status, result.ReasonCode = domain.RepositoryUnknown, reason
	}
	if snapshot.ProfileID != lifecycle.ProfileID || snapshot.ProfileDigest != lifecycle.ProfileDigest || snapshot.RepositoryBindingDigest != lifecycle.RepositoryBindingDigest {
		staleDimension(domain.ReadinessProfileConfiguration, "profile_authority_stale")
		return result
	}
	if snapshot.ConfigurationGenerationID != configuration.GenerationID || snapshot.ConfigurationDigest != configuration.Digest || snapshot.ConfigurationAuthorityVersion != configuration.AuthorityVersion {
		staleDimension(domain.ReadinessConfigurationConvergence, "configuration_authority_stale")
	}
	return result
}

func buildRepositorySnapshot(lifecycle application.RepositoryLifecycle, configuration application.ConfigurationAdmissionAuthority, results []domain.RepositoryDimensionResult, publishedAt time.Time, reason string) (application.RepositoryReadinessSnapshot, error) {
	overall, err := domain.AggregateRepositoryReadiness(results)
	if err != nil {
		return application.RepositoryReadinessSnapshot{}, err
	}
	observedAt := results[0].ObservedAt.UTC()
	for _, result := range results[1:] {
		if result.ObservedAt.Before(observedAt) {
			observedAt = result.ObservedAt.UTC()
		}
	}
	dimensionsDigest, _ := domain.RepositoryReadinessDigest(results)
	digest := repositoryDigest("repository-snapshot-v2", lifecycle.IncarnationID, lifecycle.Repository, lifecycle.ProfileID, lifecycle.ProfileDigest, lifecycle.RepositoryBindingDigest, fmt.Sprint(lifecycle.Version), fmt.Sprint(configuration.GenerationID), configuration.Digest, fmt.Sprint(configuration.AuthorityVersion), string(overall), reason, dimensionsDigest, formatTime(observedAt), formatTime(publishedAt))
	snapshot := application.RepositoryReadinessSnapshot{SnapshotID: "repository-snapshot-" + digest[:24], IncarnationID: lifecycle.IncarnationID, Repository: lifecycle.Repository, ProfileID: lifecycle.ProfileID, ProfileDigest: lifecycle.ProfileDigest, RepositoryBindingDigest: lifecycle.RepositoryBindingDigest, LifecycleVersion: lifecycle.Version, ConfigurationGenerationID: configuration.GenerationID, ConfigurationDigest: configuration.Digest, ConfigurationAuthorityVersion: configuration.AuthorityVersion, Status: overall, ReasonCode: reason, SnapshotDigest: digest, Dimensions: append([]domain.RepositoryDimensionResult(nil), results...), ObservedAt: observedAt, PublishedAt: publishedAt.UTC()}
	return snapshot, snapshot.Validate()
}

func insertRepositorySnapshotTx(ctx context.Context, tx *sql.Tx, snapshot application.RepositoryReadinessSnapshot) error {
	if snapshot.Validate() != nil {
		return errors.New("repository snapshot publication is invalid")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_readiness_snapshots(snapshot_id,incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,lifecycle_version,configuration_generation_id,configuration_digest,configuration_authority_version,overall_status,reason_code,snapshot_digest,observed_at,published_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, snapshot.SnapshotID, snapshot.IncarnationID, snapshot.Repository, snapshot.ProfileID, snapshot.ProfileDigest, snapshot.RepositoryBindingDigest, snapshot.LifecycleVersion, snapshot.ConfigurationGenerationID, snapshot.ConfigurationDigest, snapshot.ConfigurationAuthorityVersion, string(snapshot.Status), snapshot.ReasonCode, snapshot.SnapshotDigest, formatTime(snapshot.ObservedAt), formatTime(snapshot.PublishedAt)); err != nil {
		return err
	}
	for _, result := range snapshot.Dimensions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO repository_readiness_dimensions(snapshot_id,dimension,status,reason_code,identity_id,evidence_digest,observed_at) VALUES(?,?,?,?,?,?,?)`, snapshot.SnapshotID, string(result.Dimension), string(result.Status), result.ReasonCode, result.Identity, result.EvidenceDigest, formatTime(result.ObservedAt)); err != nil {
			return err
		}
	}
	return nil
}

func repositoryRecheckObservationsTx(ctx context.Context, tx *sql.Tx, attemptID string) ([]domain.RepositoryDimensionResult, error) {
	rows, err := tx.QueryContext(ctx, `SELECT dimension,status,reason_code,identity_id,evidence_digest,observed_at FROM repository_recheck_observations WHERE attempt_id=?`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []domain.RepositoryDimensionResult
	for rows.Next() {
		var result domain.RepositoryDimensionResult
		var dimension, status, observed string
		if err := rows.Scan(&dimension, &status, &result.ReasonCode, &result.Identity, &result.EvidenceDigest, &observed); err != nil {
			return nil, err
		}
		result.Dimension, result.Status, result.ObservedAt = domain.RepositoryReadinessDimension(dimension), domain.RepositoryReadinessStatus(status), parseTime(observed)
		results = append(results, result)
	}
	return results, rows.Err()
}

func settleRepositoryReceiptTx(ctx context.Context, tx *sql.Tx, operationID, state string, version int64, evidenceDigest, resultDigest string, at time.Time) (application.OperationReceipt, error) {
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, operationID)
	if err != nil || !found {
		return application.OperationReceipt{}, application.ErrOperationReceiptConflict
	}
	if receipt.Outcome == application.OperationOutcomeSucceeded {
		return receipt, nil
	}
	if receipt.Outcome != application.OperationOutcomePending || receipt.Phase != application.OperationPhaseAccepted && receipt.Phase != application.OperationPhaseApplied {
		return application.OperationReceipt{}, application.ErrOperationReceiptConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='observed',outcome='succeeded',resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,applied_at=CASE WHEN applied_at='' THEN ? ELSE applied_at END,settled_at=? WHERE operation_id=? AND outcome='pending'`, state, version, evidenceDigest, resultDigest, formatTime(at), formatTime(at), operationID)
	if err != nil {
		return application.OperationReceipt{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.OperationReceipt{}, application.ErrOperationReceiptConflict
	}
	updated, _, err := getOperationReceiptByIDTx(ctx, tx, operationID)
	return updated, err
}

func initialRepositoryReadiness(at time.Time, lifecycle application.RepositoryLifecycle) []domain.RepositoryDimensionResult {
	results := make([]domain.RepositoryDimensionResult, 0, len(domain.RepositoryReadinessDimensions))
	for _, dimension := range domain.RepositoryReadinessDimensions {
		results = append(results, domain.RepositoryDimensionResult{Dimension: dimension, Status: domain.RepositoryUnknown, ReasonCode: "initial_recheck_required", EvidenceDigest: repositoryDigest("repository-initial-readiness-v1", lifecycle.Repository, string(dimension), lifecycle.ProfileDigest, lifecycle.RepositoryBindingDigest), ObservedAt: at.UTC()})
	}
	return results
}

func insertInitialRepositoryLifecycleTx(ctx context.Context, tx *sql.Tx, lifecycle application.RepositoryLifecycle, configuration application.ConfigurationAdmissionAuthority, at time.Time) error {
	if lifecycle.Validate() != nil {
		return errors.New("repository lifecycle baseline is invalid")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_lifecycles(incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,intent,lifecycle_version,current_snapshot_id,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, lifecycle.IncarnationID, lifecycle.Repository, lifecycle.ProfileID, lifecycle.ProfileDigest, lifecycle.RepositoryBindingDigest, string(lifecycle.Intent), lifecycle.Version, "", formatTime(lifecycle.UpdatedAt)); err != nil {
		return err
	}
	results := initialRepositoryReadiness(at, lifecycle)
	snapshot, err := buildRepositorySnapshot(lifecycle, configuration, results, at, "initial_recheck_required")
	if err != nil {
		return err
	}
	if err := insertRepositorySnapshotTx(ctx, tx, snapshot); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE repository_lifecycles SET current_snapshot_id=? WHERE incarnation_id=? AND current_snapshot_id=''`, snapshot.SnapshotID, lifecycle.IncarnationID)
	return err
}

func sameRepositoryOperationAuthority(left, right application.RepositoryOperationAuthority) bool {
	return sameRepositoryBaseAuthority(left, right) && (left.Recheck == nil) == (right.Recheck == nil) && (left.Recheck == nil || left.Recheck.AttemptID == right.Recheck.AttemptID) && (left.Removal == nil) == (right.Removal == nil) && (left.Removal == nil || left.Removal.OperationID == right.Removal.OperationID && left.Removal.State == right.Removal.State)
}

func sameRepositoryBaseAuthority(left, right application.RepositoryOperationAuthority) bool {
	return left.Lifecycle.IncarnationID == right.Lifecycle.IncarnationID && left.Lifecycle.Repository == right.Lifecycle.Repository && left.Lifecycle.ProfileID == right.Lifecycle.ProfileID && left.Lifecycle.ProfileDigest == right.Lifecycle.ProfileDigest && left.Lifecycle.RepositoryBindingDigest == right.Lifecycle.RepositoryBindingDigest && left.Lifecycle.Intent == right.Lifecycle.Intent && left.Lifecycle.Version == right.Lifecycle.Version && left.Snapshot.SnapshotID == right.Snapshot.SnapshotID && left.Snapshot.SnapshotDigest == right.Snapshot.SnapshotDigest && left.ConfigurationAuthority.GenerationID == right.ConfigurationAuthority.GenerationID && left.ConfigurationAuthority.Digest == right.ConfigurationAuthority.Digest && left.ConfigurationAuthority.AuthorityVersion == right.ConfigurationAuthority.AuthorityVersion
}

func sameDimensionResults(left, right []domain.RepositoryDimensionResult) bool {
	if len(left) != len(right) {
		return false
	}
	for _, dimension := range domain.RepositoryReadinessDimensions {
		li := slices.IndexFunc(left, func(result domain.RepositoryDimensionResult) bool { return result.Dimension == dimension })
		ri := slices.IndexFunc(right, func(result domain.RepositoryDimensionResult) bool { return result.Dimension == dimension })
		if li < 0 || ri < 0 || left[li] != right[ri] {
			return false
		}
	}
	return true
}

func validRepositoryProfile(profile application.LocalRepository) bool {
	return strings.TrimSpace(profile.CanonicalRepository) != "" && strings.TrimSpace(profile.ProfileID) != "" && validRepositoryDigest(profile.ProfileDigest) && validRepositoryDigest(profile.RepositoryBindingDigest) && profile.ProfileSnapshotVersion > 0 && profile.RegistryVersion > 0
}

func profileMatchesLifecycle(profile application.LocalRepository, lifecycle application.RepositoryLifecycle) bool {
	return profile.CanonicalRepository == lifecycle.Repository && profile.ProfileID == lifecycle.ProfileID && profile.ProfileDigest == lifecycle.ProfileDigest && profile.RepositoryBindingDigest == lifecycle.RepositoryBindingDigest
}

func validRepositoryDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func repositoryDigest(namespace string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(namespace))
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func repositoryProfilesDigest(profiles []application.RepositoryProfileAuthority) string {
	parts := make([]string, 0, len(profiles)*4)
	for _, profile := range profiles {
		parts = append(parts, profile.Authority.Repository, profile.Authority.ProfileID, profile.Profile.ProfileDigest, profile.Authority.BindingDigest)
	}
	return repositoryDigest("repository-baseline-profiles-v1", parts...)
}

func repositoryIncarnationID(repository, binding string, adoptedAt time.Time) string {
	digest := repositoryDigest("repository-incarnation-v1", repository, binding, formatTime(adoptedAt))
	return "repository-incarnation-" + digest[:32]
}

func nextRepositoryIncarnationIDTx(ctx context.Context, tx *sql.Tx, repository, binding string, adoptedAt time.Time) (string, error) {
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_lifecycles WHERE repository=?`, repository).Scan(&count); err != nil {
		return "", err
	}
	return repositoryIncarnationID(repository, binding, adoptedAt.Add(time.Duration(count))), nil
}

func repositoryPlaceholders(count int) string {
	if count == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func repositoryNames(profiles []application.RepositoryProfileAuthority) []any {
	values := make([]any, 0, len(profiles))
	for _, profile := range profiles {
		values = append(values, profile.Authority.Repository)
	}
	return values
}

func repositorySnapshotReason(results []domain.RepositoryDimensionResult, overall domain.RepositoryReadinessStatus) string {
	if overall == domain.RepositoryReady {
		return "ready"
	}
	for _, status := range []domain.RepositoryReadinessStatus{domain.RepositoryConflict, domain.RepositoryUnknown, domain.RepositoryNotReady} {
		if overall != status {
			continue
		}
		for _, dimension := range domain.RepositoryReadinessDimensions {
			index := slices.IndexFunc(results, func(result domain.RepositoryDimensionResult) bool {
				return result.Dimension == dimension && result.Status == status
			})
			if index >= 0 {
				return results[index].ReasonCode
			}
		}
	}
	return "unknown"
}

func encodeRepositoryCursor(scopeDigest, repository string) string {
	payload, _ := json.Marshal(struct{ Scope, Repository string }{scopeDigest, repository})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeRepositoryCursor(cursor, scopeDigest string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", errors.New("repository cursor is invalid")
	}
	var value struct{ Scope, Repository string }
	if json.Unmarshal(payload, &value) != nil || value.Scope != scopeDigest || strings.TrimSpace(value.Repository) == "" {
		return "", errors.New("repository cursor authority changed")
	}
	return value.Repository, nil
}
