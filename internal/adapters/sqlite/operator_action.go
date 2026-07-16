package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const operatorActionSelect = `SELECT action_id,idempotency_key,payload_digest,run_id,repository,expected_state,run_idempotency_key,transition_sequence,action_type,requester_login,requester_database_id,requester_node_id,requester_actor_type,reason_code,attention_event_key,status,result_status,resulting_state,resulting_transition_sequence,evidence_digest,outcome_digest,received_at,validated_at,applied_at,observed_at FROM operator_actions`

func (s *Store) listOperatorActions(ctx context.Context, runID string) ([]application.OperatorActionRecord, error) {
	rows, err := s.db.QueryContext(ctx, operatorActionSelect+` WHERE run_id=? ORDER BY transition_sequence,action_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []application.OperatorActionRecord{}
	for rows.Next() {
		record, scanErr := scanOperatorAction(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) BeginOperatorAction(ctx context.Context, record application.OperatorActionRecord) (application.OperatorActionRecord, bool, error) {
	if err := application.ValidateOperatorActionRecord(record); err != nil {
		return application.OperatorActionRecord{}, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO operator_actions(action_id,idempotency_key,payload_digest,run_id,repository,expected_state,run_idempotency_key,transition_sequence,action_type,requester_login,requester_database_id,requester_node_id,requester_actor_type,reason_code,attention_event_key,status,result_status,received_at,validated_at)
		SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
		FROM runs
		WHERE run_id=? AND repository=? AND current_state=? AND idempotency_key=?
		AND (SELECT COALESCE(MAX(sequence),0) FROM transitions WHERE transitions.run_id=runs.run_id)=?
		AND EXISTS (
			SELECT 1 FROM operator_attention_outbox
			WHERE event_key=? AND run_id=runs.run_id AND controller_state=? AND reason_code=?
			AND rowid=(SELECT rowid FROM operator_attention_outbox current WHERE current.run_id=runs.run_id ORDER BY created_at DESC,rowid DESC LIMIT 1)
			AND CASE ?
				WHEN 'retry' THEN allowed_actions_json='["retry","abandon"]'
				WHEN 'abandon' THEN allowed_actions_json IN ('["retry","abandon"]','["abandon"]')
				ELSE 0
			END
		)
		ON CONFLICT DO NOTHING`,
		record.ActionID, record.IdempotencyKey, record.PayloadDigest, record.RunID, record.Repository, string(record.ExpectedState), record.RunIdempotencyKey, record.TransitionSequence, string(record.ActionType), record.Requester.ID, record.Requester.DatabaseID, record.Requester.NodeID, record.Requester.ActorType, record.ReasonCode, record.AttentionEventKey, record.Status, record.ResultStatus, formatTime(record.ReceivedAt), formatTime(record.ValidatedAt),
		record.RunID, record.Repository, string(record.ExpectedState), record.RunIdempotencyKey, record.TransitionSequence, record.AttentionEventKey, string(record.ExpectedState), record.ReasonCode, string(record.ActionType))
	if err != nil {
		return application.OperatorActionRecord{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return application.OperatorActionRecord{}, false, err
	}
	if affected == 1 {
		return record, true, nil
	}
	existing, found, lookupErr := scanOperatorActionMaybe(s.db.QueryRowContext(ctx, operatorActionSelect+` WHERE idempotency_key=?`, record.IdempotencyKey))
	if lookupErr != nil {
		return application.OperatorActionRecord{}, false, lookupErr
	}
	if found {
		if existing.PayloadDigest != record.PayloadDigest || existing.ActionID != record.ActionID {
			return application.OperatorActionRecord{}, false, errors.New("operator action idempotency authority conflicts")
		}
		return existing, false, nil
	}
	return application.OperatorActionRecord{}, false, errors.New("operator action current authority conflicts")
}

func (s *Store) ApplyOperatorActionResult(ctx context.Context, result application.OperatorActionMutationResult) (application.OperatorActionRecord, bool, error) {
	return s.advanceOperatorAction(ctx, result, false)
}

func (s *Store) ObserveOperatorActionResult(ctx context.Context, result application.OperatorActionMutationResult) (application.OperatorActionRecord, bool, error) {
	return s.advanceOperatorAction(ctx, result, true)
}

func (s *Store) advanceOperatorAction(ctx context.Context, result application.OperatorActionMutationResult, observed bool) (application.OperatorActionRecord, bool, error) {
	if err := application.ValidateOperatorActionMutationResult(result, observed); err != nil {
		return application.OperatorActionRecord{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.OperatorActionRecord{}, false, err
	}
	defer tx.Rollback()
	record, found, err := getOperatorActionByIDTx(ctx, tx, result.ActionID)
	if err != nil || !found {
		if err == nil {
			err = errors.New("operator action was not found")
		}
		return application.OperatorActionRecord{}, false, err
	}
	if record.Status != result.ExpectedStatus {
		if sameOperatorActionResult(record, result, observed) {
			return record, false, nil
		}
		return application.OperatorActionRecord{}, false, errors.New("operator action lifecycle authority conflicts")
	}
	if (!observed && result.At.Before(record.ValidatedAt)) || (observed && result.At.Before(record.AppliedAt)) {
		return application.OperatorActionRecord{}, false, errors.New("operator action lifecycle timestamp conflicts")
	}
	nextStatus := application.OperatorActionStatusApplied
	var update sql.Result
	if observed {
		if record.ResultingState != result.ResultingState || record.ResultingTransitionSequence != result.ResultingTransitionSequence {
			return application.OperatorActionRecord{}, false, errors.New("operator action applied result authority conflicts")
		}
		nextStatus = application.OperatorActionStatusObserved
		update, err = tx.ExecContext(ctx, `UPDATE operator_actions SET status=?,result_status=?,outcome_digest=?,observed_at=? WHERE action_id=? AND status=?`, nextStatus, result.ResultStatus, result.EvidenceDigest, formatTime(result.At), result.ActionID, result.ExpectedStatus)
	} else {
		var state string
		var sequence int64
		if err := tx.QueryRowContext(ctx, `SELECT current_state,(SELECT COALESCE(MAX(sequence),0) FROM transitions WHERE run_id=runs.run_id) FROM runs WHERE run_id=?`, record.RunID).Scan(&state, &sequence); err != nil {
			return application.OperatorActionRecord{}, false, err
		}
		if state != string(result.ResultingState) || sequence != result.ResultingTransitionSequence || sequence < record.TransitionSequence {
			return application.OperatorActionRecord{}, false, errors.New("operator action resulting authority conflicts")
		}
		update, err = tx.ExecContext(ctx, `UPDATE operator_actions SET status=?,result_status=?,resulting_state=?,resulting_transition_sequence=?,evidence_digest=?,applied_at=? WHERE action_id=? AND status=?`, nextStatus, result.ResultStatus, state, sequence, result.EvidenceDigest, formatTime(result.At), result.ActionID, result.ExpectedStatus)
	}
	if err != nil {
		return application.OperatorActionRecord{}, false, err
	}
	if affected, affectedErr := update.RowsAffected(); affectedErr != nil || affected != 1 {
		return application.OperatorActionRecord{}, false, errors.New("operator action lifecycle compare-and-swap lost")
	}
	updated, _, err := getOperatorActionByIDTx(ctx, tx, result.ActionID)
	if err != nil || application.ValidateOperatorActionRecord(updated) != nil {
		return application.OperatorActionRecord{}, false, errors.New("operator action persisted result is invalid")
	}
	if err := tx.Commit(); err != nil {
		return application.OperatorActionRecord{}, false, err
	}
	return updated, true, nil
}

func sameOperatorActionResult(record application.OperatorActionRecord, result application.OperatorActionMutationResult, observed bool) bool {
	if record.ResultStatus != result.ResultStatus || record.ResultingState != result.ResultingState || record.ResultingTransitionSequence != result.ResultingTransitionSequence {
		return false
	}
	if observed {
		return record.Status == application.OperatorActionStatusObserved && record.OutcomeDigest == result.EvidenceDigest
	}
	return record.EvidenceDigest == result.EvidenceDigest && (record.Status == application.OperatorActionStatusApplied || record.Status == application.OperatorActionStatusObserved)
}

func getOperatorActionByIDTx(ctx context.Context, tx *sql.Tx, id string) (application.OperatorActionRecord, bool, error) {
	return scanOperatorActionMaybe(tx.QueryRowContext(ctx, operatorActionSelect+` WHERE action_id=?`, id))
}

func scanOperatorActionMaybe(row rowScanner) (application.OperatorActionRecord, bool, error) {
	record, err := scanOperatorAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return application.OperatorActionRecord{}, false, nil
	}
	return record, err == nil, err
}

func scanOperatorAction(row rowScanner) (application.OperatorActionRecord, error) {
	var record application.OperatorActionRecord
	var expected, actionType, resulting, received, validated, applied, observed string
	if err := row.Scan(&record.ActionID, &record.IdempotencyKey, &record.PayloadDigest, &record.RunID, &record.Repository, &expected, &record.RunIdempotencyKey, &record.TransitionSequence, &actionType, &record.Requester.ID, &record.Requester.DatabaseID, &record.Requester.NodeID, &record.Requester.ActorType, &record.ReasonCode, &record.AttentionEventKey, &record.Status, &record.ResultStatus, &resulting, &record.ResultingTransitionSequence, &record.EvidenceDigest, &record.OutcomeDigest, &received, &validated, &applied, &observed); err != nil {
		return application.OperatorActionRecord{}, err
	}
	record.ExpectedState, record.ActionType, record.ResultingState = domain.State(expected), application.OperatorActionType(actionType), domain.State(resulting)
	record.Requester.Kind = "github_login"
	record.ReceivedAt, record.ValidatedAt, record.AppliedAt, record.ObservedAt = parseTime(received), parseTime(validated), parseTime(applied), parseTime(observed)
	if err := application.ValidateOperatorActionRecord(record); err != nil {
		return application.OperatorActionRecord{}, errors.New("operator action record is corrupt")
	}
	return record, nil
}
