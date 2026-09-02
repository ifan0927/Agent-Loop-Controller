package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const operationReceiptSelect = `SELECT operation_id,authority_key,operation_anchor_digest,operation_type,scope_kind,target_id,requester_login,requester_database_id,requester_node_id,requester_actor_type,request_digest,expected_authority_digest,target_binding_digest,phase,outcome,resulting_authority_digest,resulting_state,resulting_version,evidence_digest,result_digest,accepted_at,applied_at,settled_at FROM operation_receipts`

func (s *Store) BeginOperationReceipt(ctx context.Context, receipt application.OperationReceipt) (application.OperationReceipt, bool, error) {
	if receipt.OperationType == application.OperationRecoverCleanupSource {
		return application.OperationReceipt{}, false, errors.New("retired operation receipt is read-only")
	}
	if err := application.ValidateOperationReceipt(receipt); err != nil {
		return application.OperationReceipt{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	if err := insertOperationReceiptTx(ctx, tx, receipt, ""); err != nil {
		if existing, found, lookupErr := getOperationReceiptByIDTx(ctx, tx, receipt.OperationID); lookupErr != nil {
			return application.OperationReceipt{}, false, lookupErr
		} else if found {
			if sameAcceptedOperationReceipt(existing, receipt) {
				return existing, false, tx.Commit()
			}
			return application.OperationReceipt{}, false, fmt.Errorf("%w: identity changed", application.ErrOperationReceiptConflict)
		}
		if _, found, lookupErr := getOperationReceiptByAuthorityTx(ctx, tx, receipt.AuthorityKey); lookupErr != nil {
			return application.OperationReceipt{}, false, lookupErr
		} else if found {
			return application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
		}
		return application.OperationReceipt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.OperationReceipt{}, false, err
	}
	return receipt, true, nil
}

func insertOperationReceiptTx(ctx context.Context, tx *sql.Tx, receipt application.OperationReceipt, sourceActionID string) error {
	if receipt.OperationType == application.OperationRecoverCleanupSource {
		return errors.New("retired operation receipt is read-only")
	}
	if err := application.ValidateOperationReceipt(receipt); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operation_receipts(operation_id,authority_key,operation_anchor_digest,operation_type,scope_kind,target_id,requester_login,requester_database_id,requester_node_id,requester_actor_type,request_digest,expected_authority_digest,target_binding_digest,phase,outcome,resulting_authority_digest,resulting_state,resulting_version,evidence_digest,result_digest,accepted_at,applied_at,settled_at,source_action_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.OperationID, receipt.AuthorityKey, receipt.OperationAnchorDigest, string(receipt.OperationType), string(receipt.Scope), receipt.TargetID, receipt.Requester.Login, receipt.Requester.DatabaseID, receipt.Requester.NodeID, receipt.Requester.ActorType, receipt.RequestDigest, receipt.ExpectedAuthorityDigest, receipt.TargetBindingDigest, string(receipt.Phase), string(receipt.Outcome), receipt.ResultingAuthorityDigest, receipt.ResultingState, receipt.ResultingVersion, receipt.EvidenceDigest, receipt.ResultDigest, formatTime(receipt.AcceptedAt), formatTime(receipt.AppliedAt), formatTime(receipt.SettledAt), sourceActionID)
	return err
}

func (s *Store) AdvanceOperationReceipt(ctx context.Context, mutation application.OperationReceiptMutation) (application.OperationReceipt, bool, error) {
	if err := application.ValidateOperationReceiptMutation(mutation); err != nil {
		return application.OperationReceipt{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, mutation.OperationID)
	if err != nil || !found {
		if err == nil {
			err = application.ErrOperationReceiptNotFound
		}
		return application.OperationReceipt{}, false, err
	}
	if receipt.OperationType == application.OperationRecoverCleanupSource {
		return application.OperationReceipt{}, false, errors.New("retired operation receipt is read-only")
	}
	if sameOperationReceiptMutation(receipt, mutation) {
		return receipt, false, tx.Commit()
	}
	if receipt.Phase != mutation.ExpectedPhase {
		return application.OperationReceipt{}, false, fmt.Errorf("%w: lifecycle changed", application.ErrOperationReceiptConflict)
	}
	if mutation.At.Before(receipt.AcceptedAt) || !receipt.AppliedAt.IsZero() && mutation.At.Before(receipt.AppliedAt) {
		return application.OperationReceipt{}, false, fmt.Errorf("%w: timestamp changed", application.ErrOperationReceiptConflict)
	}
	appliedAt := receipt.AppliedAt
	if mutation.Phase == application.OperationPhaseApplied && appliedAt.IsZero() {
		appliedAt = mutation.At
	}
	settledAt := receipt.SettledAt
	if mutation.Outcome != application.OperationOutcomePending {
		settledAt = mutation.At
	}
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase=?,outcome=?,resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,applied_at=?,settled_at=? WHERE operation_id=? AND phase=?`, string(mutation.Phase), string(mutation.Outcome), mutation.ResultingAuthorityDigest, mutation.ResultingState, mutation.ResultingVersion, mutation.EvidenceDigest, mutation.ResultDigest, formatTime(appliedAt), formatTime(settledAt), mutation.OperationID, string(mutation.ExpectedPhase))
	if err != nil {
		return application.OperationReceipt{}, false, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.OperationReceipt{}, false, fmt.Errorf("%w: compare-and-swap lost", application.ErrOperationReceiptConflict)
	}
	updated, _, err := getOperationReceiptByIDTx(ctx, tx, mutation.OperationID)
	if err != nil {
		return application.OperationReceipt{}, false, err
	}
	if err := application.ValidateOperationReceipt(updated); err != nil {
		return application.OperationReceipt{}, false, err
	}
	if updated.Outcome != application.OperationOutcomePending {
		if err := appendSettledOperationActivityTx(ctx, tx, updated.OperationID, application.ActivityIngestionCurrent); err != nil {
			return application.OperationReceipt{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return application.OperationReceipt{}, false, err
	}
	return updated, true, nil
}

func (s *Store) GetOperationReceiptTarget(ctx context.Context, operationID string) (application.OperationReceiptTarget, error) {
	var scope, target, binding string
	err := s.db.QueryRowContext(ctx, `SELECT scope_kind,target_id,target_binding_digest FROM operation_receipts WHERE operation_id=?`, operationID).Scan(&scope, &target, &binding)
	if errors.Is(err, sql.ErrNoRows) {
		return application.OperationReceiptTarget{}, application.ErrOperationReceiptNotFound
	}
	if err != nil {
		return application.OperationReceiptTarget{}, err
	}
	return application.OperationReceiptTarget{Scope: application.AuthorityScopeKind(scope), TargetID: target, TargetBindingDigest: binding}, nil
}

func (s *Store) GetAuthorizedOperationReceipt(ctx context.Context, operationID string, scopes application.AuthorizedScopeSet) (application.OperationReceipt, error) {
	if scopes.Empty() {
		return application.OperationReceipt{}, errors.New("authorized operation receipt lookup is invalid")
	}
	receipt, err := scanOperationReceipt(s.db.QueryRowContext(ctx, operationReceiptSelect+` WHERE operation_id=?`, operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.OperationReceipt{}, application.ErrOperationReceiptNotFound
	}
	if err != nil {
		return application.OperationReceipt{}, err
	}
	if !scopes.AllowsOperationTarget(application.OperationReceiptTarget{Scope: receipt.Scope, TargetID: receipt.TargetID, TargetBindingDigest: receipt.TargetBindingDigest}) {
		return application.OperationReceipt{}, application.ErrOperationReceiptNotFound
	}
	return receipt, nil
}

func getOperationReceiptByIDTx(ctx context.Context, tx *sql.Tx, operationID string) (application.OperationReceipt, bool, error) {
	return getOperationReceiptByIDQuery(ctx, tx, operationID)
}

func getOperationReceiptByIDQuery(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, operationID string) (application.OperationReceipt, bool, error) {
	receipt, err := scanOperationReceipt(query.QueryRowContext(ctx, operationReceiptSelect+` WHERE operation_id=?`, operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.OperationReceipt{}, false, nil
	}
	return receipt, err == nil, err
}

func getOperationReceiptByAuthorityTx(ctx context.Context, tx *sql.Tx, authorityKey string) (application.OperationReceipt, bool, error) {
	receipt, err := scanOperationReceipt(tx.QueryRowContext(ctx, operationReceiptSelect+` WHERE authority_key=?`, authorityKey))
	if errors.Is(err, sql.ErrNoRows) {
		return application.OperationReceipt{}, false, nil
	}
	return receipt, err == nil, err
}

func scanOperationReceipt(row rowScanner) (application.OperationReceipt, error) {
	var receipt application.OperationReceipt
	var operationType, scope, phase, outcome, accepted, applied, settled string
	if err := row.Scan(&receipt.OperationID, &receipt.AuthorityKey, &receipt.OperationAnchorDigest, &operationType, &scope, &receipt.TargetID, &receipt.Requester.Login, &receipt.Requester.DatabaseID, &receipt.Requester.NodeID, &receipt.Requester.ActorType, &receipt.RequestDigest, &receipt.ExpectedAuthorityDigest, &receipt.TargetBindingDigest, &phase, &outcome, &receipt.ResultingAuthorityDigest, &receipt.ResultingState, &receipt.ResultingVersion, &receipt.EvidenceDigest, &receipt.ResultDigest, &accepted, &applied, &settled); err != nil {
		return application.OperationReceipt{}, err
	}
	receipt.OperationType = application.OperationType(operationType)
	receipt.Scope = application.AuthorityScopeKind(scope)
	receipt.Phase = application.OperationPhase(phase)
	receipt.Outcome = application.OperationOutcome(outcome)
	receipt.Requester = domain.GitHubUserIdentity{Login: receipt.Requester.Login, DatabaseID: receipt.Requester.DatabaseID, NodeID: receipt.Requester.NodeID, ActorType: receipt.Requester.ActorType}
	receipt.AcceptedAt, receipt.AppliedAt, receipt.SettledAt = parseTime(accepted), parseTime(applied), parseTime(settled)
	if err := application.ValidateOperationReceipt(receipt); err != nil {
		return application.OperationReceipt{}, errors.New("operation receipt is corrupt")
	}
	return receipt, nil
}

func sameAcceptedOperationReceipt(left, right application.OperationReceipt) bool {
	return left.OperationID == right.OperationID && left.AuthorityKey == right.AuthorityKey && left.OperationAnchorDigest == right.OperationAnchorDigest && left.OperationType == right.OperationType && left.Scope == right.Scope && left.TargetID == right.TargetID && left.Requester.Equal(right.Requester) && left.RequestDigest == right.RequestDigest && left.ExpectedAuthorityDigest == right.ExpectedAuthorityDigest && left.TargetBindingDigest == right.TargetBindingDigest
}

func sameOperationReceiptMutation(receipt application.OperationReceipt, mutation application.OperationReceiptMutation) bool {
	return receipt.Phase == mutation.Phase && receipt.Outcome == mutation.Outcome && receipt.ResultingAuthorityDigest == mutation.ResultingAuthorityDigest && receipt.ResultingState == mutation.ResultingState && receipt.ResultingVersion == mutation.ResultingVersion && receipt.EvidenceDigest == mutation.EvidenceDigest && receipt.ResultDigest == mutation.ResultDigest
}

func syncOperationReceiptForActionTx(ctx context.Context, tx *sql.Tx, actionID string) error {
	action, found, err := getOperatorActionByIDTx(ctx, tx, actionID)
	if err != nil || !found {
		if err == nil {
			err = errors.New("operator action receipt source was not found")
		}
		return err
	}
	var binding string
	if err := tx.QueryRowContext(ctx, `SELECT repository_binding_digest FROM runs WHERE run_id=?`, action.RunID).Scan(&binding); err != nil {
		return err
	}
	if !validOperatorActionDigest(binding) {
		binding = application.LegacyRunAuthorityDigest(action.Repository)
	}
	receipt, err := application.OperationReceiptForOperatorAction(action, binding)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase=?,outcome=?,resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,applied_at=?,settled_at=? WHERE source_action_id=? AND operation_id=?`, string(receipt.Phase), string(receipt.Outcome), receipt.ResultingAuthorityDigest, receipt.ResultingState, receipt.ResultingVersion, receipt.EvidenceDigest, receipt.ResultDigest, formatTime(receipt.AppliedAt), formatTime(receipt.SettledAt), actionID, receipt.OperationID)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return errors.New("operator action receipt synchronization lost")
	}
	if receipt.Outcome != application.OperationOutcomePending {
		if action.ResultingTransitionSequence > action.TransitionSequence {
			if err := appendStoredRunTransitionActivityTx(ctx, tx, action.RunID, action.ResultingTransitionSequence, receipt.OperationID); err != nil {
				return err
			}
		}
		if err := appendSettledOperationActivityTx(ctx, tx, receipt.OperationID, application.ActivityIngestionCurrent); err != nil {
			return err
		}
		if err := appendAttentionResolutionForActionTx(ctx, tx, action, receipt); err != nil {
			return err
		}
	}
	return nil
}
