package domain

import (
	"strings"
	"testing"
	"time"
)

func TestRequiredChecksFailClosed(t *testing.T) {
	base := GitHubReadEvidence{PullRequest: PullRequest{HeadSHA: "h"}, Checks: []GitHubCheck{{Name: "test", Required: true, ObservedSHA: "h", State: CheckSuccess}}}
	if got := base.RequiredChecksStatus(); got != ReconciliationPass {
		t.Fatalf("got %s", got)
	}
	base.Checks[0].State = CheckUnknown
	if got := base.RequiredChecksStatus(); got != ReconciliationInfrastructure {
		t.Fatalf("unknown got %s", got)
	}
	base.Checks = nil
	if got := base.RequiredChecksStatus(); got != ReconciliationInfrastructure {
		t.Fatalf("missing got %s", got)
	}
	base.UnknownEvents = []string{"missing_required_check:test"}
	if !base.RequiredChecksWaiting() {
		t.Fatal("missing required check was not recognized as workflow-start wait")
	}
}

func TestRequiredChecksAggregateIsOrderIndependent(t *testing.T) {
	base := GitHubReadEvidence{PullRequest: PullRequest{HeadSHA: "head"}}
	success := GitHubCheck{Name: "success", Required: true, ObservedSHA: "head", State: CheckSuccess}
	queued := GitHubCheck{Name: "queued", Required: true, ObservedSHA: "head", State: CheckQueued}
	failed := GitHubCheck{Name: "failed", Required: true, ObservedSHA: "head", State: CheckFailure}

	missing := base
	missing.Checks = []GitHubCheck{success}
	missing.UnknownEvents = []string{"missing_required_check:lint"}
	if missing.RequiredChecksStatus() != ReconciliationPending || !missing.RequiredChecksWaiting() {
		t.Fatalf("success plus missing status=%s waiting=%v", missing.RequiredChecksStatus(), missing.RequiredChecksWaiting())
	}
	for _, checks := range [][]GitHubCheck{{queued, failed}, {failed, queued}} {
		evidence := base
		evidence.Checks = checks
		if evidence.RequiredChecksStatus() != ReconciliationActionable || evidence.RequiredChecksWaiting() {
			t.Fatalf("checks=%v status=%s waiting=%v", checks, evidence.RequiredChecksStatus(), evidence.RequiredChecksWaiting())
		}
	}
	wrongSHA := failed
	wrongSHA.ObservedSHA = "other"
	stale := GitHubCheck{Name: "stale", Required: true, ObservedSHA: "head", State: CheckStale}
	for _, checks := range [][]GitHubCheck{{failed, wrongSHA}, {wrongSHA, failed}, {failed, stale}, {stale, failed}} {
		evidence := base
		evidence.Checks = checks
		if evidence.RequiredChecksStatus() != ReconciliationInfrastructure || evidence.RequiredChecksWaiting() {
			t.Fatalf("incomplete permutation=%v status=%s waiting=%v", checks, evidence.RequiredChecksStatus(), evidence.RequiredChecksWaiting())
		}
	}
	for _, checks := range [][]GitHubCheck{{failed, queued}, {queued, failed}} {
		evidence := base
		evidence.Checks = checks
		evidence.UnknownEvents = []string{"future_check_event"}
		if evidence.RequiredChecksStatus() != ReconciliationInfrastructure {
			t.Fatalf("unknown permutation=%v status=%s", checks, evidence.RequiredChecksStatus())
		}
	}
	unknown := missing
	unknown.UnknownEvents = append(unknown.UnknownEvents, "future_check_event")
	if unknown.RequiredChecksStatus() != ReconciliationInfrastructure || unknown.RequiredChecksWaiting() {
		t.Fatalf("unknown telemetry status=%s waiting=%v", unknown.RequiredChecksStatus(), unknown.RequiredChecksWaiting())
	}
}

func TestDeliveryStatusRequiresOpenPRAndExactRequiredChecks(t *testing.T) {
	base := GitHubReadEvidence{
		PullRequest: PullRequest{State: "open", HeadSHA: "head"},
		Checks:      []GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: CheckSuccess}},
	}
	if got := base.DeliveryStatus(); got != ReconciliationPass {
		t.Fatalf("status=%s", got)
	}
	base.PullRequest.State = "closed"
	if got := base.DeliveryStatus(); got != ReconciliationInfrastructure {
		t.Fatalf("closed PR status=%s", got)
	}
	base.PullRequest.State = "open"
	base.UnknownEvents = []string{"unknown_review_event:1"}
	if got := base.DeliveryStatus(); got != ReconciliationInfrastructure {
		t.Fatalf("unknown telemetry status=%s", got)
	}
	base.UnknownEvents = nil
}

func TestNormalizeHumanApprovalRequiresConfiguredImmutableUserAtExactHead(t *testing.T) {
	now := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	pr := PullRequest{Number: 7, HeadSHA: "head"}
	trusted := []ActorIdentity{{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}}
	review := GitHubReview{DatabaseID: 44, NodeID: "PRR_44", State: "APPROVED", CommitSHA: "head", SourceAt: now, Actor: trusted[0]}
	observation, approval, err := NormalizeHumanApproval(pr, []GitHubReview{review}, trusted, now.Add(time.Second))
	if err != nil || observation.Status != HumanApprovalApproved || approval == nil || approval.Actor.DatabaseID != 33 || approval.ReviewNodeID != "PRR_44" {
		t.Fatalf("observation=%+v approval=%+v err=%v", observation, approval, err)
	}
	if err := approval.Authorizes(pr, "head"); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeHumanApprovalRejectsBotAndLoginLookalike(t *testing.T) {
	now := time.Now().UTC()
	pr := PullRequest{Number: 7, HeadSHA: "head"}
	trusted := []ActorIdentity{{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}}
	for _, actor := range []ActorIdentity{
		{DatabaseID: 33, NodeID: "BOT_33", Login: "ifan0927", Type: "Bot"},
		{DatabaseID: 99, NodeID: "USER_99", Login: "ifan0927", Type: "User"},
	} {
		observation, approval, err := NormalizeHumanApproval(pr, []GitHubReview{{DatabaseID: 44, NodeID: "PRR_44", State: "APPROVED", CommitSHA: "head", SourceAt: now, Actor: actor}}, trusted, now)
		if err != nil || approval != nil || observation.Status != HumanApprovalUntrustedActor {
			t.Fatalf("actor=%+v observation=%+v approval=%+v err=%v", actor, observation, approval, err)
		}
	}
}

func TestNormalizeHumanApprovalPersistsDismissalChangesAndStaleHead(t *testing.T) {
	now := time.Now().UTC()
	pr := PullRequest{Number: 7, HeadSHA: "head"}
	trusted := []ActorIdentity{{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}}
	for _, tc := range []struct {
		state, commit string
		want          HumanApprovalStatus
	}{
		{"DISMISSED", "head", HumanApprovalDismissed},
		{"CHANGES_REQUESTED", "head", HumanApprovalChangesRequested},
		{"APPROVED", "old", HumanApprovalStaleHead},
	} {
		observation, approval, err := NormalizeHumanApproval(pr, []GitHubReview{{DatabaseID: 44, NodeID: "PRR_44", State: tc.state, CommitSHA: tc.commit, SourceAt: now, Actor: trusted[0]}}, trusted, now)
		if err != nil || approval != nil || observation.Status != tc.want || observation.SourceAt.IsZero() || observation.ObservedAt.IsZero() {
			t.Fatalf("case=%+v observation=%+v approval=%+v err=%v", tc, observation, approval, err)
		}
	}
}

func TestNormalizeHumanApprovalUsesLatestTrustedReview(t *testing.T) {
	now := time.Now().UTC()
	pr := PullRequest{Number: 7, HeadSHA: "head"}
	trusted := []ActorIdentity{{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}}
	reviews := []GitHubReview{
		{DatabaseID: 44, NodeID: "PRR_44", State: "CHANGES_REQUESTED", CommitSHA: "old", SourceAt: now, Actor: trusted[0]},
		{DatabaseID: 45, NodeID: "PRR_45", State: "APPROVED", CommitSHA: "head", SourceAt: now.Add(time.Minute), Actor: trusted[0]},
	}
	observation, approval, err := NormalizeHumanApproval(pr, reviews, trusted, now.Add(2*time.Minute))
	if err != nil || observation.Status != HumanApprovalApproved || approval == nil || approval.ReviewDatabaseID != 45 {
		t.Fatalf("observation=%+v approval=%+v err=%v", observation, approval, err)
	}
}

func TestInlineReviewBodyHandoffBoundsAndGenericEvidenceSeparation(t *testing.T) {
	body := "bounded body"
	valid := InlineReviewBodyHandoff{Comments: []InlineReviewBody{{ThreadNodeID: "THREAD", CommentNodeID: "COMMENT", Body: body, BodyDigest: TrustedReviewFeedbackDigest(body)}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*InlineReviewBodyHandoff){
		func(value *InlineReviewBodyHandoff) {
			value.Comments[0].Body = strings.Repeat("x", MaxTrustedReviewFeedbackBodyBytes+1)
		},
		func(value *InlineReviewBodyHandoff) { value.Comments[0].BodyDigest = "wrong" },
		func(value *InlineReviewBodyHandoff) { value.Comments[0].Body = "x\x00y" },
	} {
		candidate := valid
		candidate.Comments = append([]InlineReviewBody(nil), valid.Comments...)
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatal("invalid bounded handoff was accepted")
		}
	}
	count := InlineReviewBodyHandoff{}
	for index := 0; index < MaxTrustedReviewFeedbackPerHead+1; index++ {
		value := "x"
		count.Comments = append(count.Comments, InlineReviewBody{ThreadNodeID: "THREAD", CommentNodeID: string(rune('a' + index)), Body: value, BodyDigest: TrustedReviewFeedbackDigest(value)})
	}
	if err := count.Validate(); err == nil {
		t.Fatal("over-count handoff was accepted")
	}
	aggregate := InlineReviewBodyHandoff{}
	for index := 0; index < 5; index++ {
		value := strings.Repeat("x", MaxTrustedReviewFeedbackBodyBytes)
		aggregate.Comments = append(aggregate.Comments, InlineReviewBody{ThreadNodeID: "THREAD", CommentNodeID: string(rune('a' + index)), Body: value, BodyDigest: TrustedReviewFeedbackDigest(value)})
	}
	if err := aggregate.Validate(); err == nil {
		t.Fatal("over-aggregate handoff was accepted")
	}
}
