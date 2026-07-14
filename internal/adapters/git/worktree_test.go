package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorktreeManagerProvisionsRegisteredBaseAndRejectsCollision(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	path := filepath.Join(root, "worktrees", "run-1")
	runTestGit(t, root, "init", "--bare", origin)
	runTestGit(t, root, "init", "-b", "main", source)
	runTestGit(t, source, "config", "user.name", "Fixture")
	runTestGit(t, source, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module example.invalid/fixture\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "--all")
	runTestGit(t, source, "commit", "-m", "base")
	runTestGit(t, source, "remote", "add", "origin", origin)
	runTestGit(t, source, "push", "origin", "main")
	manager := WorktreeManager{}
	evidence, err := manager.Provision(context.Background(), WorktreeRequest{SourcePath: source, OriginPath: origin,
		BaseBranch: "main", Branch: "ifan/test", Path: path, Nonce: "test-nonce"})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.BaseSHA == "" {
		t.Fatal("missing base SHA")
	}
	if err := manager.ValidateOwned(context.Background(), evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Provision(context.Background(), WorktreeRequest{SourcePath: source, OriginPath: origin,
		BaseBranch: "main", Branch: "ifan/test", Path: filepath.Join(root, "other"), Nonce: "test-nonce"}); err == nil {
		t.Fatal("existing branch collision must be rejected")
	}
}

func TestWorktreeManagerRejectsMissingNonceBeforeMutation(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	path := filepath.Join(root, "worktree")
	runTestGit(t, root, "init", "--bare", origin)
	runTestGit(t, root, "init", "-b", "main", source)
	runTestGit(t, source, "config", "user.name", "Fixture")
	runTestGit(t, source, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "base.txt")
	runTestGit(t, source, "commit", "-m", "base")
	runTestGit(t, source, "remote", "add", "origin", origin)
	runTestGit(t, source, "push", "origin", "main")
	if _, err := (WorktreeManager{}).Provision(context.Background(), WorktreeRequest{SourcePath: source, OriginPath: origin, BaseBranch: "main", Branch: "ifan/missing-nonce", Path: path}); err == nil {
		t.Fatal("missing nonce was accepted")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree path was mutated: %v", err)
	}
	if _, err := (&Workspace{}).run(context.Background(), source, "show-ref", "--verify", "--quiet", "refs/heads/ifan/missing-nonce"); err == nil {
		t.Fatal("branch was created despite missing nonce")
	}
}

func TestWorktreeManagerBindsCanonicalGitHubRemoteIdentity(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	path := filepath.Join(root, "worktree")
	runTestGit(t, root, "init", "--bare", origin)
	runTestGit(t, root, "init", "-b", "main", source)
	runTestGit(t, source, "config", "user.name", "Fixture")
	runTestGit(t, source, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "base.txt")
	runTestGit(t, source, "commit", "-m", "base")
	runTestGit(t, source, "remote", "add", "origin", origin)
	runTestGit(t, source, "push", "origin", "main")

	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "git-wrapper")
	script := "#!/bin/sh\nif [ \"$1\" = remote ] && [ \"$2\" = get-url ] && [ \"$3\" = origin ]; then\n  printf '%s\\n' 'git@github.com:owner/repo.git'\n  exit 0\nfi\nexec \"" + gitBinary + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := WorktreeManager{Workspace: Workspace{Binary: wrapper}}
	evidence, err := manager.Provision(context.Background(), WorktreeRequest{SourcePath: source, OriginPath: "https://github.com/OWNER/REPO.git",
		BaseBranch: "main", Branch: "ifan/remote-binding", Path: path, Nonce: "remote-nonce"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ValidateOwned(context.Background(), evidence); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeManagerRejectsWrongOrUnsafeRemoteBeforeFetching(t *testing.T) {
	tests := []struct {
		name   string
		actual string
		wanted string
	}{
		{"different repository", "git@github.com:owner/other.git", "https://github.com/owner/repo.git"},
		{"unsafe HTTPS credential", "https://token@github.com/owner/repo.git", "git@github.com:owner/repo.git"},
		{"unsafe host", "git@github.example.invalid:owner/repo.git", "git@github.com:owner/repo.git"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if sameOriginBinding(test.actual, test.wanted) {
				t.Fatalf("unsafe binding was accepted: actual=%q expected=%q", test.actual, test.wanted)
			}
		})
	}
	if !sameOriginBinding("ssh://git@github.com/owner/repo.git", "https://github.com/OWNER/REPO.git") {
		t.Fatal("SSH and HTTPS bindings for the same canonical repository did not match")
	}
}
