package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	Nonce      string
}

type WorktreeEvidence struct {
	SourcePath string `json:"source_path"`
	OriginPath string `json:"origin_path"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	BaseSHA    string `json:"base_sha"`
	Nonce      string `json:"nonce"`
}

type WorktreeManager struct{ Workspace }

func (m WorktreeManager) Provision(ctx context.Context, request WorktreeRequest) (WorktreeEvidence, error) {
	for name, path := range map[string]string{"source": request.SourcePath, "worktree": request.Path} {
		if !filepath.IsAbs(path) {
			return WorktreeEvidence{}, fmt.Errorf("%s path must be absolute", name)
		}
	}
	if !validOriginBinding(request.OriginPath) {
		return WorktreeEvidence{}, errors.New("origin must be an absolute local path or credential-free canonical GitHub remote URL")
	}
	if strings.TrimSpace(request.Nonce) == "" {
		return WorktreeEvidence{}, errors.New("worktree ownership nonce is required")
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
	if !sameOriginBinding(strings.TrimSpace(remote), request.OriginPath) {
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
		Branch: request.Branch, BaseBranch: request.BaseBranch, BaseSHA: strings.TrimSpace(base), Nonce: request.Nonce}
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
	if !sameOriginBinding(strings.TrimSpace(remote), evidence.OriginPath) {
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

// sameOriginBinding compares a legacy local bare origin by resolved path, or a
// credential-free GitHub remote by its canonical repository identity. The
// latter deliberately allows the configured SSH/HTTPS transport to differ
// from the checkout transport while refusing every other host and repository.
func sameOriginBinding(first, second string) bool {
	if sameLocalPath(first, second) {
		return true
	}
	firstCanonical, errFirst := canonicalGitHubRemoteURL(first)
	secondCanonical, errSecond := canonicalGitHubRemoteURL(second)
	return errFirst == nil && errSecond == nil && githubRemoteIdentity(firstCanonical) == githubRemoteIdentity(secondCanonical)
}

func validOriginBinding(value string) bool {
	if filepath.IsAbs(value) {
		return sameLocalPath(value, value)
	}
	_, err := canonicalGitHubRemoteURL(value)
	return err == nil
}

func canonicalGitHubRemoteURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "git@github.com:") {
		owner, name, err := githubRemotePath(strings.TrimPrefix(value, "git@github.com:"))
		if err != nil {
			return "", err
		}
		return "git@github.com:" + strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New("remote URL is malformed")
	}
	if (parsed.Scheme != "https" && parsed.Scheme != "ssh") || !strings.EqualFold(parsed.Host, "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", errors.New("remote URL must be a github.com HTTPS or SSH URL")
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return "", errors.New("HTTPS remote URL must not contain credentials")
	}
	if parsed.Scheme == "ssh" {
		if parsed.User == nil || parsed.User.Username() != "git" {
			return "", errors.New("SSH remote URL must use the git user")
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return "", errors.New("SSH remote URL must not contain credentials")
		}
	}
	owner, name, err := githubRemotePath(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "https" {
		return "https://github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git", nil
	}
	return "ssh://git@github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git", nil
}

func githubRemoteIdentity(canonicalURL string) string {
	if strings.HasPrefix(canonicalURL, "git@github.com:") {
		return strings.TrimPrefix(strings.TrimSuffix(canonicalURL, ".git"), "git@github.com:")
	}
	parsed, err := url.Parse(canonicalURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSuffix(parsed.Path, ".git"), "/")
}

func githubRemotePath(value string) (string, string, error) {
	if !strings.HasSuffix(value, ".git") {
		return "", "", errors.New("remote URL path must end in .git")
	}
	parts := strings.Split(strings.TrimSuffix(value, ".git"), "/")
	if len(parts) != 2 || !validGitHubOwner(parts[0]) || !validGitHubRepository(parts[1]) {
		return "", "", errors.New("remote URL path must contain one valid owner and repository")
	}
	return parts[0], parts[1], nil
}

func validGitHubOwner(value string) bool {
	if len(value) == 0 || len(value) > 39 || value[0] == '-' || value[len(value)-1] == '-' || strings.Contains(value, "--") {
		return false
	}
	for _, char := range value {
		if char != '-' && !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func validGitHubRepository(value string) bool {
	if len(value) == 0 || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if char != '.' && char != '_' && char != '-' && !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
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
