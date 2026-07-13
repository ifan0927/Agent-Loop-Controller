package domain

import "testing"

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
