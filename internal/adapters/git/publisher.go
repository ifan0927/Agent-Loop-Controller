package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
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

// BuildPushSpecExpected binds the update to the remote SHA observed before the
// push. The lease is a compare-and-swap guard, not permission to overwrite a
// divergent branch; Publisher rejects non-fast-forward candidates separately.
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
		if isGitExit(err, 2) {
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

func (p Publisher) Push(ctx context.Context, workspace, branch, candidate, expectedRemote, artifactRoot string) (PushEvidence, error) {
	observedRemote, err := p.RemoteSHA(ctx, workspace, branch)
	if err != nil {
		return PushEvidence{}, err
	}
	if observedRemote != expectedRemote {
		return PushEvidence{}, errors.New("remote branch changed after pre-push reconciliation")
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
	stdoutPath, stderrPath, err := pushCapturePaths(artifactRoot)
	if err != nil {
		return PushEvidence{}, err
	}
	result, err := p.Process.Run(ctx, processadapter.Spec{Program: spec.Program, Args: spec.Args, WorkingDir: workspace, StdoutPath: stdoutPath, StderrPath: stderrPath, MustNotExist: []string{stdoutPath, stderrPath}})
	if err != nil {
		return PushEvidence{}, err
	}
	result = processadapter.NormalizeResult(result, nil)
	if !result.Valid() {
		return PushEvidence{}, errors.New("git push returned invalid process evidence")
	}
	evidence := PushEvidence{RemoteRef: "refs/heads/" + branch, SHA: candidate, ExitCode: result.ExitCode, Stdout: result.StdoutPath, Stderr: result.StderrPath}
	if !result.Succeeded() {
		return evidence, fmt.Errorf("git push exited with code %d", result.ExitCode)
	}
	remote, err := p.RemoteSHA(ctx, workspace, branch)
	if err != nil || remote != candidate {
		return evidence, fmt.Errorf("remote branch reconciliation mismatch: actual=%s expected=%s: %w", remote, candidate, err)
	}
	return evidence, nil
}

func pushCapturePaths(artifactRoot string) (string, string, error) {
	if strings.TrimSpace(artifactRoot) == "" || !filepath.IsAbs(artifactRoot) {
		return "", "", errors.New("push artifact root must be absolute")
	}
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", "", fmt.Errorf("generate push artifact name: %w", err)
	}
	name := "push-" + hex.EncodeToString(token[:])
	return filepath.Join(artifactRoot, name+".stdout"), filepath.Join(artifactRoot, name+".stderr"), nil
}
