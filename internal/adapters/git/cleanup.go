package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// Cleanup removes only resources bound to one registered local source and
// origin. Every mutating operation repeats its relevant observations so a
// resumed controller never relies on stale ownership evidence.
type Cleanup struct {
	Workspace
	SourcePath string
	OriginPath string
}

func (c Cleanup) RemoveWorktree(ctx context.Context, repository, worktree, expectedBranch, expectedSHA string) error {
	if err := c.validateRepository(repository); err != nil {
		return err
	}
	if err := c.validateSourceOrigin(ctx); err != nil {
		return err
	}
	if _, err := os.Lstat(worktree); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect worktree: %w", err)
	}
	canonical, err := canonicalPath(worktree)
	if err != nil {
		return fmt.Errorf("resolve worktree: %w", err)
	}
	branch, err := c.Branch(ctx, worktree)
	if err != nil {
		return err
	}
	if err := domain.ValidateGitBranch(expectedBranch); err != nil || strings.TrimSpace(expectedSHA) == "" || branch != expectedBranch {
		return errors.New("worktree branch ownership mismatch")
	}
	if err := c.validateRegisteredWorktree(ctx, canonical, branch); err != nil {
		return err
	}
	head, err := c.Head(ctx, worktree)
	if err != nil || head != expectedSHA {
		return errors.New("worktree HEAD ownership mismatch")
	}
	status, err := c.Status(ctx, worktree)
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("refusing to remove dirty worktree")
	}
	_, err = c.run(ctx, c.SourcePath, "worktree", "remove", worktree)
	return err
}

func (c Cleanup) DeleteLocalBranch(ctx context.Context, repository, branch, expectedSHA string) error {
	if err := c.validateRepository(repository); err != nil {
		return err
	}
	if err := domain.ValidateGitBranch(branch); err != nil {
		return err
	}
	if strings.TrimSpace(expectedSHA) == "" {
		return errors.New("local branch ownership mismatch")
	}
	if err := c.validateSourceOrigin(ctx); err != nil {
		return err
	}
	if c.branchHasWorktree(ctx, branch) {
		return errors.New("local branch remains attached to a worktree")
	}
	ref := "refs/heads/" + branch
	actual, err := c.run(ctx, c.SourcePath, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		if isGitExit(err, 128) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(actual) != expectedSHA {
		return errors.New("local branch no longer matches persisted candidate")
	}
	_, err = c.run(ctx, c.SourcePath, "update-ref", "-d", ref, expectedSHA)
	return err
}

func (c Cleanup) DeleteRemoteBranch(ctx context.Context, repository, branch, expectedSHA string) error {
	if err := c.validateRepository(repository); err != nil {
		return err
	}
	if err := domain.ValidateGitBranch(branch); err != nil || strings.TrimSpace(expectedSHA) == "" {
		return errors.New("remote branch ownership mismatch")
	}
	if err := c.validateSourceOrigin(ctx); err != nil {
		return err
	}
	remote, err := c.run(ctx, c.SourcePath, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(remote) == "" {
		return nil
	}
	fields := strings.Fields(remote)
	if len(fields) != 2 || fields[0] != expectedSHA || fields[1] != "refs/heads/"+branch {
		return errors.New("remote branch no longer matches persisted candidate")
	}
	_, err = c.run(ctx, c.SourcePath, "push", "--porcelain", "--force-with-lease=refs/heads/"+branch+":"+expectedSHA, "origin", "--delete", "refs/heads/"+branch)
	return err
}

func (c Cleanup) validateRepository(repository string) error {
	if strings.TrimSpace(repository) == "" || !filepath.IsAbs(c.SourcePath) || !validOriginBinding(c.OriginPath) {
		return errors.New("cleanup repository authority is incomplete")
	}
	return nil
}

func (c Cleanup) validateSourceOrigin(ctx context.Context) error {
	if _, err := canonicalPath(c.SourcePath); err != nil {
		return fmt.Errorf("resolve cleanup source: %w", err)
	}
	remote, err := c.run(ctx, c.SourcePath, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if !sameOriginBinding(strings.TrimSpace(remote), c.OriginPath) {
		return errors.New("source origin ownership mismatch")
	}
	return nil
}

func (c Cleanup) validateRegisteredWorktree(ctx context.Context, path, branch string) error {
	list, err := c.run(ctx, c.SourcePath, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	if !worktreeListContains(list, path, branch) {
		return errors.New("worktree is not registered to configured source")
	}
	return nil
}

func (c Cleanup) branchHasWorktree(ctx context.Context, branch string) bool {
	list, err := c.run(ctx, c.SourcePath, "worktree", "list", "--porcelain")
	if err != nil {
		return true
	}
	return strings.Contains(list, "branch refs/heads/"+branch+"\n") || strings.HasSuffix(list, "branch refs/heads/"+branch)
}

func canonicalPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("path must not be a symlink")
	}
	return filepath.EvalSymlinks(path)
}
