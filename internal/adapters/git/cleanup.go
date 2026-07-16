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

func (c Cleanup) CleanupResourceAbsent(ctx context.Context, repository, kind, name string) (bool, error) {
	if err := c.validateRepository(repository); err != nil {
		return false, err
	}
	if err := c.validateSourceOrigin(ctx); err != nil {
		return false, err
	}
	switch kind {
	case "worktree":
		return c.worktreeAbsent(ctx, name)
	case "branch":
		if err := domain.ValidateGitBranch(name); err != nil {
			return false, errors.New("local branch ownership mismatch")
		}
		_, err := c.run(ctx, c.SourcePath, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
		if err != nil && isGitExit(err, 1) {
			return true, nil
		}
		return false, err
	case "remote_branch":
		if err := domain.ValidateGitBranch(name); err != nil {
			return false, errors.New("remote branch ownership mismatch")
		}
		remote, err := c.run(ctx, c.SourcePath, "ls-remote", "origin", "refs/heads/"+name)
		return strings.TrimSpace(remote) == "", err
	default:
		return false, errors.New("cleanup resource kind is not reconcilable")
	}
}

func (c Cleanup) RemoveWorktree(ctx context.Context, repository, worktree, expectedBranch, expectedSHA string) error {
	if err := c.validateRepository(repository); err != nil {
		return err
	}
	if err := c.validateSourceOrigin(ctx); err != nil {
		return err
	}
	if _, err := os.Lstat(worktree); errors.Is(err, os.ErrNotExist) {
		absent, registrationErr := c.worktreeAbsent(ctx, worktree)
		if registrationErr != nil {
			return registrationErr
		}
		if !absent {
			return errors.New("worktree path is absent but remains registered")
		}
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
	if _, err = c.run(ctx, c.SourcePath, "worktree", "remove", worktree); err != nil {
		return err
	}
	absent, err := c.worktreeAbsent(ctx, worktree)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("worktree removal postcondition is not satisfied")
	}
	return nil
}

func (c Cleanup) DeleteLocalBranch(ctx context.Context, repository, branch, expectedSHA string) error {
	if err := c.validateRepository(repository); err != nil {
		return err
	}
	if err := domain.ValidateGitBranch(branch); err != nil {
		return errors.New("local branch ownership mismatch")
	}
	if err := c.validateSourceOrigin(ctx); err != nil {
		return err
	}
	if c.branchHasWorktree(ctx, branch) {
		return errors.New("local branch remains attached to a worktree")
	}
	ref := "refs/heads/" + branch
	if _, err := c.run(ctx, c.SourcePath, "show-ref", "--verify", "--quiet", ref); err != nil {
		if isGitExit(err, 1) {
			return nil
		}
		return err
	}
	actual, err := c.run(ctx, c.SourcePath, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(expectedSHA) == "" {
		return errors.New("local branch ownership mismatch")
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

func worktreeListHasPath(list, path string) bool {
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(line, "worktree ") && strings.TrimPrefix(line, "worktree ") == path {
			return true
		}
	}
	return false
}

func (c Cleanup) worktreeAbsentFromRegistration(ctx context.Context, path string) (bool, error) {
	canonical, err := canonicalPathAllowMissing(path)
	if err != nil {
		return false, fmt.Errorf("resolve absent worktree: %w", err)
	}
	list, err := c.run(ctx, c.SourcePath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	return !worktreeListHasPath(list, canonical), nil
}

func (c Cleanup) worktreeAbsent(ctx context.Context, path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect worktree: %w", err)
	}
	return c.worktreeAbsentFromRegistration(ctx, path)
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

func canonicalPathAllowMissing(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	current := filepath.Clean(path)
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no existing path ancestor")
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
