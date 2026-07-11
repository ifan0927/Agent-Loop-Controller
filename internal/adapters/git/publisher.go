package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type PushSpec struct {
	Program string
	Args    []string
}

func BuildPushSpec(branch string) (PushSpec, error) {
	return BuildPushSpecExpected(branch, "")
}

func BuildPushSpecExpected(branch, expectedRemote string) (PushSpec, error) {
	if err := domain.ValidateGitBranch(branch); err != nil {
		return PushSpec{}, err
	}
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	lease := "--force-with-lease=refs/heads/" + branch + ":" + expectedRemote
	return PushSpec{Program: "git", Args: []string{"push", "--porcelain", lease, "origin", refspec}}, nil
}

type PushEvidence struct {
	RemoteRef string
	SHA       string
	ExitCode  int
	Stdout    string
	Stderr    string
}

type Publisher struct {
	Workspace Workspace
	Process   processadapter.Runner
}

func (p Publisher) RemoteSHA(ctx context.Context, workspace, branch string) (string, error) {
	if err := domain.ValidateGitBranch(branch); err != nil {
		return "", err
	}
	value, err := p.Workspace.run(ctx, workspace, "ls-remote", "--exit-code", "origin", "refs/heads/"+branch)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 2") {
			return "", nil
		}
		return "", err
	}
	fields := strings.Fields(value)
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch {
		return "", errors.New("unexpected remote ref evidence")
	}
	return fields[0], nil
}

func (p Publisher) Push(ctx context.Context, workspace, branch, candidate, stdoutPath, stderrPath string) (PushEvidence, error) {
	expectedRemote, err := p.RemoteSHA(ctx, workspace, branch)
	if err != nil {
		return PushEvidence{}, err
	}
	if expectedRemote != "" {
		if _, err := p.Workspace.run(ctx, workspace, "merge-base", "--is-ancestor", expectedRemote, candidate); err != nil {
			return PushEvidence{}, errors.New("candidate is not a fast-forward of expected remote SHA")
		}
	}
	spec, err := BuildPushSpecExpected(branch, expectedRemote)
	if err != nil {
		return PushEvidence{}, err
	}
	head, err := p.Workspace.Head(ctx, workspace)
	if err != nil || head != candidate {
		return PushEvidence{}, fmt.Errorf("push candidate HEAD mismatch: actual=%s expected=%s: %w", head, candidate, err)
	}
	status, err := p.Workspace.Status(ctx, workspace)
	if err != nil || strings.TrimSpace(status) != "" {
		return PushEvidence{}, fmt.Errorf("push requires clean worktree: %w", err)
	}
	result, err := p.Process.Run(ctx, processadapter.Spec{Program: spec.Program, Args: spec.Args, WorkingDir: workspace, StdoutPath: stdoutPath, StderrPath: stderrPath})
	if err != nil {
		return PushEvidence{}, err
	}
	evidence := PushEvidence{RemoteRef: "refs/heads/" + branch, SHA: candidate, ExitCode: result.ExitCode, Stdout: result.StdoutPath, Stderr: result.StderrPath}
	if result.ExitCode != 0 {
		return evidence, fmt.Errorf("git push exited with code %d", result.ExitCode)
	}
	remote, err := p.RemoteSHA(ctx, workspace, branch)
	if err != nil || remote != candidate {
		return evidence, fmt.Errorf("remote branch reconciliation mismatch: actual=%s expected=%s: %w", remote, candidate, err)
	}
	return evidence, nil
}
