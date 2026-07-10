package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceReportsIgnoredFilesAndValidatesRemoteBase(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	workspace := filepath.Join(root, "workspace")
	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", workspace)
	runTestGit(t, workspace, "config", "user.name", "Fixture")
	runTestGit(t, workspace, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, workspace, "add", "--all")
	runTestGit(t, workspace, "commit", "-m", "base")
	runTestGit(t, workspace, "remote", "add", "origin", remote)
	runTestGit(t, workspace, "push", "origin", "main")

	adapter := Workspace{}
	head, err := adapter.Head(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ValidateRemoteBase(context.Background(), workspace, "main", head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "ignored.txt"), []byte("affects build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := adapter.Status(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "!! ignored.txt") {
		t.Fatalf("ignored file missing from status: %q", status)
	}

	if err := os.Remove(filepath.Join(workspace, "ignored.txt")); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, workspace, "switch", "--orphan", "unrelated")
	if err := os.WriteFile(filepath.Join(workspace, "other.txt"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, workspace, "add", "--all")
	runTestGit(t, workspace, "commit", "-m", "unrelated")
	unrelated, err := adapter.Head(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ValidateRemoteBase(context.Background(), workspace, "main", unrelated); err == nil {
		t.Fatal("unrelated history must be rejected")
	}
}

func runTestGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
