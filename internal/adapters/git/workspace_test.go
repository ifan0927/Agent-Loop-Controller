package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
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

func TestWorkspaceUsesManagedRuntimeAndFixedIdentity(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	workspace := filepath.Join(root, "workspace")
	logPath := filepath.Join(root, "runtime.log")
	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", workspace)
	runTestGit(t, workspace, "config", "user.name", "Ambient Local Identity")
	runTestGit(t, workspace, "config", "user.email", "ambient@example.invalid")
	if err := os.WriteFile(filepath.Join(workspace, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, workspace, "add", "--all")
	runTestGit(t, workspace, "commit", "-m", "base")
	runTestGit(t, workspace, "remote", "add", "origin", remote)
	runTestGit(t, workspace, "push", "origin", "main")

	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "git-wrapper")
	script := fmt.Sprintf("#!/bin/sh\nprintf 'path=%%s\\n' \"$PATH\" >> %q\nprintf 'linear=%%s\\n' \"${IFAN_LOOP_LINEAR_TOKEN:+present}\" >> %q\nprintf 'app_key=%%s\\n' \"${GITHUB_APP_PRIVATE_KEY:+present}\" >> %q\nprintf 'agent=%%s\\n' \"${SSH_AUTH_SOCK:+present}\" >> %q\nprintf 'config_count=%%s\\n' \"${GIT_CONFIG_COUNT:+present}\" >> %q\nprintf 'dyld=%%s\\n' \"${DYLD_INSERT_LIBRARIES:+present}\" >> %q\nprintf 'author=%%s\\n' \"$GIT_AUTHOR_NAME\" >> %q\nprintf 'author_email=%%s\\n' \"$GIT_AUTHOR_EMAIL\" >> %q\nprintf 'config_global=%%s\\n' \"$GIT_CONFIG_GLOBAL\" >> %q\nprintf 'config_nosystem=%%s\\n' \"$GIT_CONFIG_NOSYSTEM\" >> %q\nprintf 'terminal=%%s\\n' \"$GIT_TERMINAL_PROMPT\" >> %q\nprintf 'argv=%%s\\n' \"$*\" >> %q\nexec %q \"$@\"\n", logPath, logPath, logPath, logPath, logPath, logPath, logPath, logPath, logPath, logPath, logPath, logPath, gitBinary)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IFAN_LOOP_LINEAR_TOKEN", "linear-secret")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "pem-secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "config-secret")
	t.Setenv("DYLD_INSERT_LIBRARIES", "inject-secret")
	t.Setenv("RETAINED_SECRET", "must-not-enter")
	t.Setenv("GIT_AUTHOR_NAME", "ambient-author")
	t.Setenv("GIT_AUTHOR_EMAIL", "ambient-author@example.invalid")
	if err := os.WriteFile(filepath.Join(workspace, "candidate.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := Workspace{Binary: wrapper}
	candidate, err := adapter.CommitCandidate(context.Background(), workspace, "Controller-owned local candidate")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate) != 40 {
		t.Fatalf("candidate=%q", candidate)
	}
	if err := os.Unsetenv("DYLD_INSERT_LIBRARIES"); err != nil {
		t.Fatal(err)
	}
	metadata := runTestGitOutput(t, workspace, "show", "-s", "--format=%an%n%ae%n%cn%n%ce", candidate)
	for _, want := range []string{managedGitAuthorName, managedGitAuthorEmail} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("commit metadata=%q missing %q", metadata, want)
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{
		"linear=\n", "app_key=\n", "agent=\n", "config_count=\n", "dyld=\n",
		"author=" + managedGitAuthorName + "\n",
		"author_email=" + managedGitAuthorEmail + "\n", "config_global=/dev/null\n",
		"config_nosystem=1\n", "terminal=0\n", "path=" + processadapter.ManagedCommandPath + "\n",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("managed runtime log=%q missing %q", log, want)
		}
	}
	for _, forbidden := range []string{"linear-secret", "pem-secret", "ambient-author", "ambient-author@example.invalid", "config-secret", "inject-secret", "must-not-enter", "task-secret"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("managed runtime leaked %q: %q", forbidden, log)
		}
	}
}

func TestWorkspaceCancellationIsTypedAndSanitized(t *testing.T) {
	root := t.TempDir()
	wrapper := filepath.Join(root, "slow-git-wrapper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf 'secret-from-git\\n' >&2\ntrap '' INT\nsleep 10\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	adapter := Workspace{Binary: wrapper, Process: processadapter.OSRunner{InterruptGrace: 50 * time.Millisecond}}
	_, err := adapter.Head(ctx, root)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error=%v", err)
	}
	var commandError gitCommandError
	if !errors.As(err, &commandError) || commandError.category != processadapter.FailureInterrupted {
		t.Fatalf("typed cancellation error=%T %v", err, err)
	}
	if strings.Contains(err.Error(), "secret-from-git") {
		t.Fatalf("cancellation leaked child output: %v", err)
	}
}

func TestWorkspaceSanitizesProcessStartFailure(t *testing.T) {
	adapter := Workspace{Process: failingGitProcess{}}
	_, err := adapter.Head(context.Background(), t.TempDir())
	if err == nil || strings.Contains(err.Error(), "child-secret") {
		t.Fatalf("unsanitized process failure=%v", err)
	}
	var commandError gitCommandError
	if !errors.As(err, &commandError) || commandError.category != processadapter.FailureStart {
		t.Fatalf("typed process failure=%T %v", err, err)
	}
}

func TestWorkspaceMissingExecutableIsSanitizedStartFailure(t *testing.T) {
	_, err := (Workspace{Binary: "definitely-missing-git"}).Head(context.Background(), t.TempDir())
	if err == nil || strings.Contains(err.Error(), "definitely-missing-git") {
		t.Fatalf("missing executable error=%v", err)
	}
	var commandError gitCommandError
	if !errors.As(err, &commandError) || commandError.category != processadapter.FailureStart {
		t.Fatalf("typed missing executable error=%T %v", err, err)
	}
}

type failingGitProcess struct{}

func (failingGitProcess) Run(context.Context, processadapter.Spec) (processadapter.Result, error) {
	return processadapter.Result{Outcome: processadapter.OutcomeNotStarted, FailureCategory: processadapter.FailureStart, ExitCode: -1}, errors.Join(processadapter.NewFailure(processadapter.FailureStart), errors.New("child-secret"))
}

func runTestGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func runTestGitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
