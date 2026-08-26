package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

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
