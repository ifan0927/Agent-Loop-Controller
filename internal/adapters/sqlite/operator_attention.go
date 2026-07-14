package sqlite

import (
	"context"
	"errors"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

// AppendOperatorAttention inserts immutable local-only evidence. A repeated
// key is safe only when its complete sanitized payload digest is identical.
func (s *Store) AppendOperatorAttention(ctx context.Context, event application.OperatorAttentionEvent) (bool, error) {
	if err := application.ValidateOperatorAttentionEvent(event); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO operator_attention_outbox(event_key,payload_digest,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,evidence_digest,occurred_at,observed_at,delivery_status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		event.EventKey, event.PayloadDigest, event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, formatTime(event.OccurredAt), formatTime(event.ObservedAt), event.DeliveryStatus, nowText())
	if err == nil {
		count, countErr := result.RowsAffected()
		return count == 1, countErr
	}
	var persisted string
	lookupErr := s.db.QueryRowContext(ctx, `SELECT payload_digest FROM operator_attention_outbox WHERE event_key=?`, event.EventKey).Scan(&persisted)
	if lookupErr != nil {
		return false, err
	}
	if persisted != event.PayloadDigest {
		return false, application.FormatOperatorAttentionConflict(event)
	}
	return false, nil
}

// ListOperatorAttention is a bounded, local read model. It does not claim,
// deliver, acknowledge, retry, delete, or otherwise mutate any event.
func (s *Store) ListOperatorAttention(ctx context.Context, limit int) ([]application.OperatorAttentionEvent, error) {
	return s.listOperatorAttention(ctx, "", limit)
}

func (s *Store) listOperatorAttention(ctx context.Context, runID string, limit int) ([]application.OperatorAttentionEvent, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("operator attention projection limit is out of bounds")
	}
	query := `SELECT event_key,payload_digest,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,evidence_digest,occurred_at,observed_at,delivery_status FROM operator_attention_outbox`
	args := []any{}
	if runID != "" {
		query += ` WHERE run_id=?`
		args = append(args, runID)
	}
	query += ` ORDER BY occurred_at,event_key LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []application.OperatorAttentionEvent{}
	for rows.Next() {
		event, scanErr := scanOperatorAttention(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func scanOperatorAttention(row rowScanner) (application.OperatorAttentionEvent, error) {
	var event application.OperatorAttentionEvent
	var occurred, observed string
	if err := row.Scan(&event.EventKey, &event.PayloadDigest, &event.EventType, &event.RunID, &event.LinearIdentifier, &event.RepositoryProfileID, &event.RepositoryProfileName, &event.ControllerState, &event.Severity, &event.ReasonCode, &event.EvidenceDigest, &occurred, &observed, &event.DeliveryStatus); err != nil {
		return application.OperatorAttentionEvent{}, err
	}
	event.OccurredAt, event.ObservedAt = parseTime(occurred), parseTime(observed)
	if err := application.ValidateOperatorAttentionEvent(event); err != nil {
		return application.OperatorAttentionEvent{}, errors.New("operator attention outbox record is corrupt")
	}
	return event, nil
}
