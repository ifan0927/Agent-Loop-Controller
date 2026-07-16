package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	for _, resource := range []struct{ kind, name string }{{"worktree", worktree}, {"branch", branch}, {"remote_branch", branch}} {
		absent, err := cleanup.CleanupResourceAbsent(context.Background(), "owner/repo", resource.kind, resource.name)
		if err != nil || !absent {
			t.Fatalf("reconcile %s absent=%v err=%v", resource.kind, absent, err)
		}
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

func TestCleanupRequiresWorktreeRemovalPostcondition(t *testing.T) {
	root, origin, source, worktree, branch, candidate := cleanupFixture(t)
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "git-noop-worktree-remove")
	script := "#!/bin/sh\nwhile [ \"$1\" = -c ]; do shift 2; done\nif [ \"$1\" = worktree ] && [ \"$2\" = remove ]; then exit 0; fi\nexec \"" + gitBinary + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cleanup := Cleanup{Workspace: Workspace{Binary: wrapper}, SourcePath: source, OriginPath: origin}
	if err := cleanup.RemoveWorktree(context.Background(), "owner/repo", worktree, branch, candidate); err == nil || !strings.Contains(err.Error(), "postcondition") {
		t.Fatalf("false successful worktree removal err=%v", err)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("fixture worktree unexpectedly disappeared: %v", err)
	}
}

func TestCleanupAbsenceReconciliationRetainsRegisteredMissingWorktree(t *testing.T) {
	_, origin, source, worktree, _, _ := cleanupFixture(t)
	cleanup := Cleanup{Workspace: Workspace{}, SourcePath: source, OriginPath: origin}
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	absent, err := cleanup.CleanupResourceAbsent(context.Background(), "owner/repo", "worktree", worktree)
	if err != nil {
		t.Fatal(err)
	}
	if absent {
		t.Fatal("registered worktree was treated as fully absent")
	}
	if err := cleanup.RemoveWorktree(context.Background(), "owner/repo", worktree, "ifan/cleanup", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "remains registered") {
		t.Fatalf("registered missing worktree removal err=%v", err)
	}
}

func TestCleanupAbsenceReconciliationDoesNotHideCorruptLocalRef(t *testing.T) {
	_, origin, source, worktree, branch, candidate := cleanupFixture(t)
	cleanup := Cleanup{Workspace: Workspace{}, SourcePath: source, OriginPath: origin}
	if err := cleanup.RemoveWorktree(context.Background(), "owner/repo", worktree, branch, candidate); err != nil {
		t.Fatal(err)
	}
	refPath := filepath.Join(source, ".git", "refs", "heads", branch)
	if err := os.WriteFile(refPath, []byte(strings.Repeat("f", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent, err := cleanup.CleanupResourceAbsent(context.Background(), "owner/repo", "branch", branch)
	if err == nil || absent {
		t.Fatalf("corrupt ref absence=%v err=%v", absent, err)
	}
	if err := cleanup.DeleteLocalBranch(context.Background(), "owner/repo", branch, candidate); err == nil {
		t.Fatal("corrupt ref was treated as an already deleted branch")
	}
}

func TestCleanupBindsCanonicalGitHubRemoteIdentity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "git-wrapper")
	script := "#!/bin/sh\nwhile [ \"$1\" = -c ]; do shift 2; done\nif [ \"$1\" = remote ] && [ \"$2\" = get-url ] && [ \"$3\" = origin ]; then\n  printf '%s\\n' 'git@github.com:owner/repo.git'\n  exit 0\nfi\nexec \"" + gitBinary + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	cleanup := Cleanup{Workspace: Workspace{Binary: wrapper}, SourcePath: source, OriginPath: "https://github.com/OWNER/REPO.git"}
	if err := cleanup.validateRepository("owner/repo"); err != nil {
		t.Fatalf("canonical GitHub binding rejected: %v", err)
	}
	if err := cleanup.validateSourceOrigin(context.Background()); err != nil {
		t.Fatalf("equivalent checkout transport rejected: %v", err)
	}

	cleanup.OriginPath = "https://github.com/owner/other.git"
	if err := cleanup.validateSourceOrigin(context.Background()); err == nil || !strings.Contains(err.Error(), "ownership mismatch") {
		t.Fatalf("mismatched GitHub remote accepted: %v", err)
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
