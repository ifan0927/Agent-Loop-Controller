package domain

import (
	"strings"
	"testing"
)

func TestPRBodyContainsLinearMagicWordAndOwnership(t *testing.T) {
	task := CodingTask{IssueID: "IFAN-42", Goal: "Safe delivery", Description: "Implement the delivery workflow.", OutOfScope: []string{"Production credentials"}}
	body, err := PRBody(task, "go test ./...", "Sol pass", "run-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Summary", "## Scope and rationale", "## Validation", "## Fresh internal review", "## Out of scope", "Fixes IFAN-42", "controller-run:run-key"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in body", want)
		}
	}
}

func TestEveryCodeChangeInvalidatesApproval(t *testing.T) {
	pr := PullRequest{Number: 2, HeadSHA: "new"}
	approval := HumanApproval{PRNumber: 2, Approver: "I-Fan", Source: "github_review", ApprovedSHA: "old", CIStatus: "pass", ReviewSHA: "old"}
	if err := approval.Authorizes(pr, "new"); err == nil {
		t.Fatal("approval for old head authorized new code")
	}
}

func TestChecksMustBeCompleteAndBoundToExactSHA(t *testing.T) {
	snapshot := ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "old"}}}
	if snapshot.Classify() != ReconciliationInfrastructure {
		t.Fatal("check for another SHA must fail closed")
	}
	snapshot.Checks = nil
	if snapshot.Classify() != ReconciliationInfrastructure {
		t.Fatal("missing required check must fail closed")
	}
	snapshot.Checks = []Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}
	if snapshot.Classify() != ReconciliationPass {
		t.Fatal("complete exact-SHA checks should pass")
	}
}

func TestCheckFailuresDistinguishActionableFromInfrastructure(t *testing.T) {
	base := ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []Check{{Name: "test", Required: true, Status: "completed", ObservedSHA: "h1"}}}
	base.Checks[0].Conclusion = "failure"
	if base.Classify() != ReconciliationActionable {
		t.Fatal("test failure should be actionable")
	}
	base.Checks[0].Conclusion = "action_required"
	if base.Classify() != ReconciliationActionable {
		t.Fatal("action_required should be actionable")
	}
	base.Checks[0].Conclusion = "cancelled"
	if base.Classify() != ReconciliationInfrastructure {
		t.Fatal("cancelled should be infrastructure")
	}
	base.Checks[0].Conclusion = "timed_out"
	if base.Classify() != ReconciliationInfrastructure {
		t.Fatal("timed_out should be infrastructure")
	}
}
