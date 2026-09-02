package application

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type priorCollectionMetadata struct {
	Total      int    `json:"total"`
	Truncated  bool   `json:"truncated"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type priorActivityPage struct {
	Metadata   RoutineProjectionMetadata `json:"metadata"`
	Collection priorCollectionMetadata   `json:"collection"`
	Coverage   ActivityCoverage          `json:"coverage"`
	Events     []ActivityEvent           `json:"events"`
}

type priorOperationHistoryPage struct {
	Metadata   RoutineProjectionMetadata `json:"metadata"`
	Collection priorCollectionMetadata   `json:"collection"`
	Receipts   []OperationReceipt        `json:"receipts"`
}

func TestActivityEventIdentityIsDeterministicImmutableAndSanitized(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC)
	input := ActivityEventInput{SourceKind: "run_transition", SourceIdentity: "run-1:2", SourceEvidenceDigest: strings.Repeat("a", 64), Category: ActivityRun, EventKind: ActivityRunTransition, Actor: ActivityActorController, Scope: ScopeRun, TargetID: "run-1", TargetBindingDigest: strings.Repeat("b", 64), ReasonCode: ActivityReasonStateChanged, PriorState: "received", ResultingState: "executing", PriorVersion: 1, ResultingVersion: 2, OccurredAt: now, RelatedResources: []ActivityRelatedResource{{Kind: ScopeRun, ID: "run-1"}}, EvidenceDigests: []string{strings.Repeat("c", 64)}, Coverage: ActivityEventCoverage{IngestionClass: ActivityIngestionCurrent, LegacyReconstructable: true}}
	first, second := NewActivityEvent(input), NewActivityEvent(input)
	if err := ValidateActivityEvent(first); err != nil {
		t.Fatal(err)
	}
	if first.EventID != second.EventID || first.SnapshotDigest != second.SnapshotDigest {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	drift := first
	drift.ResultingState = "failed"
	if ValidateActivityEvent(drift) == nil {
		t.Fatal("mutable snapshot drift was accepted")
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"source_kind", "source_identity", "target_binding_digest", "snapshot_digest", "sqlite", "/private/", "credential"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("activity leaked %q: %s", forbidden, raw)
		}
	}
}

func TestActivityReplayCompatibilityOnlyIgnoresCurrentBackfillClassification(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	input := ActivityEventInput{SourceKind: "run_transition", SourceIdentity: "run-1:2", SourceEvidenceDigest: strings.Repeat("a", 64), Category: ActivityRun, EventKind: ActivityRunTransition, Actor: ActivityActorController, Scope: ScopeRun, TargetID: "run-1", TargetBindingDigest: strings.Repeat("b", 64), ReasonCode: ActivityReasonStateChanged, PriorState: "received", ResultingState: "executing", PriorVersion: 1, ResultingVersion: 2, OccurredAt: now, RelatedResources: []ActivityRelatedResource{{Kind: ScopeRun, ID: "run-1"}}, EvidenceDigests: []string{strings.Repeat("a", 64)}, Coverage: ActivityEventCoverage{IngestionClass: ActivityIngestionCurrent, LegacyReconstructable: true}}
	current := NewActivityEvent(input)
	input.Coverage.IngestionClass = ActivityIngestionBackfill
	backfill := NewActivityEvent(input)
	if !ActivityEventsCompatibleForReplay(current, backfill) || !ActivityEventsCompatibleForReplay(backfill, current) {
		t.Fatal("current and backfill replay should be compatible")
	}
	drift := backfill
	drift.ResultingState = "failed"
	drift.SnapshotDigest = activitySnapshotDigest(drift)
	if ActivityEventsCompatibleForReplay(current, drift) {
		t.Fatal("semantic drift was accepted as compatible replay")
	}
	runtime := backfill
	runtime.Coverage.IngestionClass = ActivityIngestionRuntime
	runtime.SnapshotDigest = activitySnapshotDigest(runtime)
	if ActivityEventsCompatibleForReplay(current, runtime) {
		t.Fatal("runtime ingestion was accepted as compatible backfill")
	}
}

func TestActivityClassificationsAndCoverageAreClosed(t *testing.T) {
	if got := ActivityCoveragePrecedence(ActivityCoverageComplete, ActivityCoverageBackfilling, ActivityCoverageDegraded); got != ActivityCoverageDegraded {
		t.Fatalf("precedence=%s", got)
	}
	coverage := ActivityCoverage{State: ActivityCoverageComplete, ReasonCode: "reconstructable_boundary_complete", LegacyLimitations: []string{"legacy_worker_readiness_unavailable"}, BackfillSourcesComplete: 6, BackfillSourcesTotal: 6}
	if err := ValidateActivityCoverage(coverage); err != nil {
		t.Fatal(err)
	}
	coverage.State = "partially_ready"
	if ValidateActivityCoverage(coverage) == nil {
		t.Fatal("open-ended coverage state was accepted")
	}
	input := ActivityEventInput{SourceKind: "source", SourceIdentity: "identity", SourceEvidenceDigest: strings.Repeat("a", 64), Category: "telemetry", EventKind: ActivityRunTransition, Actor: ActivityActorController, Scope: ScopeController, TargetID: "controller", TargetBindingDigest: strings.Repeat("b", 64), ReasonCode: ActivityReasonStateChanged, OccurredAt: time.Now().UTC(), Coverage: ActivityEventCoverage{IngestionClass: ActivityIngestionCurrent}}
	if ValidateActivityEvent(NewActivityEvent(input)) == nil {
		t.Fatal("open-ended activity category was accepted")
	}
}

func TestActivityAndOperationHistoryCursorBindingRejectsDrift(t *testing.T) {
	activity := ActivityCursor{Version: ActivitySchemaVersion, ScopeDigest: strings.Repeat("a", 64), FilterDigest: strings.Repeat("b", 64), OccurredAt: time.Now().UTC(), EventID: "activity-1", IngestionWatermark: 3}
	encoded := encodeActivityCursor(activity)
	decoded, err := decodeActivityCursor(encoded)
	if err != nil || decoded.EventID != activity.EventID || decoded.IngestionWatermark != activity.IngestionWatermark {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	filterA := activityFilterDigest(ActivityFilter{Category: ActivityRun})
	filterB := activityFilterDigest(ActivityFilter{Category: ActivityOperation})
	if filterA == filterB {
		t.Fatal("activity filter drift did not change binding")
	}
	historyA := operationHistoryFilterDigest(OperationHistoryFilter{Phase: OperationPhaseAccepted})
	historyB := operationHistoryFilterDigest(OperationHistoryFilter{Phase: OperationPhaseObserved})
	if historyA == historyB {
		t.Fatal("operation filter drift did not change binding")
	}
}

func TestSharedCollectionMetadataPreservesExactActivityAndOperationHistoryBytes(t *testing.T) {
	observedAt := time.Date(2026, 8, 29, 1, 2, 3, 4, time.UTC)
	collection := RoutineCollectionMetadata{Total: 7, Truncated: true, NextCursor: "next-page"}
	priorCollection := priorCollectionMetadata(collection)
	coverage := ActivityCoverage{State: ActivityCoverageComplete, ReasonCode: "complete", LegacyLimitations: []string{}, BackfillSourcesComplete: 6, BackfillSourcesTotal: 6}

	activity := ActivityPage{Metadata: RoutineProjectionMetadata{SchemaVersion: ActivitySchemaVersion, ObservedAt: observedAt}, Collection: collection, Coverage: coverage, Events: []ActivityEvent{}}
	priorActivity := priorActivityPage{Metadata: activity.Metadata, Collection: priorCollection, Coverage: coverage, Events: []ActivityEvent{}}
	activityDigest := activityProjectionDigest(activity)
	priorActivityRaw, err := json.Marshal(priorActivity)
	if err != nil {
		t.Fatal(err)
	}
	if want := digestText("activity-projection-v1\x00" + string(priorActivityRaw)); activityDigest != want {
		t.Fatalf("activity digest=%s want=%s", activityDigest, want)
	}
	activity.Metadata.Digest, priorActivity.Metadata.Digest = activityDigest, activityDigest
	assertExactProjectionJSON(t, activity, priorActivity)

	history := OperationHistoryPage{Metadata: RoutineProjectionMetadata{SchemaVersion: ActivitySchemaVersion, ObservedAt: observedAt}, Collection: collection, Receipts: []OperationReceipt{}}
	priorHistory := priorOperationHistoryPage{Metadata: history.Metadata, Collection: priorCollection, Receipts: []OperationReceipt{}}
	historyDigest := operationHistoryProjectionDigest(history)
	priorHistoryRaw, err := json.Marshal(priorHistory)
	if err != nil {
		t.Fatal(err)
	}
	if want := digestText("operation-history-projection-v1\x00" + string(priorHistoryRaw)); historyDigest != want {
		t.Fatalf("operation history digest=%s want=%s", historyDigest, want)
	}
	history.Metadata.Digest, priorHistory.Metadata.Digest = historyDigest, historyDigest
	assertExactProjectionJSON(t, history, priorHistory)

	activityCursor := ActivityCursor{Version: ActivitySchemaVersion, ScopeDigest: strings.Repeat("a", 64), FilterDigest: strings.Repeat("b", 64), OccurredAt: observedAt, EventID: "activity-1", IngestionWatermark: 9}
	activityCursorJSON := `{"v":"v1","s":"` + activityCursor.ScopeDigest + `","f":"` + activityCursor.FilterDigest + `","o":"2026-08-29T01:02:03.000000004Z","e":"activity-1","w":9}`
	if got, want := encodeActivityCursor(activityCursor), base64.RawURLEncoding.EncodeToString([]byte(activityCursorJSON)); got != want {
		t.Fatalf("activity cursor=%s want=%s", got, want)
	}
	historyCursor := OperationHistoryCursor{Version: ActivitySchemaVersion, ScopeDigest: strings.Repeat("c", 64), FilterDigest: strings.Repeat("d", 64), AcceptedAt: observedAt, OperationID: "operation-1", WatermarkAcceptedAt: observedAt.Add(time.Minute), WatermarkOperation: "operation-2"}
	historyCursorJSON := `{"v":"v1","s":"` + historyCursor.ScopeDigest + `","f":"` + historyCursor.FilterDigest + `","a":"2026-08-29T01:02:03.000000004Z","o":"operation-1","wa":"2026-08-29T01:03:03.000000004Z","wo":"operation-2"}`
	if got, want := encodeOperationHistoryCursor(historyCursor), base64.RawURLEncoding.EncodeToString([]byte(historyCursorJSON)); got != want {
		t.Fatalf("operation history cursor=%s want=%s", got, want)
	}
}

func assertExactProjectionJSON(t *testing.T, current, prior any) {
	t.Helper()
	currentRaw, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	priorRaw, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(currentRaw, priorRaw) {
		t.Fatalf("current=%s prior=%s", currentRaw, priorRaw)
	}
}
