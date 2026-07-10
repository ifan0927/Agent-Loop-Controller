package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type WorktreeRequest struct {
	SourcePath string
	OriginPath string
	BaseBranch string
	Branch     string
	Path       string
}

type WorktreeEvidence struct {
	SourcePath string `json:"source_path"`
	OriginPath string `json:"origin_path"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	BaseSHA    string `json:"base_sha"`
}

type WorktreeManager struct{ Workspace }

func (m WorktreeManager) Provision(ctx context.Context, request WorktreeRequest) (WorktreeEvidence, error) {
	for name, path := range map[string]string{"source": request.SourcePath, "origin": request.OriginPath, "worktree": request.Path} {
		if !filepath.IsAbs(path) {
			return WorktreeEvidence{}, fmt.Errorf("%s path must be absolute", name)
		}
	}
	if _, err := os.Lstat(request.Path); err == nil {
		return WorktreeEvidence{}, fmt.Errorf("worktree path already exists: %s", request.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return WorktreeEvidence{}, err
	}
	remote, err := m.run(ctx, request.SourcePath, "remote", "get-url", "origin")
	if err != nil {
		return WorktreeEvidence{}, fmt.Errorf("resolve source origin: %w", err)
	}
	if !sameLocalPath(strings.TrimSpace(remote), request.OriginPath) {
		return WorktreeEvidence{}, fmt.Errorf("source origin does not match registry origin")
	}
	ref := "refs/remotes/origin/" + request.BaseBranch
	if _, err := m.run(ctx, request.SourcePath, "fetch", "origin", "+refs/heads/"+request.BaseBranch+":"+ref); err != nil {
		return WorktreeEvidence{}, fmt.Errorf("fetch registered base: %w", err)
	}
	base, err := m.run(ctx, request.SourcePath, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return WorktreeEvidence{}, fmt.Errorf("resolve registered base: %w", err)
	}
	if _, err := m.run(ctx, request.SourcePath, "show-ref", "--verify", "--quiet", "refs/heads/"+request.Branch); err == nil {
		return WorktreeEvidence{}, fmt.Errorf("working branch already exists: %s", request.Branch)
	}
	if err := os.MkdirAll(filepath.Dir(request.Path), 0o700); err != nil {
		return WorktreeEvidence{}, err
	}
	if _, err := m.run(ctx, request.SourcePath, "worktree", "add", "-b", request.Branch, request.Path, ref); err != nil {
		return WorktreeEvidence{}, fmt.Errorf("create dedicated worktree: %w", err)
	}
	evidence := WorktreeEvidence{SourcePath: request.SourcePath, OriginPath: request.OriginPath, Path: request.Path,
		Branch: request.Branch, BaseBranch: request.BaseBranch, BaseSHA: strings.TrimSpace(base)}
	if err := m.ValidateOwned(ctx, evidence); err != nil {
		return WorktreeEvidence{}, err
	}
	return evidence, nil
}

func (m WorktreeManager) ValidateOwned(ctx context.Context, evidence WorktreeEvidence) error {
	resolved, err := filepath.EvalSymlinks(evidence.Path)
	if err != nil {
		return fmt.Errorf("resolve owned worktree: %w", err)
	}
	branch, err := m.Branch(ctx, evidence.Path)
	if err != nil {
		return err
	}
	if branch != evidence.Branch {
		return fmt.Errorf("owned worktree branch = %s, want %s", branch, evidence.Branch)
	}
	list, err := m.run(ctx, evidence.SourcePath, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	if !worktreeListContains(list, resolved, evidence.Branch) {
		return errors.New("worktree is not registered to the configured source repository")
	}
	remote, err := m.run(ctx, evidence.SourcePath, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if !sameLocalPath(strings.TrimSpace(remote), evidence.OriginPath) {
		return errors.New("owned source origin no longer matches registry evidence")
	}
	base, err := m.run(ctx, evidence.Path, "rev-parse", "--verify", "refs/remotes/origin/"+evidence.BaseBranch+"^{commit}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(base) != evidence.BaseSHA {
		return errors.New("remote base no longer matches the persisted exact base SHA")
	}
	head, err := m.Head(ctx, evidence.Path)
	if err != nil {
		return err
	}
	if err := m.ValidateRemoteBase(ctx, evidence.Path, evidence.BaseBranch, head); err != nil {
		return err
	}
	return nil
}

func (e WorktreeEvidence) JSON() string {
	data, _ := json.Marshal(e)
	return string(data)
}

func sameLocalPath(first, second string) bool {
	a, errA := filepath.EvalSymlinks(first)
	b, errB := filepath.EvalSymlinks(second)
	return errA == nil && errB == nil && a == b
}

func worktreeListContains(list, path, branch string) bool {
	blocks := strings.Split(strings.TrimSpace(list), "\n\n")
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		foundPath, foundBranch := false, false
		for _, line := range lines {
			if strings.TrimPrefix(line, "worktree ") == path && strings.HasPrefix(line, "worktree ") {
				foundPath = true
			}
			if strings.TrimPrefix(line, "branch ") == "refs/heads/"+branch && strings.HasPrefix(line, "branch ") {
				foundBranch = true
			}
		}
		if foundPath && foundBranch {
			return true
		}
	}
	return false
}
