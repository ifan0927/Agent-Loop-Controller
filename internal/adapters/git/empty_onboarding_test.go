package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEmptyRepositoryGitCreatesDeterministicRootAndPublishesOnce(t *testing.T) {
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(temporary, "fixture")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin.git")
	runGitCommand(t, "init", "--bare", origin)
	repositoryRoot := filepath.Join(root, "managed")
	if err := os.Mkdir(repositoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repositoryRoot, "source")
	git := EmptyRepositoryGit{remoteResolver: func(repository string) (string, error) {
		if repository != "owner/repository" {
			t.Fatalf("repository=%q", repository)
		}
		return origin, nil
	}}
	empty, err := git.ObserveRemoteRefs(context.Background(), root, "owner/repository")
	if err != nil || len(empty.Refs) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	if _, err := git.EnsureManagedSource(context.Background(), repositoryRoot, source, "owner/repository", "main"); err != nil {
		t.Fatal(err)
	}
	accepted := time.Date(2026, 8, 25, 4, 5, 6, 987654321, time.UTC)
	sha, evidence, err := git.EnsureInitialRevision(context.Background(), source, "owner/repository", "main", accepted)
	if err != nil || len(sha) != 40 || len(evidence) != 64 {
		t.Fatalf("sha=%q evidence=%q err=%v", sha, evidence, err)
	}
	replayedSHA, replayedEvidence, err := git.EnsureInitialRevision(context.Background(), source, "owner/repository", "main", accepted)
	if err != nil || replayedSHA != sha || replayedEvidence != evidence {
		t.Fatalf("replayed sha=%q evidence=%q err=%v", replayedSHA, replayedEvidence, err)
	}
	published, err := git.PublishInitialBase(context.Background(), source, "owner/repository", "main", sha)
	if err != nil || len(published.Refs) != 1 || published.Refs["refs/heads/main"] != sha {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	replayed, err := git.PublishInitialBase(context.Background(), source, "owner/repository", "main", sha)
	if err != nil || replayed.Refs["refs/heads/main"] != sha {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	if _, err := git.SettlePublishedSource(context.Background(), source, "owner/repository", "main", sha); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	metadata := runGitOutput(t, "-C", source, "show", "-s", "--format=%P|%T|%an|%ae|%at|%cn|%ce|%ct|%s", sha)
	want := "|" + emptyTreeSHA1 + "|" + managedGitAuthorName + "|" + managedGitAuthorEmail + "|1787630706|" + managedGitAuthorName + "|" + managedGitAuthorEmail + "|1787630706|" + initialCommitMessage
	if strings.TrimSpace(metadata) != want {
		t.Fatalf("metadata=%q want=%q", metadata, want)
	}
	runGitCommand(t, "--git-dir", origin, "update-ref", "refs/tags/unexpected", sha)
	diverged, err := git.PublishInitialBase(context.Background(), source, "owner/repository", "main", sha)
	if err == nil || len(diverged.Refs) != 2 || diverged.Refs["refs/heads/main"] != sha || diverged.Refs["refs/tags/unexpected"] != sha {
		t.Fatalf("diverged=%+v err=%v", diverged, err)
	}
}

func runGitCommand(t *testing.T, args ...string) {
	t.Helper()
	if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func runGitOutput(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output)
}
