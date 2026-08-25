package git

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExistingCheckoutInspectorUsesReadOnlyExactRemoteEvidence(t *testing.T) {
	root, checkout, githubOrigin := newExistingCheckoutFixture(t)
	before := gitCheckoutSnapshot(t, checkout)
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	observed, err := (ExistingCheckoutInspector{Now: func() time.Time { return now }}).Inspect(context.Background(), ExistingCheckoutRequest{SourcePath: checkout, CanonicalRepository: "owner/repository", BaseBranch: "main", ForbiddenRoots: []string{filepath.Join(root, "controller")}})
	if err != nil {
		t.Fatal(err)
	}
	if observed.CanonicalPath != checkout || observed.Origin != githubOrigin || observed.HeadSHA != observed.RemoteHeadSHA || len(observed.EvidenceDigest) != 64 || !observed.ObservedAt.Equal(now) {
		t.Fatalf("observation=%+v", observed)
	}
	if after := gitCheckoutSnapshot(t, checkout); after != before {
		t.Fatalf("preflight mutated checkout:\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := (ExistingCheckoutInspector{}).Inspect(context.Background(), ExistingCheckoutRequest{SourcePath: checkout, CanonicalRepository: "owner/repository", BaseBranch: "main", ForbiddenRoots: []string{root}}); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("overlapping controller root error=%v", err)
	}
}

func TestExistingCheckoutInspectorRejectsUnsafeAndStaleTopology(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, string)
		path   func(*testing.T, string, string) string
		branch string
	}{
		{name: "dirty", mutate: func(t *testing.T, _ string, checkout string) {
			t.Helper()
			mustWriteGitFixture(t, filepath.Join(checkout, "untracked.txt"), "dirty\n")
		}},
		{name: "in progress", mutate: func(t *testing.T, _ string, checkout string) {
			t.Helper()
			mustWriteGitFixture(t, filepath.Join(checkout, ".git", "MERGE_HEAD"), strings.Repeat("a", 40)+"\n")
		}},
		{name: "wrong branch", branch: "release"},
		{name: "remote drift", mutate: func(t *testing.T, _ string, checkout string) {
			t.Helper()
			mustWriteGitFixture(t, filepath.Join(checkout, "README.md"), "new local head\n")
			runGitTest(t, "-C", checkout, "add", "README.md")
			runGitTest(t, "-C", checkout, "commit", "-m", "local drift")
		}},
		{name: "origin mismatch", mutate: func(t *testing.T, _ string, checkout string) {
			t.Helper()
			runGitTest(t, "-C", checkout, "config", "remote.origin.url", "https://github.com/owner/other.git")
		}},
		{name: "unsafe mode", mutate: func(t *testing.T, _ string, checkout string) {
			t.Helper()
			if err := os.Chmod(checkout, 0o770); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink component", path: func(t *testing.T, root, checkout string) string {
			t.Helper()
			linked := filepath.Join(root, "linked-checkout")
			if err := os.Symlink(checkout, linked); err != nil {
				t.Fatal(err)
			}
			return linked
		}},
		{name: "nested directory", mutate: func(t *testing.T, _ string, checkout string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(checkout, "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, path: func(_ *testing.T, _, checkout string) string { return filepath.Join(checkout, "nested") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, checkout, _ := newExistingCheckoutFixture(t)
			if test.mutate != nil {
				test.mutate(t, root, checkout)
			}
			path := checkout
			if test.path != nil {
				path = test.path(t, root, checkout)
			}
			branch := test.branch
			if branch == "" {
				branch = "main"
			}
			if _, err := (ExistingCheckoutInspector{}).Inspect(context.Background(), ExistingCheckoutRequest{SourcePath: path, CanonicalRepository: "owner/repository", BaseBranch: branch}); err == nil {
				t.Fatal("unsafe or stale checkout was accepted")
			}
		})
	}
}

func newExistingCheckoutFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin.git")
	checkout := filepath.Join(root, "checkout")
	runGitTest(t, "init", "--bare", origin)
	runGitTest(t, "init", "-b", "main", checkout)
	runGitTest(t, "-C", checkout, "config", "user.name", "Fixture")
	runGitTest(t, "-C", checkout, "config", "user.email", "fixture@example.invalid")
	mustWriteGitFixture(t, filepath.Join(checkout, "README.md"), "fixture\n")
	runGitTest(t, "-C", checkout, "add", "README.md")
	runGitTest(t, "-C", checkout, "commit", "-m", "fixture")
	githubOrigin := "https://github.com/owner/repository.git"
	runGitTest(t, "-C", checkout, "remote", "add", "origin", githubOrigin)
	runGitTest(t, "-C", checkout, "config", "url.file://"+origin+".insteadOf", githubOrigin)
	runGitTest(t, "-C", checkout, "push", "origin", "main")
	return root, checkout, githubOrigin
}

func mustWriteGitFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGitTest(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitCheckoutSnapshot(t *testing.T, checkout string) string {
	t.Helper()
	gitDir := runGitTest(t, "-C", checkout, "rev-parse", "--absolute-git-dir")
	var objects []string
	err := filepath.WalkDir(filepath.Join(gitDir, "objects"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			objects = append(objects, strings.TrimPrefix(path, gitDir))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	err = filepath.WalkDir(checkout, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, fmt.Sprintf("%s %s %x", strings.TrimPrefix(path, checkout), info.Mode(), sha256.Sum256(payload)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{
		runGitTest(t, "-C", checkout, "status", "--porcelain=v1", "--untracked-files=all"),
		runGitTest(t, "-C", checkout, "for-each-ref", "--format=%(refname)%00%(objectname)", "refs/heads", "refs/remotes"),
		runGitTest(t, "-C", checkout, "ls-files", "--stage"),
		runGitTest(t, "-C", checkout, "config", "--get", "remote.origin.url"),
		strings.Join(objects, "\n"),
		strings.Join(files, "\n"),
	}, "\n---\n")
}
