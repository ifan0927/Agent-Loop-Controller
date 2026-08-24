package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const repositoryRemovalDraftSelect = `SELECT draft_id,revision,lifecycle,repository,incarnation_id,profile_id,profile_digest,repository_binding_digest,lifecycle_version,base_generation_id,base_digest,configuration_authority_version,repository_count_before,validation_candidate_digest,validation_digest,validation_valid,validation_guards_json,validated_at,preview_digest,preview_json,previewed_at,removal_operation_id,configuration_operation_id,result_generation_id,result_digest,created_at,updated_at,settled_at,reason_code FROM repository_removal_drafts`

func (s *Store) OpenRepositoryRemovalDraft(ctx context.Context, input application.RepositoryRemovalOpenInput) (application.RepositoryRemovalDraft, bool, error) {
	if !validRepositoryRemovalDraftID(input.DraftID) || input.Authority.Lifecycle.Validate() != nil || input.Authority.Lifecycle.Intent != application.RepositoryDisabled || input.Authority.Recheck != nil || input.Authority.Removal != nil || input.Authority.ConfigurationAuthority.GenerationID < 1 || !validConfigurationDigest(input.Authority.ConfigurationAuthority.Digest) || input.Authority.ConfigurationAuthority.AuthorityVersion < 1 || input.RepositoryCount < 1 || input.Requester.Identity().Validate() != nil || input.OpenedAt.IsZero() || !validRepositoryProfile(input.Profile) {
		return application.RepositoryRemovalDraft{}, false, errors.New("repository removal draft input is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RepositoryRemovalDraft{}, false, err
	}
	defer tx.Rollback()
	current, err := repositoryOperationAuthorityTx(ctx, tx, input.Authority.Lifecycle.Repository)
	if err != nil || !sameRemovalTargetAuthority(current, input.Authority) || !profileMatchesLifecycle(input.Profile, current.Lifecycle) {
		return application.RepositoryRemovalDraft{}, false, application.ErrRepositoryLifecycleConflict
	}
	var activeID string
	err = tx.QueryRowContext(ctx, `SELECT draft_id FROM repository_removal_drafts WHERE lifecycle IN ('open','applying','ambiguous')`).Scan(&activeID)
	if err == nil {
		draft, found, readErr := repositoryRemovalDraftQuery(ctx, tx, activeID)
		if readErr == nil && found && sameRemovalDraftOpen(draft, input) {
			return draft, false, tx.Commit()
		}
		return application.RepositoryRemovalDraft{}, false, application.ErrRepositoryLifecycleConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return application.RepositoryRemovalDraft{}, false, err
	}
	requester := input.Requester.Identity()
	_, err = tx.ExecContext(ctx, `INSERT INTO repository_removal_drafts(draft_id,incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,lifecycle_version,base_generation_id,base_digest,configuration_authority_version,repository_count_before,revision,lifecycle,requester_login,requester_database_id,requester_node_id,requester_actor_type,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,1,'open',?,?,?,?,?,?)`, input.DraftID, current.Lifecycle.IncarnationID, current.Lifecycle.Repository, current.Lifecycle.ProfileID, current.Lifecycle.ProfileDigest, current.Lifecycle.RepositoryBindingDigest, current.Lifecycle.Version, current.ConfigurationAuthority.GenerationID, current.ConfigurationAuthority.Digest, current.ConfigurationAuthority.AuthorityVersion, input.RepositoryCount, requester.Login, requester.DatabaseID, requester.NodeID, requester.ActorType, formatTime(input.OpenedAt), formatTime(input.OpenedAt))
	if err != nil {
		return application.RepositoryRemovalDraft{}, false, err
	}
	draft, found, err := repositoryRemovalDraftQuery(ctx, tx, input.DraftID)
	if err != nil || !found {
		return application.RepositoryRemovalDraft{}, false, err
	}
	return draft, true, tx.Commit()
}

func (s *Store) RepositoryRemovalDraft(ctx context.Context, draftID string) (application.RepositoryRemovalDraft, bool, error) {
	if !validRepositoryRemovalDraftID(draftID) {
		return application.RepositoryRemovalDraft{}, false, nil
	}
	return repositoryRemovalDraftQuery(ctx, s.db, draftID)
}

func (s *Store) RecordRepositoryRemovalMetadata(ctx context.Context, input application.RepositoryRemovalMetadataInput) (application.RepositoryRemovalDraft, error) {
	if !validRepositoryRemovalDraftID(input.DraftID) || input.ExpectedRevision != 1 || input.Validation.DraftID != input.DraftID || input.Validation.Revision != 1 || !validConfigurationDigest(input.Validation.CandidateDigest) || !validConfigurationDigest(input.Validation.ValidationDigest) || input.Validation.ValidatedAt.IsZero() || input.UpdatedAt.IsZero() {
		return application.RepositoryRemovalDraft{}, errors.New("repository removal metadata is invalid")
	}
	guardsJSON, err := json.Marshal(input.Validation.Guards)
	if err != nil {
		return application.RepositoryRemovalDraft{}, err
	}
	previewDigest, previewJSON, previewedAt := "", "", ""
	if input.Preview != nil {
		if input.Preview.DraftID != input.DraftID || input.Preview.Revision != 1 || !validConfigurationDigest(input.Preview.PreviewDigest) || input.Preview.PreviewedAt.IsZero() || input.Preview.ProposedConfigurationDigest != input.Validation.CandidateDigest {
			return application.RepositoryRemovalDraft{}, errors.New("repository removal preview is invalid")
		}
		encoded, marshalErr := json.Marshal(input.Preview)
		if marshalErr != nil {
			return application.RepositoryRemovalDraft{}, marshalErr
		}
		previewDigest, previewJSON, previewedAt = input.Preview.PreviewDigest, string(encoded), formatTime(input.Preview.PreviewedAt)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE repository_removal_drafts SET validation_candidate_digest=?,validation_digest=?,validation_valid=?,validation_guards_json=?,validated_at=?,preview_digest=?,preview_json=?,previewed_at=?,updated_at=? WHERE draft_id=? AND revision=1 AND lifecycle='open'`, input.Validation.CandidateDigest, input.Validation.ValidationDigest, boolInt(input.Validation.Valid), string(guardsJSON), formatTime(input.Validation.ValidatedAt), previewDigest, previewJSON, previewedAt, formatTime(input.UpdatedAt), input.DraftID)
	if err != nil {
		return application.RepositoryRemovalDraft{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRemovalDraft{}, application.ErrRepositoryLifecycleConflict
	}
	draft, found, err := s.RepositoryRemovalDraft(ctx, input.DraftID)
	if err != nil || !found {
		return application.RepositoryRemovalDraft{}, err
	}
	return draft, nil
}

func (s *Store) DiscardRepositoryRemovalDraft(ctx context.Context, draftID string, revision int64, at time.Time) (application.RepositoryRemovalDraft, bool, error) {
	if !validRepositoryRemovalDraftID(draftID) || revision != 1 || at.IsZero() {
		return application.RepositoryRemovalDraft{}, false, errors.New("repository removal discard is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE repository_removal_drafts SET lifecycle='discarded',reason_code='discarded',updated_at=?,settled_at=? WHERE draft_id=? AND revision=1 AND lifecycle='open'`, formatTime(at), formatTime(at), draftID)
	if err != nil {
		return application.RepositoryRemovalDraft{}, false, err
	}
	draft, found, readErr := s.RepositoryRemovalDraft(ctx, draftID)
	if readErr != nil || !found {
		return application.RepositoryRemovalDraft{}, false, readErr
	}
	if draft.State != application.RepositoryRemovalDraftDiscarded {
		return application.RepositoryRemovalDraft{}, false, application.ErrRepositoryLifecycleConflict
	}
	changed, _ := result.RowsAffected()
	return draft, changed == 1, nil
}

func (s *Store) EvaluateRepositoryRemovalGuards(ctx context.Context, expected application.RepositoryOperationAuthority, repositoryCount int, at time.Time) ([]application.RepositoryRemovalGuardResult, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	guards, err := evaluateRepositoryRemovalGuardsTx(ctx, tx, expected, repositoryCount, at)
	if err != nil {
		return nil, err
	}
	return guards, tx.Commit()
}

func (s *Store) AcceptRepositoryRemoval(ctx context.Context, input application.RepositoryRemovalAcceptance) (application.RepositoryRemovalDraft, application.OperationReceipt, bool, error) {
	if !validRepositoryRemovalDraftID(input.DraftID) || input.ExpectedRevision != 1 || !validConfigurationDigest(input.CandidateDigest) || !validConfigurationDigest(input.PreviewDigest) || application.ValidateOperationReceipt(input.Receipt) != nil || input.Receipt.OperationType != application.OperationRemoveRepository || input.AcceptedAt.IsZero() {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, errors.New("repository removal acceptance is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	draft, found, err := repositoryRemovalDraftQuery(ctx, tx, input.DraftID)
	if err != nil || !found || draft.Revision != 1 || draft.Preview == nil || draft.Preview.PreviewDigest != input.PreviewDigest || draft.Validation == nil || !draft.Validation.Valid || draft.Validation.CandidateDigest != input.CandidateDigest {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	if draft.State == application.RepositoryRemovalDraftApplying || draft.State == application.RepositoryRemovalDraftApplied {
		receipt, found, readErr := getOperationReceiptByIDTx(ctx, tx, draft.RemovalOperationID)
		if readErr == nil && found && receipt.OperationID == input.Receipt.OperationID {
			return draft, receipt, false, tx.Commit()
		}
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	current, err := repositoryOperationAuthorityTx(ctx, tx, draft.Repository)
	if err != nil || !sameRemovalTargetAuthority(current, input.Expected) || current.Lifecycle.Intent != application.RepositoryDisabled || current.Recheck != nil || current.Removal != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	guards, err := evaluateRepositoryRemovalGuardsTx(ctx, tx, input.Expected, draft.RepositoryCountBefore, input.AcceptedAt)
	if err != nil || !allGuardsAllowed(guards) {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	if err := insertOperationReceiptTx(ctx, tx, input.Receipt, ""); err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO repository_removal_intents(operation_id,draft_id,incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,lifecycle_version,base_generation_id,base_digest,configuration_authority_version,candidate_digest,preview_digest,status,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'accepted',?)`, input.Receipt.OperationID, draft.DraftID, draft.IncarnationID, draft.Repository, draft.ProfileID, draft.ProfileDigest, draft.RepositoryBindingDigest, draft.LifecycleVersion, draft.BaseGenerationID, draft.BaseDigest, draft.ConfigurationAuthorityVersion, input.CandidateDigest, input.PreviewDigest, formatTime(input.AcceptedAt))
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE repository_lifecycles SET removal_state='accepted',removal_operation_id=?,updated_at=? WHERE incarnation_id=? AND retired_at='' AND removal_state='' AND intent='disabled' AND lifecycle_version=?`, input.Receipt.OperationID, formatTime(input.AcceptedAt), draft.IncarnationID, draft.LifecycleVersion)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE repository_removal_drafts SET lifecycle='applying',removal_operation_id=?,updated_at=? WHERE draft_id=? AND lifecycle='open' AND revision=1`, input.Receipt.OperationID, formatTime(input.AcceptedAt), draft.DraftID)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	draft, _, err = repositoryRemovalDraftQuery(ctx, tx, draft.DraftID)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	return draft, input.Receipt, true, tx.Commit()
}

func (s *Store) RecordRepositoryRemovalApplied(ctx context.Context, input application.RepositoryRemovalApplied) (application.RepositoryRemovalDraft, application.OperationReceipt, bool, error) {
	if !validRepositoryRemovalDraftID(input.DraftID) || strings.TrimSpace(input.RemovalOperationID) == "" || strings.TrimSpace(input.ConfigurationOperationID) == "" || input.GenerationID < 1 || !validConfigurationDigest(input.Digest) || input.AppliedAt.IsZero() {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, errors.New("repository removal applied evidence is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	draft, found, err := repositoryRemovalDraftQuery(ctx, tx, input.DraftID)
	if err != nil || !found || draft.RemovalOperationID != input.RemovalOperationID {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	if draft.State == application.RepositoryRemovalDraftApplied {
		receipt, _, readErr := getOperationReceiptByIDTx(ctx, tx, input.RemovalOperationID)
		return draft, receipt, false, readErr
	}
	var parentID int64
	var generationOperation, generationDigest string
	if err := tx.QueryRowContext(ctx, `SELECT parent_generation_id,operation_id,digest FROM configuration_generations WHERE generation_id=?`, input.GenerationID).Scan(&parentID, &generationOperation, &generationDigest); err != nil || parentID != draft.BaseGenerationID || generationOperation != input.ConfigurationOperationID || generationDigest != input.Digest || input.Digest != draft.Validation.CandidateDigest {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE repository_removal_intents SET status='applied',configuration_operation_id=?,result_generation_id=?,applied_at=? WHERE operation_id=? AND status='accepted' AND candidate_digest=?`, input.ConfigurationOperationID, input.GenerationID, formatTime(input.AppliedAt), input.RemovalOperationID, input.Digest)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE repository_lifecycles SET removal_state=?,removal_result_generation_id=?,removal_result_digest=?,updated_at=? WHERE incarnation_id=? AND retired_at='' AND removal_state='accepted' AND removal_operation_id=?`, application.RepositoryRemovalPending, input.GenerationID, input.Digest, formatTime(input.AppliedAt), draft.IncarnationID, input.RemovalOperationID)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE repository_removal_drafts SET lifecycle='applied',configuration_operation_id=?,result_generation_id=?,result_digest=?,updated_at=? WHERE draft_id=? AND lifecycle='applying' AND removal_operation_id=?`, input.ConfigurationOperationID, input.GenerationID, input.Digest, formatTime(input.AppliedAt), input.DraftID, input.RemovalOperationID)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrRepositoryLifecycleConflict
	}
	evidence := repositoryDigest("repository-removal-applied-v1", draft.IncarnationID, input.Digest, input.ConfigurationOperationID)
	result, err = tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='applied',outcome='pending',resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,applied_at=? WHERE operation_id=? AND phase='accepted' AND outcome='pending'`, input.Digest, application.RepositoryRemovalPending, input.GenerationID, evidence, evidence, formatTime(input.AppliedAt), input.RemovalOperationID)
	if err != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	draft, _, err = repositoryRemovalDraftQuery(ctx, tx, input.DraftID)
	receipt, _, receiptErr := getOperationReceiptByIDTx(ctx, tx, input.RemovalOperationID)
	if err != nil || receiptErr != nil {
		return application.RepositoryRemovalDraft{}, application.OperationReceipt{}, false, errors.Join(err, receiptErr)
	}
	return draft, receipt, true, tx.Commit()
}

func evaluateRepositoryRemovalGuardsTx(ctx context.Context, tx *sql.Tx, expected application.RepositoryOperationAuthority, repositoryCount int, at time.Time) ([]application.RepositoryRemovalGuardResult, error) {
	current, err := repositoryOperationAuthorityTx(ctx, tx, expected.Lifecycle.Repository)
	if err != nil || !sameRemovalTargetAuthority(current, expected) {
		return nil, application.ErrRepositoryLifecycleConflict
	}
	guard := func(name string, allowed bool, blockedReason, next string) application.RepositoryRemovalGuardResult {
		if allowed {
			return application.RepositoryRemovalGuardResult{Guard: name, Allowed: true, ReasonCode: "clear", NextAction: "none"}
		}
		return application.RepositoryRemovalGuardResult{Guard: name, Allowed: false, ReasonCode: blockedReason, NextAction: next}
	}
	results := []application.RepositoryRemovalGuardResult{guard("lifecycle_disabled", current.Lifecycle.Intent == application.RepositoryDisabled, "repository_must_be_disabled", "disable_repository")}
	var nonterminal, recheck, operations, slots, leases, permits, scheduling, cleanup, mutation int
	queries := []struct {
		target *int
		query  string
		args   []any
	}{
		{&nonterminal, `SELECT COUNT(*) FROM runs WHERE repository_binding_digest=? AND current_state NOT IN ('rejected','failed','completed')`, []any{current.Lifecycle.RepositoryBindingDigest}},
		{&recheck, `SELECT COUNT(*) FROM repository_recheck_attempts WHERE incarnation_id=? AND status='in_progress'`, []any{current.Lifecycle.IncarnationID}},
		{&operations, `SELECT COUNT(*) FROM operation_receipts WHERE scope_kind='repository' AND target_id=? AND operation_type IN ('recheck_repository','enable_repository','disable_repository') AND outcome='pending'`, []any{current.Lifecycle.Repository}},
		{&slots, `SELECT COUNT(*) FROM repository_slots WHERE repository_binding_digest=?`, []any{current.Lifecycle.RepositoryBindingDigest}},
		{&leases, `SELECT COUNT(*) FROM runs WHERE repository_binding_digest=? AND lease_owner<>'' AND lease_expires_unix>?`, []any{current.Lifecycle.RepositoryBindingDigest, at.UTC().UnixNano()}},
		{&permits, `SELECT COUNT(*) FROM heavy_permits WHERE run_id IN (SELECT run_id FROM runs WHERE repository_binding_digest=?)`, []any{current.Lifecycle.RepositoryBindingDigest}},
		{&scheduling, `SELECT COUNT(*) FROM run_scheduling WHERE run_id IN (SELECT run_id FROM runs WHERE repository_binding_digest=?) AND supervisor_state<>'terminal'`, []any{current.Lifecycle.RepositoryBindingDigest}},
		{&cleanup, `SELECT COUNT(*) FROM cleanup_results WHERE run_id IN (SELECT run_id FROM runs WHERE repository_binding_digest=?) AND resource_kind IN ('source_checkout','worktree','branch','local_branch','remote_branch','pull_request') AND status NOT IN ('deleted','retained','synced','not_applicable')`, []any{current.Lifecycle.RepositoryBindingDigest}},
		{&mutation, `SELECT COUNT(*) FROM configuration_apply_intents WHERE status IN ('accepted','ambiguous')`, nil},
	}
	for _, query := range queries {
		if err := tx.QueryRowContext(ctx, query.query, query.args...).Scan(query.target); err != nil {
			return nil, err
		}
	}
	results = append(results,
		// Onboarding has no persistent saga yet. This explicit guard must move to
		// durable onboarding evidence before such a workflow can be introduced.
		guard("no_active_onboarding", true, "repository_onboarding_in_progress", "wait_for_repository_onboarding"),
		guard("no_nonterminal_run", nonterminal == 0, "nonterminal_run_exists", "settle_or_abandon_run"),
		guard("no_recheck", recheck == 0, "repository_recheck_in_progress", "wait_for_recheck"),
		guard("no_repository_mutation", operations == 0, "repository_mutation_in_progress", "wait_for_repository_operation"),
		guard("no_repository_slot", slots == 0, "repository_slot_unresolved", "reconcile_scheduling"),
		guard("no_execution_lease", leases == 0, "execution_lease_unresolved", "wait_for_execution_lease"),
		guard("no_heavy_permit", permits == 0, "heavy_permit_unresolved", "reconcile_scheduling"),
		guard("no_scheduling_conflict", scheduling == 0, "scheduling_conflict", "reconcile_scheduling"),
		guard("cleanup_settled", cleanup == 0, "cleanup_residue_unresolved", "resolve_cleanup_residue"),
		guard("configuration_mutation_idle", mutation == 0, "configuration_apply_in_progress", "wait_for_configuration_apply"),
	)
	configuration, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found {
		return nil, application.ErrConfigurationAuthorityConflict
	}
	converged := configuration.Incomplete == nil && configuration.IncompleteRecovery == nil && configuration.Desired.GenerationID == expected.ConfigurationAuthority.GenerationID && configuration.Desired.Digest == expected.ConfigurationAuthority.Digest && configuration.Version == expected.ConfigurationAuthority.AuthorityVersion && configuration.EffectiveID == configuration.Desired.GenerationID && configuration.Desired.State == application.ConfigurationGenerationEffective
	results = append(results, guard("configuration_converged", converged, "configuration_not_converged", "restore_exact_configuration_convergence"))
	_ = repositoryCount
	return results, nil
}

func repositoryRemovalDraftQuery(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, draftID string) (application.RepositoryRemovalDraft, bool, error) {
	draft, err := scanRepositoryRemovalDraft(query.QueryRowContext(ctx, repositoryRemovalDraftSelect+` WHERE draft_id=?`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.RepositoryRemovalDraft{}, false, nil
	}
	if err != nil {
		return application.RepositoryRemovalDraft{}, false, err
	}
	if draft.RemovalOperationID != "" {
		receipt, found, receiptErr := getOperationReceiptByIDQuery(ctx, query, draft.RemovalOperationID)
		if receiptErr != nil || !found {
			return application.RepositoryRemovalDraft{}, false, errors.New("repository removal receipt evidence is unavailable")
		}
		draft.Receipt = &receipt
	}
	return draft, true, nil
}

func scanRepositoryRemovalDraft(row interface{ Scan(...any) error }) (application.RepositoryRemovalDraft, error) {
	var draft application.RepositoryRemovalDraft
	var validationCandidate, validationDigest, guardsJSON, validatedAt, previewDigest, previewJSON, previewedAt string
	var validationValid int
	var createdAt, updatedAt, settledAt string
	if err := row.Scan(&draft.DraftID, &draft.Revision, &draft.State, &draft.Repository, &draft.IncarnationID, &draft.ProfileID, &draft.ProfileDigest, &draft.RepositoryBindingDigest, &draft.LifecycleVersion, &draft.BaseGenerationID, &draft.BaseDigest, &draft.ConfigurationAuthorityVersion, &draft.RepositoryCountBefore, &validationCandidate, &validationDigest, &validationValid, &guardsJSON, &validatedAt, &previewDigest, &previewJSON, &previewedAt, &draft.RemovalOperationID, &draft.ConfigurationOperationID, &draft.ResultGenerationID, &draft.ResultDigest, &createdAt, &updatedAt, &settledAt, &draft.ReasonCode); err != nil {
		return application.RepositoryRemovalDraft{}, err
	}
	draft.CreatedAt, draft.UpdatedAt, draft.SettledAt = parseTime(createdAt), parseTime(updatedAt), parseTime(settledAt)
	if validationDigest != "" {
		var guards []application.RepositoryRemovalGuardResult
		if json.Unmarshal([]byte(guardsJSON), &guards) != nil {
			return application.RepositoryRemovalDraft{}, errors.New("repository removal guard evidence is corrupt")
		}
		draft.Validation = &application.RepositoryRemovalValidation{DraftID: draft.DraftID, Revision: draft.Revision, CandidateDigest: validationCandidate, ValidationDigest: validationDigest, Valid: validationValid == 1, Guards: guards, ValidatedAt: parseTime(validatedAt)}
	}
	if previewDigest != "" {
		var preview application.RepositoryRemovalPreview
		if json.Unmarshal([]byte(previewJSON), &preview) != nil || preview.PreviewDigest != previewDigest {
			return application.RepositoryRemovalDraft{}, errors.New("repository removal preview evidence is corrupt")
		}
		draft.Preview = &preview
		_ = previewedAt
	}
	return draft, nil
}

func repositoryRemovalProjectionTx(ctx context.Context, tx *sql.Tx, incarnationID string) (*application.RepositoryRemovalProjection, error) {
	var state, operationID, resultDigest, updated string
	var generationID int64
	err := tx.QueryRowContext(ctx, `SELECT removal_state,removal_operation_id,removal_result_generation_id,removal_result_digest,updated_at FROM repository_lifecycles WHERE incarnation_id=? AND retired_at=''`, incarnationID).Scan(&state, &operationID, &generationID, &resultDigest, &updated)
	if err != nil {
		return nil, err
	}
	if state == "" {
		return nil, nil
	}
	next := "apply_removal_draft"
	if state == application.RepositoryRemovalPending {
		next = "restart_worker_and_wait_for_convergence"
	}
	return &application.RepositoryRemovalProjection{OperationID: operationID, ResultGenerationID: generationID, ResultDigest: resultDigest, State: state, NextAction: next, UpdatedAt: parseTime(updated)}, nil
}

func sameRemovalTargetAuthority(current, expected application.RepositoryOperationAuthority) bool {
	return current.Lifecycle.IncarnationID == expected.Lifecycle.IncarnationID && current.Lifecycle.Repository == expected.Lifecycle.Repository && current.Lifecycle.ProfileID == expected.Lifecycle.ProfileID && current.Lifecycle.ProfileDigest == expected.Lifecycle.ProfileDigest && current.Lifecycle.RepositoryBindingDigest == expected.Lifecycle.RepositoryBindingDigest && current.Lifecycle.Intent == application.RepositoryDisabled && current.Lifecycle.Version == expected.Lifecycle.Version && current.ConfigurationAuthority.GenerationID == expected.ConfigurationAuthority.GenerationID && current.ConfigurationAuthority.Digest == expected.ConfigurationAuthority.Digest && current.ConfigurationAuthority.AuthorityVersion == expected.ConfigurationAuthority.AuthorityVersion && (current.Removal == nil) == (expected.Removal == nil) && (current.Removal == nil || current.Removal.OperationID == expected.Removal.OperationID)
}

func sameRemovalDraftOpen(draft application.RepositoryRemovalDraft, input application.RepositoryRemovalOpenInput) bool {
	return draft.State == application.RepositoryRemovalDraftOpen && draft.Repository == input.Authority.Lifecycle.Repository && draft.IncarnationID == input.Authority.Lifecycle.IncarnationID && draft.ProfileID == input.Authority.Lifecycle.ProfileID && draft.ProfileDigest == input.Authority.Lifecycle.ProfileDigest && draft.RepositoryBindingDigest == input.Authority.Lifecycle.RepositoryBindingDigest && draft.LifecycleVersion == input.Authority.Lifecycle.Version && draft.BaseGenerationID == input.Authority.ConfigurationAuthority.GenerationID && draft.BaseDigest == input.Authority.ConfigurationAuthority.Digest && draft.ConfigurationAuthorityVersion == input.Authority.ConfigurationAuthority.AuthorityVersion
}

func validRepositoryRemovalDraftID(value string) bool {
	if !strings.HasPrefix(value, "repository-removal-draft-") || len(value) != len("repository-removal-draft-")+32 {
		return false
	}
	for _, char := range value[len("repository-removal-draft-"):] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func allGuardsAllowed(guards []application.RepositoryRemovalGuardResult) bool {
	if len(guards) == 0 {
		return false
	}
	for _, guard := range guards {
		if !guard.Allowed {
			return false
		}
	}
	return true
}

func settleRepositoryRemovalForEffectiveTx(ctx context.Context, tx *sql.Tx, observation application.ConfigurationEffectiveObservation) error {
	rows, err := tx.QueryContext(ctx, `SELECT operation_id,draft_id,incarnation_id,result_generation_id,candidate_digest,status,base_generation_id FROM repository_removal_intents WHERE status IN ('accepted','applied') AND candidate_digest=? AND result_generation_id IN (0,?) ORDER BY operation_id`, observation.ExpectedDigest, observation.ExpectedGenerationID)
	if err != nil {
		return err
	}
	type pending struct {
		operationID, draftID, incarnationID, digest, status string
		generationID, baseGenerationID                      int64
	}
	var values []pending
	for rows.Next() {
		var value pending
		if err := rows.Scan(&value.operationID, &value.draftID, &value.incarnationID, &value.generationID, &value.digest, &value.status, &value.baseGenerationID); err != nil {
			rows.Close()
			return err
		}
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range values {
		if value.status == "accepted" {
			var parentID int64
			var configurationOperationID string
			if err := tx.QueryRowContext(ctx, `SELECT parent_generation_id,operation_id FROM configuration_generations WHERE generation_id=? AND digest=?`, observation.ExpectedGenerationID, observation.ExpectedDigest).Scan(&parentID, &configurationOperationID); err != nil || parentID != value.baseGenerationID || configurationOperationID == "" {
				return application.ErrRepositoryLifecycleConflict
			}
			value.generationID = observation.ExpectedGenerationID
			result, err := tx.ExecContext(ctx, `UPDATE repository_removal_intents SET status='applied',configuration_operation_id=?,result_generation_id=?,applied_at=? WHERE operation_id=? AND status='accepted'`, configurationOperationID, value.generationID, formatTime(observation.ObservedAt), value.operationID)
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return application.ErrRepositoryLifecycleConflict
			}
			result, err = tx.ExecContext(ctx, `UPDATE repository_lifecycles SET removal_state=?,removal_result_generation_id=?,removal_result_digest=?,updated_at=? WHERE incarnation_id=? AND retired_at='' AND removal_state='accepted' AND removal_operation_id=?`, application.RepositoryRemovalPending, value.generationID, value.digest, formatTime(observation.ObservedAt), value.incarnationID, value.operationID)
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return application.ErrRepositoryLifecycleConflict
			}
			result, err = tx.ExecContext(ctx, `UPDATE repository_removal_drafts SET lifecycle='applied',configuration_operation_id=?,result_generation_id=?,result_digest=?,updated_at=? WHERE draft_id=? AND lifecycle='applying' AND removal_operation_id=?`, configurationOperationID, value.generationID, value.digest, formatTime(observation.ObservedAt), value.draftID, value.operationID)
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return application.ErrRepositoryLifecycleConflict
			}
			appliedEvidence := repositoryDigest("repository-removal-applied-v1", value.incarnationID, value.digest, configurationOperationID)
			result, err = tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='applied',outcome='pending',resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,applied_at=? WHERE operation_id=? AND phase='accepted' AND outcome='pending'`, value.digest, application.RepositoryRemovalPending, value.generationID, appliedEvidence, appliedEvidence, formatTime(observation.ObservedAt), value.operationID)
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return application.ErrOperationReceiptConflict
			}
		}
		evidence := repositoryDigest("repository-retirement-v1", value.incarnationID, value.operationID, value.digest, observation.EvidenceDigest, formatTime(observation.ObservedAt))
		result, err := tx.ExecContext(ctx, `UPDATE repository_lifecycles SET removal_state='',retired_at=?,retirement_evidence_digest=?,updated_at=? WHERE incarnation_id=? AND retired_at='' AND removal_state=? AND removal_operation_id=? AND removal_result_generation_id=? AND removal_result_digest=? AND intent='disabled'`, formatTime(observation.ObservedAt), evidence, formatTime(observation.ObservedAt), value.incarnationID, application.RepositoryRemovalPending, value.operationID, value.generationID, value.digest)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.ErrRepositoryLifecycleConflict
		}
		result, err = tx.ExecContext(ctx, `UPDATE repository_removal_intents SET status='observed',settled_at=?,retirement_evidence_digest=? WHERE operation_id=? AND status='applied'`, formatTime(observation.ObservedAt), evidence, value.operationID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.ErrRepositoryLifecycleConflict
		}
		result, err = tx.ExecContext(ctx, `UPDATE repository_removal_drafts SET settled_at=?,reason_code='retired',updated_at=? WHERE draft_id=? AND lifecycle='applied' AND removal_operation_id=?`, formatTime(observation.ObservedAt), formatTime(observation.ObservedAt), value.draftID, value.operationID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.ErrRepositoryLifecycleConflict
		}
		resultDigest := repositoryDigest("repository-removal-result-v1", value.incarnationID, value.digest, evidence)
		result, err = tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='observed',outcome='succeeded',resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,settled_at=? WHERE operation_id=? AND phase='applied' AND outcome='pending'`, value.digest, application.RepositoryRemovalRetired, value.generationID, evidence, resultDigest, formatTime(observation.ObservedAt), value.operationID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return application.ErrOperationReceiptConflict
		}
	}
	return nil
}
