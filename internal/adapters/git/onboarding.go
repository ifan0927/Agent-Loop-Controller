package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type ExistingCheckoutRequest struct {
	SourcePath          string
	CanonicalRepository string
	BaseBranch          string
	ForbiddenRoots      []string
}

type ExistingCheckoutObservation struct {
	CanonicalPath  string
	Origin         string
	HeadSHA        string
	RemoteHeadSHA  string
	EvidenceDigest string
	ObservedAt     time.Time
}

type ExistingCheckoutInspector struct {
	GitBinary string
	Timeout   time.Duration
	Now       func() time.Time
}

func (i ExistingCheckoutInspector) Inspect(ctx context.Context, request ExistingCheckoutRequest) (ExistingCheckoutObservation, error) {
	if !filepath.IsAbs(request.SourcePath) || filepath.Clean(request.SourcePath) != request.SourcePath || strings.Count(request.CanonicalRepository, "/") != 1 || strings.TrimSpace(request.BaseBranch) == "" {
		return ExistingCheckoutObservation{}, errors.New("existing checkout request is invalid")
	}
	canonical, err := inspectOnboardingDirectory(request.SourcePath)
	if err != nil || unsafeOnboardingRoot(canonical, request.ForbiddenRoots) {
		return ExistingCheckoutObservation{}, errors.New("existing checkout path is unsafe")
	}
	git := i.GitBinary
	if git == "" {
		git = "git"
	}
	timeout := i.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	run := func(args ...string) (string, error) {
		commandCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		command := exec.CommandContext(commandCtx, git, args...)
		command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never")
		output, err := command.Output()
		if err != nil || len(output) > 1<<20 {
			return "", errors.New("bounded Git observation failed")
		}
		return strings.TrimSpace(string(output)), nil
	}
	top, err := run("-C", canonical, "rev-parse", "--show-toplevel")
	if err != nil || top != canonical {
		return ExistingCheckoutObservation{}, errors.New("source path is not the exact checkout top level")
	}
	bare, err := run("-C", canonical, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "false" {
		return ExistingCheckoutObservation{}, errors.New("source path is not a non-bare checkout")
	}
	gitDir, err := run("-C", canonical, "rev-parse", "--absolute-git-dir")
	if err != nil || gitDir != filepath.Join(canonical, ".git") {
		return ExistingCheckoutObservation{}, errors.New("checkout Git metadata is unsafe")
	}
	if _, err := inspectOnboardingDirectory(gitDir); err != nil || onboardingGitOperationActive(gitDir) {
		return ExistingCheckoutObservation{}, errors.New("checkout has an in-progress Git operation")
	}
	status, err := run("-C", canonical, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || status != "" {
		return ExistingCheckoutObservation{}, errors.New("checkout is not clean")
	}
	branch, err := run("-C", canonical, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != request.BaseBranch {
		return ExistingCheckoutObservation{}, errors.New("checkout is not on the selected base branch")
	}
	head, err := run("-C", canonical, "rev-parse", "HEAD")
	if err != nil || !validGitObjectID(head) {
		return ExistingCheckoutObservation{}, errors.New("checkout head is unavailable")
	}
	origin, err := run("-C", canonical, "config", "--get", "remote.origin.url")
	if err != nil {
		return ExistingCheckoutObservation{}, errors.New("checkout origin is unavailable")
	}
	owner, repository, err := onboardingGitHubRemoteIdentity(origin)
	if err != nil || strings.ToLower(owner+"/"+repository) != request.CanonicalRepository {
		return ExistingCheckoutObservation{}, errors.New("checkout origin identity conflicts")
	}
	remote, err := run("-C", canonical, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+request.BaseBranch)
	fields := strings.Fields(remote)
	if err != nil || len(fields) != 2 || fields[1] != "refs/heads/"+request.BaseBranch || !validGitObjectID(fields[0]) {
		return ExistingCheckoutObservation{}, errors.New("remote base head is unavailable")
	}
	if fields[0] != head {
		return ExistingCheckoutObservation{}, errors.New("local head differs from the remote base head")
	}
	refs, err := run("-C", canonical, "for-each-ref", "--format=%(refname)%00%(objectname)", "refs/heads", "refs/remotes")
	if err != nil {
		return ExistingCheckoutObservation{}, errors.New("checkout refs are unavailable")
	}
	index, err := run("-C", canonical, "ls-files", "--stage")
	if err != nil {
		return ExistingCheckoutObservation{}, errors.New("checkout index is unavailable")
	}
	now := time.Now().UTC()
	if i.Now != nil {
		now = i.Now().UTC()
	}
	digest := onboardingGitDigest(canonical, request.CanonicalRepository, request.BaseBranch, head, fields[0], origin, refs, index)
	return ExistingCheckoutObservation{CanonicalPath: canonical, Origin: origin, HeadSHA: head, RemoteHeadSHA: fields[0], EvidenceDigest: digest, ObservedAt: now}, nil
}

func inspectOnboardingDirectory(path string) (string, error) {
	volume := filepath.VolumeName(path)
	current := string(filepath.Separator)
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	parts := strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("path contains an unavailable or symlink component")
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("directory ownership or mode is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return "", errors.New("directory ownership is unsafe")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", errors.New("directory path is not canonical")
	}
	return path, nil
}

func unsafeOnboardingRoot(path string, forbidden []string) bool {
	home, _ := os.UserHomeDir()
	if path == string(filepath.Separator) || home != "" && path == filepath.Clean(home) {
		return true
	}
	for _, root := range forbidden {
		if root == "" {
			continue
		}
		if pathsOverlap(path, filepath.Clean(root)) {
			return true
		}
	}
	return false
}

func pathsOverlap(left, right string) bool {
	rel, err := filepath.Rel(left, right)
	if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return true
	}
	rel, err = filepath.Rel(right, left)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func onboardingGitOperationActive(gitDir string) bool {
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "rebase-apply", "rebase-merge", "sequencer"} {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func onboardingGitHubRemoteIdentity(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	var path string
	if strings.HasPrefix(value, "git@github.com:") {
		path = strings.TrimPrefix(value, "git@github.com:")
	} else {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", "", errors.New("origin is not a credential-free GitHub remote")
		}
		hasPassword := false
		if parsed.User != nil {
			_, hasPassword = parsed.User.Password()
		}
		httpsRemote := parsed.Scheme == "https" && parsed.User == nil
		sshRemote := parsed.Scheme == "ssh" && parsed.User != nil && parsed.User.Username() == "git" && !hasPassword
		if !strings.EqualFold(parsed.Host, "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" || !httpsRemote && !sshRemote {
			return "", "", errors.New("origin is not a credential-free GitHub remote")
		}
		path = strings.TrimPrefix(parsed.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("origin repository identity is invalid")
	}
	return parts[0], parts[1], nil
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func onboardingGitDigest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
