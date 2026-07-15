package application

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	codexadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type fixtureCodexProcess struct{}

func (fixtureCodexProcess) Run(_ context.Context, spec processadapter.Spec) (processadapter.Result, error) {
	if err := os.WriteFile(spec.StdoutPath, nil, 0o600); err != nil {
		return processadapter.Result{}, err
	}
	if err := os.WriteFile(spec.StderrPath, nil, 0o600); err != nil {
		return processadapter.Result{}, err
	}
	if len(spec.Args) == 1 && spec.Args[0] == "--version" {
		return processadapter.Result{Outcome: processadapter.OutcomeExited, Stdout: []byte("codex-cli fixture\n")}, nil
	}
	if len(spec.Args) == 2 && spec.Args[0] == "exec" && spec.Args[1] == "--help" {
		return processadapter.Result{Outcome: processadapter.OutcomeExited, Stdout: []byte("--model --ignore-user-config --sandbox --cd --json --output-schema --output-last-message --ephemeral")}, nil
	}
	if len(spec.Args) == 3 && spec.Args[0] == "exec" && spec.Args[1] == "resume" && spec.Args[2] == "--help" {
		return processadapter.Result{Outcome: processadapter.OutcomeExited, Stdout: []byte("Usage: codex exec resume [OPTIONS] [SESSION_ID]\n--model --ignore-user-config --config --json --output-schema --output-last-message")}, nil
	}
	output := argumentValue(spec.Args, "--output-last-message")
	if argumentValue(spec.Args, "--sandbox") == "workspace-write" {
		if err := os.WriteFile(filepath.Join(spec.WorkingDir, "mathutil", "add.go"), []byte("package mathutil\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
			return processadapter.Result{}, err
		}
		if err := os.WriteFile(filepath.Join(spec.WorkingDir, "mathutil", "add_test.go"), []byte("package mathutil\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(-2, 3) != 1 { t.Fatal(\"bad sum\") } }\n"), 0o600); err != nil {
			return processadapter.Result{}, err
		}
		message := `{"status":"completed","summary":"Added Add and its test.","decision_request":null,"discovered_issues":[],"suggested_checks":[],"implementation_sha":null}`
		if err := os.WriteFile(output, []byte(message), 0o600); err != nil {
			return processadapter.Result{}, err
		}
		return processadapter.Result{Outcome: processadapter.OutcomeExited, Stdout: []byte("{\"type\":\"thread.started\",\"thread_id\":\"fixture-implementation\"}\n{\"type\":\"future.telemetry\"}\n")}, nil
	}
	head, err := (gitadapter.Workspace{}).Head(context.Background(), spec.WorkingDir)
	if err != nil {
		return processadapter.Result{}, err
	}
	message := fmt.Sprintf(`{"verdict":"pass","summary":"Fixture is ready.","reviewed_head_sha":%q,"findings":[]}`, head)
	if err := os.WriteFile(output, []byte(message), 0o600); err != nil {
		return processadapter.Result{}, err
	}
	return processadapter.Result{Outcome: processadapter.OutcomeExited, Stdout: []byte("{\"type\":\"thread.started\",\"thread_id\":\"fixture-review\"}\n")}, nil
}

func TestSpikeFixtureIntegration(t *testing.T) {
	workspace := newFixtureRepository(t)
	artifacts := t.TempDir()
	process := processadapter.OSRunner{}
	git := gitadapter.Workspace{}
	registry := verifier.NewRegistry(map[string]verifier.Command{
		"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}},
	}, process, git)
	executor := codexadapter.NewExecutor(fixtureCodexProcess{}, "codex")
	result, err := NewSpike("codex", executor, registry, git).Run(context.Background(), fixtureTask(), workspace, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_ready_simulation" || result.CandidateHeadSHA != result.VerificationHeadSHA {
		t.Fatalf("unexpected result: %+v", result)
	}
	status, err := git.Status(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(status) != "" {
		t.Fatalf("fixture worktree is dirty: %s", status)
	}
}

func newFixtureRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	workspace := filepath.Join(root, "workspace")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", "-b", "main", workspace)
	runGit(t, workspace, "config", "user.name", "Fixture")
	runGit(t, workspace, "config", "user.email", "fixture@example.invalid")
	if err := os.Mkdir(filepath.Join(workspace, "mathutil"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.invalid/fixture\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "mathutil", "doc.go"), []byte("package mathutil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "add", "--all")
	runGit(t, workspace, "commit", "-m", "Fixture base")
	runGit(t, workspace, "remote", "add", "origin", remote)
	runGit(t, workspace, "push", "origin", "main")
	runGit(t, workspace, "switch", "-c", "fixture/phase-1a")
	return workspace
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func fixtureTask() domain.CodingTask {
	return domain.CodingTask{
		RunID: "fixture-run", IssueID: "FIXTURE-1", Title: "Add", Repository: "local/fixture",
		BaseBranch: "main", WorkingBranch: "fixture/phase-1a", Goal: "Add pure function",
		AcceptanceCriteria: []string{"go test passes"}, VerifierIDs: []string{"fixture-go-test"},
		Policy: domain.TaskPolicy{HumanApprovalRequired: true, MergeMethod: "squash"}, SourceRevision: "fixture-v1",
	}
}

func argumentValue(args []string, name string) string {
	for index := 0; index < len(args)-1; index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}
