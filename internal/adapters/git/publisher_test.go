package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildPushSpecUsesExplicitRefspecWithoutForce(t *testing.T) {
	spec, err := BuildPushSpec("ifan/ifan-42-safe")
	if err != nil {
		t.Fatal(err)
	}
	want := "refs/heads/ifan/ifan-42-safe:refs/heads/ifan/ifan-42-safe"
	if !slices.Contains(spec.Args, want) {
		t.Fatalf("missing explicit refspec: %v", spec.Args)
	}
	for _, arg := range spec.Args {
		if arg == "--force" || arg == "-f" {
			t.Fatalf("force option: %s", arg)
		}
	}
	if !slices.Contains(spec.Args, "--force-with-lease=refs/heads/ifan/ifan-42-safe:") {
		t.Fatalf("missing atomic empty-branch lease: %v", spec.Args)
	}
}

func TestBuildPushSpecBindsExpectedRemoteSHA(t *testing.T) {
	spec, err := BuildPushSpecExpected("ifan/ifan-42-safe", "oldsha")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Args, "--force-with-lease=refs/heads/ifan/ifan-42-safe:oldsha") {
		t.Fatalf("missing expected-old-SHA lease: %v", spec.Args)
	}
}

func TestExplicitPushToDisposableBareOrigin(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	runGit(t, root, "init", "--bare", origin)
	runGit(t, root, "init", "-b", "main", repo)
	runGit(t, repo, "config", "user.email", "controller@example.invalid")
	runGit(t, repo, "config", "user.name", "Agent Loop Controller")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "a.txt")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "remote", "add", "origin", origin)
	runGit(t, repo, "push", "origin", "main")
	runGit(t, repo, "checkout", "-b", "ifan/ifan-42-safe")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "b.txt")
	runGit(t, repo, "commit", "-m", "candidate")
	spec, _ := BuildPushSpec("ifan/ifan-42-safe")
	runGit(t, repo, spec.Args...)
	head := stringOutput(t, repo, "rev-parse", "HEAD")
	remote := stringOutput(t, repo, "ls-remote", "origin", "refs/heads/ifan/ifan-42-safe")
	if len(remote) < len(head) || remote[:len(head)] != head {
		t.Fatalf("remote=%q head=%q", remote, head)
	}
}

func TestPublisherRejectsNonFastForwardCandidateEvenWithMatchingLease(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	other := filepath.Join(root, "other")
	runGit(t, root, "init", "--bare", origin)
	runGit(t, root, "init", "-b", "main", repo)
	runGit(t, repo, "config", "user.email", "controller@example.invalid")
	runGit(t, repo, "config", "user.name", "Controller")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "base.txt")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "remote", "add", "origin", origin)
	runGit(t, repo, "push", "origin", "main")
	runGit(t, repo, "checkout", "-b", "ifan/ifan-42-safe")
	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "candidate.txt")
	runGit(t, repo, "commit", "-m", "candidate")
	candidate := stringOutput(t, repo, "rev-parse", "HEAD")

	runGit(t, root, "clone", origin, other)
	runGit(t, other, "config", "user.email", "external@example.invalid")
	runGit(t, other, "config", "user.name", "External")
	runGit(t, other, "checkout", "-b", "ifan/ifan-42-safe", "origin/main")
	if err := os.WriteFile(filepath.Join(other, "external.txt"), []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "add", "external.txt")
	runGit(t, other, "commit", "-m", "external")
	runGit(t, other, "push", "origin", "ifan/ifan-42-safe")
	expected := stringOutput(t, other, "rev-parse", "HEAD")

	_, err := (Publisher{Workspace: Workspace{}}).Push(context.Background(), repo, "ifan/ifan-42-safe", candidate, expected, root)
	if err == nil || !strings.Contains(err.Error(), "not a fast-forward") {
		t.Fatalf("push error=%v", err)
	}
	if remote := stringOutput(t, repo, "ls-remote", "origin", "refs/heads/ifan/ifan-42-safe"); !strings.HasPrefix(remote, expected) {
		t.Fatalf("remote=%q expected=%q", remote, expected)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func stringOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:len(out)-1])
}
