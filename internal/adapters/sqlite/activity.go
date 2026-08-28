package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const activityEventSelect = `SELECT ingestion_sequence,event_id,schema_version,source_kind,source_identity,source_evidence_digest,snapshot_digest,category,event_kind,actor,scope_kind,target_id,target_binding_digest,reason_code,prior_state,resulting_state,prior_version,resulting_version,occurred_at,observed_at,settled_at,related_resources_json,operation_ids_json,evidence_digests_json,ingestion_class,legacy_reconstructable FROM activity_events`

func (s *Store) AppendActivityEvent(ctx context.Context, event application.ActivityEvent) (application.ActivityEvent, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ActivityEvent{}, false, err
	}
	defer tx.Rollback()
	persisted, created, err := appendActivityEventTx(ctx, tx, event)
	if err != nil {
		return application.ActivityEvent{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.ActivityEvent{}, false, err
	}
	return persisted, created, nil
}

func appendActivityEventTx(ctx context.Context, tx *sql.Tx, event application.ActivityEvent) (application.ActivityEvent, bool, error) {
	if err := application.ValidateActivityEvent(event); err != nil {
		return application.ActivityEvent{}, false, err
	}
	related, _ := json.Marshal(event.RelatedResources)
	operations, _ := json.Marshal(event.OperationIDs)
	evidence, _ := json.Marshal(event.EvidenceDigests)
	legacy := 0
	if event.Coverage.LegacyReconstructable {
		legacy = 1
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO activity_events(event_id,schema_version,source_kind,source_identity,source_evidence_digest,snapshot_digest,category,event_kind,actor,scope_kind,target_id,target_binding_digest,reason_code,prior_state,resulting_state,prior_version,resulting_version,occurred_at,observed_at,settled_at,related_resources_json,operation_ids_json,evidence_digests_json,ingestion_class,legacy_reconstructable) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
		event.EventID, event.SchemaVersion, event.SourceKind, event.SourceIdentity, event.SourceEvidenceDigest, event.SnapshotDigest, string(event.Category), string(event.EventKind), string(event.Actor), string(event.Scope), event.TargetID, event.TargetBindingDigest, string(event.ReasonCode), event.PriorState, event.ResultingState, event.PriorVersion, event.ResultingVersion, formatTime(event.OccurredAt), formatTimePtr(event.ObservedAt), formatTimePtr(event.SettledAt), string(related), string(operations), string(evidence), string(event.Coverage.IngestionClass), legacy)
	if err != nil {
		return application.ActivityEvent{}, false, classifyActivityInsertError(err)
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		existing, lookupErr := scanActivityEvent(tx.QueryRowContext(ctx, activityEventSelect+` WHERE event_id=?`, event.EventID))
		if lookupErr != nil {
			return application.ActivityEvent{}, false, lookupErr
		}
		if !application.ActivityEventsCompatibleForReplay(existing, event) {
			return application.ActivityEvent{}, false, fmt.Errorf("%w: immutable snapshot changed", application.ErrActivityConflict)
		}
		if err := ensureActivityOperationLinksTx(ctx, tx, existing); err != nil {
			return application.ActivityEvent{}, false, err
		}
		return existing, false, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT ingestion_sequence FROM activity_events WHERE event_id=?`, event.EventID).Scan(&event.IngestionSequence); err != nil {
		return application.ActivityEvent{}, false, err
	}
	if err := ensureActivityOperationLinksTx(ctx, tx, event); err != nil {
		return application.ActivityEvent{}, false, err
	}
	return event, true, nil
}

func classifyActivityInsertError(err error) error {
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "unique constraint") || strings.Contains(strings.ToLower(err.Error()), "constraint failed")) {
		return fmt.Errorf("%w: source identity or operation link changed", application.ErrActivityConflict)
	}
	return err
}

func ensureActivityOperationLinksTx(ctx context.Context, tx *sql.Tx, event application.ActivityEvent) error {
	for _, operationID := range event.OperationIDs {
		result, err := tx.ExecContext(ctx, `INSERT INTO activity_operation_links(operation_id,event_id) VALUES(?,?) ON CONFLICT(operation_id) DO NOTHING`, operationID, event.EventID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed == 0 {
			var existing string
			if err := tx.QueryRowContext(ctx, `SELECT event_id FROM activity_operation_links WHERE operation_id=?`, operationID).Scan(&existing); err != nil {
				return err
			}
			if existing != event.EventID {
				return fmt.Errorf("%w: operation already has a primary event", application.ErrActivityConflict)
			}
		}
	}
	return nil
}

func (s *Store) ListActivity(ctx context.Context, query application.ActivityStoreQuery) (application.ActivityStorePage, error) {
	if !query.Scopes.HasController() || query.Limit < 1 || query.Limit > application.ActivityMaximumLimit {
		return application.ActivityStorePage{}, errors.New("authorized activity query is invalid")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.ActivityStorePage{}, err
	}
	defer tx.Rollback()

	watermark := int64(0)
	if query.Cursor != nil {
		watermark = query.Cursor.IngestionWatermark
	} else if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ingestion_sequence),0) FROM activity_events`).Scan(&watermark); err != nil {
		return application.ActivityStorePage{}, err
	}
	page := application.ActivityStorePage{IngestionWatermark: watermark}
	coverage, err := readActivityCoverageTx(ctx, tx)
	if err != nil {
		return application.ActivityStorePage{}, err
	}
	page.Coverage = coverage
	if watermark == 0 {
		return page, tx.Commit()
	}
	where, args := activityWhere(query.Filter, watermark)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity_events WHERE `+where, args...).Scan(&page.Total); err != nil {
		return application.ActivityStorePage{}, err
	}
	pageWhere := where
	pageArgs := append([]any(nil), args...)
	if query.Cursor != nil {
		pageWhere += ` AND (occurred_at<? OR (occurred_at=? AND event_id<?))`
		position := formatTime(query.Cursor.OccurredAt)
		pageArgs = append(pageArgs, position, position, query.Cursor.EventID)
	}
	pageArgs = append(pageArgs, query.Limit+1)
	rows, err := tx.QueryContext(ctx, activityEventSelect+` WHERE `+pageWhere+` ORDER BY occurred_at DESC,event_id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return application.ActivityStorePage{}, err
	}
	for rows.Next() {
		event, scanErr := scanActivityEvent(rows)
		if scanErr != nil {
			rows.Close()
			return application.ActivityStorePage{}, scanErr
		}
		page.Events = append(page.Events, event)
	}
	if err := rows.Close(); err != nil {
		return application.ActivityStorePage{}, err
	}
	page.HasMore = len(page.Events) > query.Limit
	if page.HasMore {
		page.Events = page.Events[:query.Limit]
	}
	return page, tx.Commit()
}

func activityWhere(filter application.ActivityFilter, watermark int64) (string, []any) {
	// Controller scope is established before this helper. Keep the authorization
	// predicate first so hidden rows never influence count, ordering, or cursors.
	where := `1=1 AND ingestion_sequence<=?`
	args := []any{watermark}
	if filter.Category != "" {
		where += ` AND category=?`
		args = append(args, string(filter.Category))
	}
	if filter.Scope != "" {
		where += ` AND scope_kind=?`
		args = append(args, string(filter.Scope))
	}
	if filter.TargetID != "" {
		where += ` AND target_id=?`
		args = append(args, filter.TargetID)
	}
	if filter.OccurredFrom != nil {
		where += ` AND occurred_at>=?`
		args = append(args, formatTime(filter.OccurredFrom.UTC()))
	}
	if filter.OccurredThrough != nil {
		where += ` AND occurred_at<=?`
		args = append(args, formatTime(filter.OccurredThrough.UTC()))
	}
	return where, args
}

func (s *Store) GetActivity(ctx context.Context, eventID string, scopes application.AuthorizedScopeSet) (application.ActivityEvent, application.ActivityCoverage, error) {
	if !scopes.HasController() || strings.TrimSpace(eventID) == "" {
		return application.ActivityEvent{}, application.ActivityCoverage{}, application.ErrActivityNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.ActivityEvent{}, application.ActivityCoverage{}, err
	}
	defer tx.Rollback()
	event, err := scanActivityEvent(tx.QueryRowContext(ctx, activityEventSelect+` WHERE 1=1 AND event_id=?`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.ActivityEvent{}, application.ActivityCoverage{}, application.ErrActivityNotFound
	}
	if err != nil {
		return application.ActivityEvent{}, application.ActivityCoverage{}, err
	}
	coverage, err := readActivityCoverageTx(ctx, tx)
	if err != nil {
		return application.ActivityEvent{}, application.ActivityCoverage{}, err
	}
	return event, coverage, tx.Commit()
}

func scanActivityEvent(row rowScanner) (application.ActivityEvent, error) {
	var event application.ActivityEvent
	var category, kind, actor, scope, occurred, observed, settled, related, operations, evidence, ingestion string
	var legacy int
	if err := row.Scan(&event.IngestionSequence, &event.EventID, &event.SchemaVersion, &event.SourceKind, &event.SourceIdentity, &event.SourceEvidenceDigest, &event.SnapshotDigest, &category, &kind, &actor, &scope, &event.TargetID, &event.TargetBindingDigest, &event.ReasonCode, &event.PriorState, &event.ResultingState, &event.PriorVersion, &event.ResultingVersion, &occurred, &observed, &settled, &related, &operations, &evidence, &ingestion, &legacy); err != nil {
		return application.ActivityEvent{}, err
	}
	event.Category = application.ActivityCategory(category)
	event.EventKind = application.ActivityEventKind(kind)
	event.Actor = application.ActivityActor(actor)
	event.Scope = application.AuthorityScopeKind(scope)
	event.OccurredAt = parseTime(occurred)
	event.ObservedAt = parseOptionalTime(observed)
	event.SettledAt = parseOptionalTime(settled)
	event.Coverage = application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionClass(ingestion), LegacyReconstructable: legacy != 0}
	if json.Unmarshal([]byte(related), &event.RelatedResources) != nil || json.Unmarshal([]byte(operations), &event.OperationIDs) != nil || json.Unmarshal([]byte(evidence), &event.EvidenceDigests) != nil {
		return application.ActivityEvent{}, errors.New("activity event is corrupt")
	}
	if err := application.ValidateActivityEvent(event); err != nil {
		return application.ActivityEvent{}, errors.New("activity event is corrupt")
	}
	return event, nil
}

func readActivityCoverageTx(ctx context.Context, tx *sql.Tx) (application.ActivityCoverage, error) {
	coverage := application.ActivityCoverage{State: application.ActivityCoverageUnknown, ReasonCode: "coverage_unavailable", LegacyLimitations: []string{
		"legacy_worker_readiness_unavailable",
		"legacy_nonpersisted_runtime_transitions_unavailable",
		"legacy_repository_intent_history_unavailable",
		"legacy_attention_resolution_history_partially_reconstructable",
		"legacy_admission_capacity_history_not_backfilled",
	}}
	var pending, running, complete, conflicts int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='pending'),0),COALESCE(SUM(status='running'),0),COALESCE(SUM(status='complete'),0),COALESCE(SUM(status='conflict'),0),COUNT(*) FROM activity_backfill_progress`).Scan(&pending, &running, &complete, &conflicts, &coverage.BackfillSourcesTotal); err != nil {
		return application.ActivityCoverage{}, err
	}
	coverage.BackfillSourcesComplete = complete
	var runtimeDegraded, runtimeConflict int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='degraded'),0),COALESCE(SUM(status='conflict'),0) FROM activity_runtime_state`).Scan(&runtimeDegraded, &runtimeConflict); err != nil {
		return application.ActivityCoverage{}, err
	}
	switch {
	case conflicts != 0 || runtimeConflict != 0:
		coverage.State, coverage.ReasonCode = application.ActivityCoverageConflict, "indexing_conflict"
	case runtimeDegraded != 0:
		coverage.State, coverage.ReasonCode = application.ActivityCoverageDegraded, "runtime_reconciliation_degraded"
	case pending != 0 || running != 0:
		coverage.State, coverage.ReasonCode = application.ActivityCoverageBackfilling, "backfill_in_progress"
	case coverage.BackfillSourcesTotal != 0 && complete == coverage.BackfillSourcesTotal:
		coverage.State, coverage.ReasonCode = application.ActivityCoverageComplete, "reconstructable_boundary_complete"
	}
	var first, last string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(occurred_at),''),COALESCE(MAX(occurred_at),'') FROM activity_events`).Scan(&first, &last); err != nil {
		return application.ActivityCoverage{}, err
	}
	coverage.ProvenFrom = parseOptionalTime(first)
	coverage.IndexedThrough = parseOptionalTime(last)
	var freshness string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(observed_at),'') FROM activity_runtime_state`).Scan(&freshness); err != nil {
		return application.ActivityCoverage{}, err
	}
	coverage.FreshnessObservedAt = parseOptionalTime(freshness)
	if err := application.ValidateActivityCoverage(coverage); err != nil {
		return application.ActivityCoverage{}, errors.New("activity coverage is corrupt")
	}
	return coverage, nil
}

func appendSettledOperationActivityTx(ctx context.Context, tx *sql.Tx, operationID string, ingestion application.ActivityIngestionClass) error {
	available, err := activitySchemaAvailableTx(ctx, tx)
	if err != nil || !available {
		return err
	}
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT event_id FROM activity_operation_links WHERE operation_id=?`, operationID).Scan(&existing); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, operationID)
	if err != nil || !found {
		if err == nil {
			err = application.ErrOperationReceiptNotFound
		}
		return err
	}
	if receipt.Outcome == application.OperationOutcomePending || receipt.SettledAt.IsZero() {
		return nil
	}
	reason := application.ActivityReasonForOperation(receipt.Outcome)
	if reason == "" {
		return errors.New("settled operation activity classification is invalid")
	}
	evidence := compactDigests(receipt.EvidenceDigest, receipt.ResultDigest, receipt.ResultingAuthorityDigest)
	sourceDigest := digestActivitySource(strings.Join([]string{receipt.OperationID, string(receipt.Phase), string(receipt.Outcome), receipt.ResultDigest, receipt.EvidenceDigest}, "\x00"))
	event := application.NewActivityEvent(application.ActivityEventInput{
		SourceKind: "operation_receipt", SourceIdentity: receipt.OperationID, SourceEvidenceDigest: sourceDigest,
		Category: application.ActivityOperation, EventKind: application.ActivityOperationSettled, Actor: application.ActivityActorForOperation(receipt),
		Scope: receipt.Scope, TargetID: receipt.TargetID, TargetBindingDigest: receipt.TargetBindingDigest, ReasonCode: reason,
		ResultingState: receipt.ResultingState, ResultingVersion: receipt.ResultingVersion, OccurredAt: receipt.SettledAt, SettledAt: &receipt.SettledAt,
		RelatedResources: []application.ActivityRelatedResource{{Kind: receipt.Scope, ID: receipt.TargetID}}, OperationIDs: []string{receipt.OperationID}, EvidenceDigests: evidence,
		Coverage: application.ActivityEventCoverage{IngestionClass: ingestion, LegacyReconstructable: true},
	})
	_, _, err = appendActivityEventTx(ctx, tx, event)
	return err
}

func appendRunTransitionActivityTx(ctx context.Context, tx *sql.Tx, runID string, sequence int64, from, to, reason, evidence, head string, occurredAt time.Time, operationID string) error {
	available, err := activitySchemaAvailableTx(ctx, tx)
	if err != nil || !available {
		return err
	}
	if operationID == "" {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_actions WHERE run_id=? AND status='validated' AND expected_state=?`, runID, from).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			// The operator-action transaction appends this transition after the
			// receipt settles so the domain event can own the sole operation link.
			return nil
		}
	}
	var binding, repository string
	if err := tx.QueryRowContext(ctx, `SELECT repository_binding_digest,repository FROM runs WHERE run_id=?`, runID).Scan(&binding, &repository); err != nil {
		return err
	}
	if !validOperatorActionDigest(binding) {
		binding = application.LegacyRunAuthorityDigest(repository)
	}
	event := newRunTransitionActivityEvent(runID, sequence, from, to, reason, evidence, head, occurredAt, binding, operationID, application.ActivityIngestionCurrent)
	_, _, err = appendActivityEventTx(ctx, tx, event)
	return err
}

func activitySchemaAvailableTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='activity_events'`).Scan(&count); err != nil {
		return false, err
	}
	return count == 1, nil
}

func appendStoredRunTransitionActivityTx(ctx context.Context, tx *sql.Tx, runID string, sequence int64, operationID string) error {
	var from, to, reason, evidence, head, occurred string
	if err := tx.QueryRowContext(ctx, `SELECT from_state,to_state,reason,evidence_reference,bound_head,created_at FROM transitions WHERE run_id=? AND sequence=?`, runID, sequence).Scan(&from, &to, &reason, &evidence, &head, &occurred); err != nil {
		return err
	}
	return appendRunTransitionActivityTx(ctx, tx, runID, sequence, from, to, reason, evidence, head, parseTime(occurred), operationID)
}

func appendConfigurationActivityTx(ctx context.Context, tx *sql.Tx, eventType string, generationID int64, operationID, digest, evidence string, occurredAt time.Time) error {
	available, err := activitySchemaAvailableTx(ctx, tx)
	if err != nil || !available {
		return err
	}
	kind, reason := application.ActivityConfigurationApplied, application.ActivityReasonSucceeded
	switch eventType {
	case "drift_entered":
		kind, reason = application.ActivityConfigurationDrifted, application.ActivityReasonDriftDetected
	case "drift_cleared", "effective", "effective_observed":
		kind, reason = application.ActivityConfigurationConverged, application.ActivityReasonConverged
	case "rollback_committed":
		kind, reason = application.ActivityConfigurationRolledBack, application.ActivityReasonSucceeded
	case "apply_failed":
		reason = application.ActivityReasonFailed
	case "apply_ambiguous":
		reason = application.ActivityReasonAmbiguous
	}
	sourceDigest := digestActivitySource(strings.Join([]string{eventType, fmt.Sprintf("%d", generationID), operationID, digest, evidence, formatTime(occurredAt)}, "\x00"))
	event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "configuration", SourceIdentity: fmt.Sprintf("%s:%d:%s", eventType, generationID, sourceDigest[:16]), SourceEvidenceDigest: sourceDigest, Category: application.ActivityConfiguration, EventKind: kind, Actor: application.ActivityActorController, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, TargetBindingDigest: digest, ReasonCode: reason, ResultingState: eventType, ResultingVersion: generationID, OccurredAt: occurredAt, OperationIDs: compactStrings(operationID), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeController, ID: application.ConfigurationTargetID}}, EvidenceDigests: compactDigests(evidence, digest, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true}})
	_, _, err = appendActivityEventTx(ctx, tx, event)
	return err
}

func appendRepositoryLifecycleActivityTx(ctx context.Context, tx *sql.Tx, repository, incarnationID, binding, prior, resulting string, version int64, operationID, evidence string, occurredAt time.Time) error {
	available, err := activitySchemaAvailableTx(ctx, tx)
	if err != nil || !available {
		return err
	}
	sourceDigest := digestActivitySource(strings.Join([]string{incarnationID, fmt.Sprintf("%d", version), prior, resulting, operationID, evidence, formatTime(occurredAt)}, "\x00"))
	event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "repository_lifecycle", SourceIdentity: fmt.Sprintf("%s:%d", incarnationID, version), SourceEvidenceDigest: sourceDigest, Category: application.ActivityRepository, EventKind: application.ActivityRepositoryLifecycleChange, Actor: application.ActivityActorConfiguredOperator, Scope: application.ScopeRepository, TargetID: repository, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonStateChanged, PriorState: prior, ResultingState: resulting, PriorVersion: max(version-1, 0), ResultingVersion: version, OccurredAt: occurredAt, OperationIDs: compactStrings(operationID), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeRepository, ID: repository}}, EvidenceDigests: compactDigests(evidence, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true}})
	_, _, err = appendActivityEventTx(ctx, tx, event)
	return err
}

func appendAdmissionConflictActivityTx(ctx context.Context, tx *sql.Tx, decision application.SchedulingDecision) error {
	available, err := activitySchemaAvailableTx(ctx, tx)
	if err != nil || !available {
		return err
	}
	if decision.Classification != application.QueueCandidateWaiting && decision.Classification != application.QueueCandidateBlockedByActiveRepository && decision.Classification != application.QueueCandidateAmbiguous {
		return nil
	}
	sourceDigest := digestActivitySource(strings.Join([]string{decision.DecisionID, decision.SnapshotDigest, decision.CapacityIdentity, decision.Classification, decision.ReasonCode, formatTime(decision.ObservedAt)}, "\x00"))
	event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "admission_capacity", SourceIdentity: decision.DecisionID, SourceEvidenceDigest: sourceDigest, Category: application.ActivityAdmissionCapacity, EventKind: application.ActivityAdmissionConflict, Actor: application.ActivityActorController, Scope: application.ScopeController, TargetID: "controller-admission-capacity", TargetBindingDigest: decision.SnapshotDigest, ReasonCode: application.ActivityReasonCapacityConflict, ResultingState: decision.Classification, OccurredAt: decision.ObservedAt, EvidenceDigests: compactDigests(decision.SnapshotDigest, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true}})
	_, _, err = appendActivityEventTx(ctx, tx, event)
	return err
}

func appendOperatorAttentionActivityTx(ctx context.Context, tx *sql.Tx, event application.OperatorAttentionEvent, supersededEventID string) error {
	scope, target, binding := application.ScopeController, "controller", event.PayloadDigest
	if event.RunID != "" {
		scope, target = application.ScopeRun, event.RunID
		if err := tx.QueryRowContext(ctx, `SELECT repository_binding_digest FROM runs WHERE run_id=?`, event.RunID).Scan(&binding); err != nil {
			return err
		}
	} else if event.RepositoryProfileID != "" && event.RepositoryProfileID != "automation" {
		var repository string
		if err := tx.QueryRowContext(ctx, `SELECT repository,repository_binding_digest FROM repository_lifecycles WHERE profile_id=? AND retired_at='' ORDER BY updated_at DESC LIMIT 1`, event.RepositoryProfileID).Scan(&repository, &binding); err == nil {
			scope, target = application.ScopeRepository, repository
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	opened := newOperatorAttentionActivityEvent(event.EventKey, event.PayloadDigest, event.ReasonCode, event.EvidenceDigest, event.OccurredAt, event.ObservedAt, scope, target, binding, application.ActivityIngestionCurrent)
	if _, _, err := appendActivityEventTx(ctx, tx, opened); err != nil {
		return err
	}
	if supersededEventID == "" {
		return nil
	}
	supersededDigest := digestActivitySource(strings.Join([]string{supersededEventID, event.EventKey, opened.SourceEvidenceDigest}, "\x00"))
	superseded := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "operator_attention_supersession", SourceIdentity: supersededEventID + ":" + event.EventKey, SourceEvidenceDigest: supersededDigest, Category: application.ActivityAttention, EventKind: application.ActivityAttentionSuperseded, Actor: application.ActivityActorController, Scope: scope, TargetID: target, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonSuperseded, OccurredAt: event.OccurredAt, ObservedAt: &event.ObservedAt, RelatedResources: []application.ActivityRelatedResource{{Kind: scope, ID: target}}, EvidenceDigests: []string{supersededDigest}, Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true}})
	_, _, err := appendActivityEventTx(ctx, tx, superseded)
	return err
}

func appendAttentionResolutionForActionTx(ctx context.Context, tx *sql.Tx, action application.OperatorActionRecord, receipt application.OperationReceipt) error {
	available, err := activitySchemaAvailableTx(ctx, tx)
	if err != nil || !available {
		return err
	}
	if receipt.Outcome != application.OperationOutcomeSucceeded || action.AttentionEventKey == "" {
		return nil
	}
	attention, err := scanOperatorAttention(tx.QueryRowContext(ctx, operatorAttentionSelect+` WHERE event_key=?`, action.AttentionEventKey))
	if err != nil {
		return err
	}
	scope, target, binding := application.ScopeRun, action.RunID, receipt.TargetBindingDigest
	if action.RunID == "" {
		scope, target = receipt.Scope, receipt.TargetID
	}
	sourceDigest := digestActivitySource(strings.Join([]string{action.AttentionEventKey, action.ActionID, receipt.OperationID, receipt.ResultDigest}, "\x00"))
	occurredAt := receipt.SettledAt
	event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "operator_attention_resolution", SourceIdentity: action.AttentionEventKey + ":" + action.ActionID, SourceEvidenceDigest: sourceDigest, Category: application.ActivityAttention, EventKind: application.ActivityAttentionResolved, Actor: application.ActivityActorConfiguredOperator, Scope: scope, TargetID: target, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonResolved, PriorState: attention.ControllerState, ResultingState: receipt.ResultingState, OccurredAt: occurredAt, SettledAt: &occurredAt, RelatedResources: []application.ActivityRelatedResource{{Kind: scope, ID: target}}, EvidenceDigests: compactDigests(attention.EvidenceDigest, receipt.ResultDigest, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true}})
	_, _, err = appendActivityEventTx(ctx, tx, event)
	return err
}

func compactDigests(values ...string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func digestActivitySource(value string) string {
	sum := sha256.Sum256([]byte("activity-source-v1\x00" + value))
	return hex.EncodeToString(sum[:])
}

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatTime(value.UTC())
}

func parseOptionalTime(value string) *time.Time {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed := parseTime(value)
	if parsed.IsZero() {
		return nil
	}
	return &parsed
}

func (s *Store) ListAuthorizedOperationReceipts(ctx context.Context, query application.OperationHistoryStoreQuery) (application.OperationHistoryStorePage, error) {
	if !query.Scopes.HasController() || query.Limit < 1 || query.Limit > application.OperationHistoryMaximumLimit {
		return application.OperationHistoryStorePage{}, errors.New("authorized operation history query is invalid")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.OperationHistoryStorePage{}, err
	}
	defer tx.Rollback()
	page := application.OperationHistoryStorePage{}
	if query.Cursor != nil {
		page.WatermarkAcceptedAt, page.WatermarkOperationID = query.Cursor.WatermarkAcceptedAt, query.Cursor.WatermarkOperation
	} else {
		var accepted string
		err := tx.QueryRowContext(ctx, `SELECT accepted_at,operation_id FROM operation_receipts ORDER BY accepted_at DESC,operation_id DESC LIMIT 1`).Scan(&accepted, &page.WatermarkOperationID)
		if errors.Is(err, sql.ErrNoRows) {
			return page, tx.Commit()
		}
		if err != nil {
			return application.OperationHistoryStorePage{}, err
		}
		page.WatermarkAcceptedAt = parseTime(accepted)
	}
	where, args := operationHistoryWhere(query.Filter, page.WatermarkAcceptedAt, page.WatermarkOperationID)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_receipts WHERE `+where, args...).Scan(&page.Total); err != nil {
		return application.OperationHistoryStorePage{}, err
	}
	if query.Cursor != nil {
		position := formatTime(query.Cursor.AcceptedAt)
		where += ` AND (accepted_at<? OR (accepted_at=? AND operation_id<?))`
		args = append(args, position, position, query.Cursor.OperationID)
	}
	args = append(args, query.Limit+1)
	rows, err := tx.QueryContext(ctx, operationReceiptSelect+` WHERE `+where+` ORDER BY accepted_at DESC,operation_id DESC LIMIT ?`, args...)
	if err != nil {
		return application.OperationHistoryStorePage{}, err
	}
	for rows.Next() {
		receipt, scanErr := scanOperationReceipt(rows)
		if scanErr != nil {
			rows.Close()
			return application.OperationHistoryStorePage{}, scanErr
		}
		page.Receipts = append(page.Receipts, receipt)
	}
	if err := rows.Close(); err != nil {
		return application.OperationHistoryStorePage{}, err
	}
	page.HasMore = len(page.Receipts) > query.Limit
	if page.HasMore {
		page.Receipts = page.Receipts[:query.Limit]
	}
	return page, tx.Commit()
}

func operationHistoryWhere(filter application.OperationHistoryFilter, watermarkAt time.Time, watermarkID string) (string, []any) {
	watermark := formatTime(watermarkAt)
	where := `(accepted_at<? OR (accepted_at=? AND operation_id<=?))`
	args := []any{watermark, watermark, watermarkID}
	if filter.Scope != "" {
		where += ` AND scope_kind=?`
		args = append(args, string(filter.Scope))
	}
	if filter.TargetID != "" {
		where += ` AND target_id=?`
		args = append(args, filter.TargetID)
	}
	if filter.OperationType != "" {
		where += ` AND operation_type=?`
		args = append(args, string(filter.OperationType))
	}
	if filter.Phase != "" {
		where += ` AND phase=?`
		args = append(args, string(filter.Phase))
	}
	if filter.Outcome != "" {
		where += ` AND outcome=?`
		args = append(args, string(filter.Outcome))
	}
	return where, args
}
