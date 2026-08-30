package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const cleanupSourceRecoverySelect = `SELECT request_id,operation_id,run_id,repository,transition_sequence,abandon_action_digest,attention_event_key,attention_evidence_digest,ownership_digest,cleanup_digest,frozen_source_digest,replacement_source_digest,replacement_identity_digest,repository_origin_digest,registration_digest,repository_binding_digest,branch,candidate_head,preview_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,stage,created_at,updated_at,settled_at FROM cleanup_source_recovery_intents`

func (s *Store) GetCleanupSourceRecovery(ctx context.Context, requestID string) (application.CleanupSourceRecoveryIntent, application.OperationReceipt, bool, error) {
	intent, err := scanCleanupSourceRecovery(s.db.QueryRowContext(ctx, cleanupSourceRecoverySelect+` WHERE request_id=?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, nil
	}
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	receipt, err := scanOperationReceipt(s.db.QueryRowContext(ctx, operationReceiptSelect+` WHERE operation_id=?`, intent.OperationID))
	return intent, receipt, err == nil, err
}

func (s *Store) ValidateCleanupSourceRecoveryAuthority(ctx context.Context, expected application.CleanupSourceRecoveryAuthority, requester domain.GitHubUserIdentity) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCleanupSourceRecoveryAuthorityTx(ctx, tx, expected, requester, true); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginCleanupSourceRecovery(ctx context.Context, intent application.CleanupSourceRecoveryIntent, receipt application.OperationReceipt) (application.CleanupSourceRecoveryIntent, application.OperationReceipt, bool, error) {
	if err := validateCleanupSourceRecoveryIntent(intent, receipt); err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	if existing, found, lookupErr := cleanupSourceRecoveryByRequestTx(ctx, tx, intent.RequestID); lookupErr != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, lookupErr
	} else if found {
		persistedReceipt, receiptFound, receiptErr := getOperationReceiptByIDTx(ctx, tx, existing.OperationID)
		if receiptErr != nil || !receiptFound {
			return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("cleanup source recovery receipt is unavailable")
		}
		if !sameCleanupSourceRecoveryAuthority(existing, intent) {
			return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
		}
		return existing, persistedReceipt, false, tx.Commit()
	}
	if _, found, lookupErr := cleanupSourceRecoveryByRunTx(ctx, tx, intent.Authority.RunID); lookupErr != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, lookupErr
	} else if found {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	if _, found, lookupErr := getOperationReceiptByAuthorityTx(ctx, tx, receipt.AuthorityKey); lookupErr != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, lookupErr
	} else if found {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	if err := validateCleanupSourceRecoveryAuthorityTx(ctx, tx, intent.Authority, intent.Requester, intent.Stage == application.CleanupSourceRecoveryAccepted); err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if err := insertOperationReceiptTx(ctx, tx, receipt, ""); err != nil {
		if isCleanupSourceRecoveryConstraint(err) {
			return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
		}
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cleanup_source_recovery_intents(request_id,operation_id,run_id,repository,transition_sequence,abandon_action_digest,attention_event_key,attention_evidence_digest,ownership_digest,cleanup_digest,frozen_source_digest,replacement_source_digest,replacement_identity_digest,repository_origin_digest,registration_digest,repository_binding_digest,branch,candidate_head,preview_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,stage,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		intent.RequestID, intent.OperationID, intent.Authority.RunID, intent.Authority.Repository, intent.Authority.TransitionSequence, intent.Authority.AbandonActionDigest, intent.Authority.AttentionEventKey, intent.Authority.AttentionEvidenceDigest, intent.Authority.OwnershipDigest, intent.Authority.CleanupDigest, intent.Authority.FrozenSourceDigest, intent.Authority.ReplacementSourceDigest, intent.Authority.ReplacementIdentityDigest, intent.Authority.RepositoryOriginDigest, intent.Authority.RegistrationDigest, intent.Authority.RepositoryBindingDigest, intent.Authority.Branch, intent.Authority.CandidateHead, intent.Authority.PreviewDigest, intent.Requester.Login, intent.Requester.DatabaseID, intent.Requester.NodeID, intent.Requester.ActorType, string(intent.Stage), formatTime(intent.CreatedAt), formatTime(intent.UpdatedAt))
	if err != nil {
		if isCleanupSourceRecoveryConstraint(err) {
			return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
		}
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	return intent, receipt, true, nil
}

func (s *Store) AdvanceCleanupSourceRecovery(ctx context.Context, requestID string, expected, next application.CleanupSourceRecoveryStage, at time.Time) (application.CleanupSourceRecoveryIntent, application.OperationReceipt, bool, error) {
	if strings.TrimSpace(requestID) == "" || at.IsZero() || !validCleanupSourceRecoveryAdvance(expected, next) {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("cleanup source recovery advance is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	intent, found, err := cleanupSourceRecoveryByRequestTx(ctx, tx, requestID)
	if err != nil || !found {
		if err == nil {
			err = application.ErrOperationReceiptNotFound
		}
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	receipt, receiptFound, err := getOperationReceiptByIDTx(ctx, tx, intent.OperationID)
	if err != nil || !receiptFound {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("cleanup source recovery receipt is unavailable")
	}
	if intent.Stage == next {
		return intent, receipt, false, tx.Commit()
	}
	if intent.Stage != expected {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	if err := validateCleanupSourceRecoveryAuthorityTx(ctx, tx, intent.Authority, intent.Requester, true); err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE cleanup_source_recovery_intents SET stage=?,updated_at=? WHERE request_id=? AND stage=?`, string(next), formatTime(at), requestID, string(expected))
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	if expected == application.CleanupSourceRecoveryAccepted {
		evidence := application.SHA256Digest("cleanup-source-repair-intent-v1", intent.OperationID, intent.Authority.RegistrationDigest)
		result, err = tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='applied',outcome='pending',evidence_digest=?,applied_at=? WHERE operation_id=? AND operation_type='recover_cleanup_source' AND phase='accepted' AND outcome='pending'`, evidence, formatTime(at), intent.OperationID)
		if err != nil {
			return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
		}
	}
	intent.Stage, intent.UpdatedAt = next, at.UTC()
	receipt, _, err = getOperationReceiptByIDTx(ctx, tx, intent.OperationID)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	return intent, receipt, true, tx.Commit()
}

func (s *Store) SettleCleanupSourceRecovery(ctx context.Context, requestID string, at time.Time) (application.CleanupSourceRecoveryIntent, application.OperationReceipt, bool, error) {
	if strings.TrimSpace(requestID) == "" || at.IsZero() {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("cleanup source recovery settlement is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	intent, found, err := cleanupSourceRecoveryByRequestTx(ctx, tx, requestID)
	if err != nil || !found {
		if err == nil {
			err = application.ErrOperationReceiptNotFound
		}
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	receipt, receiptFound, err := getOperationReceiptByIDTx(ctx, tx, intent.OperationID)
	if err != nil || !receiptFound {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("cleanup source recovery receipt is unavailable")
	}
	if intent.Stage == application.CleanupSourceRecoverySucceeded {
		return intent, receipt, false, tx.Commit()
	}
	if intent.Stage != application.CleanupSourceRecoveryCleanupObserved {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	if err := validateCleanupSourceRecoveryAuthorityTx(ctx, tx, intent.Authority, intent.Requester, true); err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	var worktree string
	if err := tx.QueryRowContext(ctx, `SELECT worktree_path FROM runs WHERE run_id=?`, intent.Authority.RunID).Scan(&worktree); err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE cleanup_results SET status='deleted',error_class='',last_error='',updated_at=? WHERE run_id=? AND resource_kind='worktree' AND resource_name=? AND status IN ('failed','intent')`, formatTime(at), intent.Authority.RunID, worktree)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE owned_resources SET ownership_status='deleted' WHERE owning_run=? AND resource_kind='worktree' AND resource_name=? AND ownership_status IN ('owned','reserved')`, intent.Authority.RunID, worktree)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	resultDigest := application.SHA256Digest("cleanup-source-recovery-succeeded-v1", intent.OperationID, intent.Authority.RegistrationDigest, intent.Authority.CandidateHead)
	result, err = tx.ExecContext(ctx, `UPDATE cleanup_source_recovery_intents SET stage='succeeded',updated_at=?,settled_at=? WHERE request_id=? AND stage='cleanup_observed'`, formatTime(at), formatTime(at), requestID)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='observed',outcome='succeeded',resulting_authority_digest=?,resulting_state='cleanup_reconciled',resulting_version=1,result_digest=?,settled_at=? WHERE operation_id=? AND operation_type='recover_cleanup_source' AND phase='applied' AND outcome='pending'`, resultDigest, resultDigest, formatTime(at), intent.OperationID)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
	}
	if err := appendSettledOperationActivityTx(ctx, tx, intent.OperationID, application.ActivityIngestionCurrent); err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	intent.Stage, intent.UpdatedAt = application.CleanupSourceRecoverySucceeded, at.UTC()
	receipt, _, err = getOperationReceiptByIDTx(ctx, tx, intent.OperationID)
	if err != nil {
		return application.CleanupSourceRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	return intent, receipt, true, tx.Commit()
}

func validateCleanupSourceRecoveryAuthorityTx(ctx context.Context, tx *sql.Tx, expected application.CleanupSourceRecoveryAuthority, requester domain.GitHubUserIdentity, requireResidue bool) error {
	configuration, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || !configuration.Desired.ConfiguredOperator.Equal(requester) {
		return application.ErrOperationReceiptConflict
	}
	run, err := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, expected.RunID))
	if err != nil {
		return err
	}
	if run.State != domain.StateFailed || run.LastError != application.AutomaticAdmissionAbandonTransition || run.Repository != expected.Repository || run.RepositoryBindingDigest != expected.RepositoryBindingDigest || run.WorkingBranch != expected.Branch || run.CandidateHead != expected.CandidateHead {
		return application.ErrOperationReceiptConflict
	}
	var repository application.LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil || repository.CanonicalRepository != run.Repository || application.DigestCleanupFrozenSource(repository.SourcePath) != expected.FrozenSourceDigest {
		return application.ErrOperationReceiptConflict
	}
	if run.LeaseOwner != "" || !run.LeaseExpiresAt.IsZero() {
		return application.ErrOperationReceiptConflict
	}
	if application.AuthorizePersistedRequester(run, application.Requester{ID: requester.Login, Kind: "github_login", DatabaseID: requester.DatabaseID, NodeID: requester.NodeID, ActorType: requester.ActorType}) != nil {
		return application.ErrOperationReceiptConflict
	}
	var seq int64
	var to, reason string
	if err := tx.QueryRowContext(ctx, `SELECT sequence,to_state,reason FROM transitions WHERE run_id=? ORDER BY sequence DESC LIMIT 1`, run.ID).Scan(&seq, &to, &reason); err != nil || seq != expected.TransitionSequence || to != string(domain.StateFailed) || reason != application.AutomaticAdmissionAbandonTransition {
		return application.ErrOperationReceiptConflict
	}
	var abandonActions int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_actions WHERE run_id=? AND action_type='abandon' AND status='observed' AND result_status='succeeded' AND resulting_transition_sequence=?`, run.ID, seq).Scan(&abandonActions); err != nil || abandonActions != 1 {
		return application.ErrOperationReceiptConflict
	}
	var action application.OperatorActionRecord
	if err := tx.QueryRowContext(ctx, `SELECT action_id,payload_digest,request_digest,expected_authority_digest,evidence_digest,outcome_digest,transition_sequence,resulting_transition_sequence FROM operator_actions WHERE run_id=? AND action_type='abandon' AND status='observed' AND result_status='succeeded' AND resulting_transition_sequence=?`, run.ID, seq).Scan(&action.ActionID, &action.PayloadDigest, &action.RequestDigest, &action.ExpectedAuthorityDigest, &action.EvidenceDigest, &action.OutcomeDigest, &action.TransitionSequence, &action.ResultingTransitionSequence); err != nil || application.CleanupSourceRecoveryAbandonActionDigest(action) != expected.AbandonActionDigest {
		return application.ErrOperationReceiptConflict
	}
	event, err := scanOperatorAttention(tx.QueryRowContext(ctx, operatorAttentionSelect+` WHERE run_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, run.ID))
	if err != nil || event.EventKey != expected.AttentionEventKey || event.EventKey != application.CleanupSourceRecoveryAttentionEventKey(run.ID, seq) || event.EventType != application.OperatorAttentionCleanupResidue || event.EvidenceDigest != expected.AttentionEvidenceDigest || event.ControllerState != string(domain.StateFailed) {
		return application.ErrOperationReceiptConflict
	}
	var active int
	queries := []string{`SELECT COUNT(*) FROM attempts WHERE run_id=? AND status IN ('prepared','started')`, `SELECT COUNT(*) FROM heavy_permits WHERE run_id=?`, `SELECT COUNT(*) FROM repository_slots WHERE run_id=?`, `SELECT COUNT(*) FROM run_scheduling WHERE run_id=? AND supervisor_state<>'terminal'`}
	for _, query := range queries {
		if err := tx.QueryRowContext(ctx, query, run.ID).Scan(&active); err != nil || active != 0 {
			return application.ErrOperationReceiptConflict
		}
	}
	if !requireResidue {
		return nil
	}
	resources, err := cleanupRecoveryResourcesTx(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	cleanup, err := cleanupRecoveryProgressTx(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	ownershipDigest, cleanupDigest, err := application.ValidateCleanupSourceRecoveryResidue(run, resources, cleanup)
	if err != nil || ownershipDigest != expected.OwnershipDigest || cleanupDigest != expected.CleanupDigest {
		return application.ErrOperationReceiptConflict
	}
	return nil
}

func cleanupRecoveryResourcesTx(ctx context.Context, tx *sql.Tx, runID string) ([]application.OwnedResource, error) {
	rows, err := tx.QueryContext(ctx, `SELECT resource_id,owning_run,resource_kind,resource_name,creation_evidence,ownership_status,created_at FROM owned_resources WHERE owning_run=? ORDER BY resource_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []application.OwnedResource
	for rows.Next() {
		var v application.OwnedResource
		var at string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Kind, &v.Name, &v.CreationEvidence, &v.Status, &at); err != nil {
			return nil, err
		}
		v.CreatedAt = parseTime(at)
		values = append(values, v)
	}
	return values, rows.Err()
}

func cleanupRecoveryProgressTx(ctx context.Context, tx *sql.Tx, runID string) ([]application.CleanupRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT cleanup_id,run_id,resource_kind,resource_name,status,error_class,last_error,updated_at FROM cleanup_results WHERE run_id=? ORDER BY cleanup_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []application.CleanupRecord
	for rows.Next() {
		var v application.CleanupRecord
		var at string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Kind, &v.Name, &v.Status, &v.ErrorClass, &v.LastError, &at); err != nil {
			return nil, err
		}
		v.UpdatedAt = parseTime(at)
		values = append(values, v)
	}
	return values, rows.Err()
}

func cleanupSourceRecoveryByRequestTx(ctx context.Context, tx *sql.Tx, requestID string) (application.CleanupSourceRecoveryIntent, bool, error) {
	value, err := scanCleanupSourceRecovery(tx.QueryRowContext(ctx, cleanupSourceRecoverySelect+` WHERE request_id=?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return value, false, nil
	}
	return value, err == nil, err
}

func cleanupSourceRecoveryByRunTx(ctx context.Context, tx *sql.Tx, runID string) (application.CleanupSourceRecoveryIntent, bool, error) {
	value, err := scanCleanupSourceRecovery(tx.QueryRowContext(ctx, cleanupSourceRecoverySelect+` WHERE run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return value, false, nil
	}
	return value, err == nil, err
}

func isCleanupSourceRecoveryConstraint(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 19
}

func scanCleanupSourceRecovery(row rowScanner) (application.CleanupSourceRecoveryIntent, error) {
	var value application.CleanupSourceRecoveryIntent
	var stage, created, updated, settled string
	err := row.Scan(&value.RequestID, &value.OperationID, &value.Authority.RunID, &value.Authority.Repository, &value.Authority.TransitionSequence, &value.Authority.AbandonActionDigest, &value.Authority.AttentionEventKey, &value.Authority.AttentionEvidenceDigest, &value.Authority.OwnershipDigest, &value.Authority.CleanupDigest, &value.Authority.FrozenSourceDigest, &value.Authority.ReplacementSourceDigest, &value.Authority.ReplacementIdentityDigest, &value.Authority.RepositoryOriginDigest, &value.Authority.RegistrationDigest, &value.Authority.RepositoryBindingDigest, &value.Authority.Branch, &value.Authority.CandidateHead, &value.Authority.PreviewDigest, &value.Requester.Login, &value.Requester.DatabaseID, &value.Requester.NodeID, &value.Requester.ActorType, &stage, &created, &updated, &settled)
	value.Stage = application.CleanupSourceRecoveryStage(stage)
	value.CreatedAt = parseTime(created)
	value.UpdatedAt = parseTime(updated)
	_ = settled
	return value, err
}

func validateCleanupSourceRecoveryIntent(intent application.CleanupSourceRecoveryIntent, receipt application.OperationReceipt) error {
	if strings.TrimSpace(intent.RequestID) == "" || len(intent.RequestID) > 128 || intent.OperationID != receipt.OperationID || intent.Stage != application.CleanupSourceRecoveryAccepted || intent.Requester.Validate() != nil || intent.Authority.RunID == "" || intent.Authority.Repository == "" || intent.Authority.Branch == "" || intent.Authority.CandidateHead == "" || intent.CreatedAt.IsZero() || !intent.CreatedAt.Equal(intent.UpdatedAt) || receipt.OperationType != application.OperationRecoverCleanupSource || receipt.Scope != application.ScopeRun || receipt.TargetID != intent.Authority.RunID || receipt.TargetBindingDigest != intent.Authority.RepositoryBindingDigest || receipt.Requester != intent.Requester || receipt.ExpectedAuthorityDigest != application.CleanupSourceRecoveryExpectedAuthorityDigest(intent.Authority) {
		return errors.New("cleanup source recovery intent is invalid")
	}
	for _, digest := range []string{intent.Authority.RepositoryBindingDigest, intent.Authority.AbandonActionDigest, intent.Authority.AttentionEvidenceDigest, intent.Authority.OwnershipDigest, intent.Authority.CleanupDigest, intent.Authority.FrozenSourceDigest, intent.Authority.ReplacementSourceDigest, intent.Authority.ReplacementIdentityDigest, intent.Authority.RepositoryOriginDigest, intent.Authority.RegistrationDigest, intent.Authority.PreviewDigest} {
		if len(digest) != 64 {
			return errors.New("cleanup source recovery digest is invalid")
		}
	}
	return application.ValidateOperationReceipt(receipt)
}

func sameCleanupSourceRecoveryAuthority(left, right application.CleanupSourceRecoveryIntent) bool {
	return left.RequestID == right.RequestID && left.OperationID == right.OperationID && left.Authority == right.Authority && left.Requester == right.Requester
}

func validCleanupSourceRecoveryAdvance(expected, next application.CleanupSourceRecoveryStage) bool {
	order := []application.CleanupSourceRecoveryStage{application.CleanupSourceRecoveryAccepted, application.CleanupSourceRecoveryRepairIntent, application.CleanupSourceRecoveryRepairObserved, application.CleanupSourceRecoveryDetachIntent, application.CleanupSourceRecoveryDetachObserved, application.CleanupSourceRecoveryCleanupIntent, application.CleanupSourceRecoveryCleanupObserved}
	for i := 0; i < len(order)-1; i++ {
		if order[i] == expected && order[i+1] == next {
			return true
		}
	}
	return false
}
