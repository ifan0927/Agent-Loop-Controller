package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func (s *Store) ListAuthorizedOnboardings(ctx context.Context, input application.AuthorizedOnboardingQuery) (application.AuthorizedOnboardingPage, error) {
	if input.Requester.Validate() != nil || input.Scopes.Empty() || input.Limit < 1 || input.Limit > application.RoutineQueryMaximumLimit+1 || input.BeforeUpdatedAt.IsZero() != (input.BeforeOnboardingID == "") {
		return application.AuthorizedOnboardingPage{}, errors.New("authorized onboarding collection is invalid")
	}
	bindings := input.Scopes.RepositoryBindingDigests()
	where := `(repository_binding_digest='' AND lower(requester_login)=lower(?) AND requester_database_id=? AND requester_node_id=? AND requester_actor_type=?)`
	args := []any{input.Requester.Login, input.Requester.DatabaseID, input.Requester.NodeID, input.Requester.ActorType}
	if len(bindings) != 0 {
		where += ` OR repository_binding_digest IN (` + strings.TrimSuffix(strings.Repeat("?,", len(bindings)), ",") + `)`
		for _, binding := range bindings {
			args = append(args, binding)
		}
	}
	where = `(` + where + `)`
	if input.Repository != "" {
		where += ` AND canonical_repository=?`
		args = append(args, input.Repository)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.AuthorizedOnboardingPage{}, err
	}
	defer tx.Rollback()
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_onboardings WHERE `+where, args...).Scan(&total); err != nil {
		return application.AuthorizedOnboardingPage{}, err
	}
	pageWhere := where
	pageArgs := append([]any(nil), args...)
	if !input.BeforeUpdatedAt.IsZero() {
		pageWhere += ` AND (updated_at<? OR (updated_at=? AND onboarding_id<?))`
		formatted := formatTime(input.BeforeUpdatedAt)
		pageArgs = append(pageArgs, formatted, formatted, input.BeforeOnboardingID)
	}
	pageArgs = append(pageArgs, input.Limit)
	rows, err := tx.QueryContext(ctx, `SELECT onboarding_id FROM repository_onboardings WHERE `+pageWhere+` ORDER BY updated_at DESC,onboarding_id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return application.AuthorizedOnboardingPage{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return application.AuthorizedOnboardingPage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return application.AuthorizedOnboardingPage{}, err
	}
	page := application.AuthorizedOnboardingPage{Total: total}
	for _, id := range ids {
		value, found, err := onboardingByID(ctx, tx, id)
		if err != nil || !found {
			return application.AuthorizedOnboardingPage{}, fmt.Errorf("authorized onboarding snapshot conflicts")
		}
		value.CompletedSteps, err = onboardingCompletedSteps(ctx, tx, id)
		if err != nil {
			return application.AuthorizedOnboardingPage{}, err
		}
		page.Onboardings = append(page.Onboardings, value)
	}
	return page, tx.Commit()
}

func (s *Store) CurrentRepositoryOnboarding(ctx context.Context, repository string) (application.Onboarding, bool, error) {
	if strings.TrimSpace(repository) == "" {
		return application.Onboarding{}, false, errors.New("repository onboarding target is invalid")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.Onboarding{}, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT onboarding_id FROM repository_onboardings WHERE canonical_repository=? AND repository_binding_digest<>'' ORDER BY updated_at DESC,onboarding_id DESC LIMIT 1`, repository).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return application.Onboarding{}, false, nil
	}
	if err != nil {
		return application.Onboarding{}, false, err
	}
	value, found, err := onboardingByID(ctx, tx, id)
	if err != nil || !found {
		return application.Onboarding{}, false, err
	}
	value.CompletedSteps, err = onboardingCompletedSteps(ctx, tx, id)
	if err != nil {
		return application.Onboarding{}, false, err
	}
	return value, true, tx.Commit()
}

const onboardingSelect = `SELECT onboarding_id,onboarding_kind,canonical_repository,private_input_digest,source_path_digest,request_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,base_generation_id,base_digest,configuration_authority_version,status,reason_code,preflight_digest,preview_digest,COALESCE(operation_id,''),profile_id,profile_digest,repository_binding_digest,configuration_generation_id,incarnation_id,readiness_snapshot_id,linear_label_id,initial_revision_sha,accepted_at,created_at,updated_at,settled_at FROM repository_onboardings`

func (s *Store) OpenOnboarding(ctx context.Context, input application.OnboardingOpenInput) (application.Onboarding, bool, error) {
	if !validOnboardingOpen(input) {
		return application.Onboarding{}, false, errors.New("onboarding open input is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Onboarding{}, false, err
	}
	defer tx.Rollback()
	if existing, found, err := onboardingByID(ctx, tx, input.OnboardingID); err != nil {
		return application.Onboarding{}, false, err
	} else if found {
		claims, claimsErr := onboardingPathClaims(ctx, tx, input.OnboardingID)
		if claimsErr != nil || !sameOnboardingOpen(existing, input) || !slices.Equal(claims, input.SourceAncestorDigests) {
			return application.Onboarding{}, false, application.ErrOnboardingConflict
		}
		return existing, false, tx.Commit()
	}
	activeMutations, err := activeOnboardingConfigurationMutations(ctx, tx)
	if err != nil || activeMutations != 0 {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	var currentLifecycle int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_lifecycles WHERE repository=? AND retired_at=''`, input.CanonicalRepository).Scan(&currentLifecycle); err != nil || currentLifecycle != 0 {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(input.SourceAncestorDigests)), ",")
	arguments := make([]any, 0, len(input.SourceAncestorDigests)+1)
	for _, digest := range input.SourceAncestorDigests {
		arguments = append(arguments, digest)
	}
	arguments = append(arguments, input.SourcePathDigest)
	var overlaps int
	overlapQuery := `SELECT COUNT(*) FROM repository_onboardings o WHERE o.status NOT IN ('cancelled','conflict','ready_disabled') AND (o.source_path_digest IN (` + placeholders + `) OR EXISTS(SELECT 1 FROM repository_onboarding_path_claims c WHERE c.onboarding_id=o.onboarding_id AND c.path_digest=?))`
	if err := tx.QueryRowContext(ctx, overlapQuery, arguments...).Scan(&overlaps); err != nil || overlaps != 0 {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO repository_onboardings(onboarding_id,onboarding_kind,canonical_repository,private_input_digest,source_path_digest,request_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,base_generation_id,base_digest,configuration_authority_version,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'opened',?,?)`, input.OnboardingID, string(input.Kind), input.CanonicalRepository, input.PrivateInputDigest, input.SourcePathDigest, input.RequestDigest, input.Requester.Login, input.Requester.DatabaseID, input.Requester.NodeID, input.Requester.ActorType, input.ConfigurationBaseGenerationID, input.ConfigurationBaseDigest, input.ConfigurationAuthorityVersion, formatTime(input.OpenedAt), formatTime(input.OpenedAt))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return application.Onboarding{}, false, application.ErrOnboardingConflict
		}
		return application.Onboarding{}, false, err
	}
	for _, digest := range input.SourceAncestorDigests {
		if _, err := tx.ExecContext(ctx, `INSERT INTO repository_onboarding_path_claims(onboarding_id,path_digest) VALUES(?,?)`, input.OnboardingID, digest); err != nil {
			return application.Onboarding{}, false, err
		}
	}
	opened, found, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil || !found {
		return application.Onboarding{}, false, errors.New("onboarding open settlement is unavailable")
	}
	return opened, true, tx.Commit()
}

func (s *Store) Onboarding(ctx context.Context, onboardingID string) (application.Onboarding, bool, error) {
	value, found, err := onboardingByID(ctx, s.db, onboardingID)
	if err != nil || !found {
		return value, found, err
	}
	steps, err := onboardingCompletedSteps(ctx, s.db, onboardingID)
	value.CompletedSteps = steps
	plan, ok := domain.OnboardingStepPlan(value.Kind)
	if !ok || len(steps) > len(plan) {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	for index := range steps {
		if steps[index] != plan[index] {
			return application.Onboarding{}, false, application.ErrOnboardingConflict
		}
	}
	return value, true, err
}

func (s *Store) SaveOnboardingPreflight(ctx context.Context, input application.OnboardingPreflightInput) (application.Onboarding, error) {
	if input.OnboardingID == "" || !validConfigurationDigest(input.PreflightDigest) || !validConfigurationDigest(input.EvidenceDigest) || input.ObservedAt.IsZero() || input.ExpectedStatus != domain.OnboardingOpened && input.ExpectedStatus != domain.OnboardingPreflightReady {
		return application.Onboarding{}, errors.New("onboarding preflight input is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Onboarding{}, err
	}
	defer tx.Rollback()
	current, found, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil || !found {
		return application.Onboarding{}, application.ErrOnboardingNotFound
	}
	if current.Status == domain.OnboardingPreflightReady && current.PreflightDigest == input.PreflightDigest {
		return current, tx.Commit()
	}
	if current.Status != input.ExpectedStatus || !domain.OnboardingCanCancel(current.Status) {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE repository_onboardings SET status='preflight_ready',preflight_digest=?,preflight_evidence_digest=?,reason_code='',updated_at=? WHERE onboarding_id=? AND status=?`, input.PreflightDigest, input.EvidenceDigest, formatTime(input.ObservedAt), input.OnboardingID, string(input.ExpectedStatus))
	if err != nil {
		return application.Onboarding{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	updated, _, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil {
		return application.Onboarding{}, err
	}
	return updated, tx.Commit()
}

func (s *Store) StartOnboarding(ctx context.Context, input application.OnboardingStartAcceptance) (application.Onboarding, application.OperationReceipt, bool, error) {
	if input.OnboardingID == "" || input.Expected.Status != domain.OnboardingPreflightReady || input.PreflightDigest != input.Expected.PreflightDigest || !validConfigurationDigest(input.PreviewDigest) || application.ValidateOperationReceipt(input.Receipt) != nil || input.Receipt.OperationType != application.OperationOnboardRepository || input.Receipt.Scope != application.ScopeOnboarding || input.Receipt.TargetID != input.OnboardingID || input.Profile.CanonicalRepository != input.Expected.CanonicalRepository {
		return application.Onboarding{}, application.OperationReceipt{}, false, errors.New("onboarding start acceptance is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Onboarding{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	current, found, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil || !found {
		return application.Onboarding{}, application.OperationReceipt{}, false, application.ErrOnboardingNotFound
	}
	if current.OperationID != "" {
		receipt, receiptFound, receiptErr := getOperationReceiptByIDTx(ctx, tx, current.OperationID)
		if receiptErr == nil && receiptFound && current.OperationID == input.Receipt.OperationID && current.PreflightDigest == input.PreflightDigest && current.PreviewDigest == input.PreviewDigest && current.ProfileDigest == input.Profile.ProfileDigest && current.RepositoryBindingDigest == input.Profile.RepositoryBindingDigest {
			return current, receipt, false, tx.Commit()
		}
		return application.Onboarding{}, application.OperationReceipt{}, false, application.ErrOnboardingConflict
	}
	if current.Status != domain.OnboardingPreflightReady || current.RequestDigest != input.Expected.RequestDigest || current.PrivateInputDigest != input.Expected.PrivateInputDigest || current.ConfigurationBaseGenerationID != input.Expected.ConfigurationBaseGenerationID || current.ConfigurationBaseDigest != input.Expected.ConfigurationBaseDigest || current.ConfigurationAuthorityVersion != input.Expected.ConfigurationAuthorityVersion {
		return application.Onboarding{}, application.OperationReceipt{}, false, application.ErrOnboardingConflict
	}
	activeMutations, mutationErr := activeOnboardingConfigurationMutations(ctx, tx)
	if mutationErr != nil || activeMutations != 0 {
		return application.Onboarding{}, application.OperationReceipt{}, false, application.ErrOnboardingConflict
	}
	var currentLifecycle int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_lifecycles WHERE repository=? AND retired_at=''`, current.CanonicalRepository).Scan(&currentLifecycle); err != nil || currentLifecycle != 0 {
		return application.Onboarding{}, application.OperationReceipt{}, false, application.ErrOnboardingConflict
	}
	if err := insertOperationReceiptTx(ctx, tx, input.Receipt, ""); err != nil {
		return application.Onboarding{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE repository_onboardings SET status='accepted',preview_digest=?,operation_id=?,profile_id=?,profile_digest=?,repository_binding_digest=?,accepted_at=?,updated_at=? WHERE onboarding_id=? AND status='preflight_ready'`, input.PreviewDigest, input.Receipt.OperationID, input.Profile.ProfileID, input.Profile.ProfileDigest, input.Profile.RepositoryBindingDigest, formatTime(input.AcceptedAt.UTC().Truncate(time.Second)), formatTime(input.AcceptedAt), input.OnboardingID)
	if err != nil {
		return application.Onboarding{}, application.OperationReceipt{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.Onboarding{}, application.OperationReceipt{}, false, application.ErrOnboardingConflict
	}
	started, _, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil {
		return application.Onboarding{}, application.OperationReceipt{}, false, err
	}
	return started, input.Receipt, true, tx.Commit()
}

func (s *Store) CancelOnboarding(ctx context.Context, onboardingID string, at time.Time) (application.Onboarding, bool, error) {
	return s.setOnboardingTerminal(ctx, onboardingID, at, domain.OnboardingCancelled, "cancelled_before_start")
}

func (s *Store) ResumeOnboarding(ctx context.Context, onboardingID string, at time.Time) (application.Onboarding, bool, error) {
	if onboardingID == "" || at.IsZero() {
		return application.Onboarding{}, false, errors.New("onboarding resume input is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Onboarding{}, false, err
	}
	defer tx.Rollback()
	current, found, err := onboardingByID(ctx, tx, onboardingID)
	if err != nil || !found {
		return application.Onboarding{}, false, application.ErrOnboardingNotFound
	}
	if current.Status == domain.OnboardingRunning {
		return current, false, tx.Commit()
	}
	if current.Status != domain.OnboardingWaitingForOperator {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repository_onboarding_steps WHERE onboarding_id=? AND step_order=? AND outcome IN ('failed','pending')`, onboardingID, currentStepIndex(ctx, tx, onboardingID)+1); err != nil {
		return application.Onboarding{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE repository_onboardings SET status='running',reason_code='',updated_at=? WHERE onboarding_id=? AND status='waiting_for_operator'`, formatTime(at), onboardingID)
	if err != nil {
		return application.Onboarding{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	updated, _, err := onboardingByID(ctx, tx, onboardingID)
	return updated, true, commitOrError(tx, err)
}

func (s *Store) BeginOnboardingStep(ctx context.Context, input application.OnboardingStepIntent) (bool, error) {
	if input.OnboardingID == "" || !validConfigurationDigest(input.IntentDigest) || input.IntendedAt.IsZero() {
		return false, errors.New("onboarding step intent is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	current, found, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil || !found {
		return false, application.ErrOnboardingNotFound
	}
	order := onboardingStepOrder(current.Kind, input.Step)
	if order == 0 {
		return false, application.ErrOnboardingConflict
	}
	if current.Status != domain.OnboardingAccepted && current.Status != domain.OnboardingRunning || currentStepIndex(ctx, tx, input.OnboardingID)+1 != order {
		return false, application.ErrOnboardingConflict
	}
	var digest, status string
	err = tx.QueryRowContext(ctx, `SELECT intent_digest,status FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name=?`, input.OnboardingID, string(input.Step)).Scan(&digest, &status)
	if err == nil {
		if digest == input.IntentDigest && status == "intended" {
			return false, tx.Commit()
		}
		return false, application.ErrOnboardingConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_onboarding_steps(onboarding_id,step_name,step_order,intent_digest,status,outcome,intended_at) VALUES(?,?,?,?,'intended','pending',?)`, input.OnboardingID, string(input.Step), order, input.IntentDigest, formatTime(input.IntendedAt)); err != nil {
		return false, application.ErrOnboardingConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repository_onboardings SET status='running',updated_at=? WHERE onboarding_id=?`, formatTime(input.IntendedAt), input.OnboardingID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) SettleOnboardingStep(ctx context.Context, input application.OnboardingStepSettlement) (application.Onboarding, error) {
	if input.OnboardingID == "" || input.ObservedAt.IsZero() || !validConfigurationDigest(input.Observation.EvidenceDigest) || input.Observation.Outcome == application.OperationOutcomePending && input.Observation.ReasonCode == "" {
		return application.Onboarding{}, errors.New("onboarding step settlement is invalid")
	}
	if input.Observation.LinearLabelID != "" && (input.Step != domain.OnboardingStepLinearLabelObserved || len(input.Observation.LinearLabelID) > 128 || strings.ContainsRune(input.Observation.LinearLabelID, '\x00')) || input.Step == domain.OnboardingStepLinearLabelObserved && input.Observation.Outcome == application.OperationOutcomeSucceeded && input.Observation.LinearLabelID == "" {
		return application.Onboarding{}, errors.New("onboarding label settlement is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Onboarding{}, err
	}
	defer tx.Rollback()
	current, found, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil || !found {
		return application.Onboarding{}, application.ErrOnboardingNotFound
	}
	order := onboardingStepOrder(current.Kind, input.Step)
	if order == 0 {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	if input.Observation.InitialRevisionSHA != "" && (input.Step != domain.OnboardingStepInitialRevisionCreated || input.Observation.Outcome != application.OperationOutcomeSucceeded || !validOnboardingSHA(input.Observation.InitialRevisionSHA)) || input.Step == domain.OnboardingStepInitialRevisionCreated && input.Observation.Outcome == application.OperationOutcomeSucceeded && !validOnboardingSHA(input.Observation.InitialRevisionSHA) {
		return application.Onboarding{}, errors.New("onboarding initial revision settlement is invalid")
	}
	if input.Step == domain.OnboardingStepInitialBasePublished && input.Observation.Outcome == application.OperationOutcomeSucceeded && !validOnboardingSHA(current.InitialRevisionSHA) {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	var status, outcome, evidence string
	if err := tx.QueryRowContext(ctx, `SELECT status,outcome,evidence_digest FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name=? AND step_order=?`, input.OnboardingID, string(input.Step), order).Scan(&status, &outcome, &evidence); err != nil {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	if status == "observed" {
		if outcome == string(input.Observation.Outcome) && evidence == input.Observation.EvidenceDigest {
			return current, tx.Commit()
		}
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	if status != "intended" || currentStepIndex(ctx, tx, input.OnboardingID)+1 != order {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repository_onboarding_steps SET status='observed',outcome=?,reason_code=?,evidence_digest=?,observed_at=? WHERE onboarding_id=? AND step_name=? AND status='intended'`, string(input.Observation.Outcome), input.Observation.ReasonCode, input.Observation.EvidenceDigest, formatTime(input.ObservedAt), input.OnboardingID, string(input.Step)); err != nil {
		return application.Onboarding{}, err
	}
	if input.Step == domain.OnboardingStepConfigurationApplied && input.Observation.Outcome == application.OperationOutcomeSucceeded {
		if !validConfigurationDigest(input.Observation.ProfileDigest) || !validConfigurationDigest(input.Observation.RepositoryBindingDigest) || input.Observation.ProfileID == "" {
			return application.Onboarding{}, application.ErrOnboardingConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET target_binding_digest=? WHERE operation_id=? AND scope_kind='onboarding' AND target_id=? AND phase='accepted' AND outcome='pending'`, input.Observation.RepositoryBindingDigest, current.OperationID, current.OnboardingID)
		if err != nil {
			return application.Onboarding{}, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.Onboarding{}, application.ErrOperationReceiptConflict
		}
	}
	nextStatus := domain.OnboardingRunning
	stepIndex := order - 1
	settled := ""
	switch input.Observation.Outcome {
	case application.OperationOutcomeSucceeded:
		stepIndex = order
		if input.Step == domain.OnboardingStepSettled {
			nextStatus, settled = domain.OnboardingReadyDisabled, formatTime(input.ObservedAt)
		}
	case application.OperationOutcomeFailed, application.OperationOutcomePending:
		nextStatus = domain.OnboardingWaitingForOperator
	case application.OperationOutcomeConflict, application.OperationOutcomeAmbiguous:
		nextStatus, settled = domain.OnboardingConflict, formatTime(input.ObservedAt)
	default:
		return application.Onboarding{}, errors.New("onboarding step outcome is invalid")
	}
	_, err = tx.ExecContext(ctx, `UPDATE repository_onboardings SET status=?,step_index=?,reason_code=?,profile_id=CASE WHEN ?<>'' THEN ? ELSE profile_id END,profile_digest=CASE WHEN ?<>'' THEN ? ELSE profile_digest END,repository_binding_digest=CASE WHEN ?<>'' THEN ? ELSE repository_binding_digest END,configuration_generation_id=CASE WHEN ?>0 THEN ? ELSE configuration_generation_id END,incarnation_id=CASE WHEN ?<>'' THEN ? ELSE incarnation_id END,readiness_snapshot_id=CASE WHEN ?<>'' THEN ? ELSE readiness_snapshot_id END,linear_label_id=CASE WHEN ?<>'' THEN ? ELSE linear_label_id END,initial_revision_sha=CASE WHEN ?<>'' THEN ? ELSE initial_revision_sha END,updated_at=?,settled_at=? WHERE onboarding_id=?`, string(nextStatus), stepIndex, input.Observation.ReasonCode, input.Observation.ProfileID, input.Observation.ProfileID, input.Observation.ProfileDigest, input.Observation.ProfileDigest, input.Observation.RepositoryBindingDigest, input.Observation.RepositoryBindingDigest, input.Observation.ConfigurationGenerationID, input.Observation.ConfigurationGenerationID, input.Observation.IncarnationID, input.Observation.IncarnationID, input.Observation.ReadinessSnapshotID, input.Observation.ReadinessSnapshotID, input.Observation.LinearLabelID, input.Observation.LinearLabelID, input.Observation.InitialRevisionSHA, input.Observation.InitialRevisionSHA, formatTime(input.ObservedAt), settled, input.OnboardingID)
	if err != nil {
		return application.Onboarding{}, err
	}
	binding := current.RepositoryBindingDigest
	if input.Observation.RepositoryBindingDigest != "" {
		binding = input.Observation.RepositoryBindingDigest
	}
	if binding == "" {
		binding = current.RequestDigest
	}
	event, validActivity := newOnboardingActivityEvent(input.OnboardingID, input.Step, int64(order), input.Observation.Outcome, input.Observation.ReasonCode, input.Observation.EvidenceDigest, input.ObservedAt, binding, current.OperationID, application.ActivityIngestionCurrent)
	if !validActivity || event.ResultingState != string(nextStatus) || event.ResultingVersion != int64(stepIndex) {
		return application.Onboarding{}, errors.New("onboarding activity classification is invalid")
	}
	if available, err := activitySchemaAvailableTx(ctx, tx); err != nil {
		return application.Onboarding{}, err
	} else if available {
		if _, _, err := appendActivityEventTx(ctx, tx, event); err != nil {
			return application.Onboarding{}, err
		}
	}
	if nextStatus == domain.OnboardingReadyDisabled || nextStatus == domain.OnboardingConflict {
		receipt, found, receiptErr := getOperationReceiptByIDTx(ctx, tx, current.OperationID)
		if receiptErr != nil || !found {
			return application.Onboarding{}, application.ErrOperationReceiptConflict
		}
		receiptOutcome := application.OperationOutcomeSucceeded
		resultState := string(domain.OnboardingReadyDisabled)
		if nextStatus == domain.OnboardingConflict {
			receiptOutcome, resultState = application.OperationOutcomeConflict, string(domain.OnboardingConflict)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='observed',outcome=?,resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,applied_at=CASE WHEN applied_at='' THEN ? ELSE applied_at END,settled_at=? WHERE operation_id=? AND outcome='pending'`, string(receiptOutcome), current.RepositoryBindingDigest, resultState, stepIndex, input.Observation.EvidenceDigest, input.Observation.EvidenceDigest, formatTime(input.ObservedAt), formatTime(input.ObservedAt), receipt.OperationID); err != nil {
			return application.Onboarding{}, err
		}
		if err := appendSettledOperationActivityTx(ctx, tx, receipt.OperationID, application.ActivityIngestionCurrent); err != nil {
			return application.Onboarding{}, err
		}
	}
	updated, _, err := onboardingByID(ctx, tx, input.OnboardingID)
	if err != nil {
		return application.Onboarding{}, err
	}
	updated.CompletedSteps, err = onboardingCompletedSteps(ctx, tx, input.OnboardingID)
	return updated, commitOrError(tx, err)
}

func (s *Store) ListRunnableOnboardings(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("onboarding runnable limit is invalid")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT onboarding_id FROM repository_onboardings WHERE status IN ('accepted','running') OR (status='waiting_for_operator' AND reason_code='worker_restart_required') ORDER BY updated_at,onboarding_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Store) OnboardingAuthority(ctx context.Context, onboardingID string) (application.OnboardingAuthority, bool, error) {
	value, found, err := s.Onboarding(ctx, onboardingID)
	if err != nil || !found {
		return application.OnboardingAuthority{}, found, err
	}
	authority := application.OnboardingAuthority{OnboardingID: value.OnboardingID}
	if value.RepositoryBindingDigest != "" && value.ProfileID != "" {
		authority.BoundRepository = &application.RepositoryAuthority{Repository: value.CanonicalRepository, ProfileID: value.ProfileID, BindingDigest: value.RepositoryBindingDigest, AllowedLogins: []string{strings.ToLower(value.Requester.Login)}, TrustedOperators: []domain.GitHubUserIdentity{value.Requester}}
	}
	return authority, true, nil
}

func (s *Store) setOnboardingTerminal(ctx context.Context, onboardingID string, at time.Time, status domain.OnboardingStatus, reason string) (application.Onboarding, bool, error) {
	if onboardingID == "" || at.IsZero() || status != domain.OnboardingCancelled {
		return application.Onboarding{}, false, errors.New("onboarding terminal input is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE repository_onboardings SET status=?,reason_code=?,updated_at=?,settled_at=? WHERE onboarding_id=? AND status IN ('opened','preflight_ready')`, string(status), reason, formatTime(at), formatTime(at), onboardingID)
	if err != nil {
		return application.Onboarding{}, false, err
	}
	changed, _ := result.RowsAffected()
	value, found, err := s.Onboarding(ctx, onboardingID)
	if err != nil || !found {
		return application.Onboarding{}, false, application.ErrOnboardingNotFound
	}
	if changed == 0 && value.Status != status {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	return value, changed == 1, nil
}

func onboardingByID(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (application.Onboarding, bool, error) {
	var value application.Onboarding
	var kind, status, accepted, created, updated, settled string
	err := query.QueryRowContext(ctx, onboardingSelect+` WHERE onboarding_id=?`, id).Scan(&value.OnboardingID, &kind, &value.CanonicalRepository, &value.PrivateInputDigest, &value.SourcePathDigest, &value.RequestDigest, &value.Requester.Login, &value.Requester.DatabaseID, &value.Requester.NodeID, &value.Requester.ActorType, &value.ConfigurationBaseGenerationID, &value.ConfigurationBaseDigest, &value.ConfigurationAuthorityVersion, &status, &value.ReasonCode, &value.PreflightDigest, &value.PreviewDigest, &value.OperationID, &value.ProfileID, &value.ProfileDigest, &value.RepositoryBindingDigest, &value.ConfigurationGenerationID, &value.IncarnationID, &value.ReadinessSnapshotID, &value.LinearLabelID, &value.InitialRevisionSHA, &accepted, &created, &updated, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return application.Onboarding{}, false, nil
	}
	if err != nil {
		return application.Onboarding{}, false, err
	}
	value.Kind, value.Status = domain.OnboardingKind(kind), domain.OnboardingStatus(status)
	value.Requester = domain.GitHubUserIdentity{Login: value.Requester.Login, DatabaseID: value.Requester.DatabaseID, NodeID: value.Requester.NodeID, ActorType: value.Requester.ActorType}
	value.AcceptedAt, value.CreatedAt, value.UpdatedAt, value.SettledAt = parseTime(accepted), parseTime(created), parseTime(updated), parseTime(settled)
	return value, true, nil
}

func onboardingCompletedSteps(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string) ([]domain.OnboardingStep, error) {
	rows, err := query.QueryContext(ctx, `SELECT step_name FROM repository_onboarding_steps WHERE onboarding_id=? AND status='observed' AND outcome='succeeded' ORDER BY step_order`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.OnboardingStep
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			return nil, err
		}
		result = append(result, domain.OnboardingStep(step))
	}
	return result, rows.Err()
}

func onboardingPathClaims(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string) ([]string, error) {
	rows, err := query.QueryContext(ctx, `SELECT path_digest FROM repository_onboarding_path_claims WHERE onboarding_id=? ORDER BY path_digest`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, err
		}
		result = append(result, digest)
	}
	return result, rows.Err()
}

func currentStepIndex(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) int {
	var index int
	_ = query.QueryRowContext(ctx, `SELECT step_index FROM repository_onboardings WHERE onboarding_id=?`, id).Scan(&index)
	return index
}

func onboardingStepOrder(kind domain.OnboardingKind, step domain.OnboardingStep) int {
	plan, ok := domain.OnboardingStepPlan(kind)
	if !ok {
		return 0
	}
	for index, candidate := range plan {
		if candidate == step {
			return index + 1
		}
	}
	return 0
}

func validOnboardingOpen(input application.OnboardingOpenInput) bool {
	_, validKind := domain.OnboardingStepPlan(input.Kind)
	return strings.HasPrefix(input.OnboardingID, "onboarding-") && validKind && strings.Count(input.CanonicalRepository, "/") == 1 && input.Requester.Validate() == nil && validConfigurationDigest(input.PrivateInputDigest) && validConfigurationDigest(input.SourcePathDigest) && validOnboardingPathClaims(input.SourcePathDigest, input.SourceAncestorDigests) && validConfigurationDigest(input.RequestDigest) && input.ConfigurationBaseGenerationID > 0 && validConfigurationDigest(input.ConfigurationBaseDigest) && input.ConfigurationAuthorityVersion > 0 && !input.OpenedAt.IsZero()
}

func validOnboardingSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validOnboardingPathClaims(source string, claims []string) bool {
	if len(claims) == 0 || len(claims) > 256 || !slices.IsSorted(claims) {
		return false
	}
	found := false
	for index, digest := range claims {
		if !validConfigurationDigest(digest) || index > 0 && claims[index-1] == digest {
			return false
		}
		found = found || digest == source
	}
	return found
}

func activeOnboardingConfigurationMutations(ctx context.Context, tx *sql.Tx) (int, error) {
	var active int
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM configuration_drafts WHERE lifecycle IN ('open','applying','ambiguous')) +
		(SELECT COUNT(*) FROM repository_removal_drafts WHERE lifecycle IN ('open','applying','ambiguous')) +
		(SELECT COUNT(*) FROM configuration_apply_intents WHERE status IN ('accepted','ambiguous')) +
		(SELECT COUNT(*) FROM configuration_recovery_intents WHERE status IN ('accepted','ambiguous'))`).Scan(&active)
	return active, err
}

func sameOnboardingOpen(value application.Onboarding, input application.OnboardingOpenInput) bool {
	return value.OnboardingID == input.OnboardingID && value.Kind == input.Kind && value.CanonicalRepository == input.CanonicalRepository && value.Requester.Equal(input.Requester) && value.PrivateInputDigest == input.PrivateInputDigest && value.SourcePathDigest == input.SourcePathDigest && value.RequestDigest == input.RequestDigest && value.ConfigurationBaseGenerationID == input.ConfigurationBaseGenerationID && value.ConfigurationBaseDigest == input.ConfigurationBaseDigest
}

func commitOrError(tx *sql.Tx, err error) error {
	if err != nil {
		return err
	}
	return tx.Commit()
}

var _ application.OnboardingStore = (*Store)(nil)
