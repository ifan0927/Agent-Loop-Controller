package codex

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	ImplementationModel = "gpt-5.6-terra"
	ReviewModel         = "gpt-5.6-sol"
)

// Controller-managed Codex subprocesses must not inherit trigger credentials.
var controllerManagedExcludedEnvironment = []string{"IFAN_LOOP_LINEAR_TOKEN"}

type CommandSpec struct {
	Program           string   `json:"program"`
	Args              []string `json:"args"`
	WorkingDir        string   `json:"working_dir"`
	Stdin             string   `json:"stdin"`
	StdoutFormat      string   `json:"stdout_format"`
	StderrPolicy      string   `json:"stderr_policy"`
	FreshSession      bool     `json:"fresh_session"`
	ExpectedScope     string   `json:"expected_scope"`
	MustNotExist      []string `json:"must_not_exist_before_start"`
	ProcessControlKey string   `json:"-"`
}

type CommandBuilder struct {
	binary string
}

func NewCommandBuilder(binary string) CommandBuilder {
	if strings.TrimSpace(binary) == "" {
		binary = "codex"
	}
	return CommandBuilder{binary: binary}
}

func (b CommandBuilder) Implementation(task domain.CodingTask, workspace, artifacts string) CommandSpec {
	return CommandSpec{
		Program: b.binary,
		Args: []string{
			"exec",
			"--model", ImplementationModel,
			"--ignore-user-config",
			"--cd", workspace,
			"--sandbox", "workspace-write",
			"--json",
			"--output-schema", filepath.Join(artifacts, "implementation-outcome.schema.json"),
			"--output-last-message", filepath.Join(artifacts, "implementation-outcome.json"),
			"-",
		},
		WorkingDir:    workspace,
		Stdin:         implementationPrompt(task),
		StdoutFormat:  "jsonl",
		StderrPolicy:  "capture_separately",
		FreshSession:  true,
		ExpectedScope: "workspace_mutation",
		MustNotExist:  []string{filepath.Join(artifacts, "implementation-outcome.json")},
	}
}

func (b CommandBuilder) Resume(sessionID, model, workspace, artifacts, instructions string) (CommandSpec, error) {
	if strings.TrimSpace(sessionID) == "" {
		return CommandSpec{}, fmt.Errorf("session ID must not be blank")
	}
	if model != ImplementationModel {
		return CommandSpec{}, fmt.Errorf("persisted implementation model %q is not supported by controller policy", model)
	}
	return CommandSpec{
		Program: b.binary,
		Args: []string{
			"exec", "resume",
			"--model", model,
			"--ignore-user-config",
			"--config", `sandbox_mode="workspace-write"`,
			"--json",
			"--output-schema", filepath.Join(artifacts, "implementation-outcome.schema.json"),
			"--output-last-message", filepath.Join(artifacts, "implementation-outcome.json"),
			sessionID,
			"-",
		},
		WorkingDir:    workspace,
		Stdin:         instructions,
		StdoutFormat:  "jsonl",
		StderrPolicy:  "capture_separately",
		FreshSession:  false,
		ExpectedScope: "workspace_mutation",
		MustNotExist:  []string{filepath.Join(artifacts, "implementation-outcome.json")},
	}, nil
}

func (b CommandBuilder) FreshReview(task domain.CodingTask, workspace, artifacts string) CommandSpec {
	return CommandSpec{
		Program: b.binary,
		Args: []string{
			"exec",
			"--model", ReviewModel,
			"--ephemeral",
			"--ignore-user-config",
			"--sandbox", "read-only",
			"--cd", workspace,
			"--json",
			"--output-schema", filepath.Join(artifacts, "review-outcome.schema.json"),
			"--output-last-message", filepath.Join(artifacts, "review-outcome.json"),
			"-",
		},
		WorkingDir:    workspace,
		Stdin:         reviewPrompt(task),
		StdoutFormat:  "jsonl",
		StderrPolicy:  "capture_separately_and_assert_no_mutation",
		FreshSession:  true,
		ExpectedScope: "read_only_review",
		MustNotExist:  []string{filepath.Join(artifacts, "review-outcome.json")},
	}
}

func implementationPrompt(task domain.CodingTask) string {
	return fmt.Sprintf(`You are implementing one Linear-managed coding task.

Issue: %s
Title: %s
Goal: %s

Description:
%s

Acceptance criteria:
%s

Out of scope:
%s

Required verifier profiles (resolved only by the controller's repository-owned registry):
%s

Read and follow all applicable AGENTS.md files and repository rules. Inspect the
relevant specifications and existing tests before editing. Make only changes
required by this task. Run the narrowest sufficient verification. Do not create
or modify Linear issues, branches, commits, pull requests, or external systems;
the controller owns those operations.

Stop with needs_human_decision if API, schema, domain behavior, scope, destructive
data changes, or conflicting repository contracts require a human choice.

For needs_human_decision, provide at least two options with unique IDs. Set
recommendation to exactly one of those option IDs, not to free-form prose.
`, task.IssueID, task.Title, task.Goal, task.Description, bullets(task.AcceptanceCriteria), bullets(task.OutOfScope), bullets(task.VerifierIDs))
}

func reviewPrompt(task domain.CodingTask) string {
	return fmt.Sprintf(`Perform a fresh, independent, read-only review of the complete branch delta
for %s. Inspect the exact current HEAD against origin/%s using Git. Return the exact
current HEAD commit as reviewed_head_sha.

Review against the issue goal and acceptance criteria, applicable AGENTS.md files,
repository specifications, tests, security boundaries, and maintainability. Focus
on real behavioral, integration, authorization, data, and regression risks. Do not
edit files. A pass means the exact reviewed head is ready to open as a pull request.

Goal: %s

Description:
%s

Acceptance criteria:
%s

Out of scope:
%s
`, task.IssueID, task.BaseBranch, task.Goal, task.Description, bullets(task.AcceptanceCriteria), bullets(task.OutOfScope))
}

func bullets(items []string) string {
	if len(items) == 0 {
		return "- None"
	}
	lines := make([]string, len(items))
	for i, item := range items {
		lines[i] = "- " + item
	}
	return strings.Join(lines, "\n")
}
