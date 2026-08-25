package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func (s *Store) ListRoutineAttentionCandidates(ctx context.Context, input application.RoutineAttentionCandidateQuery) ([]application.OperatorAttentionEvent, error) {
	if input.Scopes.Empty() || input.Limit < 1 || input.Limit > 1001 {
		return nil, errors.New("routine attention candidate query is invalid")
	}
	where := "1=1"
	args := []any{}
	switch input.Scope {
	case application.ScopeController:
		if !input.Scopes.HasController() {
			return nil, errors.New("routine attention controller scope is invalid")
		}
	case application.ScopeRepository:
		if input.TargetID == "" || input.RepositoryProfileID == "" || len(input.Scopes.RepositoryBindingDigests()) != 1 {
			return nil, errors.New("routine attention repository scope is invalid")
		}
		where = "repository_profile_id=?"
		args = append(args, input.RepositoryProfileID)
	case application.ScopeRun:
		if input.TargetID == "" {
			return nil, errors.New("routine attention run scope is invalid")
		}
		var binding string
		if err := s.db.QueryRowContext(ctx, `SELECT repository_binding_digest FROM runs WHERE run_id=?`, input.TargetID).Scan(&binding); err != nil || !input.Scopes.AllowsRun(input.TargetID, binding) {
			return nil, application.ErrRunNotFound
		}
		where = "run_id=?"
		args = append(args, input.TargetID)
	default:
		return nil, errors.New("routine attention scope is invalid")
	}
	args = append(args, input.Limit)
	query := `SELECT event_key,payload_digest,schema_version,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,allowed_actions_json,evidence_digest,occurred_at,observed_at,legacy_payload_digest,legacy_delivery_status,retry_failure_class FROM (
		SELECT event_key,payload_digest,schema_version,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,allowed_actions_json,evidence_digest,occurred_at,observed_at,legacy_payload_digest,legacy_delivery_status,retry_failure_class,
		ROW_NUMBER() OVER (PARTITION BY event_type,run_id,linear_identifier,repository_profile_id ORDER BY occurred_at DESC,event_key DESC) AS routine_rank
		FROM operator_attention_outbox WHERE ` + where + `
	) WHERE routine_rank=1 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []application.OperatorAttentionEvent
	for rows.Next() {
		event, err := scanOperatorAttention(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) LatestQueueAttention(ctx context.Context, scopes application.AuthorizedScopeSet) (application.RoutineQueueAttention, bool, error) {
	if !scopes.HasController() {
		return application.RoutineQueueAttention{}, false, errors.New("routine queue attention scope is invalid")
	}
	var occurredAt, severity, eventType string
	err := s.db.QueryRowContext(ctx, `SELECT occurred_at,severity,event_type FROM operator_attention_outbox WHERE event_type IN (?,?) ORDER BY occurred_at DESC,event_key DESC LIMIT 1`, application.OperatorAttentionCandidateScan, application.OperatorAttentionSchedulerLease).Scan(&occurredAt, &severity, &eventType)
	if errors.Is(err, sql.ErrNoRows) {
		return application.RoutineQueueAttention{}, false, nil
	}
	if err != nil {
		return application.RoutineQueueAttention{}, false, err
	}
	observed := parseTime(occurredAt)
	if observed.IsZero() {
		return application.RoutineQueueAttention{}, false, errors.New("routine queue attention is corrupt")
	}
	reason := "candidate_scan_attention"
	if eventType == application.OperatorAttentionSchedulerLease {
		reason = "scheduler_attention"
	}
	return application.RoutineQueueAttention{OccurredAt: observed, Degraded: severity == "critical" || severity == "error", ReasonCode: reason}, true, nil
}

// AppendOperatorAttention inserts immutable local-only evidence. A repeated
// key is safe only when its complete sanitized payload digest is identical.
func (s *Store) AppendOperatorAttention(ctx context.Context, event application.OperatorAttentionEvent) (bool, error) {
	if err := application.ValidateOperatorAttentionEvent(event); err != nil {
		return false, err
	}
	actions, err := json.Marshal(event.AllowedActions)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO operator_attention_outbox(event_key,payload_digest,schema_version,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,allowed_actions_json,evidence_digest,occurred_at,observed_at,legacy_payload_digest,legacy_delivery_status,created_at,retry_failure_class) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		event.EventKey, event.PayloadDigest, event.SchemaVersion, event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, string(actions), event.EvidenceDigest, formatTime(event.OccurredAt), formatTime(event.ObservedAt), "", "", nowText(), event.RetryFailureClass)
	if err == nil {
		count, countErr := result.RowsAffected()
		return count == 1, countErr
	}
	persisted, lookupErr := scanOperatorAttention(s.db.QueryRowContext(ctx, operatorAttentionSelect+` WHERE event_key=?`, event.EventKey))
	if lookupErr != nil {
		return false, err
	}
	if persisted.PayloadDigest == event.PayloadDigest || (persisted.SchemaVersion == application.OperatorAttentionLegacySchemaVersion && application.OperatorAttentionContentDigest(persisted) == application.OperatorAttentionContentDigest(event)) {
		return false, nil
	}
	return false, application.FormatOperatorAttentionConflict(event)
}

// ListOperatorAttention is a bounded, local read model. It does not claim,
// deliver, acknowledge, retry, delete, or otherwise mutate any event.
func (s *Store) ListOperatorAttention(ctx context.Context, input application.OperatorAttentionQueryInput) ([]application.OperatorAttentionEvent, error) {
	return s.listOperatorAttention(ctx, input.RunID, input.Limit)
}

func (s *Store) CurrentOperatorAttention(ctx context.Context, runID string) (application.OperatorAttentionEvent, bool, error) {
	if runID == "" {
		return application.OperatorAttentionEvent{}, false, errors.New("operator attention run is required")
	}
	event, err := scanOperatorAttention(s.db.QueryRowContext(ctx, operatorAttentionSelect+` WHERE run_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.OperatorAttentionEvent{}, false, nil
	}
	return event, err == nil, err
}

func (s *Store) listOperatorAttention(ctx context.Context, runID string, limit int) ([]application.OperatorAttentionEvent, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("operator attention projection limit is out of bounds")
	}
	query := operatorAttentionSelect
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

const operatorAttentionSelect = `SELECT event_key,payload_digest,schema_version,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,allowed_actions_json,evidence_digest,occurred_at,observed_at,legacy_payload_digest,legacy_delivery_status,retry_failure_class FROM operator_attention_outbox`

func scanOperatorAttention(row rowScanner) (application.OperatorAttentionEvent, error) {
	var event application.OperatorAttentionEvent
	var occurred, observed, actions, legacyPayload, legacyDelivery string
	if err := row.Scan(&event.EventKey, &event.PayloadDigest, &event.SchemaVersion, &event.EventType, &event.RunID, &event.LinearIdentifier, &event.RepositoryProfileID, &event.RepositoryProfileName, &event.ControllerState, &event.Severity, &event.ReasonCode, &actions, &event.EvidenceDigest, &occurred, &observed, &legacyPayload, &legacyDelivery, &event.RetryFailureClass); err != nil {
		return application.OperatorAttentionEvent{}, err
	}
	if err := json.Unmarshal([]byte(actions), &event.AllowedActions); err != nil {
		return application.OperatorAttentionEvent{}, errors.New("operator attention outbox record is corrupt")
	}
	event.OccurredAt, event.ObservedAt = parseTime(occurred), parseTime(observed)
	if event.SchemaVersion == application.OperatorAttentionLegacySchemaVersion {
		if legacyPayload != event.PayloadDigest || legacyDelivery != "pending_local" || legacyOperatorAttentionPayloadDigest(event, legacyDelivery) != event.PayloadDigest || application.ValidateLegacyOperatorAttentionEvent(event) != nil {
			return application.OperatorAttentionEvent{}, errors.New("operator attention outbox record is corrupt")
		}
		return event, nil
	}
	if event.SchemaVersion == application.OperatorAttentionPreviousSchemaVersion {
		if legacyPayload != "" || legacyDelivery != "" || application.ValidatePreviousOperatorAttentionEvent(event) != nil {
			return application.OperatorAttentionEvent{}, errors.New("operator attention outbox record is corrupt")
		}
		return event, nil
	}
	if legacyPayload != "" || legacyDelivery != "" || application.ValidateOperatorAttentionEvent(event) != nil {
		return application.OperatorAttentionEvent{}, errors.New("operator attention outbox record is corrupt")
	}
	return event, nil
}

func legacyOperatorAttentionPayloadDigest(event application.OperatorAttentionEvent, deliveryStatus string) string {
	payload := struct {
		EventType, RunID, LinearIdentifier, RepositoryProfileID, RepositoryProfileName, ControllerState, Severity, ReasonCode, EvidenceDigest, OccurredAt, ObservedAt, DeliveryStatus string
	}{event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ObservedAt.UTC().Format(time.RFC3339Nano), deliveryStatus}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ensureOperatorAttentionRetryFailureClassTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(operator_attention_outbox)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "retry_failure_class" {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE operator_attention_outbox ADD COLUMN retry_failure_class TEXT NOT NULL DEFAULT ''`)
	return err
}

func migrateOperatorAttentionV28Tx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, operatorAttentionSelect+` WHERE schema_version=?`, application.OperatorAttentionPreviousSchemaVersion)
	if err != nil {
		return err
	}
	var events []application.OperatorAttentionEvent
	for rows.Next() {
		event, err := scanOperatorAttention(rows)
		if err != nil {
			rows.Close()
			return err
		}
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, event := range events {
		if event.EventType == application.OperatorAttentionRetry && event.RunID != "" {
			var failureClass string
			_ = tx.QueryRowContext(ctx, `SELECT failure_class FROM automatic_retry_schedules WHERE run_id=? AND controller_state=?`, event.RunID, event.ControllerState).Scan(&failureClass)
			event.RetryFailureClass = application.RetryFailureClass(failureClass)
		}
		upgraded, err := application.UpgradePreviousOperatorAttentionEvent(event)
		if err != nil {
			return err
		}
		actions, _ := json.Marshal(upgraded.AllowedActions)
		result, err := tx.ExecContext(ctx, `UPDATE operator_attention_outbox SET schema_version=?,payload_digest=?,allowed_actions_json=?,retry_failure_class=? WHERE event_key=? AND schema_version=? AND payload_digest=?`, upgraded.SchemaVersion, upgraded.PayloadDigest, string(actions), upgraded.RetryFailureClass, event.EventKey, event.SchemaVersion, event.PayloadDigest)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return errors.New("operator attention schema-2 upgrade compare failed")
		}
	}
	return nil
}
