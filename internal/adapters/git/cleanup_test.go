package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupDeletesOnlyExactOwnedResources(t *testing.T) {
	root, origin, source, worktree, branch, candidate := cleanupFixture(t)
	_ = root
	cleanup := Cleanup{Workspace: Workspace{}, SourcePath: source, OriginPath: origin}
	if err := cleanup.RemoveWorktree(context.Background(), "owner/repo", worktree, branch, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if err := cleanup.RemoveWorktree(context.Background(), "owner/repo", worktree, branch, candidate); err != nil {
		t.Fatalf("absent worktree was not reconciled: %v", err)
	}
	if err := cleanup.DeleteRemoteBranch(context.Background(), "owner/repo", branch, candidate); err != nil {
		t.Fatal(err)
	}
	remote, err := (Workspace{}).run(context.Background(), source, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		t.Fatal(err)
	}
	if remote != "" {
		t.Fatalf("remote branch still exists: %q", remote)
	}
	if err := cleanup.DeleteLocalBranch(context.Background(), "owner/repo", branch, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Workspace{}).run(context.Background(), source, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}"); err == nil {
		t.Fatal("local branch still exists")
	}
}

func TestCleanupRefusesDirtyMovedAndUnexpectedResources(t *testing.T) {
	_, origin, source, worktree, branch, candidate := cleanupFixture(t)
	cleanup := Cleanup{Workspace: Workspace{}, SourcePath: source, OriginPath: origin}
	if err := os.WriteFile(filepath.Join(worktree, "untracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.RemoveWorktree(context.Background(), "owner/repo", worktree, branch, candidate); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty worktree error=%v", err)
	}
	if err := cleanup.DeleteRemoteBranch(context.Background(), "owner/repo", branch, "different"); err == nil || !strings.Contains(err.Error(), "matches") {
		t.Fatalf("unexpected remote SHA error=%v", err)
	}
	remote, err := (Workspace{}).run(context.Background(), source, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(remote, candidate) {
		t.Fatalf("remote was deleted despite mismatched SHA: %q", remote)
	}
	if err := cleanup.DeleteLocalBranch(context.Background(), "owner/repo", branch, candidate); err == nil || !strings.Contains(err.Error(), "attached") {
		t.Fatalf("attached local branch error=%v", err)
	}
}

func TestCleanupRefusesWorktreeWithUnexpectedHead(t *testing.T) {
	_, origin, source, worktree, branch, _ := cleanupFixture(t)
	cleanup := Cleanup{Workspace: Workspace{}, SourcePath: source, OriginPath: origin}
	if err := cleanup.RemoveWorktree(context.Background(), "owner/repo", worktree, branch, "different"); err == nil || !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("unexpected worktree head error=%v", err)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree was removed: %v", err)
	}
}

func cleanupFixture(t *testing.T) (root, origin, source, worktree, branch, candidate string) {
	t.Helper()
	root = t.TempDir()
	origin = filepath.Join(root, "origin.git")
	source = filepath.Join(root, "source")
	worktree = filepath.Join(root, "worktrees", "run-1")
	branch = "ifan/cleanup"
	runGit(t, root, "init", "--bare", origin)
	runGit(t, root, "init", "-b", "main", source)
	runGit(t, source, "config", "user.name", "Fixture")
	runGit(t, source, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "base.txt")
	runGit(t, source, "commit", "-m", "base")
	runGit(t, source, "remote", "add", "origin", origin)
	runGit(t, source, "push", "origin", "main")
	if _, err := (WorktreeManager{}).Provision(context.Background(), WorktreeRequest{SourcePath: source, OriginPath: origin, BaseBranch: "main", Branch: branch, Path: worktree, Nonce: "fixture-nonce"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "add", "candidate.txt")
	runGit(t, worktree, "commit", "-m", "candidate")
	candidate = stringOutput(t, worktree, "rev-parse", "HEAD")
	runGit(t, worktree, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	return root, origin, source, worktree, branch, candidate
}
