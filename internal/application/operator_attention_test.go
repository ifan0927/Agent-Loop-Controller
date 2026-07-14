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
		if test.err != nil || test.event.DeliveryStatus != OperatorAttentionDeliveryPendingLocal || test.event.PayloadDigest == "" || !strings.HasPrefix(test.event.EventKey, "automation:scan-1:") {
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
