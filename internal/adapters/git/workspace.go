package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

type Workspace struct {
	Binary  string
	Process processadapter.Runner
}

const (
	managedGitAuthorName  = "Agent Loop Controller"
	managedGitAuthorEmail = "agent-loop-controller@users.noreply.github.com"
	maxGitOutputBytes     = 4 << 20
)

var managedGitExcludedEnvironment = []string{
	"IFAN_LOOP_LINEAR_TOKEN",
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"GITHUB_APP_TOKEN",
	"GITHUB_APP_PRIVATE_KEY",
	"GITHUB_APP_PRIVATE_KEY_FILE",
	"IFAN_LOOP_GITHUB_APP_PRIVATE_KEY",
	"IFAN_LOOP_GITHUB_APP_PEM",
	"GIT_ASKPASS",
	"SSH_ASKPASS",
	"SSH_AUTH_SOCK",
	"GIT_SSH_COMMAND",
}

var managedGitEnvironment = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_SYSTEM=/dev/null",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_AUTHOR_NAME=" + managedGitAuthorName,
	"GIT_AUTHOR_EMAIL=" + managedGitAuthorEmail,
	"GIT_COMMITTER_NAME=" + managedGitAuthorName,
	"GIT_COMMITTER_EMAIL=" + managedGitAuthorEmail,
}

func (w Workspace) CommitMetadata(ctx context.Context, directory, head string) (parent, subject string, err error) {
	parentOutput, err := w.run(ctx, directory, "rev-parse", head+"^")
	if err != nil {
		return "", "", err
	}
	subjectOutput, err := w.run(ctx, directory, "show", "-s", "--format=%s", head)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(parentOutput), strings.TrimSpace(subjectOutput), nil
}

func (w Workspace) Head(ctx context.Context, directory string) (string, error) {
	output, err := w.run(ctx, directory, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func (w Workspace) Branch(ctx context.Context, directory string) (string, error) {
	output, err := w.run(ctx, directory, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func (w Workspace) Status(ctx context.Context, directory string) (string, error) {
	return w.run(ctx, directory, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
}

func (w Workspace) ValidateRemoteBase(ctx context.Context, directory, baseBranch, head string) error {
	ref := "refs/remotes/origin/" + baseBranch
	base, err := w.run(ctx, directory, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve remote base %s: %w", ref, err)
	}
	if err := w.runExpectingSuccess(ctx, directory, "merge-base", "--is-ancestor", strings.TrimSpace(base), head); err != nil {
		if isGitExit(err, 1) {
			return fmt.Errorf("remote base %s is not an ancestor of %s", ref, head)
		}
		return fmt.Errorf("validate remote base ancestry: %w", err)
	}
	return nil
}

func (w Workspace) CommitCandidate(ctx context.Context, directory, message string) (string, error) {
	if _, err := w.run(ctx, directory, "add", "--all"); err != nil {
		return "", fmt.Errorf("stage candidate: %w", err)
	}
	hasChanges, err := w.hasStagedChanges(ctx, directory)
	if err != nil {
		return "", err
	}
	if !hasChanges {
		return "", fmt.Errorf("candidate contains no changes")
	}
	if _, err := w.run(ctx, directory, "commit", "-m", message); err != nil {
		return "", fmt.Errorf("commit candidate: %w", err)
	}
	return w.Head(ctx, directory)
}

func (w Workspace) hasStagedChanges(ctx context.Context, directory string) (bool, error) {
	err := w.runExpectingSuccess(ctx, directory, "diff", "--cached", "--quiet")
	if err == nil {
		return false, nil
	}
	if isGitExit(err, 1) {
		return true, nil
	}
	return false, fmt.Errorf("inspect staged candidate: %w", err)
}

func (w Workspace) run(ctx context.Context, directory string, args ...string) (string, error) {
	captureRoot, err := os.MkdirTemp("", "ifan-loop-git-")
	if err != nil {
		return "", gitCommandError{category: processadapter.FailureArtifactSetup}
	}
	defer os.RemoveAll(captureRoot)
	stdoutPath := filepath.Join(captureRoot, "stdout")
	stderrPath := filepath.Join(captureRoot, "stderr")
	runner := w.Process
	if runner == nil {
		runner = processadapter.OSRunner{}
	}
	result, runErr := runner.Run(ctx, processadapter.Spec{
		Program:              w.binary(),
		Args:                 managedGitArgs(args),
		WorkingDir:           directory,
		StdoutPath:           stdoutPath,
		StderrPath:           stderrPath,
		MustNotExist:         []string{stdoutPath, stderrPath},
		ExcludedEnv:          managedGitExcludedEnvironment,
		Environment:          managedGitEnvironment,
		EnvironmentAllowlist: []string{"HOME"},
	})
	result = processadapter.NormalizeResult(result, runErr)
	if runErr != nil {
		return "", gitFailure(result, runErr)
	}
	if !result.Valid() {
		return "", gitCommandError{category: processadapter.FailureInvalidResult}
	}
	if result.Outcome != processadapter.OutcomeExited {
		return "", gitCommandError{category: processadapter.FailureUnknown}
	}
	if result.ExitCode != 0 {
		return "", gitCommandError{exitCode: result.ExitCode}
	}
	output := result.Stdout
	if output == nil {
		output, err = os.ReadFile(stdoutPath)
		if err != nil {
			return "", gitCommandError{category: processadapter.FailureArtifactSetup}
		}
	}
	if len(output) > maxGitOutputBytes {
		return "", gitCommandError{category: processadapter.FailureUnknown}
	}
	return string(output), nil
}

// Run executes one controller-owned Git argv through the same managed runtime
// used by the typed workspace methods. Callers must pass Git subcommand argv,
// never a shell command or task text assembled into one string.
func (w Workspace) Run(ctx context.Context, directory string, args ...string) (string, error) {
	return w.run(ctx, directory, args...)
}

func (w Workspace) runExpectingSuccess(ctx context.Context, directory string, args ...string) error {
	_, err := w.run(ctx, directory, args...)
	return err
}

func (w Workspace) binary() string {
	if strings.TrimSpace(w.Binary) == "" {
		return "git"
	}
	return w.Binary
}

func managedGitArgs(args []string) []string {
	config := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "rebase.autoStash=false",
		"-c", "merge.autoStash=false",
		"-c", "credential.helper=",
	}
	return append(config, args...)
}

type gitCommandError struct {
	exitCode int
	category processadapter.FailureCategory
	cause    error
}

func (e gitCommandError) Error() string {
	if e.category != processadapter.FailureNone {
		return "managed git command failure: " + string(e.category)
	}
	if e.exitCode != 0 {
		return fmt.Sprintf("managed git command exited with code %d", e.exitCode)
	}
	return "managed git command failure"
}

func (e gitCommandError) Unwrap() error { return e.cause }

func gitFailure(result processadapter.Result, runErr error) error {
	category := result.FailureCategory
	if category == processadapter.FailureNone {
		category = processadapter.FailureUnknown
	}
	var cause error
	if errors.Is(runErr, context.Canceled) {
		cause = context.Canceled
	} else if errors.Is(runErr, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	return gitCommandError{category: category, cause: cause}
}

func isGitExit(err error, code int) bool {
	var commandError gitCommandError
	return errors.As(err, &commandError) && commandError.exitCode == code
}
