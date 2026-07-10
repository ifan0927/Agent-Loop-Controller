package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type Workspace struct {
	Binary string
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
	binary := w.Binary
	if strings.TrimSpace(binary) == "" {
		binary = "git"
	}
	command := exec.CommandContext(ctx, binary, "merge-base", "--is-ancestor", strings.TrimSpace(base), head)
	command.Dir = directory
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return fmt.Errorf("remote base %s is not an ancestor of %s", ref, head)
		}
		return fmt.Errorf("validate remote base ancestry: %w: %s", err, strings.TrimSpace(stderr.String()))
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
	binary := w.Binary
	if strings.TrimSpace(binary) == "" {
		binary = "git"
	}
	command := exec.CommandContext(ctx, binary, "diff", "--cached", "--quiet")
	command.Dir = directory
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return false, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("inspect staged candidate: %w: %s", err, strings.TrimSpace(stderr.String()))
}

func (w Workspace) run(ctx context.Context, directory string, args ...string) (string, error) {
	binary := w.Binary
	if strings.TrimSpace(binary) == "" {
		binary = "git"
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
