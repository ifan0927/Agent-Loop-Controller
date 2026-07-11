package github

import (
	"slices"
	"strings"
	"testing"
)

func TestCreatePRUsesBodyStdinAndExactHeadBase(t *testing.T) {
	body := "untrusted $(touch /tmp/nope)\n\nFixes IFAN-42"
	command, err := CreatePRCommand("ifan/repo", "ifan/ifan-42-safe", "main", "Title", body)
	if err != nil {
		t.Fatal(err)
	}
	if command.Stdin != body || slices.Contains(command.Args, body) {
		t.Fatal("PR body must be stdin data, not an argument")
	}
	joined := strings.Join(command.Args, " ")
	if !strings.Contains(joined, "--head ifan/ifan-42-safe --base main") {
		t.Fatalf("wrong command: %v", command.Args)
	}
}

func TestSquashMergeIsOnlyMethodAndDoesNotDeleteBranch(t *testing.T) {
	command, err := SquashMergeCommand("ifan/repo", 7, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.Args, " ")
	if !strings.Contains(joined, "--squash --match-head-commit abc123") || strings.Contains(joined, "delete-branch") {
		t.Fatalf("unsafe merge command: %v", command.Args)
	}
}

func TestCleanupRejectsBaseBranch(t *testing.T) {
	if _, err := DeleteRemoteBranchCommand("main", "sha"); err == nil {
		t.Fatal("expected base branch rejection")
	}
}

func TestRemoteDeleteBindsExpectedOldSHA(t *testing.T) {
	command, err := DeleteRemoteBranchCommand("ifan/one", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.Args, " ")
	if !strings.Contains(joined, "--force-with-lease=refs/heads/ifan/one:abc123") {
		t.Fatalf("unsafe delete: %v", command.Args)
	}
}
