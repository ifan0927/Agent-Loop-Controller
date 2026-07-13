package domain

import (
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
}

func TestDeliveryStatusRequiresOpenPRTrustedCodeRabbitAndExactFindings(t *testing.T) {
	base := GitHubReadEvidence{
		PullRequest:    PullRequest{State: "open", HeadSHA: "head"},
		Checks:         []GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: CheckSuccess}},
		ReviewDecision: "REVIEW_REQUIRED",
		CodeRabbit:     CodeRabbitPass,
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
	base.Findings = []NormalizedFinding{{Source: "coderabbit_review_comment", SourceID: "1", BodyDigest: "digest", HeadSHA: "head"}}
	if got := base.DeliveryStatus(); got != ReconciliationActionable {
		t.Fatalf("active finding status=%s", got)
	}
	base.Findings[0].Resolved = true
	base.CodeRabbit = CodeRabbitPending
	if got := base.DeliveryStatus(); got != ReconciliationPending {
		t.Fatalf("pending CodeRabbit status=%s", got)
	}
	base.CodeRabbit = CodeRabbitPass
	base.ReviewDecision = ""
	if got := base.DeliveryStatus(); got != ReconciliationPending {
		t.Fatalf("missing review decision status=%s", got)
	}
	base.ReviewDecision = "REVIEW_REQUIRED"
	if got := base.DeliveryStatus(); got != ReconciliationPass {
		t.Fatalf("review required status=%s", got)
	}
	base.ReviewDecision = "UNRECOGNIZED"
	if got := base.DeliveryStatus(); got != ReconciliationInfrastructure {
		t.Fatalf("unknown review decision status=%s", got)
	}
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
