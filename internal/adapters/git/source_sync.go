package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type SourceSyncStatus string

const (
	SourceSyncSynced           SourceSyncStatus = "synced"
	SourceSyncSkippedAttention SourceSyncStatus = "skipped_attention"
	SourceSyncRetryableFailure SourceSyncStatus = "retryable_failure"
)

type SourceSyncOutcome string

const (
	SourceSyncFastForwarded         SourceSyncOutcome = "fast_forwarded"
	SourceSyncAlreadyAtTarget       SourceSyncOutcome = "already_at_target"
	SourceSyncAlreadyContainsTarget SourceSyncOutcome = "already_contains_target"
	SourceSyncNotApplied            SourceSyncOutcome = "not_applied"
)

type SourceSyncReason string

const (
	SourceSyncReasonNone           SourceSyncReason = ""
	SourceSyncReasonDirtySource    SourceSyncReason = "dirty_source"
	SourceSyncReasonWrongBranch    SourceSyncReason = "wrong_branch"
	SourceSyncReasonDetachedHead   SourceSyncReason = "detached_head"
	SourceSyncReasonSourceDiverged SourceSyncReason = "source_diverged"
	SourceSyncReasonStateDrift     SourceSyncReason = "state_drift"
	SourceSyncReasonFetchFailed    SourceSyncReason = "fetch_failed"
	SourceSyncReasonGitUncertain   SourceSyncReason = "git_uncertain"
)

type SourceSyncRequest struct {
	Repository string
	SourcePath string
	OriginPath string
	BaseBranch string
	MergeSHA   string
}

// SourceSyncResult intentionally contains only persisted Git identifiers and
// allowlisted classifications. It must be safe to project to operator records.
type SourceSyncResult struct {
	Status    SourceSyncStatus
	Outcome   SourceSyncOutcome
	Reason    SourceSyncReason
	BeforeSHA string
	AfterSHA  string
	MergeSHA  string
}

// SourceSynchronizer advances a configured source checkout only by a guarded
// exact fast-forward. It is deliberately independent of cleanup orchestration.
type SourceSynchronizer struct {
	Workspace

	// run and afterFetch are private deterministic test seams. Production uses
	// the argv-only OS command path and has no callback.
	run        func(context.Context, string, ...string) (string, error)
	afterFetch func()
}

var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func (s SourceSynchronizer) Sync(ctx context.Context, request SourceSyncRequest) (SourceSyncResult, error) {
	if err := validateSourceSyncRequest(request); err != nil {
		return SourceSyncResult{}, err
	}
	result := SourceSyncResult{MergeSHA: request.MergeSHA}

	if err := s.validateOrigin(ctx, request); err != nil {
		return SourceSyncResult{}, err
	}
	before, result, err := s.observe(ctx, request, result)
	if err != nil {
		return s.retry(result, SourceSyncReasonGitUncertain), nil
	}
	if skip, reason := sourceSyncPrecondition(before, request.BaseBranch); skip {
		return s.skip(result, before.head, reason), nil
	}

	if _, err := s.command(ctx, request.SourcePath, "fetch", "--no-tags", "origin", "refs/heads/"+request.BaseBranch); err != nil {
		return s.reconcileFetchFailure(ctx, request, result), nil
	}
	if s.afterFetch != nil {
		s.afterFetch()
	}
	kind, err := s.command(ctx, request.SourcePath, "cat-file", "-t", request.MergeSHA)
	if err != nil || strings.TrimSpace(kind) != "commit" {
		return SourceSyncResult{}, errors.New("sync merge authority could not be proven")
	}
	reachable, err := s.isAncestor(ctx, request.SourcePath, request.MergeSHA, "FETCH_HEAD")
	if err != nil {
		return SourceSyncResult{}, errors.New("sync merge authority could not be proven")
	}
	if !reachable {
		return SourceSyncResult{}, errors.New("sync merge authority could not be proven")
	}

	if err := s.validateOrigin(ctx, request); err != nil {
		return SourceSyncResult{}, err
	}
	afterFetch, result, err := s.observe(ctx, request, result)
	if err != nil {
		return s.retry(result, SourceSyncReasonGitUncertain), nil
	}
	if skip, _ := sourceSyncPrecondition(afterFetch, request.BaseBranch); skip {
		return s.skip(result, afterFetch.head, SourceSyncReasonStateDrift), nil
	}
	if afterFetch.head != before.head {
		return s.skip(result, afterFetch.head, SourceSyncReasonStateDrift), nil
	}
	if afterFetch.head == request.MergeSHA {
		return s.synced(result, SourceSyncAlreadyAtTarget, afterFetch.head), nil
	}
	contains, err := s.isAncestor(ctx, request.SourcePath, request.MergeSHA, afterFetch.head)
	if err != nil {
		return s.retry(result, SourceSyncReasonGitUncertain), nil
	}
	if contains {
		return s.synced(result, SourceSyncAlreadyContainsTarget, afterFetch.head), nil
	}
	canFastForward, err := s.isAncestor(ctx, request.SourcePath, afterFetch.head, request.MergeSHA)
	if err != nil {
		return s.retry(result, SourceSyncReasonGitUncertain), nil
	}
	if !canFastForward {
		return s.skip(result, afterFetch.head, SourceSyncReasonSourceDiverged), nil
	}

	_, mergeErr := s.command(ctx, request.SourcePath, "merge", "--ff-only", "--no-edit", request.MergeSHA)
	reconciled, result, observeErr := s.observe(ctx, request, result)
	if observeErr == nil && reconciled.branch == request.BaseBranch && reconciled.status == "" && reconciled.head == request.MergeSHA {
		return s.synced(result, SourceSyncFastForwarded, reconciled.head), nil
	}
	if mergeErr != nil || observeErr != nil {
		if observeErr == nil {
			result.AfterSHA = reconciled.head
		}
		return s.retry(result, SourceSyncReasonGitUncertain), nil
	}
	result.AfterSHA = reconciled.head
	return s.retry(result, SourceSyncReasonGitUncertain), nil
}

type sourceSyncObservation struct {
	branch string
	head   string
	status string
}

func (s SourceSynchronizer) observe(ctx context.Context, request SourceSyncRequest, result SourceSyncResult) (sourceSyncObservation, SourceSyncResult, error) {
	branch, err := s.command(ctx, request.SourcePath, "branch", "--show-current")
	if err != nil {
		return sourceSyncObservation{}, result, err
	}
	head, err := s.command(ctx, request.SourcePath, "rev-parse", "HEAD")
	if err != nil {
		return sourceSyncObservation{}, result, err
	}
	status, err := s.command(ctx, request.SourcePath, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return sourceSyncObservation{}, result, err
	}
	head = strings.TrimSpace(head)
	if result.BeforeSHA == "" {
		result.BeforeSHA = head
	}
	return sourceSyncObservation{branch: strings.TrimSpace(branch), head: head, status: strings.TrimSpace(status)}, result, nil
}

func (s SourceSynchronizer) validateOrigin(ctx context.Context, request SourceSyncRequest) error {
	remote, err := s.command(ctx, request.SourcePath, "remote", "get-url", "origin")
	if err != nil || !sameOriginBinding(strings.TrimSpace(remote), request.OriginPath) {
		return errors.New("sync source origin authority mismatch")
	}
	return nil
}

func validateSourceSyncRequest(request SourceSyncRequest) error {
	if !validRepositoryIdentity(request.Repository) || domain.ValidateGitBranch(request.BaseBranch) != nil || !fullSHA.MatchString(request.MergeSHA) || request.MergeSHA != strings.ToLower(request.MergeSHA) || !validOriginBinding(request.OriginPath) {
		return errors.New("sync authority is invalid")
	}
	if origin, err := canonicalGitHubRemoteURL(request.OriginPath); err == nil && githubRemoteIdentity(origin) != request.Repository {
		return errors.New("sync authority is invalid")
	}
	if !filepath.IsAbs(request.SourcePath) || filepath.Clean(request.SourcePath) != request.SourcePath {
		return errors.New("sync source authority is invalid")
	}
	info, err := os.Lstat(request.SourcePath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("sync source authority is invalid")
	}
	canonical, err := filepath.EvalSymlinks(request.SourcePath)
	if err != nil || canonical != request.SourcePath {
		return errors.New("sync source authority is invalid")
	}
	return nil
}

func validRepositoryIdentity(value string) bool {
	value = strings.TrimSpace(value)
	owner, name, err := githubRemotePath(value + ".git")
	return err == nil && value == strings.ToLower(owner)+"/"+strings.ToLower(name)
}

func sourceSyncPrecondition(observation sourceSyncObservation, base string) (bool, SourceSyncReason) {
	if observation.branch == "" {
		return true, SourceSyncReasonDetachedHead
	}
	if observation.branch != base {
		return true, SourceSyncReasonWrongBranch
	}
	if observation.status != "" {
		return true, SourceSyncReasonDirtySource
	}
	return false, SourceSyncReasonNone
}

func (s SourceSynchronizer) synced(result SourceSyncResult, outcome SourceSyncOutcome, head string) SourceSyncResult {
	result.Status, result.Outcome, result.Reason, result.AfterSHA = SourceSyncSynced, outcome, SourceSyncReasonNone, head
	return result
}

func (s SourceSynchronizer) skip(result SourceSyncResult, head string, reason SourceSyncReason) SourceSyncResult {
	result.Status, result.Outcome, result.Reason, result.AfterSHA = SourceSyncSkippedAttention, SourceSyncNotApplied, reason, head
	return result
}

func (s SourceSynchronizer) retry(result SourceSyncResult, reason SourceSyncReason) SourceSyncResult {
	result.Status, result.Outcome, result.Reason = SourceSyncRetryableFailure, SourceSyncNotApplied, reason
	return result
}

func (s SourceSynchronizer) reconcileFetchFailure(ctx context.Context, request SourceSyncRequest, result SourceSyncResult) SourceSyncResult {
	reconciled, result, err := s.observe(ctx, request, result)
	if err == nil {
		result.AfterSHA = reconciled.head
	}
	return s.retry(result, SourceSyncReasonFetchFailed)
}

func (s SourceSynchronizer) isAncestor(ctx context.Context, directory, ancestor, descendant string) (bool, error) {
	_, err := s.command(ctx, directory, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var commandError gitCommandError
	if errors.As(err, &commandError) && commandError.exitCode == 1 {
		return false, nil
	}
	return false, err
}

func (s SourceSynchronizer) command(ctx context.Context, directory string, args ...string) (string, error) {
	if s.run != nil {
		return s.run(ctx, directory, args...)
	}
	return s.Workspace.run(ctx, directory, args...)
}
