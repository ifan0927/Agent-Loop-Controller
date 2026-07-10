package domain

import "testing"

func TestCodingTaskMissingFieldsAreDeterministic(t *testing.T) {
	err := (CodingTask{}).Validate()
	want := "missing required task fields: run_id, issue_id, title, repository, base_branch, working_branch, goal, source_revision"
	if err == nil || err.Error() != want {
		t.Fatalf("Validate() error = %v, want %q", err, want)
	}
}

func TestCodingTaskRejectsBlankAcceptanceAndVerification(t *testing.T) {
	task := validCodingTask()
	task.AcceptanceCriteria = []string{" "}
	if err := task.Validate(); err == nil {
		t.Fatal("blank acceptance criterion must be rejected")
	}
	task = validCodingTask()
	task.VerifierIDs = []string{"go test ./..."}
	if err := task.Validate(); err == nil {
		t.Fatal("executable verification text must be rejected as a verifier ID")
	}
}

func TestCodingTaskRejectsUnsafeGitBranches(t *testing.T) {
	for _, branch := range []string{"-option", "bad..branch", "bad@{ref", "bad branch", "bad.lock", "bad\nbranch"} {
		task := validCodingTask()
		task.WorkingBranch = branch
		if err := task.Validate(); err == nil {
			t.Fatalf("unsafe branch %q must be rejected", branch)
		}
	}
}

func validCodingTask() CodingTask {
	return CodingTask{
		RunID:              "run-1",
		IssueID:            "IFAN-1",
		Title:              "Example",
		Repository:         "owner/repo",
		BaseBranch:         "dev",
		WorkingBranch:      "ifan/ifan-1-example",
		Goal:               "Implement example",
		AcceptanceCriteria: []string{"Works"},
		VerifierIDs:        []string{"go-test-all"},
		Policy: TaskPolicy{
			HumanApprovalRequired: true,
			MergeMethod:           "squash",
		},
		SourceRevision: "revision-1",
	}
}

func TestAgentOutcomeRequiresDecisionRequest(t *testing.T) {
	outcome := AgentOutcome{Status: AgentNeedsHumanDecision, Summary: "A choice is required"}
	if err := outcome.Validate(); err == nil {
		t.Fatal("needs_human_decision without a request must be rejected")
	}
}

func TestAgentOutcomeRejectsUnusableDecisionRequest(t *testing.T) {
	outcome := AgentOutcome{
		Status:  AgentNeedsHumanDecision,
		Summary: "A choice is required",
		DecisionRequest: &DecisionRequest{
			Question:       " ",
			Context:        "Context",
			BlockingReason: "Blocked",
		},
	}
	if err := outcome.Validate(); err == nil {
		t.Fatal("blank decision question must be rejected")
	}
}

func TestDecisionRecommendationMustReferenceUniqueOption(t *testing.T) {
	request := DecisionRequest{
		Question:       "Choose behavior",
		Context:        "The contract is ambiguous",
		BlockingReason: "Implementation cannot continue",
		Recommendation: "missing",
		Options: []DecisionOption{
			{ID: "preserve", Description: "Preserve existing behavior"},
			{ID: "change", Description: "Change the contract"},
		},
	}
	if err := request.Validate(); err == nil {
		t.Fatal("unknown recommendation must be rejected")
	}
}

func TestReviewPassRejectsFindings(t *testing.T) {
	outcome := ReviewOutcome{
		Verdict:         ReviewPass,
		ReviewedHeadSHA: "abc123",
		Findings:        []ReviewFinding{{ID: "finding-1"}},
	}
	if err := outcome.Validate(); err == nil {
		t.Fatal("pass with findings must be rejected")
	}
}

func TestReviewFindingsRequiresFinding(t *testing.T) {
	outcome := ReviewOutcome{Verdict: ReviewFindings, Summary: "Findings exist", ReviewedHeadSHA: "abc123"}
	if err := outcome.Validate(); err == nil {
		t.Fatal("findings verdict without findings must be rejected")
	}
}

func TestReviewRequiresSummary(t *testing.T) {
	outcome := ReviewOutcome{Verdict: ReviewPass, ReviewedHeadSHA: "abc123"}
	if err := outcome.Validate(); err == nil {
		t.Fatal("review without summary must be rejected")
	}
}

func TestReviewRejectsUnusableFinding(t *testing.T) {
	outcome := ReviewOutcome{
		Verdict:         ReviewFindings,
		Summary:         "A finding exists",
		ReviewedHeadSHA: "abc123",
		Findings: []ReviewFinding{
			{ID: " ", Severity: "medium", Title: "Missing ID", Body: "Fix this"},
		},
	}
	if err := outcome.Validate(); err == nil {
		t.Fatal("finding with blank ID must be rejected")
	}
}
