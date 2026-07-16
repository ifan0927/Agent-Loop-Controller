package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOperatorAttentionProducerMappingsAreAllowlistedAndDeterministic(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	profile := OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}
	cases := []struct {
		name  string
		event OperatorAttentionEvent
		err   error
	}{
		{"priority tie", mustCandidateTie(t, profile, digest, now), nil},
		{"scan incomplete", mustCandidateScan(t, profile, digest, now), nil},
		{"lease", mustSchedulerLease(t, profile, digest, now), nil},
	}
	for _, test := range cases {
		if test.err != nil || test.event.SchemaVersion != OperatorAttentionSchemaVersion || test.event.PayloadDigest == "" || test.event.AllowedActions == nil || !strings.HasPrefix(test.event.EventKey, "automation:scan-1:") {
			t.Fatalf("%s event=%+v err=%v", test.name, test.event, test.err)
		}
		if err := ValidateOperatorAttentionEvent(test.event); err != nil {
			t.Fatalf("%s validation: %v", test.name, err)
		}
	}
	if event := mustCandidateTie(t, profile, digest, now); event.EventKey != "automation:scan-1:candidate_priority_tie:"+digest {
		t.Fatalf("event key=%q", event.EventKey)
	}
}

func TestAutomaticRetryAttentionAdvertisesOnlyTypedPresentationActions(t *testing.T) {
	run := operatorAttentionTestRun(t, domain.StateExecuting)
	now := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	schedule := RetrySchedule{RunID: run.ID, Phase: AutomaticRetryPhaseForRun(run), ControllerState: string(run.State), AttemptCount: 4, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureProcessStart, ReasonCode: RetryReasonBudgetExhausted, Status: RetryScheduleAttention, AttentionAt: now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	event, err := AutomaticRetryAttentionEvent(run, schedule)
	if err != nil {
		t.Fatal(err)
	}
	want := []OperatorAttentionActionID{OperatorAttentionActionRetry, OperatorAttentionActionAbandon}
	if !equalOperatorAttentionActions(event.AllowedActions, want) {
		t.Fatalf("allowed actions=%v", event.AllowedActions)
	}
	tampered := event
	tampered.AllowedActions = []OperatorAttentionActionID{OperatorAttentionActionAbandon}
	tampered.PayloadDigest = OperatorAttentionPayloadDigest(tampered)
	if err := ValidateOperatorAttentionEvent(tampered); err == nil {
		t.Fatal("expected presentation action policy mismatch")
	}
}

func TestOperatorAttentionPreservesEveryRetryControllerState(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	for _, state := range []domain.State{domain.StateReplyingReviewFeedback, domain.StateAwaitingLinearCompletion} {
		t.Run(string(state), func(t *testing.T) {
			run := operatorAttentionTestRun(t, state)
			schedule := RetrySchedule{RunID: run.ID, Phase: AutomaticRetryPhaseForRun(run), ControllerState: string(state), AttemptCount: 4, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureProcessStart, ReasonCode: RetryReasonBudgetExhausted, Status: RetryScheduleAttention, AttentionAt: now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
			event, err := AutomaticRetryAttentionEvent(run, schedule)
			if err != nil || event.ControllerState != string(state) || !equalOperatorAttentionActions(event.AllowedActions, []OperatorAttentionActionID{OperatorAttentionActionRetry, OperatorAttentionActionAbandon}) {
				t.Fatalf("event=%+v err=%v", event, err)
			}
		})
	}
}

func TestManualInterventionAttentionIsStableAndAdvertisesAbandonOnly(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	run := operatorAttentionTestRun(t, domain.StateManualIntervention)
	run.CandidateHead, run.UpdatedAt = strings.Repeat("a", 40), now
	transition := Transition{Sequence: 9, From: domain.StateMerging, To: domain.StateManualIntervention, Reason: "authority conflict", EvidenceReference: "merge_evidence", BoundHead: run.CandidateHead, CreatedAt: now}
	first, err := ManualInterventionAttentionEvent(run, transition)
	if err != nil {
		t.Fatal(err)
	}
	run.UpdatedAt = now.Add(time.Hour)
	second, err := ManualInterventionAttentionEvent(run, transition)
	if err != nil || first.EventKey != second.EventKey || first.PayloadDigest != second.PayloadDigest || !equalOperatorAttentionActions(first.AllowedActions, []OperatorAttentionActionID{OperatorAttentionActionAbandon}) {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
}

func TestOperatorAttentionUnknownInputsMapToGenericWithoutLeakage(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	secret := "Authorization: Bearer not-for-output"
	event, err := newOperatorAttentionEvent(operatorAttentionEventInput{ScopeID: "scan-1", EventType: secret, Profile: OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}, State: secret, Severity: secret, ReasonCode: secret, EvidenceDigest: strings.Repeat("b", 64), OccurredAt: now, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(event)
	if event.EventType != operatorAttentionUnknown || event.ReasonCode != operatorAttentionUnknown || event.ControllerState != operatorAttentionUnknown || event.Severity != "warning" || strings.Contains(string(raw), secret) {
		t.Fatalf("unsafe generic event=%s", raw)
	}
}

func TestOperatorAttentionRejectsURLPathAndCredentialLikeProfiles(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	digest := strings.Repeat("b", 64)
	profiles := []OperatorAttentionProfile{
		{ID: "https://host/path", Name: "owner/repo"},
		{ID: "file:/tmp/controller", Name: "owner/repo"},
		{ID: "profile", Name: "/private/repo"},
		{ID: "profile", Name: "https://host/repo"},
		{ID: "Authorization-Bearer-secret", Name: "owner/repo"},
		{ID: "password", Name: "owner/repo"},
		{ID: "api-key", Name: "owner/repo"},
		{ID: "profile", Name: "bearer-token"},
		{ID: "profile", Name: "credential_backup"},
	}
	for _, profile := range profiles {
		if _, err := CandidateScanIncompleteAttentionEvent("scan-1", profile, "truncated", digest, now); err == nil {
			t.Fatalf("unsafe profile accepted: %+v", profile)
		}
	}
}

func TestLegacyOperatorAttentionProjectionRedactsProfileValuesRejectedByCurrentContract(t *testing.T) {
	event := OperatorAttentionEvent{SchemaVersion: OperatorAttentionLegacySchemaVersion, EventKey: "automation:scan-1:candidate_scan_incomplete:" + strings.Repeat("a", 64), EventType: OperatorAttentionCandidateScan, RepositoryProfileID: "https://host/profile", RepositoryProfileName: "file:/private/repo", ControllerState: "scan", Severity: "warning", ReasonCode: "truncated", AllowedActions: []OperatorAttentionActionID{}, PayloadDigest: strings.Repeat("b", 64), EvidenceDigest: strings.Repeat("a", 64), OccurredAt: time.Now().UTC(), ObservedAt: time.Now().UTC()}
	result := projectInspection(RunInspection{OperatorAttention: []OperatorAttentionEvent{event}})
	raw, _ := json.Marshal(result)
	if len(result.OperatorAttentionEvents) != 1 || result.OperatorAttentionEvents[0].RepositoryProfileID != "legacy-profile" || result.OperatorAttentionEvents[0].RepositoryProfileName != "legacy-repository" || strings.Contains(string(raw), "https://") || strings.Contains(string(raw), "/private/") {
		t.Fatalf("unsafe legacy projection=%s", raw)
	}
}

func TestSourceCheckoutAttentionUsesRunProfileAndTransitionSequence(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	config, err := json.Marshal(LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run-1", Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", RepositoryConfigJSON: string(config), State: domain.StateCleaning}
	event, err := SourceCheckoutSkippedAttentionEvent(run, 7, string(SourceSyncReasonDirtySource), strings.Repeat("c", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	if event.EventKey != "automation:run-1:source_checkout_skipped_attention:7" || event.RepositoryProfileName != "owner/repo" || event.ControllerState != string(domain.StateCleaning) {
		t.Fatalf("event=%+v", event)
	}
}

func operatorAttentionTestRun(t *testing.T, state domain.State) Run {
	t.Helper()
	config, err := json.Marshal(LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	return Run{ID: "run-1", Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", RepositoryConfigJSON: string(config), State: state}
}

func mustCandidateTie(t *testing.T, profile OperatorAttentionProfile, digest string, now time.Time) OperatorAttentionEvent {
	t.Helper()
	event, err := CandidatePriorityTieAttentionEvent("scan-1", "IFAN-12", profile, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func mustCandidateScan(t *testing.T, profile OperatorAttentionProfile, digest string, now time.Time) OperatorAttentionEvent {
	t.Helper()
	event, err := CandidateScanIncompleteAttentionEvent("scan-1", profile, "truncated", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func mustSchedulerLease(t *testing.T, profile OperatorAttentionProfile, digest string, now time.Time) OperatorAttentionEvent {
	t.Helper()
	event, err := SchedulerLeaseAttentionEvent("scan-1", profile, "lease_lost", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
