package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestImplementationUsesStdinAndSafeSandbox(t *testing.T) {
	task := testTask()
	spec := NewCommandBuilder("codex").Implementation(task, "/tmp/worktree", "/tmp/run")

	if !slices.Contains(spec.Args, "workspace-write") {
		t.Fatal("implementation must use workspace-write sandbox")
	}
	if spec.Args[len(spec.Args)-1] != "-" {
		t.Fatal("prompt must be read from stdin")
	}
	if slices.Contains(spec.Args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatal("unsafe sandbox bypass must never be used")
	}
	if slices.Contains(spec.Args, "--strict-config") {
		t.Fatal("runs must not fail because of unrelated global config drift")
	}
	if !slices.Contains(spec.Args, "--ignore-user-config") {
		t.Fatal("managed runs must not load global MCP, hook, or tool configuration")
	}
	if len(spec.MustNotExist) != 1 {
		t.Fatal("implementation output leaf must be absent before start")
	}
	if !strings.Contains(spec.Stdin, task.IssueID) {
		t.Fatal("implementation prompt must identify the issue")
	}
}

func TestReviewIsFreshAndCoversBranchDelta(t *testing.T) {
	task := testTask()
	spec := NewCommandBuilder("codex").FreshReview(task, "/tmp/worktree", "/tmp/run")

	if !spec.FreshSession || !slices.Contains(spec.Args, "--ephemeral") {
		t.Fatal("review must be a fresh ephemeral session")
	}
	if !containsPair(spec.Args, "--sandbox", "read-only") {
		t.Fatal("review must use the read-only sandbox")
	}
	if spec.ExpectedScope != "read_only_review" {
		t.Fatal("review must be declared read-only")
	}
	if !containsPair(spec.Args, "--cd", "/tmp/worktree") {
		t.Fatal("review must run in the owned worktree")
	}
	if !slices.Contains(spec.Args, "--ignore-user-config") {
		t.Fatal("fresh review must not load global MCP, hook, or tool configuration")
	}
	if len(spec.MustNotExist) != 1 {
		t.Fatal("review output leaf must be absent before start")
	}
	if slices.Contains(spec.Args, "review") {
		t.Fatal("Codex 0.144.1 built-in review cannot provide the required structured outcome")
	}
	if !strings.Contains(spec.Stdin, "origin/dev") {
		t.Fatal("review prompt must define the complete branch delta base")
	}
}

func TestResumeRequiresExplicitSessionID(t *testing.T) {
	spec, err := NewCommandBuilder("codex").Resume("session-123", "/tmp/worktree", "/tmp/run", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(spec.Args, "--last") {
		t.Fatal("controller must never resume the globally latest session")
	}
	if !slices.Contains(spec.Args, "session-123") {
		t.Fatal("resume must use an explicit session ID")
	}
	if !containsPair(spec.Args, "--config", `sandbox_mode="workspace-write"`) {
		t.Fatal("resume must explicitly preserve the workspace-write sandbox")
	}
}

func TestResumeRejectsBlankSessionID(t *testing.T) {
	if _, err := NewCommandBuilder("codex").Resume(" ", "/tmp/worktree", "/tmp/run", "continue"); err == nil {
		t.Fatal("blank session ID must be rejected")
	}
}

func TestPromptsContainCompleteTaskScope(t *testing.T) {
	task := testTask()
	task.Description = "Detailed Linear issue context"
	task.OutOfScope = []string{"Do not change the API"}
	builder := NewCommandBuilder("codex")

	implementation := builder.Implementation(task, "/tmp/worktree", "/tmp/run")
	review := builder.FreshReview(task, "/tmp/worktree", "/tmp/run")
	for name, prompt := range map[string]string{
		"implementation": implementation.Stdin,
		"review":         review.Stdin,
	} {
		if !strings.Contains(prompt, task.Description) || !strings.Contains(prompt, task.OutOfScope[0]) {
			t.Fatalf("%s prompt must contain description and out-of-scope contract", name)
		}
	}
}

func containsPair(values []string, first, second string) bool {
	for i := range len(values) - 1 {
		if values[i] == first && values[i+1] == second {
			return true
		}
	}
	return false
}

func testTask() domain.CodingTask {
	return domain.CodingTask{
		IssueID:            "IFAN-123",
		Title:              "Example",
		Goal:               "Implement the example",
		BaseBranch:         "dev",
		AcceptanceCriteria: []string{"Tests pass"},
	}
}
