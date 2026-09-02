package sqlite

import (
	"context"
	"database/sql"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

var _ application.RunProgressEvidenceStore = (*Store)(nil)

// ReadRunProgressEvidence returns one consistent, bounded, read-only snapshot
// of the retired CI-wait recovery authority. Duplicate rows are represented by
// counts and intentionally not expanded into an unbounded result.
func (s *Store) ReadRunProgressEvidence(ctx context.Context, runID string) (application.RunProgressEvidence, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.RunProgressEvidence{}, err
	}
	defer tx.Rollback()

	var result application.RunProgressEvidence
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_attention_outbox WHERE run_id=? AND event_type=?`, runID, application.OperatorAttentionCIWaitRecovery).Scan(&result.CIWaitRecoveryAttentionCount); err != nil {
		return application.RunProgressEvidence{}, err
	}
	if result.CIWaitRecoveryAttentionCount == 1 {
		event, scanErr := scanOperatorAttention(tx.QueryRowContext(ctx, operatorAttentionSelect+` WHERE run_id=? AND event_type=? LIMIT 1`, runID, application.OperatorAttentionCIWaitRecovery))
		if scanErr != nil {
			return application.RunProgressEvidence{}, scanErr
		}
		result.CIWaitRecoveryAttention = &event
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_actions WHERE run_id=? AND action_type=?`, runID, application.OperatorActionRecoverCIWait).Scan(&result.CIWaitRecoveryActionCount); err != nil {
		return application.RunProgressEvidence{}, err
	}
	if result.CIWaitRecoveryActionCount == 1 {
		action, scanErr := scanOperatorAction(tx.QueryRowContext(ctx, operatorActionSelect+` WHERE run_id=? AND action_type=? LIMIT 1`, runID, application.OperatorActionRecoverCIWait))
		if scanErr != nil {
			return application.RunProgressEvidence{}, scanErr
		}
		result.CIWaitRecoveryAction = &action

		phase := application.AutomaticRetryPhaseForRun(application.Run{State: action.ExpectedState})
		schedule, found, scheduleErr := retryScheduleTx(ctx, tx, runID, phase)
		if scheduleErr != nil {
			return application.RunProgressEvidence{}, scheduleErr
		}
		if found {
			result.RetrySchedule = &schedule
		}
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_receipts WHERE target_id=? AND operation_type=?`, runID, application.OperationRecoverCIWait).Scan(&result.CIWaitRecoveryReceiptCount); err != nil {
		return application.RunProgressEvidence{}, err
	}
	if result.CIWaitRecoveryReceiptCount == 1 {
		var operationID string
		if err := tx.QueryRowContext(ctx, `SELECT operation_id,source_action_id FROM operation_receipts WHERE target_id=? AND operation_type=? LIMIT 1`, runID, application.OperationRecoverCIWait).Scan(&operationID, &result.ReceiptSourceActionID); err != nil {
			return application.RunProgressEvidence{}, err
		}
		receipt, found, receiptErr := getOperationReceiptByIDQuery(ctx, tx, operationID)
		if receiptErr != nil {
			return application.RunProgressEvidence{}, receiptErr
		}
		if found {
			result.CIWaitRecoveryReceipt = &receipt
		}
	}
	if err := tx.Commit(); err != nil {
		return application.RunProgressEvidence{}, err
	}
	return result, nil
}
