package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func (s *Store) ListControllerOnboardings(ctx context.Context, input application.ControllerOnboardingQuery) (application.ControllerOnboardingPage, error) {
	if !input.Authority.MatchesRequester(input.ConfiguredRequester) || input.Limit < 1 || input.Limit > application.RoutineQueryMaximumLimit+1 || input.BeforeUpdatedAt.IsZero() != (input.BeforeOnboardingID == "") || input.BeforeOnboardingID != "" && (strings.TrimSpace(input.BeforeOnboardingID) != input.BeforeOnboardingID || strings.ContainsRune(input.BeforeOnboardingID, '\x00')) || input.CanonicalRepository != "" && !validControllerOnboardingRepository(input.CanonicalRepository) {
		return application.ControllerOnboardingPage{}, errors.New("controller onboarding collection is invalid")
	}
	where := `(status IN ('accepted','running','waiting_for_operator','conflict','ready_disabled') OR repository_binding_digest<>'' OR (status IN ('opened','preflight_ready','cancelled') AND lower(requester_login)=lower(?) AND requester_database_id=? AND requester_node_id=? AND requester_actor_type=?))`
	args := []any{input.ConfiguredRequester.Login, input.ConfiguredRequester.DatabaseID, input.ConfiguredRequester.NodeID, input.ConfiguredRequester.ActorType}
	if input.CanonicalRepository != "" {
		where += ` AND canonical_repository=?`
		args = append(args, input.CanonicalRepository)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.ControllerOnboardingPage{}, err
	}
	defer tx.Rollback()
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_onboardings WHERE `+where, args...).Scan(&total); err != nil {
		return application.ControllerOnboardingPage{}, err
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
		return application.ControllerOnboardingPage{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return application.ControllerOnboardingPage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return application.ControllerOnboardingPage{}, err
	}
	page := application.ControllerOnboardingPage{Total: total}
	for _, id := range ids {
		value, found, err := onboardingByID(ctx, tx, id)
		if err != nil || !found {
			return application.ControllerOnboardingPage{}, fmt.Errorf("controller onboarding snapshot conflicts")
		}
		value.CompletedSteps, err = onboardingCompletedSteps(ctx, tx, id)
		if err != nil {
			return application.ControllerOnboardingPage{}, err
		}
		if !validControllerOnboardingRow(ctx, tx, value, input.ConfiguredRequester) {
			return application.ControllerOnboardingPage{}, errors.New("controller onboarding snapshot conflicts")
		}
		page.Onboardings = append(page.Onboardings, value)
	}
	return page, tx.Commit()
}

func validControllerOnboardingRepository(value string) bool {
	parts := strings.Split(value, "/")
	return value == strings.TrimSpace(value) && value == strings.ToLower(value) && !strings.ContainsRune(value, '\x00') && len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func validControllerOnboardingRow(ctx context.Context, tx *sql.Tx, value application.Onboarding, configured domain.GitHubUserIdentity) bool {
	if strings.TrimSpace(value.OnboardingID) == "" || strings.TrimSpace(value.CanonicalRepository) == "" || !application.ControllerOnboardingCollectionLifecycleValid(value, configured) {
		return false
	}
	switch value.Status {
	case domain.OnboardingOpened, domain.OnboardingPreflightReady, domain.OnboardingCancelled:
		return true
	}
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, value.OperationID)
	if err != nil || !found || !receipt.AcceptedAt.UTC().Truncate(time.Second).Equal(value.AcceptedAt) {
		return false
	}
	targetBinding := onboardingV46IdentityDigest(value.Requester)
	for _, step := range value.CompletedSteps {
		if step == domain.OnboardingStepConfigurationApplied {
			targetBinding = value.RepositoryBindingDigest
			break
		}
	}
	anchor := digestBytes([]byte("onboarding-start-v1\x00" + value.OnboardingID + "\x00" + value.PreflightDigest + "\x00" + value.PreviewDigest))
	expected := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: value.OnboardingID, Requester: value.Requester, RequestDigest: value.RequestDigest, ExpectedAuthorityDigest: value.ConfigurationBaseDigest, OperationAnchorDigest: anchor, TargetBindingDigest: targetBinding, AcceptedAt: value.AcceptedAt})
	if !sameAcceptedOperationReceipt(receipt, expected) {
		return false
	}
	switch value.Status {
	case domain.OnboardingAccepted, domain.OnboardingRunning, domain.OnboardingWaitingForOperator:
		return receipt.Phase == application.OperationPhaseAccepted && receipt.Outcome == application.OperationOutcomePending && receipt.ResultingAuthorityDigest == "" && receipt.ResultingState == "" && receipt.ResultingVersion == 0 && receipt.EvidenceDigest == "" && receipt.ResultDigest == "" && receipt.AppliedAt.IsZero() && receipt.SettledAt.IsZero()
	case domain.OnboardingConflict:
		return receipt.Phase == application.OperationPhaseObserved && receipt.Outcome == application.OperationOutcomeConflict && receipt.ResultingAuthorityDigest == value.RepositoryBindingDigest && receipt.ResultingState == string(domain.OnboardingConflict) && receipt.ResultingVersion == int64(len(value.CompletedSteps)) && validConfigurationDigest(receipt.EvidenceDigest) && receipt.ResultDigest == receipt.EvidenceDigest && receipt.AppliedAt.Equal(value.SettledAt) && receipt.SettledAt.Equal(value.SettledAt)
	case domain.OnboardingReadyDisabled:
		return receipt.Phase == application.OperationPhaseObserved && receipt.Outcome == application.OperationOutcomeSucceeded && receipt.ResultingAuthorityDigest == value.RepositoryBindingDigest && receipt.ResultingState == string(domain.OnboardingReadyDisabled) && receipt.ResultingVersion == int64(len(value.CompletedSteps)) && validConfigurationDigest(receipt.EvidenceDigest) && receipt.ResultDigest == receipt.EvidenceDigest && receipt.AppliedAt.Equal(value.SettledAt) && receipt.SettledAt.Equal(value.SettledAt)
	default:
		return false
	}
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
	result, err := tx.ExecContext(ctx, `UPDATE repository_onboarding_steps SET attempt_number=attempt_number+1,status='intended',outcome='pending',reason_code='',evidence_digest='',intended_at=?,observed_at='' WHERE onboarding_id=? AND step_order=? AND status='observed' AND outcome IN ('failed','pending') AND NOT EXISTS(SELECT 1 FROM repository_onboarding_step_claims c WHERE c.onboarding_id=repository_onboarding_steps.onboarding_id AND c.step_name=repository_onboarding_steps.step_name AND c.attempt_number=repository_onboarding_steps.attempt_number AND c.status='active')`, formatTime(at), onboardingID, currentStepIndex(ctx, tx, onboardingID)+1)
	if err != nil {
		return application.Onboarding{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE repository_onboardings SET status='running',reason_code='',updated_at=? WHERE onboarding_id=? AND status='waiting_for_operator'`, formatTime(at), onboardingID)
	if err != nil {
		return application.Onboarding{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.Onboarding{}, false, application.ErrOnboardingConflict
	}
	updated, _, err := onboardingByID(ctx, tx, onboardingID)
	return updated, true, commitOrError(tx, err)
}

func (s *Store) AcquireOnboardingStepClaim(ctx context.Context, request application.OnboardingStepClaimRequest) (application.OnboardingStepClaimResult, error) {
	input, authority := request.Intent, request.Authority
	if input.OnboardingID == "" || !validConfigurationDigest(input.IntentDigest) || input.IntendedAt.IsZero() || !validOnboardingClaimComponent(authority.SupervisorID) || !validOnboardingClaimComponent(authority.ExecutionNonce) {
		return application.OnboardingStepClaimResult{}, errors.New("onboarding step claim request is invalid")
	}
	if !s.authorizedSupervisorOwner(authority.SupervisorID) {
		return application.OnboardingStepClaimResult{}, application.ErrOnboardingConflict
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return application.OnboardingStepClaimResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return application.OnboardingStepClaimResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	commit := func(result application.OnboardingStepClaimResult) (application.OnboardingStepClaimResult, error) {
		if _, commitErr := conn.ExecContext(ctx, `COMMIT`); commitErr != nil {
			return application.OnboardingStepClaimResult{}, commitErr
		}
		committed = true
		return result, nil
	}
	current, found, err := onboardingByID(ctx, conn, input.OnboardingID)
	if err != nil || !found {
		return application.OnboardingStepClaimResult{}, application.ErrOnboardingNotFound
	}
	order := onboardingStepOrder(current.Kind, input.Step)
	if order == 0 || current.Status != domain.OnboardingAccepted && current.Status != domain.OnboardingRunning || currentStepIndex(ctx, conn, input.OnboardingID)+1 != order {
		return application.OnboardingStepClaimResult{}, application.ErrOnboardingConflict
	}
	var intentDigest, stepStatus string
	var attempt int64
	err = conn.QueryRowContext(ctx, `SELECT intent_digest,status,attempt_number FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name=?`, input.OnboardingID, string(input.Step)).Scan(&intentDigest, &stepStatus, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		attempt = 1
		if _, err := conn.ExecContext(ctx, `INSERT INTO repository_onboarding_steps(onboarding_id,step_name,step_order,intent_digest,status,outcome,intended_at,attempt_number) VALUES(?,?,?,?,'intended','pending',?,?)`, input.OnboardingID, string(input.Step), order, input.IntentDigest, formatTime(input.IntendedAt), attempt); err != nil {
			return application.OnboardingStepClaimResult{}, application.ErrOnboardingConflict
		}
		if _, err := conn.ExecContext(ctx, `UPDATE repository_onboardings SET status='running',updated_at=? WHERE onboarding_id=?`, formatTime(input.IntendedAt), input.OnboardingID); err != nil {
			return application.OnboardingStepClaimResult{}, err
		}
		intentDigest, stepStatus = input.IntentDigest, "intended"
	} else if err != nil {
		return application.OnboardingStepClaimResult{}, err
	}
	if intentDigest != input.IntentDigest || stepStatus != "intended" || attempt < 1 {
		return application.OnboardingStepClaimResult{}, application.ErrOnboardingConflict
	}

	var active application.OnboardingStepClaimToken
	var activeStatus string
	err = conn.QueryRowContext(ctx, `SELECT onboarding_id,step_name,attempt_number,intent_digest,supervisor_id,execution_nonce,claim_version,claim_digest,status FROM repository_onboarding_step_claims WHERE onboarding_id=? AND step_name=? AND attempt_number=? AND status='active'`, input.OnboardingID, string(input.Step), attempt).Scan(&active.OnboardingID, &active.Step, &active.AttemptNumber, &active.IntentDigest, &active.SupervisorID, &active.ExecutionNonce, &active.ClaimVersion, &active.ClaimDigest, &activeStatus)
	if err == nil {
		expected := onboardingStepClaimDigest(active.OnboardingID, active.Step, active.AttemptNumber, active.IntentDigest, active.SupervisorID, active.ExecutionNonce, active.ClaimVersion)
		if activeStatus != "active" || active.ClaimDigest != expected {
			return application.OnboardingStepClaimResult{}, application.ErrOnboardingConflict
		}
		if active.SupervisorID == authority.SupervisorID {
			if active.ExecutionNonce == authority.ExecutionNonce {
				return commit(application.OnboardingStepClaimResult{Classification: application.OnboardingStepClaimReplayed, Claim: active})
			}
			return commit(application.OnboardingStepClaimResult{Classification: application.OnboardingStepClaimBusy})
		}
		claimedAt := formatTime(input.IntendedAt)
		result, err := conn.ExecContext(ctx, `UPDATE repository_onboarding_step_claims SET status='superseded',superseded_at=? WHERE onboarding_id=? AND step_name=? AND attempt_number=? AND claim_version=? AND claim_digest=? AND status='active'`, claimedAt, active.OnboardingID, string(active.Step), active.AttemptNumber, active.ClaimVersion, active.ClaimDigest)
		if err != nil {
			return application.OnboardingStepClaimResult{}, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.OnboardingStepClaimResult{}, application.ErrOnboardingConflict
		}
		claim := newOnboardingStepClaim(input, authority, attempt, active.ClaimVersion+1)
		if err := insertOnboardingStepClaim(ctx, conn, claim, claimedAt); err != nil {
			return application.OnboardingStepClaimResult{}, err
		}
		return commit(application.OnboardingStepClaimResult{Classification: application.OnboardingStepClaimAdopted, Claim: claim})
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return application.OnboardingStepClaimResult{}, err
	}
	var historical int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(claim_version),0) FROM repository_onboarding_step_claims WHERE onboarding_id=? AND step_name=? AND attempt_number=?`, input.OnboardingID, string(input.Step), attempt).Scan(&historical); err != nil {
		return application.OnboardingStepClaimResult{}, err
	}
	if historical != 0 {
		return application.OnboardingStepClaimResult{}, application.ErrOnboardingConflict
	}
	claim := newOnboardingStepClaim(input, authority, attempt, 1)
	if err := insertOnboardingStepClaim(ctx, conn, claim, formatTime(input.IntendedAt)); err != nil {
		return application.OnboardingStepClaimResult{}, err
	}
	return commit(application.OnboardingStepClaimResult{Classification: application.OnboardingStepClaimAcquired, Claim: claim})
}

func newOnboardingStepClaim(input application.OnboardingStepIntent, authority application.OnboardingExecutionAuthority, attempt, version int64) application.OnboardingStepClaimToken {
	claim := application.OnboardingStepClaimToken{OnboardingID: input.OnboardingID, Step: input.Step, AttemptNumber: attempt, IntentDigest: input.IntentDigest, SupervisorID: authority.SupervisorID, ExecutionNonce: authority.ExecutionNonce, ClaimVersion: version}
	claim.ClaimDigest = onboardingStepClaimDigest(claim.OnboardingID, claim.Step, claim.AttemptNumber, claim.IntentDigest, claim.SupervisorID, claim.ExecutionNonce, claim.ClaimVersion)
	return claim
}

func insertOnboardingStepClaim(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, claim application.OnboardingStepClaimToken, claimedAt string) error {
	_, err := executor.ExecContext(ctx, `INSERT INTO repository_onboarding_step_claims(onboarding_id,step_name,attempt_number,claim_version,supervisor_id,execution_nonce,intent_digest,claim_digest,status,claimed_at) VALUES(?,?,?,?,?,?,?,?,'active',?)`, claim.OnboardingID, string(claim.Step), claim.AttemptNumber, claim.ClaimVersion, claim.SupervisorID, claim.ExecutionNonce, claim.IntentDigest, claim.ClaimDigest, claimedAt)
	return err
}

func onboardingStepClaimDigest(onboardingID string, step domain.OnboardingStep, attempt int64, intentDigest, supervisorID, executionNonce string, version int64) string {
	return digestBytes([]byte(fmt.Sprintf("onboarding-step-claim-v1\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%d", onboardingID, step, attempt, intentDigest, supervisorID, executionNonce, version)))
}

func validOnboardingClaimComponent(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 128 && !strings.ContainsRune(value, '\x00')
}

func (s *Store) SettleOnboardingStep(ctx context.Context, input application.OnboardingStepSettlement) (application.Onboarding, error) {
	claim := input.Claim
	if input.OnboardingID == "" || input.ObservedAt.IsZero() || !validConfigurationDigest(input.Observation.EvidenceDigest) || input.Observation.Outcome == application.OperationOutcomePending && input.Observation.ReasonCode == "" || claim.OnboardingID != input.OnboardingID || claim.Step != input.Step || claim.AttemptNumber < 1 || claim.ClaimVersion < 1 || !validConfigurationDigest(claim.IntentDigest) || !validConfigurationDigest(claim.ClaimDigest) || !validOnboardingClaimComponent(claim.SupervisorID) || !validOnboardingClaimComponent(claim.ExecutionNonce) {
		return application.Onboarding{}, errors.New("onboarding step settlement is invalid")
	}
	if claim.ClaimDigest != onboardingStepClaimDigest(claim.OnboardingID, claim.Step, claim.AttemptNumber, claim.IntentDigest, claim.SupervisorID, claim.ExecutionNonce, claim.ClaimVersion) {
		return application.Onboarding{}, application.ErrOnboardingConflict
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
	var status, outcome, evidence, intentDigest string
	var attempt int64
	if err := tx.QueryRowContext(ctx, `SELECT status,outcome,evidence_digest,attempt_number,intent_digest FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name=? AND step_order=?`, input.OnboardingID, string(input.Step), order).Scan(&status, &outcome, &evidence, &attempt, &intentDigest); err != nil || attempt < 1 {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	if attempt != claim.AttemptNumber || intentDigest != claim.IntentDigest {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	var claimStatus, storedIntent, storedSupervisor, storedExecution, storedDigest string
	if err := tx.QueryRowContext(ctx, `SELECT status,intent_digest,supervisor_id,execution_nonce,claim_digest FROM repository_onboarding_step_claims WHERE onboarding_id=? AND step_name=? AND attempt_number=? AND claim_version=?`, claim.OnboardingID, string(claim.Step), claim.AttemptNumber, claim.ClaimVersion).Scan(&claimStatus, &storedIntent, &storedSupervisor, &storedExecution, &storedDigest); err != nil || storedIntent != claim.IntentDigest || storedSupervisor != claim.SupervisorID || storedExecution != claim.ExecutionNonce || storedDigest != claim.ClaimDigest {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	if status == "observed" {
		if claimStatus == "settled" && outcome == string(input.Observation.Outcome) && evidence == input.Observation.EvidenceDigest {
			return current, tx.Commit()
		}
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	if status != "intended" || claimStatus != "active" || currentStepIndex(ctx, tx, input.OnboardingID)+1 != order {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE repository_onboarding_steps SET status='observed',outcome=?,reason_code=?,evidence_digest=?,observed_at=? WHERE onboarding_id=? AND step_name=? AND status='intended' AND attempt_number=? AND intent_digest=?`, string(input.Observation.Outcome), input.Observation.ReasonCode, input.Observation.EvidenceDigest, formatTime(input.ObservedAt), input.OnboardingID, string(input.Step), attempt, claim.IntentDigest)
	if err != nil {
		return application.Onboarding{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.Onboarding{}, application.ErrOnboardingConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE repository_onboarding_step_claims SET status='settled',settled_at=? WHERE onboarding_id=? AND step_name=? AND attempt_number=? AND claim_version=? AND supervisor_id=? AND execution_nonce=? AND intent_digest=? AND claim_digest=? AND status='active'`, formatTime(input.ObservedAt), claim.OnboardingID, string(claim.Step), claim.AttemptNumber, claim.ClaimVersion, claim.SupervisorID, claim.ExecutionNonce, claim.IntentDigest, claim.ClaimDigest)
	if err != nil {
		return application.Onboarding{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.Onboarding{}, application.ErrOnboardingConflict
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
	event, validActivity := newOnboardingActivityEvent(input.OnboardingID, input.Step, int64(order), attempt, input.Observation.Outcome, input.Observation.ReasonCode, input.Observation.EvidenceDigest, input.ObservedAt, binding, current.OperationID, application.ActivityIngestionCurrent)
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

type onboardingV46MigrationCandidate struct {
	id, kind, requestDigest, repositoryBindingDigest, operationID string
	baseDigest, preflightDigest, previewDigest, acceptedAt        string
	requester                                                     domain.GitHubUserIdentity
	stepIndex, stepOrder                                          int64
}

func migrateOnboardingAttemptsV46Tx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT o.onboarding_id,o.onboarding_kind,o.request_digest,o.repository_binding_digest,COALESCE(o.operation_id,''),o.base_digest,o.preflight_digest,o.preview_digest,o.accepted_at,o.requester_login,o.requester_database_id,o.requester_node_id,o.requester_actor_type,o.step_index,s.step_order
		FROM repository_onboardings o JOIN repository_onboarding_steps s ON s.onboarding_id=o.onboarding_id
		WHERE o.status='running' AND o.reason_code='' AND o.onboarding_kind='existing_checkout' AND s.step_name='linear_label_observed' AND s.step_order=2
		AND s.status='intended' AND s.outcome='pending' AND s.reason_code='' AND s.evidence_digest=''
		AND s.observed_at='' AND s.attempt_number=1 AND o.step_index=s.step_order-1
		ORDER BY o.onboarding_id`)
	if err != nil {
		return err
	}
	var candidates []onboardingV46MigrationCandidate
	for rows.Next() {
		var candidate onboardingV46MigrationCandidate
		if err := rows.Scan(&candidate.id, &candidate.kind, &candidate.requestDigest, &candidate.repositoryBindingDigest, &candidate.operationID, &candidate.baseDigest, &candidate.preflightDigest, &candidate.previewDigest, &candidate.acceptedAt, &candidate.requester.Login, &candidate.requester.DatabaseID, &candidate.requester.NodeID, &candidate.requester.ActorType, &candidate.stepIndex, &candidate.stepOrder); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, candidate := range candidates {
		if !validOnboardingV46CandidateTx(ctx, tx, candidate) {
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE repository_onboarding_steps SET attempt_number=2
			WHERE onboarding_id=? AND step_name='linear_label_observed' AND step_order=?
			AND status='intended' AND outcome='pending' AND reason_code='' AND evidence_digest=''
			AND observed_at='' AND attempt_number=1
			AND EXISTS(SELECT 1 FROM repository_onboardings o WHERE o.onboarding_id=? AND o.status='running' AND o.reason_code='' AND o.step_index=?)`, candidate.id, candidate.stepOrder, candidate.id, candidate.stepIndex)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("onboarding attempt migration authority changed")
		}
	}
	return nil
}

func validOnboardingV46CandidateTx(ctx context.Context, tx *sql.Tx, candidate onboardingV46MigrationCandidate) bool {
	plan, ok := domain.OnboardingStepPlan(domain.OnboardingKind(candidate.kind))
	acceptedAt := parseTime(candidate.acceptedAt)
	if !ok || domain.OnboardingKind(candidate.kind) != domain.OnboardingExistingCheckout || candidate.stepOrder != 2 || candidate.stepIndex != 1 || plan[0] != domain.OnboardingStepRootsCreated || plan[1] != domain.OnboardingStepLinearLabelObserved || !validConfigurationDigest(candidate.requestDigest) || !validConfigurationDigest(candidate.baseDigest) || !validConfigurationDigest(candidate.preflightDigest) || !validConfigurationDigest(candidate.previewDigest) || candidate.requester.Validate() != nil || acceptedAt.IsZero() || candidate.operationID == "" {
		return false
	}
	binding := candidate.repositoryBindingDigest
	if binding == "" {
		binding = candidate.requestDigest
	}
	if !validConfigurationDigest(binding) {
		return false
	}
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, candidate.operationID)
	anchor := digestBytes([]byte("onboarding-start-v1\x00" + candidate.id + "\x00" + candidate.preflightDigest + "\x00" + candidate.previewDigest))
	identityBinding := onboardingV46IdentityDigest(candidate.requester)
	expectedReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: candidate.id, Requester: candidate.requester, RequestDigest: candidate.requestDigest, ExpectedAuthorityDigest: candidate.baseDigest, OperationAnchorDigest: anchor, TargetBindingDigest: identityBinding, AcceptedAt: acceptedAt})
	if err != nil || !found || receipt.Phase != application.OperationPhaseAccepted || receipt.Outcome != application.OperationOutcomePending || !sameAcceptedOperationReceipt(receipt, expectedReceipt) {
		return false
	}
	steps, err := tx.QueryContext(ctx, `SELECT step_name,step_order,intent_digest,status,outcome,reason_code,evidence_digest,intended_at,observed_at,attempt_number FROM repository_onboarding_steps WHERE onboarding_id=? ORDER BY step_order`, candidate.id)
	if err != nil {
		return false
	}
	type storedStep struct {
		name, intent, status, outcome, reason, evidence, intended, observed string
		order, attempt                                                      int64
	}
	var stored []storedStep
	for steps.Next() {
		var step storedStep
		if err := steps.Scan(&step.name, &step.order, &step.intent, &step.status, &step.outcome, &step.reason, &step.evidence, &step.intended, &step.observed, &step.attempt); err != nil {
			steps.Close()
			return false
		}
		stored = append(stored, step)
	}
	if steps.Close() != nil || len(stored) != int(candidate.stepOrder) {
		return false
	}
	lastObserved := receipt.AcceptedAt
	for index, step := range stored {
		intendedAt := parseTime(step.intended)
		if step.order != int64(index+1) || domain.OnboardingStep(step.name) != plan[index] || !validConfigurationDigest(step.intent) || step.attempt != 1 || intendedAt.IsZero() || intendedAt.Before(lastObserved) {
			return false
		}
		if index < len(stored)-1 {
			expectedIntent := digestBytes([]byte("onboarding-step-intent-v1\x00" + candidate.id + "\x00" + step.name + "\x00" + candidate.requestDigest))
			observedAt := parseTime(step.observed)
			if step.intent != expectedIntent || step.status != "observed" || step.outcome != string(application.OperationOutcomeSucceeded) || !validConfigurationDigest(step.evidence) || observedAt.IsZero() || observedAt.Before(intendedAt) {
				return false
			}
			lastObserved = observedAt
			continue
		}
		intentParts := "onboarding-step-intent-v2\x00" + candidate.id + "\x00" + candidate.kind + "\x00" + string(domain.OnboardingStepLinearLabelObserved) + "\x00" + candidate.requestDigest
		if domain.OnboardingKind(candidate.kind) == domain.OnboardingExistingCheckout {
			intentParts = "onboarding-step-intent-v1\x00" + candidate.id + "\x00" + string(domain.OnboardingStepLinearLabelObserved) + "\x00" + candidate.requestDigest
		}
		if step.intent != digestBytes([]byte(intentParts)) || step.status != "intended" || step.outcome != string(application.OperationOutcomePending) || step.reason != "" || step.evidence != "" || step.observed != "" {
			return false
		}
	}
	event, err := scanActivityEvent(tx.QueryRowContext(ctx, activityEventSelect+` WHERE source_kind='onboarding' AND source_identity=? AND event_kind='onboarding_milestone'`, candidate.id+":"+string(domain.OnboardingStepLinearLabelObserved)))
	currentIntendedAt := parseTime(stored[len(stored)-1].intended)
	if err != nil || event.Coverage.IngestionClass != application.ActivityIngestionCurrent || event.OccurredAt.Before(lastObserved) || event.OccurredAt.After(currentIntendedAt) {
		return false
	}
	expected, valid := newOnboardingActivityEvent(candidate.id, domain.OnboardingStepLinearLabelObserved, candidate.stepOrder, 1, application.OperationOutcomeFailed, "linear_label_outcome_unknown", application.ConfigurationEvidenceDigest("onboarding-linear-unknown-v1", candidate.id), event.OccurredAt, binding, candidate.operationID, application.ActivityIngestionCurrent)
	event.IngestionSequence = 0
	return valid && reflect.DeepEqual(event, expected)
}

func onboardingV46IdentityDigest(identity domain.GitHubUserIdentity) string {
	return digestBytes([]byte(strings.ToLower(identity.Login) + "\x00" + hex.EncodeToString([]byte(identity.NodeID)) + "\x00" + identity.ActorType + "\x00" + strconv.FormatInt(identity.DatabaseID, 10)))
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
