package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	emptyTreeSHA1        = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	initialCommitMessage = "Initialize repository"
)

type RemoteRefObservation struct {
	Refs           map[string]string
	EvidenceDigest string
	ObservedAt     time.Time
}

type EmptyRepositoryGit struct {
	Workspace Workspace
	Timeout   time.Duration
	Now       func() time.Time
	// remoteResolver is a package-private deterministic fixture seam. Production
	// callers can only use the canonical credential-free GitHub SSH identity.
	remoteResolver func(string) (string, error)
}

func CanonicalGitHubSSHRemote(repository string) (string, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || repository != strings.ToLower(repository) {
		return "", errors.New("canonical repository identity is invalid")
	}
	return "git@github.com:" + repository + ".git", nil
}

func (g EmptyRepositoryGit) ObserveRemoteRefs(ctx context.Context, workingDirectory, repository string) (RemoteRefObservation, error) {
	remote, err := g.remote(repository)
	if err != nil {
		return RemoteRefObservation{}, err
	}
	commandCtx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	output, err := g.Workspace.Run(commandCtx, workingDirectory, "ls-remote", "--refs", remote)
	if err != nil {
		return RemoteRefObservation{}, errors.New("managed remote observation unavailable")
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validGitObjectID(fields[0]) || !strings.HasPrefix(fields[1], "refs/") || strings.ContainsAny(fields[1], "\x00\r\n") {
			return RemoteRefObservation{}, errors.New("managed remote observation is invalid")
		}
		if _, duplicate := refs[fields[1]]; duplicate {
			return RemoteRefObservation{}, errors.New("managed remote observation is ambiguous")
		}
		refs[fields[1]] = fields[0]
	}
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	parts := []string{"empty-repository-remote-refs-v1", repository}
	for _, ref := range keys {
		parts = append(parts, ref, refs[ref])
	}
	return RemoteRefObservation{Refs: refs, EvidenceDigest: onboardingGitDigest(parts...), ObservedAt: g.now()}, nil
}

func (g EmptyRepositoryGit) ValidateBaseBranch(ctx context.Context, workingDirectory, baseBranch string) error {
	commandCtx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	output, err := g.Workspace.Run(commandCtx, workingDirectory, "check-ref-format", "--branch", baseBranch)
	if err != nil || strings.TrimSpace(output) != baseBranch {
		return errors.New("base branch authority is invalid")
	}
	return nil
}

func (g EmptyRepositoryGit) EnsureManagedSource(ctx context.Context, repositoryRoot, sourcePath, repository, baseBranch string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	ctx = commandCtx
	remote, err := g.remote(repository)
	if err != nil || filepath.Dir(sourcePath) != repositoryRoot {
		return "", errors.New("managed source authority is invalid")
	}
	if _, statErr := os.Lstat(sourcePath); errors.Is(statErr, os.ErrNotExist) {
		if _, err := g.Workspace.Run(ctx, repositoryRoot, "init", "--object-format=sha1", "--initial-branch="+baseBranch, sourcePath); err != nil {
			return "", errors.New("managed source creation failed")
		}
	} else if statErr != nil {
		return "", errors.New("managed source is unavailable")
	}
	if _, err := g.validateManagedSourceState(ctx, sourcePath, repository, baseBranch, "", false); err != nil {
		return "", err
	}
	remotes, err := g.Workspace.Run(ctx, sourcePath, "remote")
	if err != nil {
		return "", errors.New("managed source remote observation failed")
	}
	if strings.TrimSpace(remotes) == "" {
		if _, err := g.Workspace.Run(ctx, sourcePath, "remote", "add", "origin", remote); err != nil {
			return "", errors.New("managed source origin creation failed")
		}
	}
	evidence, err := g.validateManagedSource(ctx, sourcePath, repository, baseBranch, "")
	if err != nil {
		return "", err
	}
	return evidence, nil
}

func (g EmptyRepositoryGit) EnsureInitialRevision(ctx context.Context, sourcePath, repository, baseBranch string, acceptedAt time.Time) (string, string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	ctx = commandCtx
	if acceptedAt.IsZero() {
		return "", "", errors.New("accepted operation time is unavailable")
	}
	head, err := g.optionalHead(ctx, sourcePath)
	if err != nil {
		return "", "", err
	}
	stamp := acceptedAt.UTC().Truncate(time.Second)
	gitDate := strconv.FormatInt(stamp.Unix(), 10) + " +0000"
	if head == "" {
		if _, err := g.Workspace.runWithEnvironment(ctx, sourcePath, []string{"GIT_AUTHOR_DATE=" + gitDate, "GIT_COMMITTER_DATE=" + gitDate}, "commit", "--allow-empty", "--no-gpg-sign", "-m", initialCommitMessage); err != nil {
			return "", "", errors.New("initial revision creation failed")
		}
		head, err = g.Workspace.Head(ctx, sourcePath)
		if err != nil {
			return "", "", errors.New("initial revision observation failed")
		}
	}
	evidence, err := g.validateManagedSource(ctx, sourcePath, repository, baseBranch, head)
	if err != nil {
		return "", "", err
	}
	metadata, err := g.Workspace.Run(ctx, sourcePath, "show", "-s", "--format=%P%x00%T%x00%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%B", head)
	if err != nil {
		return "", "", errors.New("initial revision metadata unavailable")
	}
	fields := strings.Split(strings.TrimSuffix(metadata, "\n"), "\x00")
	wantUnix := strconv.FormatInt(stamp.Unix(), 10)
	if len(fields) != 9 || fields[0] != "" || fields[1] != emptyTreeSHA1 || fields[2] != managedGitAuthorName || fields[3] != managedGitAuthorEmail || fields[4] != wantUnix || fields[5] != managedGitAuthorName || fields[6] != managedGitAuthorEmail || fields[7] != wantUnix || strings.TrimSuffix(fields[8], "\n") != initialCommitMessage {
		return "", "", errors.New("initial revision metadata conflicts")
	}
	return head, onboardingGitDigest("initial-revision-v1", repository, baseBranch, head, stamp.Format(time.RFC3339), evidence), nil
}

func (g EmptyRepositoryGit) PublishInitialBase(ctx context.Context, sourcePath, repository, baseBranch, sha string) (RemoteRefObservation, error) {
	before, err := g.ObserveRemoteRefs(ctx, sourcePath, repository)
	if err != nil {
		return RemoteRefObservation{}, err
	}
	ref := "refs/heads/" + baseBranch
	if len(before.Refs) == 1 && before.Refs[ref] == sha {
		return before, nil
	}
	if len(before.Refs) != 0 {
		return before, errors.New("initial remote authority is stale")
	}
	refspec := ref + ":" + ref
	commandCtx, cancel := context.WithTimeout(ctx, g.timeout())
	_, _ = g.Workspace.Run(commandCtx, sourcePath, "push", "origin", refspec)
	cancel()
	return g.ObserveRemoteRefs(ctx, sourcePath, repository)
}

func (g EmptyRepositoryGit) SettlePublishedSource(ctx context.Context, sourcePath, repository, baseBranch, sha string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	ctx = commandCtx
	ref := "refs/remotes/origin/" + baseBranch
	if _, err := g.Workspace.Run(ctx, sourcePath, "update-ref", ref, sha); err != nil {
		return "", errors.New("remote tracking settlement failed")
	}
	evidence, err := g.validateManagedSource(ctx, sourcePath, repository, baseBranch, sha)
	if err != nil {
		return "", err
	}
	remote, err := g.Workspace.Run(ctx, sourcePath, "rev-parse", "--verify", ref)
	if err != nil || strings.TrimSpace(remote) != sha {
		return "", errors.New("remote tracking authority conflicts")
	}
	return onboardingGitDigest("published-managed-source-v1", evidence, ref, sha), nil
}

func (g EmptyRepositoryGit) validateManagedSource(ctx context.Context, sourcePath, repository, baseBranch, expectedHead string) (string, error) {
	return g.validateManagedSourceState(ctx, sourcePath, repository, baseBranch, expectedHead, true)
}

func (g EmptyRepositoryGit) validateManagedSourceState(ctx context.Context, sourcePath, repository, baseBranch, expectedHead string, requireOrigin bool) (string, error) {
	canonical, err := inspectOnboardingDirectory(sourcePath)
	if err != nil || canonical != sourcePath {
		return "", errors.New("managed source path is unsafe")
	}
	checks := [][]string{{"rev-parse", "--show-toplevel"}, {"rev-parse", "--absolute-git-dir"}, {"rev-parse", "--is-bare-repository"}, {"rev-parse", "--show-object-format"}, {"symbolic-ref", "--quiet", "--short", "HEAD"}, {"remote"}, {"status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching"}, {"ls-files", "--stage"}, {"for-each-ref", "--format=%(refname)%00%(objectname)"}}
	values := make([]string, 0, len(checks))
	for _, args := range checks {
		value, runErr := g.Workspace.Run(ctx, sourcePath, args...)
		if runErr != nil {
			return "", errors.New("managed source observation failed")
		}
		values = append(values, strings.TrimSpace(value))
	}
	if values[0] != sourcePath || values[1] != filepath.Join(sourcePath, ".git") || values[2] != "false" || values[3] != "sha1" || values[4] != baseBranch || values[6] != "" || values[7] != "" || onboardingGitOperationActive(filepath.Join(sourcePath, ".git")) {
		return "", errors.New("managed source authority conflicts")
	}
	remote, _ := g.remote(repository)
	if values[5] != "" && values[5] != "origin" || requireOrigin && values[5] != "origin" {
		return "", errors.New("managed source remote authority conflicts")
	}
	if values[5] == "origin" {
		origin, runErr := g.Workspace.Run(ctx, sourcePath, "remote", "get-url", "origin")
		if runErr != nil || strings.TrimSpace(origin) != remote {
			return "", errors.New("managed source remote authority conflicts")
		}
	}
	configuration, runErr := g.Workspace.Run(ctx, sourcePath, "config", "--local", "--null", "--list")
	if runErr != nil || !validManagedSourceConfiguration(configuration, remote, values[5] == "origin") {
		return "", errors.New("managed source configuration conflicts")
	}
	refs := values[8]
	if expectedHead == "" {
		if refs != "" {
			return "", errors.New("managed source contains unexpected refs")
		}
	} else {
		wantHead := "refs/heads/" + baseBranch + "\x00" + expectedHead
		wantRemote := "refs/remotes/origin/" + baseBranch + "\x00" + expectedHead
		if refs != wantHead && refs != wantHead+"\n"+wantRemote {
			return "", errors.New("managed source refs conflict")
		}
	}
	return onboardingGitDigest(append([]string{"managed-source-v1", repository, baseBranch, expectedHead}, values...)...), nil
}

func validManagedSourceConfiguration(raw, remote string, hasOrigin bool) bool {
	entries := make(map[string]string)
	for _, item := range strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00") {
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "\n", 2)
		if len(parts) != 2 || parts[0] == "" {
			return false
		}
		key := strings.ToLower(parts[0])
		if _, duplicate := entries[key]; duplicate {
			return false
		}
		entries[key] = parts[1]
	}
	required := map[string]string{"core.repositoryformatversion": "0", "core.filemode": "true", "core.bare": "false", "core.logallrefupdates": "true"}
	if hasOrigin {
		required["remote.origin.url"] = remote
		required["remote.origin.fetch"] = "+refs/heads/*:refs/remotes/origin/*"
	}
	for key, value := range required {
		if entries[key] != value {
			return false
		}
		delete(entries, key)
	}
	for key, value := range entries {
		if (key != "core.ignorecase" && key != "core.precomposeunicode") || value != "true" {
			return false
		}
	}
	return true
}

func (g EmptyRepositoryGit) optionalHead(ctx context.Context, sourcePath string) (string, error) {
	value, err := g.Workspace.Run(ctx, sourcePath, "rev-parse", "--verify", "HEAD")
	if err != nil {
		refs, refsErr := g.Workspace.Run(ctx, sourcePath, "for-each-ref", "--format=%(refname)")
		if refsErr == nil && strings.TrimSpace(refs) == "" {
			return "", nil
		}
		return "", errors.New("managed source head observation failed")
	}
	value = strings.TrimSpace(value)
	if !validGitObjectID(value) {
		return "", fmt.Errorf("managed source head is invalid")
	}
	return value, nil
}

func (g EmptyRepositoryGit) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return 20 * time.Second
}

func (g EmptyRepositoryGit) remote(repository string) (string, error) {
	if g.remoteResolver != nil {
		return g.remoteResolver(repository)
	}
	return CanonicalGitHubSSHRemote(repository)
}

func (g EmptyRepositoryGit) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}
