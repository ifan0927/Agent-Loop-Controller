package git

import (
	"context"
	"os"
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
		BaseBranch: "main", Branch: "ifan/test", Path: path})
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
		BaseBranch: "main", Branch: "ifan/test", Path: filepath.Join(root, "other")}); err == nil {
		t.Fatal("existing branch collision must be rejected")
	}
}
