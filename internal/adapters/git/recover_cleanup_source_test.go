package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

func TestCleanupSourceRecoveryRepairsMovedSourceAndReconcilesExactLocalResources(t *testing.T) {
	request := cleanupSourceRelocationFixture(t)
	adapter := CleanupSourceRecovery{}
	refsBefore := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref")
	before, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if before.LinkRepaired || before.HeadDetached || !before.WorktreePresent || before.BranchPresent || !before.WorktreeClean {
		t.Fatalf("before=%+v", before)
	}
	request.ExpectedRegistrationDigest = before.RegistrationDigest
	if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	after, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LinkRepaired || after.HeadDetached || after.RegistrationDigest != before.RegistrationDigest || after.ReplacementIdentityDigest != before.ReplacementIdentityDigest {
		t.Fatalf("after=%+v", after)
	}
	if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
		t.Fatalf("repair replay: %v", err)
	}
	if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err != nil {
		t.Fatalf("detach replay: %v", err)
	}
	if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err != nil {
		t.Fatalf("remove replay: %v", err)
	}
	partial, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request)
	if err != nil || partial.WorktreePresent || partial.BranchPresent {
		t.Fatalf("partial=%+v err=%v", partial, err)
	}
	settled, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request)
	if err != nil || settled.WorktreePresent || settled.BranchPresent || settled.RegistrationDigest != before.RegistrationDigest {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if refsAfter := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref"); refsAfter != refsBefore {
		t.Fatal("cleanup source recovery mutated a branch ref")
	}
}

func TestCleanupSourceRecoveryDisablesLazyFetchAndReplaceObjectsForEveryGitInvocation(t *testing.T) {
	request := cleanupSourceRelocationFixture(t)
	runner := &cleanupRecoveryRecordingRunner{}
	adapter := CleanupSourceRecovery{Workspace: Workspace{Process: runner}}
	if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(runner.specs) == 0 {
		t.Fatal("recovery executed no Git commands")
	}
	for _, spec := range runner.specs {
		if !slices.Contains(spec.Environment, "GIT_NO_LAZY_FETCH=1") || !slices.Contains(spec.Environment, "GIT_NO_REPLACE_OBJECTS=1") {
			t.Fatalf("recovery Git environment missing local-only controls: %v", spec.Environment)
		}
	}
}

type cleanupRecoveryRecordingRunner struct{ specs []processadapter.Spec }

func (r *cleanupRecoveryRecordingRunner) Run(ctx context.Context, spec processadapter.Spec) (processadapter.Result, error) {
	r.specs = append(r.specs, spec)
	return (processadapter.OSRunner{}).Run(ctx, spec)
}

func TestCleanupSourceRecoveryRepairedObservationDoesNotRefreshIndex(t *testing.T) {
	request := cleanupSourceRelocationFixture(t)
	adapter := CleanupSourceRecovery{}
	if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(request.WorktreePath, "candidate.txt")
	trackedInfo, err := os.Stat(tracked)
	if err != nil {
		t.Fatal(err)
	}
	changed := trackedInfo.ModTime().Add(-2 * time.Hour)
	if err := os.Chtimes(tracked, changed, changed); err != nil {
		t.Fatal(err)
	}
	indexPath := strings.TrimSpace(runGitTestOutput(t, "-C", request.WorktreePath, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	indexInfoBefore, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request)
	if err != nil || !observation.LinkRepaired || !observation.WorktreeClean {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	indexInfoAfter, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !indexInfoAfter.ModTime().Equal(indexInfoBefore.ModTime()) || string(indexAfter) != string(indexBefore) {
		t.Fatal("read-only recovery observation refreshed the worktree index")
	}
}

func TestCleanupSourceRecoveryRepairForcesAbsoluteLinksWithoutRepositoryConfigurationMutation(t *testing.T) {
	request := cleanupSourceRelocationFixture(t)
	adapter := CleanupSourceRecovery{}
	runGitTest(t, "-C", request.ReplacementSourcePath, "config", "worktree.useRelativePaths", "true")
	config := filepath.Join(request.ReplacementSourcePath, ".git", "config")
	configBefore, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	refsBefore := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref")
	if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	configAfter, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Fatal("worktree repair changed common repository configuration")
	}
	if refsAfter := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref"); refsAfter != refsBefore {
		t.Fatal("worktree repair changed refs")
	}
	worktreeLink, err := os.ReadFile(filepath.Join(request.WorktreePath, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if target := strings.TrimSpace(strings.TrimPrefix(string(worktreeLink), "gitdir: ")); !filepath.IsAbs(target) || !strings.HasPrefix(target, filepath.Join(request.ReplacementSourcePath, ".git", "worktrees")+string(filepath.Separator)) {
		t.Fatalf("worktree link is not an absolute replacement-admin path: %q", target)
	}
	admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
	adminLink, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		t.Fatal(err)
	}
	if target := strings.TrimSpace(string(adminLink)); !filepath.IsAbs(target) || target != filepath.Join(request.WorktreePath, ".git") {
		t.Fatalf("admin backlink is not exact and absolute: %q", target)
	}
}

func TestCleanupSourceRecoveryDoesNotExecuteRepositoryHooks(t *testing.T) {
	request := cleanupSourceRelocationFixture(t)
	adapter := CleanupSourceRecovery{}
	if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(filepath.Dir(request.ReplacementSourcePath), "reference-transaction-ran")
	hook := filepath.Join(request.ReplacementSourcePath, ".git", "hooks", "reference-transaction")
	content := "#!/bin/sh\nprintf ran > '" + sentinel + "'\n"
	if err := os.WriteFile(hook, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery executed repository hook: %v", err)
	}
}

func TestCleanupSourceRecoveryRejectsExternalFiltersWithoutExecutingThem(t *testing.T) {
	for _, included := range []bool{false, true} {
		name := "local"
		if included {
			name = "included"
		}
		t.Run(name, func(t *testing.T) {
			request := cleanupSourceRelocationFixture(t)
			adapter := CleanupSourceRecovery{}
			sentinel := filepath.Join(filepath.Dir(request.ReplacementSourcePath), "filter-ran")
			script := filepath.Join(filepath.Dir(request.ReplacementSourcePath), "filter-driver")
			content := "#!/bin/sh\nprintf ran > '" + sentinel + "'\ncat\n"
			if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
				t.Fatal(err)
			}
			if included {
				include := filepath.Join(filepath.Dir(request.ReplacementSourcePath), "included-config")
				if err := os.WriteFile(include, []byte("[filter \"evil\"]\n\tclean = "+script+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGitTest(t, "-C", request.ReplacementSourcePath, "config", "--add", "include.path", include)
			} else {
				runGitTest(t, "-C", request.ReplacementSourcePath, "config", "filter.evil.clean", script)
			}
			mustWriteGitFixture(t, filepath.Join(request.WorktreePath, ".gitattributes"), "*.txt filter=evil\n")
			if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
				t.Fatal("external filter configuration was accepted")
			}
			if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err == nil {
				t.Fatal("external filter configuration reached repair")
			}
			if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
				t.Fatal("external filter configuration reached detach")
			}
			if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err == nil {
				t.Fatal("external filter configuration reached removal")
			}
			if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery executed external filter: %v", err)
			}
		})
	}
}

func TestCleanupSourceRecoveryRejectsGitlinkBeforeStatusOrDiff(t *testing.T) {
	request := cleanupSourceRelocationFixture(t)
	admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
	runGitTest(t, "--git-dir="+admin, "--work-tree="+request.WorktreePath, "update-index", "--add", "--cacheinfo", "160000,"+request.CandidateHead+",nested")
	runner := &cleanupRecoveryRecordingRunner{}
	adapter := CleanupSourceRecovery{Workspace: Workspace{Process: runner}}
	if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
		t.Fatal("gitlink index entry was accepted")
	}
	for _, spec := range runner.specs {
		if slices.Contains(spec.Args, "status") || slices.Contains(spec.Args, "diff-index") {
			t.Fatalf("gitlink rejection reached recursive clean proof: %v", spec.Args)
		}
	}
}

func TestCleanupSourceRecoveryRejectsPerWorktreeOperationStateWithoutMutation(t *testing.T) {
	states := []struct {
		name string
		make func(*testing.T, string, string)
	}{
		{"AUTO_MERGE", writeRecoveryStateFile},
		{"CHERRY_PICK_HEAD", writeRecoveryStateFile},
		{"MERGE_AUTOSTASH", writeRecoveryStateFile},
		{"MERGE_HEAD", writeRecoveryStateFile},
		{"MERGE_MODE", writeRecoveryStateFile},
		{"MERGE_MSG", writeRecoveryStateFile},
		{"REVERT_HEAD", writeRecoveryStateFile},
		{"SQUASH_MSG", writeRecoveryStateFile},
		{"BISECT_LOG", writeRecoveryStateFile},
		{"unexpected-state", writeRecoveryStateFile},
		{"sequencer", makeRecoveryStateDirectory},
		{"rebase-apply", makeRecoveryStateDirectory},
		{"rebase-merge", makeRecoveryStateDirectory},
		{"refs/bisect", makeRecoveryStateDirectory},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			request := cleanupSourceRelocationFixture(t)
			adapter := CleanupSourceRecovery{}
			if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
			statePath := filepath.Join(admin, filepath.FromSlash(state.name))
			state.make(t, statePath, request.CandidateHead)
			sentinelPath := statePath
			if info, err := os.Stat(statePath); err != nil {
				t.Fatal(err)
			} else if info.IsDir() {
				sentinelPath = filepath.Join(statePath, "sentinel")
			}
			stateBefore, err := os.ReadFile(sentinelPath)
			if err != nil {
				t.Fatal(err)
			}
			refsBefore := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref")
			headBefore, err := os.ReadFile(filepath.Join(admin, "HEAD"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
				t.Fatal("operation state was accepted")
			}
			if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
				t.Fatal("operation state was detached")
			}
			if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err == nil {
				t.Fatal("operation state worktree was removed")
			}
			if _, err := os.Stat(request.WorktreePath); err != nil {
				t.Fatalf("operation state worktree changed: %v", err)
			}
			headAfter, err := os.ReadFile(filepath.Join(admin, "HEAD"))
			if err != nil || !bytes.Equal(headBefore, headAfter) {
				t.Fatalf("operation state HEAD changed: %v", err)
			}
			if refsAfter := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref"); refsAfter != refsBefore {
				t.Fatal("operation state rejection changed refs")
			}
			stateAfter, err := os.ReadFile(sentinelPath)
			if err != nil || !bytes.Equal(stateBefore, stateAfter) {
				t.Fatalf("operation state changed: %v", err)
			}
		})
	}
}

func writeRecoveryStateFile(t *testing.T, path, candidate string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(candidate+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeRecoveryStateDirectory(t *testing.T, path, candidate string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRecoveryStateFile(t, filepath.Join(path, "sentinel"), candidate)
}

func TestCleanupSourceRecoveryFailsClosedOnDirtyDriftAndHalfRepair(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(request.WorktreePath, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("dirty worktree was accepted")
		}
		if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err == nil || !strings.Contains(err.Error(), "precondition") {
			t.Fatalf("dirty removal err=%v", err)
		}
	})
	t.Run("head drift", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		request.CandidateHead = strings.Repeat("f", 40)
		if _, err := (CleanupSourceRecovery{}).ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("head drift accepted")
		}
	})
	t.Run("replacement path drift", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		request.ReplacementSourcePath = filepath.Dir(request.ReplacementSourcePath)
		if _, err := (CleanupSourceRecovery{}).ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("replacement drift accepted")
		}
	})
	t.Run("half repaired link", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		if err := os.WriteFile(filepath.Join(request.WorktreePath, ".git"), []byte("gitdir: "+filepath.Join(request.ReplacementSourcePath, ".git", "worktrees", "wrong")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (CleanupSourceRecovery{}).ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("half repair accepted")
		}
	})
	t.Run("candidate branch recreated", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		runGitTest(t, "-C", request.ReplacementSourcePath, "branch", request.Branch, request.CandidateHead)
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
			t.Fatal("recreated candidate branch was accepted")
		}
		if head := strings.TrimSpace(runGitTestOutput(t, "-C", request.ReplacementSourcePath, "rev-parse", "refs/heads/"+request.Branch)); head != request.CandidateHead {
			t.Fatalf("candidate ref changed: %s", head)
		}
	})
	t.Run("candidate branch attached after detach", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		runGitTest(t, "-C", request.ReplacementSourcePath, "branch", request.Branch, request.CandidateHead)
		runGitTest(t, "-C", request.ReplacementSourcePath, "switch", request.Branch)
		if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err == nil {
			t.Fatal("attached candidate branch was accepted")
		}
		if head := strings.TrimSpace(runGitTestOutput(t, "-C", request.ReplacementSourcePath, "rev-parse", "refs/heads/"+request.Branch)); head != request.CandidateHead {
			t.Fatalf("attached candidate ref changed: %s", head)
		}
	})
	t.Run("candidate object missing", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		request.CandidateHead = strings.Repeat("f", 40)
		if _, err := (CleanupSourceRecovery{}).ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("missing candidate object was accepted")
		}
	})
	t.Run("replace ref cannot substitute missing candidate", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		main := strings.TrimSpace(runGitTestOutput(t, "-C", request.ReplacementSourcePath, "rev-parse", "refs/heads/main"))
		runGitTest(t, "-C", request.ReplacementSourcePath, "replace", request.CandidateHead, main)
		refsBefore := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref")
		object := filepath.Join(request.ReplacementSourcePath, ".git", "objects", request.CandidateHead[:2], request.CandidateHead[2:])
		if err := os.Remove(object); err != nil {
			t.Fatal(err)
		}
		if _, err := (CleanupSourceRecovery{}).ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("replace ref substituted for the missing persisted candidate")
		}
		if refsAfter := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref"); refsAfter != refsBefore {
			t.Fatal("failed replacement-object observation mutated refs")
		}
	})
	t.Run("staged index drift", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		mustWriteGitFixture(t, filepath.Join(request.WorktreePath, "candidate.txt"), "changed\n")
		runGitTest(t, "-C", request.WorktreePath, "add", "candidate.txt")
		if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("staged index drift was accepted")
		}
	})
	for _, test := range []struct {
		name, flag string
		remove     bool
	}{
		{"assume unchanged modified", "--assume-unchanged", false},
		{"skip worktree modified", "--skip-worktree", false},
		{"skip worktree missing", "--skip-worktree", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := cleanupSourceRelocationFixture(t)
			adapter := CleanupSourceRecovery{}
			if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			refsBefore := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref")
			tracked := filepath.Join(request.WorktreePath, "candidate.txt")
			runGitTest(t, "-C", request.WorktreePath, "update-index", test.flag, "candidate.txt")
			if test.remove {
				if err := os.Remove(tracked); err != nil {
					t.Fatal(err)
				}
			} else {
				mustWriteGitFixture(t, tracked, "masked change\n")
			}
			if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
				t.Fatal("index-masked worktree drift was accepted")
			}
			if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err == nil {
				t.Fatal("index-masked worktree was removed")
			}
			if refsAfter := runGitTestOutput(t, "-C", request.ReplacementSourcePath, "show-ref"); refsAfter != refsBefore {
				t.Fatal("index-mask rejection mutated refs")
			}
		})
	}
	t.Run("ignored file drift", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		common := strings.TrimSpace(runGitTestOutput(t, "-C", request.ReplacementSourcePath, "rev-parse", "--path-format=absolute", "--git-common-dir"))
		mustWriteGitFixture(t, filepath.Join(common, "info", "exclude"), "ignored.txt\n")
		mustWriteGitFixture(t, filepath.Join(request.WorktreePath, "ignored.txt"), "ignored\n")
		if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("ignored worktree drift was accepted")
		}
	})
	t.Run("ambiguous owned HEAD", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		admin := strings.TrimSpace(runGitTestOutput(t, "-C", request.WorktreePath, "rev-parse", "--path-format=absolute", "--absolute-git-dir"))
		mustWriteGitFixture(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/main\n")
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
			t.Fatal("ambiguous owned HEAD was detached")
		}
		if _, err := execGitTest("-C", request.ReplacementSourcePath, "show-ref", "--verify", "--quiet", "refs/heads/"+request.Branch); !isExitCode(err, 1) {
			t.Fatalf("candidate ref was unexpectedly created: %v", err)
		}
	})
	t.Run("held index lock", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		admin := strings.TrimSpace(runGitTestOutput(t, "-C", request.WorktreePath, "rev-parse", "--path-format=absolute", "--absolute-git-dir"))
		lock, err := os.OpenFile(filepath.Join(admin, "index.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
			t.Fatal("detach ignored concurrent index ownership")
		}
		head, err := os.ReadFile(filepath.Join(admin, "HEAD"))
		if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/"+request.Branch {
			t.Fatalf("HEAD changed while index was locked: %q err=%v", head, err)
		}
	})
	t.Run("symlink Git authority leaves", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		link := filepath.Join(request.WorktreePath, ".git")
		real := link + ".real"
		if err := os.Rename(link, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, err := (CleanupSourceRecovery{}).ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("symlink worktree Git leaf was accepted")
		}
	})
	t.Run("symlink replacement Git directory", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		gitDir := filepath.Join(request.ReplacementSourcePath, ".git")
		real := gitDir + ".real"
		if err := os.Rename(gitDir, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, gitDir); err != nil {
			t.Fatal(err)
		}
		if _, err := (CleanupSourceRecovery{}).ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("symlink replacement Git directory was accepted")
		}
	})
	for _, leaf := range []string{"HEAD", "index", "ORIG_HEAD"} {
		t.Run("symlink admin "+leaf, func(t *testing.T) {
			request := cleanupSourceRelocationFixture(t)
			adapter := CleanupSourceRecovery{}
			if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			admin := strings.TrimSpace(runGitTestOutput(t, "-C", request.WorktreePath, "rev-parse", "--path-format=absolute", "--absolute-git-dir"))
			path := filepath.Join(admin, leaf)
			real := path + ".real"
			if err := os.Rename(path, real); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(real)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, path); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
				t.Fatalf("symlink admin %s was accepted", leaf)
			}
			after, err := os.ReadFile(real)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("outside %s target changed: %v", leaf, err)
			}
		})
	}
	t.Run("ambiguous ORIG_HEAD", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
		mustWriteGitFixture(t, filepath.Join(admin, "ORIG_HEAD"), strings.Repeat("a", 40)+"\n")
		if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("ambiguous ORIG_HEAD was accepted")
		}
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
			t.Fatal("ambiguous ORIG_HEAD was detached")
		}
	})
	for _, leaf := range []string{"ORIG_HEAD", "COMMIT_EDITMSG"} {
		t.Run("persisted admin digest drift "+leaf, func(t *testing.T) {
			request := cleanupSourceRelocationFixture(t)
			adapter := CleanupSourceRecovery{}
			before, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedRegistrationDigest = before.RegistrationDigest
			admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
			value := "changed admin evidence\n"
			if leaf == "ORIG_HEAD" {
				value = request.CandidateHead + "\n"
			}
			mustWriteGitFixture(t, filepath.Join(admin, leaf), value)
			if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
				t.Fatalf("%s-only drift preserved registration authority", leaf)
			}
			if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err == nil {
				t.Fatalf("%s-only drift reached repair", leaf)
			}
		})
	}
	for _, leaf := range []string{"gitdir", "commondir"} {
		t.Run("symlink admin backlink "+leaf, func(t *testing.T) {
			request := cleanupSourceRelocationFixture(t)
			adapter := CleanupSourceRecovery{}
			admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
			path := filepath.Join(admin, leaf)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(filepath.Dir(request.ReplacementSourcePath), "outside-"+leaf)
			if err := os.WriteFile(sentinel, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sentinel, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(sentinel)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
				t.Fatalf("symlink admin %s backlink was accepted", leaf)
			}
			if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err == nil {
				t.Fatalf("repair followed symlink admin %s backlink", leaf)
			}
			after, err := os.ReadFile(sentinel)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("outside %s sentinel changed: %v", leaf, err)
			}
		})
	}
	t.Run("symlink admin HEAD reflog", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
		reflog := filepath.Join(admin, "logs", "HEAD")
		original, err := os.ReadFile(reflog)
		if err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(filepath.Dir(request.ReplacementSourcePath), "outside-reflog")
		if err := os.WriteFile(sentinel, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(reflog); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sentinel, reflog); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("symlink worktree HEAD reflog was accepted")
		}
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
			t.Fatal("detach followed symlink worktree HEAD reflog")
		}
		after, err := os.ReadFile(sentinel)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("outside reflog sentinel changed: %v", err)
		}
	})
	t.Run("symlink admin reflog directory", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		admin := cleanupRecoveryAdminPath(t, request.ReplacementSourcePath)
		logs := filepath.Join(admin, "logs")
		real := logs + ".real"
		if err := os.Rename(logs, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, logs); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(real, "HEAD"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("symlink worktree reflog directory was accepted")
		}
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err == nil {
			t.Fatal("detach followed symlink worktree reflog directory")
		}
		after, err := os.ReadFile(filepath.Join(real, "HEAD"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("outside reflog directory changed: %v", err)
		}
	})
	t.Run("missing candidate after removal", func(t *testing.T) {
		request := cleanupSourceRelocationFixture(t)
		adapter := CleanupSourceRecovery{}
		if err := adapter.RepairCleanupWorktreeLink(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if err := adapter.DetachRecoveredWorktreeHead(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if err := adapter.RemoveRecoveredWorktree(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		object := filepath.Join(request.ReplacementSourcePath, ".git", "objects", request.CandidateHead[:2], request.CandidateHead[2:])
		if err := os.Remove(object); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.ObserveCleanupSourceRecovery(context.Background(), request); err == nil {
			t.Fatal("missing candidate after removal was accepted")
		}
	})
}

func cleanupRecoveryAdminPath(t *testing.T, replacement string) string {
	t.Helper()
	common := strings.TrimSpace(runGitTestOutput(t, "-C", replacement, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("recovery admin entries=%d err=%v", len(entries), err)
	}
	return filepath.Join(common, "worktrees", entries[0].Name())
}

func isExitCode(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func cleanupSourceRelocationFixture(t *testing.T) CleanupSourceRecoveryRequest {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldSource := filepath.Join(root, "old-source")
	replacement := filepath.Join(root, "replacement-source")
	worktree := filepath.Join(root, "owned-worktree")
	runGitTest(t, "init", "-b", "main", oldSource)
	runGitTest(t, "-C", oldSource, "config", "user.name", "Fixture")
	runGitTest(t, "-C", oldSource, "config", "user.email", "fixture@example.invalid")
	mustWriteGitFixture(t, filepath.Join(oldSource, "README.md"), "fixture\n")
	runGitTest(t, "-C", oldSource, "add", "README.md")
	runGitTest(t, "-C", oldSource, "commit", "-m", "fixture")
	origin := "https://github.com/owner/repository.git"
	runGitTest(t, "-C", oldSource, "remote", "add", "origin", origin)
	runGitTest(t, "-C", oldSource, "worktree", "add", "-b", "codex/recovery", worktree)
	mustWriteGitFixture(t, filepath.Join(worktree, "candidate.txt"), "candidate\n")
	runGitTest(t, "-C", worktree, "add", "candidate.txt")
	runGitTest(t, "-C", worktree, "commit", "-m", "candidate")
	candidate := strings.TrimSpace(runGitTestOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	runGitTest(t, "-C", oldSource, "update-ref", "-d", "refs/heads/codex/recovery", candidate)
	if err := os.Rename(oldSource, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old source still exists: %v", err)
	}
	return CleanupSourceRecoveryRequest{Repository: "owner/repository", FrozenSourcePath: oldSource, ReplacementSourcePath: replacement, ExpectedOrigin: origin, WorktreePath: worktree, Branch: "codex/recovery", CandidateHead: candidate}
}

func runGitTestOutput(t *testing.T, args ...string) string {
	t.Helper()
	output, err := execGitTest(args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return output
}

func execGitTest(args ...string) (string, error) {
	command := exec.Command("git", args...)
	output, err := command.CombinedOutput()
	return string(output), err
}
