package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const defaultActivityBackfillBatch = 25

func (s *Store) BackfillActivityBatch(ctx context.Context, limit int, observedAt time.Time) (application.ActivityBackfillResult, error) {
	if limit == 0 {
		limit = defaultActivityBackfillBatch
	}
	if limit < 1 || limit > application.ActivityMaximumLimit || observedAt.IsZero() {
		return application.ActivityBackfillResult{}, errors.New("activity backfill batch is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ActivityBackfillResult{}, err
	}
	defer tx.Rollback()
	var source string
	var cursor int64
	err = tx.QueryRowContext(ctx, `SELECT source_kind,cursor_sequence FROM activity_backfill_progress WHERE status IN ('pending','running') ORDER BY CASE source_kind WHEN 'run_transition' THEN 1 WHEN 'repository_lifecycle' THEN 2 WHEN 'onboarding' THEN 3 WHEN 'configuration' THEN 4 WHEN 'operator_attention' THEN 5 WHEN 'operation_receipt' THEN 6 ELSE 7 END LIMIT 1`).Scan(&source, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return application.ActivityBackfillResult{Complete: true}, tx.Commit()
	}
	if err != nil {
		return application.ActivityBackfillResult{}, err
	}
	result := application.ActivityBackfillResult{SourceKind: source}
	lastCursor, indexedThrough, exhausted, conflictDigest, err := backfillActivitySourceTx(ctx, tx, source, cursor, limit, &result)
	if err != nil {
		return application.ActivityBackfillResult{}, err
	}
	status, reason := "running", ""
	if conflictDigest != "" {
		status, reason, result.Conflict = "conflict", "immutable_source_conflict", true
	} else if exhausted {
		status, result.Complete = "complete", true
	}
	if lastCursor < cursor {
		lastCursor = cursor
	}
	_, err = tx.ExecContext(ctx, `UPDATE activity_backfill_progress SET cursor_sequence=?,status=?,indexed_through=?,evidence_digest=?,reason_code=?,updated_at=? WHERE source_kind=? AND status IN ('pending','running')`, lastCursor, status, formatTime(indexedThrough), conflictDigest, reason, formatTime(observedAt.UTC()), source)
	if err != nil {
		return application.ActivityBackfillResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return application.ActivityBackfillResult{}, err
	}
	return result, nil
}

func backfillActivitySourceTx(ctx context.Context, tx *sql.Tx, source string, cursor int64, limit int, result *application.ActivityBackfillResult) (last int64, indexedThrough time.Time, exhausted bool, conflictDigest string, err error) {
	last = cursor
	appendEvent := func(rowID int64, event application.ActivityEvent) bool {
		if ctx.Err() != nil {
			err = ctx.Err()
			return false
		}
		_, _, appendErr := appendActivityEventTx(ctx, tx, event)
		if appendErr != nil {
			if errors.Is(appendErr, application.ErrActivityConflict) {
				conflictDigest = digestActivitySource(source + "\x00" + strconv.FormatInt(rowID, 10))
				return false
			}
			err = appendErr
			return false
		}
		last = rowID
		result.Indexed++
		if event.OccurredAt.After(indexedThrough) {
			indexedThrough = event.OccurredAt
		}
		return true
	}

	switch source {
	case "run_transition":
		rows, queryErr := tx.QueryContext(ctx, `SELECT t.rowid,t.run_id,t.sequence,t.from_state,t.to_state,t.reason,t.evidence_reference,t.bound_head,t.created_at,r.repository_binding_digest,COALESCE((SELECT receipts.operation_id FROM operation_receipts receipts JOIN operator_actions actions ON actions.action_id=receipts.source_action_id WHERE actions.run_id=t.run_id AND actions.resulting_transition_sequence=t.sequence AND receipts.outcome<>'pending' LIMIT 1),'') FROM transitions t JOIN runs r ON r.run_id=t.run_id WHERE t.rowid>? ORDER BY t.rowid LIMIT ?`, cursor, limit)
		if queryErr != nil {
			return last, indexedThrough, false, "", queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var rowID, sequence int64
			var runID, from, to, reason, evidence, head, created, binding, operationID string
			if scanErr := rows.Scan(&rowID, &runID, &sequence, &from, &to, &reason, &evidence, &head, &created, &binding, &operationID); scanErr != nil {
				return last, indexedThrough, false, "", scanErr
			}
			sourceDigest := digestActivitySource(strings.Join([]string{runID, strconv.FormatInt(sequence, 10), from, to, reason, evidence, head, created}, "\x00"))
			event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: source, SourceIdentity: runID + ":" + strconv.FormatInt(sequence, 10), SourceEvidenceDigest: sourceDigest, Category: application.ActivityRun, EventKind: application.ActivityRunTransition, Actor: application.ActivityActorController, Scope: application.ScopeRun, TargetID: runID, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonStateChanged, PriorState: from, ResultingState: to, PriorVersion: max(sequence-1, 0), ResultingVersion: sequence, OccurredAt: parseTime(created), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeRun, ID: runID}}, OperationIDs: compactStrings(operationID), EvidenceDigests: []string{sourceDigest}, Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionBackfill, LegacyReconstructable: true}})
			if !appendEvent(rowID, event) {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
	case "operation_receipt":
		rows, queryErr := tx.QueryContext(ctx, `SELECT rowid,operation_id,settled_at FROM operation_receipts WHERE rowid>? AND outcome<>'pending' AND settled_at<>'' ORDER BY rowid LIMIT ?`, cursor, limit)
		if queryErr != nil {
			return last, indexedThrough, false, "", queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var rowID int64
			var operationID, settled string
			if scanErr := rows.Scan(&rowID, &operationID, &settled); scanErr != nil {
				return last, indexedThrough, false, "", scanErr
			}
			if appendErr := appendSettledOperationActivityTx(ctx, tx, operationID, application.ActivityIngestionBackfill); appendErr != nil {
				if errors.Is(appendErr, application.ErrActivityConflict) {
					conflictDigest = digestActivitySource(source + "\x00" + operationID)
					break
				}
				return last, indexedThrough, false, "", appendErr
			}
			last, result.Indexed = rowID, result.Indexed+1
			if at := parseTime(settled); at.After(indexedThrough) {
				indexedThrough = at
			}
		}
		if err == nil {
			err = rows.Err()
		}
	case "operator_attention":
		rows, queryErr := tx.QueryContext(ctx, `SELECT rowid,event_key,payload_digest,run_id,repository_profile_id,reason_code,evidence_digest,occurred_at,observed_at FROM operator_attention_outbox WHERE rowid>? ORDER BY rowid LIMIT ?`, cursor, limit)
		if queryErr != nil {
			return last, indexedThrough, false, "", queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var rowID int64
			var eventKey, payload, runID, profileID, reason, evidence, occurred, observed string
			if scanErr := rows.Scan(&rowID, &eventKey, &payload, &runID, &profileID, &reason, &evidence, &occurred, &observed); scanErr != nil {
				return last, indexedThrough, false, "", scanErr
			}
			scope, target, binding := application.ScopeController, "controller", payload
			if runID != "" {
				scope, target = application.ScopeRun, runID
				if lookupErr := tx.QueryRowContext(ctx, `SELECT repository_binding_digest FROM runs WHERE run_id=?`, runID).Scan(&binding); lookupErr != nil {
					return last, indexedThrough, false, "", lookupErr
				}
			} else if profileID != "" && profileID != "automation" {
				var repository string
				if lookupErr := tx.QueryRowContext(ctx, `SELECT repository,repository_binding_digest FROM repository_lifecycles WHERE profile_id=? ORDER BY updated_at DESC LIMIT 1`, profileID).Scan(&repository, &binding); lookupErr == nil {
					scope, target = application.ScopeRepository, repository
				} else if !errors.Is(lookupErr, sql.ErrNoRows) {
					return last, indexedThrough, false, "", lookupErr
				}
			}
			sourceDigest := digestActivitySource(strings.Join([]string{eventKey, payload, reason, evidence, occurred, observed}, "\x00"))
			event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: source, SourceIdentity: eventKey, SourceEvidenceDigest: sourceDigest, Category: application.ActivityAttention, EventKind: application.ActivityAttentionOpened, Actor: application.ActivityActorController, Scope: scope, TargetID: target, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonOpened, OccurredAt: parseTime(occurred), ObservedAt: parseOptionalTime(observed), RelatedResources: []application.ActivityRelatedResource{{Kind: scope, ID: target}}, EvidenceDigests: compactDigests(evidence, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionBackfill, LegacyReconstructable: true}})
			if !appendEvent(rowID, event) {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
	case "repository_lifecycle":
		rows, queryErr := tx.QueryContext(ctx, `SELECT s.rowid,s.snapshot_id,s.repository,s.repository_binding_digest,s.lifecycle_version,s.overall_status,s.reason_code,s.snapshot_digest,s.observed_at,s.published_at,COALESCE((SELECT operation_id FROM repository_recheck_attempts WHERE result_snapshot_id=s.snapshot_id AND status='published' LIMIT 1),'') FROM repository_readiness_snapshots s WHERE s.rowid>? ORDER BY s.rowid LIMIT ?`, cursor, limit)
		if queryErr != nil {
			return last, indexedThrough, false, "", queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var rowID, version int64
			var id, repository, binding, status, reason, snapshot, observed, published, operationID string
			if scanErr := rows.Scan(&rowID, &id, &repository, &binding, &version, &status, &reason, &snapshot, &observed, &published, &operationID); scanErr != nil {
				return last, indexedThrough, false, "", scanErr
			}
			sourceDigest := digestActivitySource(strings.Join([]string{id, status, reason, snapshot, published}, "\x00"))
			event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: source, SourceIdentity: id, SourceEvidenceDigest: sourceDigest, Category: application.ActivityRepository, EventKind: application.ActivityRepositoryGateChange, Actor: application.ActivityActorController, Scope: application.ScopeRepository, TargetID: repository, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonReadinessChanged, ResultingState: status, ResultingVersion: version, OccurredAt: parseTime(published), ObservedAt: parseOptionalTime(observed), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeRepository, ID: repository}}, OperationIDs: compactStrings(operationID), EvidenceDigests: compactDigests(snapshot, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionBackfill, LegacyReconstructable: true}})
			if !appendEvent(rowID, event) {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
	case "onboarding":
		rows, queryErr := tx.QueryContext(ctx, `SELECT s.rowid,s.onboarding_id,s.step_name,s.step_order,s.outcome,s.reason_code,s.evidence_digest,s.observed_at,o.repository_binding_digest,o.request_digest,o.operation_id FROM repository_onboarding_steps s JOIN repository_onboardings o ON o.onboarding_id=s.onboarding_id WHERE s.rowid>? AND s.status='observed' ORDER BY s.rowid LIMIT ?`, cursor, limit)
		if queryErr != nil {
			return last, indexedThrough, false, "", queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var rowID, order int64
			var id, step, outcome, reason, evidence, observed, binding, requestDigest, operationID string
			if scanErr := rows.Scan(&rowID, &id, &step, &order, &outcome, &reason, &evidence, &observed, &binding, &requestDigest, &operationID); scanErr != nil {
				return last, indexedThrough, false, "", scanErr
			}
			if binding == "" {
				binding = requestDigest
			}
			kind, eventReason := application.ActivityOnboardingMilestone, application.ActivityReasonMilestone
			if step == "settled" && outcome == "succeeded" {
				kind, eventReason = application.ActivityOnboardingCompleted, application.ActivityReasonCompleted
			} else if outcome == "conflict" {
				kind, eventReason = application.ActivityOnboardingConflict, application.ActivityReasonConflict
			}
			sourceDigest := digestActivitySource(strings.Join([]string{id, step, outcome, reason, evidence, observed}, "\x00"))
			operations := []string(nil)
			if step == "settled" || outcome == "conflict" {
				operations = compactStrings(operationID)
			}
			event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: source, SourceIdentity: id + ":" + step, SourceEvidenceDigest: sourceDigest, Category: application.ActivityOnboarding, EventKind: kind, Actor: application.ActivityActorController, Scope: application.ScopeOnboarding, TargetID: id, TargetBindingDigest: binding, ReasonCode: eventReason, ResultingState: step + ":" + outcome, ResultingVersion: order, OccurredAt: parseTime(observed), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeOnboarding, ID: id}}, OperationIDs: operations, EvidenceDigests: compactDigests(evidence, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionBackfill, LegacyReconstructable: true}})
			if !appendEvent(rowID, event) {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
	case "configuration":
		rows, queryErr := tx.QueryContext(ctx, `SELECT generation_id,origin,digest,lifecycle,reason_code,operation_id,created_at,settled_at FROM configuration_generations WHERE generation_id>? AND settled_at<>'' ORDER BY generation_id LIMIT ?`, cursor, limit)
		if queryErr != nil {
			return last, indexedThrough, false, "", queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var generation int64
			var origin, digest, lifecycle, reason, operationID, created, settled string
			if scanErr := rows.Scan(&generation, &origin, &digest, &lifecycle, &reason, &operationID, &created, &settled); scanErr != nil {
				return last, indexedThrough, false, "", scanErr
			}
			kind := application.ActivityConfigurationApplied
			if origin == "rollback" {
				kind = application.ActivityConfigurationRolledBack
			}
			if lifecycle == "effective" {
				kind = application.ActivityConfigurationConverged
			}
			sourceDigest := digestActivitySource(strings.Join([]string{strconv.FormatInt(generation, 10), origin, digest, lifecycle, reason, settled}, "\x00"))
			event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: source, SourceIdentity: strconv.FormatInt(generation, 10), SourceEvidenceDigest: sourceDigest, Category: application.ActivityConfiguration, EventKind: kind, Actor: application.ActivityActorConfiguredOperator, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, TargetBindingDigest: digest, ReasonCode: application.ActivityReasonConverged, ResultingState: lifecycle, ResultingVersion: generation, OccurredAt: parseTime(settled), SettledAt: parseOptionalTime(settled), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeController, ID: application.ConfigurationTargetID}}, OperationIDs: compactStrings(operationID), EvidenceDigests: compactDigests(digest, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionBackfill, LegacyReconstructable: true}})
			if !appendEvent(generation, event) {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
	default:
		return last, indexedThrough, false, "", fmt.Errorf("unknown activity backfill source %q", source)
	}
	if err != nil || conflictDigest != "" {
		return last, indexedThrough, false, conflictDigest, err
	}
	exhausted = result.Indexed < limit
	return last, indexedThrough, exhausted, "", nil
}

func compactStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

func (s *Store) ReconcileWorkerActivity(ctx context.Context, observation application.RuntimeActivityObservation) (application.ActivityEvent, bool, error) {
	if strings.TrimSpace(observation.SourceKind) == "" || strings.TrimSpace(observation.SourceIdentity) == "" || strings.TrimSpace(observation.Classification) == "" || !validOperatorActionDigest(observation.SourceEvidenceDigest) || !validOperatorActionDigest(observation.TargetBindingDigest) || observation.OccurredAt.IsZero() || observation.ObservedAt.IsZero() {
		return application.ActivityEvent{}, false, errors.New("runtime activity observation is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ActivityEvent{}, false, err
	}
	defer tx.Rollback()
	var previous, previousDigest, status string
	lookupErr := tx.QueryRowContext(ctx, `SELECT classification,source_evidence_digest,status FROM activity_runtime_state WHERE source_kind=? AND source_identity=?`, observation.SourceKind, observation.SourceIdentity).Scan(&previous, &previousDigest, &status)
	if lookupErr == nil && status == "conflict" {
		return application.ActivityEvent{}, false, application.ErrActivityConflict
	}
	if lookupErr == nil && previous == observation.Classification {
		if _, err := tx.ExecContext(ctx, `UPDATE activity_runtime_state SET source_evidence_digest=?,observed_at=?,status='ready',reason_code='unchanged_classification' WHERE source_kind=? AND source_identity=?`, observation.SourceEvidenceDigest, formatTime(observation.ObservedAt), observation.SourceKind, observation.SourceIdentity); err != nil {
			return application.ActivityEvent{}, false, err
		}
		return application.ActivityEvent{}, false, tx.Commit()
	}
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return application.ActivityEvent{}, false, lookupErr
	}
	sourceIdentity := observation.SourceIdentity + ":" + observation.SourceEvidenceDigest
	event := application.NewActivityEvent(application.ActivityEventInput{SourceKind: observation.SourceKind, SourceIdentity: sourceIdentity, SourceEvidenceDigest: observation.SourceEvidenceDigest, Category: application.ActivityWorker, EventKind: application.ActivityWorkerReadinessChange, Actor: application.ActivityActorController, Scope: application.ScopeController, TargetID: "controller-worker", TargetBindingDigest: observation.TargetBindingDigest, ReasonCode: application.ActivityReasonReadinessChanged, PriorState: previous, ResultingState: observation.Classification, OccurredAt: observation.OccurredAt, ObservedAt: &observation.ObservedAt, EvidenceDigests: []string{observation.SourceEvidenceDigest}, Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionRuntime, LegacyReconstructable: false}})
	persisted, created, appendErr := appendActivityEventTx(ctx, tx, event)
	if appendErr != nil {
		if errors.Is(appendErr, application.ErrActivityConflict) {
			_, _ = tx.ExecContext(ctx, `INSERT INTO activity_runtime_state(source_kind,source_identity,classification,source_evidence_digest,observed_at,status,reason_code) VALUES(?,?,?,?,?,'conflict','immutable_source_conflict') ON CONFLICT(source_kind,source_identity) DO UPDATE SET status='conflict',reason_code='immutable_source_conflict',observed_at=excluded.observed_at`, observation.SourceKind, observation.SourceIdentity, observation.Classification, observation.SourceEvidenceDigest, formatTime(observation.ObservedAt))
			if commitErr := tx.Commit(); commitErr != nil {
				return application.ActivityEvent{}, false, commitErr
			}
		}
		return application.ActivityEvent{}, false, appendErr
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO activity_runtime_state(source_kind,source_identity,classification,source_evidence_digest,observed_at,status,reason_code) VALUES(?,?,?,?,?,'ready','classification_changed') ON CONFLICT(source_kind,source_identity) DO UPDATE SET classification=excluded.classification,source_evidence_digest=excluded.source_evidence_digest,observed_at=excluded.observed_at,status='ready',reason_code='classification_changed'`, observation.SourceKind, observation.SourceIdentity, observation.Classification, observation.SourceEvidenceDigest, formatTime(observation.ObservedAt))
	if err != nil {
		return application.ActivityEvent{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.ActivityEvent{}, false, err
	}
	_ = previousDigest
	return persisted, created, nil
}

func (s *Store) RecordActivityIndexingFailure(ctx context.Context, sourceKind, reasonCode string, observedAt time.Time) error {
	if strings.TrimSpace(sourceKind) == "" || strings.TrimSpace(reasonCode) == "" || observedAt.IsZero() || strings.ContainsRune(sourceKind, '\x00') || strings.ContainsRune(reasonCode, '\x00') {
		return errors.New("activity indexing failure is invalid")
	}
	evidence := digestActivitySource(sourceKind + "\x00" + reasonCode)
	_, err := s.db.ExecContext(ctx, `INSERT INTO activity_runtime_state(source_kind,source_identity,classification,source_evidence_digest,observed_at,status,reason_code) VALUES('activity_indexing',?,?,?,?,'degraded',?) ON CONFLICT(source_kind,source_identity) DO UPDATE SET classification=excluded.classification,source_evidence_digest=excluded.source_evidence_digest,observed_at=excluded.observed_at,status='degraded',reason_code=excluded.reason_code`, sourceKind, "degraded", evidence, formatTime(observedAt.UTC()), reasonCode)
	return err
}

func (s *Store) RecordActivityIndexingRecovery(ctx context.Context, sourceKind string, observedAt time.Time) error {
	if strings.TrimSpace(sourceKind) == "" || observedAt.IsZero() || strings.ContainsRune(sourceKind, '\x00') {
		return errors.New("activity indexing recovery is invalid")
	}
	evidence := digestActivitySource(sourceKind + "\x00recovered")
	_, err := s.db.ExecContext(ctx, `INSERT INTO activity_runtime_state(source_kind,source_identity,classification,source_evidence_digest,observed_at,status,reason_code) VALUES('activity_indexing',?,?,?,?,'ready','recovered') ON CONFLICT(source_kind,source_identity) DO UPDATE SET classification='ready',source_evidence_digest=excluded.source_evidence_digest,observed_at=excluded.observed_at,status='ready',reason_code='recovered'`, sourceKind, "ready", evidence, formatTime(observedAt.UTC()))
	return err
}
