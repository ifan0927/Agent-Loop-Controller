package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// CleanupSourceRecovery is deliberately local-only. Its closed argv set has no
// fetch, push, ls-remote, or other network-capable Git operation.
type CleanupSourceRecovery struct{ Workspace }

type CleanupSourceRecoveryRequest struct {
	Repository, FrozenSourcePath, ReplacementSourcePath, ExpectedOrigin, WorktreePath, Branch, CandidateHead, ExpectedRegistrationDigest string
}

type CleanupSourceRecoveryObservation struct {
	ReplacementSourceDigest, ReplacementIdentityDigest, RepositoryOriginDigest, RegistrationDigest string
	Branch, CandidateHead                                                                          string
	LinkRepaired, HeadDetached, WorktreePresent, BranchPresent, WorktreeClean                      bool
}

type recoveryRegistration struct {
	path, head, branch string
}

var cleanupSourceRecoveryEnvironment = []string{"GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1"}
var cleanupSourceRecoveryReadEnvironment = []string{"GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0"}
var cleanupSourceRecoveryGitConfig = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "diff.ignoreSubmodules=none"}

func (a CleanupSourceRecovery) runRecovery(ctx context.Context, directory string, args ...string) (string, error) {
	return a.runWithEnvironment(ctx, directory, cleanupSourceRecoveryEnvironment, append(cleanupSourceRecoveryGitConfig, args...)...)
}

func (a CleanupSourceRecovery) observeRecovery(ctx context.Context, directory string, args ...string) (string, error) {
	return a.runWithEnvironment(ctx, directory, cleanupSourceRecoveryReadEnvironment, append(cleanupSourceRecoveryGitConfig, args...)...)
}

func (a CleanupSourceRecovery) ObserveCleanupSourceRecovery(ctx context.Context, request CleanupSourceRecoveryRequest) (CleanupSourceRecoveryObservation, error) {
	if err := validateCleanupSourceRecoveryRequest(request); err != nil {
		return CleanupSourceRecoveryObservation{}, err
	}
	if _, err := os.Lstat(request.FrozenSourcePath); err == nil {
		return CleanupSourceRecoveryObservation{}, errors.New("frozen source checkout remains available")
	} else if !errors.Is(err, os.ErrNotExist) {
		return CleanupSourceRecoveryObservation{}, errors.New("frozen source checkout availability is ambiguous")
	}
	replacement, replacementInfo, err := canonicalOwnedDirectory(request.ReplacementSourcePath)
	if err != nil {
		return CleanupSourceRecoveryObservation{}, errors.New("replacement source checkout is unsafe")
	}
	worktree, err := canonicalPathAllowMissing(request.WorktreePath)
	if err != nil || worktree != filepath.Clean(request.WorktreePath) {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree path is unsafe")
	}
	worktreeInfo, worktreeErr := os.Lstat(worktree)
	worktreePresent := worktreeErr == nil
	if worktreeErr != nil && !errors.Is(worktreeErr, os.ErrNotExist) {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree availability is ambiguous")
	}
	if worktreePresent && (!worktreeInfo.IsDir() || worktreeInfo.Mode()&os.ModeSymlink != 0) {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree is unsafe")
	}
	if replacementInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || worktreePresent && worktreeInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return CleanupSourceRecoveryObservation{}, errors.New("cleanup recovery path owner mismatch")
	}
	if replacement == worktree || pathContains(replacement, worktree) || pathContains(worktree, replacement) {
		return CleanupSourceRecoveryObservation{}, errors.New("cleanup recovery paths overlap")
	}
	if output, runErr := a.observeRecovery(ctx, replacement, "rev-parse", "--is-inside-work-tree"); runErr != nil || strings.TrimSpace(output) != "true" {
		return CleanupSourceRecoveryObservation{}, errors.New("replacement source is not a Git checkout")
	}
	commonRaw, err := a.observeRecovery(ctx, replacement, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return CleanupSourceRecoveryObservation{}, errors.New("replacement common directory is unavailable")
	}
	common, commonInfo, err := canonicalOwnedDirectory(strings.TrimSpace(commonRaw))
	if err != nil || commonInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return CleanupSourceRecoveryObservation{}, errors.New("replacement common directory is unsafe")
	}
	replacementGit, replacementGitInfo, err := canonicalOwnedDirectory(filepath.Join(replacement, ".git"))
	if err != nil || replacementGitInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || common != replacementGit {
		return CleanupSourceRecoveryObservation{}, errors.New("replacement source is not the primary checkout")
	}
	origin, err := a.observeRecovery(ctx, replacement, "remote", "get-url", "origin")
	if err != nil || !sameOriginBinding(strings.TrimSpace(origin), request.ExpectedOrigin) {
		return CleanupSourceRecoveryObservation{}, errors.New("replacement source origin mismatch")
	}
	registrations, err := a.registrations(ctx, replacement)
	if err != nil {
		return CleanupSourceRecoveryObservation{}, errors.New("replacement worktree registration is unavailable")
	}
	matches := make([]recoveryRegistration, 0, 1)
	for _, item := range registrations {
		if item.path == worktree {
			matches = append(matches, item)
		}
		if item.path != worktree && item.branch == request.Branch {
			return CleanupSourceRecoveryObservation{}, errors.New("candidate branch is attached outside the owned worktree")
		}
	}
	_, branchPresent, err := a.localBranch(ctx, replacement, request.Branch)
	if err != nil || branchPresent {
		return CleanupSourceRecoveryObservation{}, errors.New("recovered local branch must remain absent")
	}
	identity := cleanupRecoveryDigest("cleanup-source-identity-v1", replacement, common, fmt.Sprint(replacementInfo.Sys().(*syscall.Stat_t).Dev), fmt.Sprint(replacementInfo.Sys().(*syscall.Stat_t).Ino), fmt.Sprint(commonInfo.Sys().(*syscall.Stat_t).Dev), fmt.Sprint(commonInfo.Sys().(*syscall.Stat_t).Ino))
	baseObservation := CleanupSourceRecoveryObservation{
		ReplacementSourceDigest: cleanupSourcePathDigest(replacement), ReplacementIdentityDigest: identity,
		RepositoryOriginDigest: cleanupRecoveryDigest("cleanup-origin-v1", normalizedOriginBinding(strings.TrimSpace(origin))),
		Branch:                 request.Branch, CandidateHead: request.CandidateHead, WorktreePresent: worktreePresent, BranchPresent: branchPresent,
	}
	if _, err := a.observeRecovery(ctx, replacement, "cat-file", "-e", request.CandidateHead+"^{commit}"); err != nil {
		return CleanupSourceRecoveryObservation{}, errors.New("persisted candidate commit is unavailable")
	}
	if !worktreePresent {
		if len(matches) != 0 {
			return CleanupSourceRecoveryObservation{}, errors.New("absent worktree remains registered")
		}
		if request.ExpectedRegistrationDigest == "" {
			baseObservation.RegistrationDigest = cleanupRecoveryDigest("cleanup-registration-v2", worktree, request.Branch, request.CandidateHead, "absent")
		} else {
			baseObservation.RegistrationDigest = request.ExpectedRegistrationDigest
		}
		baseObservation.WorktreeClean = true
		return baseObservation, nil
	}
	if len(matches) != 1 {
		return CleanupSourceRecoveryObservation{}, errors.New("exact worktree registration mismatch")
	}
	admin, repaired, originalHead, adminStateDigest, err := recoveryWorktreeAdmin(worktree, request.FrozenSourcePath, common)
	if err != nil {
		return CleanupSourceRecoveryObservation{}, err
	}
	if _, err := a.observeRecovery(ctx, replacement, "cat-file", "-e", originalHead+"^{commit}"); err != nil {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree ORIG_HEAD commit is unavailable")
	}
	baseObservation.RegistrationDigest = cleanupRecoveryDigest("cleanup-registration-v2", worktree, request.Branch, request.CandidateHead, adminStateDigest)
	if request.ExpectedRegistrationDigest != "" && baseObservation.RegistrationDigest != request.ExpectedRegistrationDigest {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree administration state changed")
	}
	headRaw, err := readOwnedRegularFile(filepath.Join(admin, "HEAD"), 4<<10)
	if err != nil {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree HEAD is unavailable")
	}
	head := strings.TrimSpace(string(headRaw))
	detached := head == request.CandidateHead
	symbolic := head == "ref: refs/heads/"+request.Branch
	if !detached && !symbolic {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree HEAD state is ambiguous")
	}
	if symbolic && (matches[0].branch != request.Branch || strings.Trim(matches[0].head, "0") != "") {
		return CleanupSourceRecoveryObservation{}, errors.New("symbolic worktree registration mismatch")
	}
	if detached && (matches[0].branch != "" || matches[0].head != request.CandidateHead) {
		return CleanupSourceRecoveryObservation{}, errors.New("detached worktree registration mismatch")
	}
	if _, err := readOwnedRegularFile(filepath.Join(admin, "index"), 64<<20); err != nil {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree index is unsafe")
	}
	if _, err := os.Lstat(filepath.Join(admin, "index.lock")); !errors.Is(err, os.ErrNotExist) {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree index is concurrently locked")
	}
	if err := a.rejectRecoveryExternalGitBehavior(ctx, worktree, admin); err != nil {
		return CleanupSourceRecoveryObservation{}, err
	}
	clean, err := a.recoveryWorktreeClean(ctx, admin, worktree, request.CandidateHead, detached)
	if err != nil || !clean {
		return CleanupSourceRecoveryObservation{}, errors.New("owned worktree is not clean against the persisted candidate")
	}
	baseObservation.LinkRepaired, baseObservation.HeadDetached, baseObservation.WorktreeClean = repaired, detached, true
	return baseObservation, nil
}

func (a CleanupSourceRecovery) rejectRecoveryExternalGitBehavior(ctx context.Context, worktree, admin string) error {
	gitDir := "--git-dir=" + admin
	workTree := "--work-tree=" + worktree
	if output, err := a.observeRecovery(ctx, worktree, gitDir, workTree, "config", "--includes", "--get-regexp", `^filter\.`); err == nil {
		if strings.TrimSpace(output) != "" {
			return errors.New("owned worktree has external filter configuration")
		}
	} else if !isGitExit(err, 1) {
		return errors.New("owned worktree filter configuration is ambiguous")
	}
	output, err := a.observeRecovery(ctx, worktree, gitDir, workTree, "ls-files", "--stage", "-z")
	if err != nil {
		return errors.New("owned worktree index topology is unavailable")
	}
	for _, record := range strings.Split(output, "\x00") {
		if strings.HasPrefix(record, "160000 ") {
			return errors.New("owned worktree contains a submodule entry")
		}
	}
	return nil
}

func recoveryWorktreeAdmin(worktree, frozenSource, common string) (string, bool, string, string, error) {
	raw, err := readOwnedRegularFile(filepath.Join(worktree, ".git"), 4<<10)
	if err != nil {
		return "", false, "", "", errors.New("owned worktree Git link is unavailable")
	}
	link := strings.TrimSpace(string(raw))
	prefixes := []struct {
		root     string
		repaired bool
	}{{filepath.Join(filepath.Clean(frozenSource), ".git", "worktrees"), false}, {filepath.Join(common, "worktrees"), true}}
	for _, prefix := range prefixes {
		marker := "gitdir: " + prefix.root + string(filepath.Separator)
		if !strings.HasPrefix(link, marker) {
			continue
		}
		name := strings.TrimPrefix(link, marker)
		if name == "" || strings.Contains(name, string(filepath.Separator)) {
			return "", false, "", "", errors.New("owned worktree Git link identity is invalid")
		}
		admin, adminInfo, adminErr := canonicalOwnedDirectory(filepath.Join(common, "worktrees", name))
		if adminErr != nil || adminInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			return "", false, "", "", errors.New("replacement worktree administration is unavailable")
		}
		gitdir, gitdirErr := readOwnedRegularFile(filepath.Join(admin, "gitdir"), 4<<10)
		commondir, commondirErr := readOwnedRegularFile(filepath.Join(admin, "commondir"), 4<<10)
		if gitdirErr != nil || commondirErr != nil || strings.TrimSpace(string(gitdir)) != filepath.Join(worktree, ".git") || strings.TrimSpace(string(commondir)) != "../.." {
			return "", false, "", "", errors.New("replacement worktree administration links are unsafe")
		}
		if err := validateRecoveryReflog(admin); err != nil {
			return "", false, "", "", err
		}
		originalHead, adminStateDigest, err := validateRecoveryAdminState(admin)
		if err != nil {
			return "", false, "", "", err
		}
		return admin, prefix.repaired, originalHead, adminStateDigest, nil
	}
	return "", false, "", "", errors.New("owned worktree Git link authority conflicts")
}

func validateRecoveryAdminState(admin string) (string, string, error) {
	entries, err := os.ReadDir(admin)
	if err != nil {
		return "", "", errors.New("owned worktree administration is unreadable")
	}
	allowed := map[string]bool{"COMMIT_EDITMSG": true, "HEAD": true, "ORIG_HEAD": true, "commondir": true, "gitdir": true, "index": true, "logs": true, "refs": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return "", "", errors.New("owned worktree has in-progress operation state")
		}
	}
	origHead, err := readOwnedRegularFile(filepath.Join(admin, "ORIG_HEAD"), 4<<10)
	originalHead := strings.TrimSpace(string(origHead))
	if err != nil || len(originalHead) != 40 {
		return "", "", errors.New("owned worktree ORIG_HEAD is ambiguous")
	}
	if _, err := hex.DecodeString(originalHead); err != nil {
		return "", "", errors.New("owned worktree ORIG_HEAD is ambiguous")
	}
	commitMessage := filepath.Join(admin, "COMMIT_EDITMSG")
	commitMessageDigest := cleanupRecoveryDigest("cleanup-admin-commit-message-v1", "absent")
	if _, err := os.Lstat(commitMessage); err == nil {
		content, err := readOwnedRegularFile(commitMessage, 1<<20)
		if err != nil {
			return "", "", errors.New("owned worktree commit message leaf is unsafe")
		}
		commitMessageDigest = cleanupRecoveryDigest("cleanup-admin-commit-message-v1", string(content))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("owned worktree commit message leaf is ambiguous")
	}
	refs, refsInfo, err := canonicalOwnedDirectory(filepath.Join(admin, "refs"))
	if err != nil || refs != filepath.Join(admin, "refs") || refsInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return "", "", errors.New("owned worktree private refs are unsafe")
	}
	refEntries, err := os.ReadDir(refs)
	if err != nil || len(refEntries) != 0 {
		return "", "", errors.New("owned worktree private operation refs remain")
	}
	logs := filepath.Join(admin, "logs")
	if _, err := os.Lstat(logs); errors.Is(err, os.ErrNotExist) {
		return originalHead, cleanupRecoveryDigest("cleanup-admin-state-v1", cleanupRecoveryDigest("cleanup-admin-orig-head-v1", string(origHead)), commitMessageDigest), nil
	}
	logEntries, err := os.ReadDir(logs)
	if err != nil || len(logEntries) > 1 || len(logEntries) == 1 && logEntries[0].Name() != "HEAD" {
		return "", "", errors.New("owned worktree has ambiguous reflog state")
	}
	return originalHead, cleanupRecoveryDigest("cleanup-admin-state-v1", cleanupRecoveryDigest("cleanup-admin-orig-head-v1", string(origHead)), commitMessageDigest), nil
}

func validateRecoveryReflog(admin string) error {
	logs := filepath.Join(admin, "logs")
	info, err := os.Lstat(logs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	canonical, canonicalInfo, err := canonicalOwnedDirectory(logs)
	if err != nil || canonical != logs || canonicalInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("owned worktree reflog directory is unsafe")
	}
	head := filepath.Join(logs, "HEAD")
	if _, err := os.Lstat(head); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("owned worktree reflog is ambiguous")
	}
	if _, err := readOwnedRegularFile(head, 64<<20); err != nil {
		return errors.New("owned worktree reflog is unsafe")
	}
	return nil
}

func readOwnedRegularFile(path string, maximum int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, errors.New("owned recovery file is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return nil, errors.New("owned recovery file identity is unsafe")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("owned recovery file is unreadable")
	}
	return content, nil
}

func (a CleanupSourceRecovery) recoveryWorktreeClean(ctx context.Context, admin, worktree, candidate string, detached bool) (bool, error) {
	environment := cleanupSourceRecoveryReadEnvironment
	gitDir := "--git-dir=" + admin
	workTree := "--work-tree=" + worktree
	for _, option := range []string{"-v", "-f"} {
		output, err := a.runWithEnvironment(ctx, worktree, environment, append(cleanupSourceRecoveryGitConfig, gitDir, workTree, "ls-files", option, "-z")...)
		if err != nil || maskedIndexEntries(output) {
			return false, err
		}
	}
	if detached {
		output, err := a.runWithEnvironment(ctx, worktree, environment, append(cleanupSourceRecoveryGitConfig, gitDir, workTree, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")...)
		return strings.TrimSpace(output) == "", err
	}
	if _, err := a.runWithEnvironment(ctx, worktree, environment, append(cleanupSourceRecoveryGitConfig, gitDir, workTree, "diff-index", "--quiet", candidate, "--")...); err != nil {
		return false, err
	}
	for _, args := range [][]string{{gitDir, workTree, "ls-files", "--others", "--exclude-standard"}, {gitDir, workTree, "ls-files", "--others", "--ignored", "--exclude-standard"}} {
		output, err := a.runWithEnvironment(ctx, worktree, environment, append(cleanupSourceRecoveryGitConfig, args...)...)
		if err != nil || strings.TrimSpace(output) != "" {
			return false, err
		}
	}
	return true, nil
}

func maskedIndexEntries(output string) bool {
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 3 || record[0] != 'H' || record[1] != ' ' {
			return true
		}
	}
	return false
}

func (a CleanupSourceRecovery) RepairCleanupWorktreeLink(ctx context.Context, request CleanupSourceRecoveryRequest) error {
	before, err := a.ObserveCleanupSourceRecovery(ctx, request)
	if err != nil {
		return err
	}
	if before.LinkRepaired {
		return nil
	}
	request.ExpectedRegistrationDigest = before.RegistrationDigest
	if _, err := a.runRecovery(ctx, request.ReplacementSourcePath, "worktree", "repair", "--no-relative-paths", request.WorktreePath); err != nil {
		return errors.New("worktree repair failed")
	}
	after, err := a.ObserveCleanupSourceRecovery(ctx, request)
	if err != nil || !after.LinkRepaired || after.HeadDetached || !after.WorktreeClean || !sameCleanupRecoveryIdentity(before, after) {
		return errors.New("worktree repair postcondition failed")
	}
	return nil
}

func (a CleanupSourceRecovery) DetachRecoveredWorktreeHead(ctx context.Context, request CleanupSourceRecoveryRequest) error {
	before, err := a.ObserveCleanupSourceRecovery(ctx, request)
	if err != nil || !before.WorktreePresent || !before.LinkRepaired || before.BranchPresent || !before.WorktreeClean {
		return errors.New("recovered worktree detach precondition failed")
	}
	if before.HeadDetached {
		return nil
	}
	request.ExpectedRegistrationDigest = before.RegistrationDigest
	commonRaw, err := a.observeRecovery(ctx, request.ReplacementSourcePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return errors.New("replacement common directory is unavailable")
	}
	common, err := canonicalPath(strings.TrimSpace(commonRaw))
	if err != nil {
		return errors.New("replacement common directory is unsafe")
	}
	admin, repaired, _, _, err := recoveryWorktreeAdmin(request.WorktreePath, request.FrozenSourcePath, common)
	if err != nil || !repaired {
		return errors.New("recovered worktree detach authority is unavailable")
	}
	indexBefore, err := readOwnedRegularFile(filepath.Join(admin, "index"), 64<<20)
	if err != nil {
		return errors.New("recovered worktree index is unavailable")
	}
	if _, err := a.runRecovery(ctx, request.WorktreePath, "update-ref", "--no-deref", "HEAD", request.CandidateHead); err != nil {
		return errors.New("recovered worktree HEAD detach failed")
	}
	indexAfter, err := readOwnedRegularFile(filepath.Join(admin, "index"), 64<<20)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) {
		return errors.New("recovered worktree detach changed the index")
	}
	after, err := a.ObserveCleanupSourceRecovery(ctx, request)
	if err != nil || !after.HeadDetached || !after.WorktreeClean || !sameCleanupRecoveryIdentity(before, after) {
		return errors.New("recovered worktree detach postcondition failed")
	}
	return nil
}

func (a CleanupSourceRecovery) RemoveRecoveredWorktree(ctx context.Context, request CleanupSourceRecoveryRequest) error {
	before, err := a.ObserveCleanupSourceRecovery(ctx, request)
	if err != nil {
		return errors.New("recovered worktree removal precondition failed")
	}
	if !before.WorktreePresent {
		return nil
	}
	if !before.LinkRepaired || !before.HeadDetached || before.BranchPresent || !before.WorktreeClean {
		return errors.New("recovered worktree removal precondition failed")
	}
	request.ExpectedRegistrationDigest = before.RegistrationDigest
	if _, err := a.runRecovery(ctx, request.ReplacementSourcePath, "worktree", "remove", request.WorktreePath); err != nil {
		return errors.New("recovered worktree removal failed")
	}
	after, err := a.ObserveCleanupSourceRecovery(ctx, request)
	if err != nil || after.WorktreePresent || after.BranchPresent || !sameCleanupRecoveryIdentity(before, after) {
		return errors.New("recovered worktree removal postcondition failed")
	}
	return nil
}

func sameCleanupRecoveryIdentity(left, right CleanupSourceRecoveryObservation) bool {
	return left.ReplacementSourceDigest == right.ReplacementSourceDigest &&
		left.ReplacementIdentityDigest == right.ReplacementIdentityDigest &&
		left.RepositoryOriginDigest == right.RepositoryOriginDigest &&
		left.RegistrationDigest == right.RegistrationDigest &&
		left.Branch == right.Branch && left.CandidateHead == right.CandidateHead
}

func validateCleanupSourceRecoveryRequest(request CleanupSourceRecoveryRequest) error {
	if strings.TrimSpace(request.Repository) == "" || !filepath.IsAbs(request.FrozenSourcePath) || !filepath.IsAbs(request.ReplacementSourcePath) || !filepath.IsAbs(request.WorktreePath) || !validOriginBinding(request.ExpectedOrigin) || domain.ValidateGitBranch(request.Branch) != nil || !validCleanupRecoveryHex(strings.TrimSpace(request.CandidateHead), 20) || request.ExpectedRegistrationDigest != "" && !validCleanupRecoveryHex(request.ExpectedRegistrationDigest, 32) {
		return errors.New("cleanup source recovery request is invalid")
	}
	return nil
}

func validCleanupRecoveryHex(value string, size int) bool {
	if len(value) != size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size
}

func canonicalOwnedDirectory(path string) (string, os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return "", nil, errors.New("path is not absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("path is not a safe directory")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != filepath.Clean(path) {
		return "", nil, errors.New("path is not canonical")
	}
	return canonical, info, nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (a CleanupSourceRecovery) registrations(ctx context.Context, source string) ([]recoveryRegistration, error) {
	raw, err := a.observeRecovery(ctx, source, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var result []recoveryRegistration
	for _, block := range strings.Split(strings.TrimSpace(raw), "\n\n") {
		var item recoveryRegistration
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				item.path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "HEAD "):
				item.head = strings.TrimPrefix(line, "HEAD ")
			case strings.HasPrefix(line, "branch refs/heads/"):
				item.branch = strings.TrimPrefix(line, "branch refs/heads/")
			}
		}
		if item.path != "" {
			canonical, canonicalErr := canonicalPathAllowMissing(item.path)
			if canonicalErr != nil {
				return nil, canonicalErr
			}
			item.path = canonical
			result = append(result, item)
		}
	}
	return result, nil
}

func (a CleanupSourceRecovery) registrationPathCount(ctx context.Context, source, worktree string) (int, error) {
	canonical, err := canonicalPathAllowMissing(worktree)
	if err != nil {
		return 0, err
	}
	items, err := a.registrations(ctx, source)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range items {
		if item.path == canonical {
			count++
		}
	}
	return count, nil
}

func (a CleanupSourceRecovery) localBranch(ctx context.Context, source, branch string) (string, bool, error) {
	ref := "refs/heads/" + branch
	if _, err := a.observeRecovery(ctx, source, "show-ref", "--verify", "--quiet", ref); err != nil {
		if isGitExit(err, 1) {
			return "", false, nil
		}
		return "", false, err
	}
	head, err := a.observeRecovery(ctx, source, "rev-parse", "--verify", ref+"^{commit}")
	return strings.TrimSpace(head), err == nil, err
}

func normalizedOriginBinding(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, ".git")
	return strings.ToLower(value)
}

func cleanupRecoveryDigest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cleanupSourcePathDigest(path string) string {
	sum := sha256.Sum256([]byte("cleanup-source-path-v1\x00" + path))
	return hex.EncodeToString(sum[:])
}
