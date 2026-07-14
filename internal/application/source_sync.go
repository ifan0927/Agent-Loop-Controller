package application

import "context"

// SourceSyncPort advances only the frozen source checkout binding to the exact
// persisted merge commit. It has no authority to repair or otherwise modify a
// checkout that needs operator attention.
type SourceSyncPort interface {
	Sync(context.Context, SourceSyncRequest) (SourceSyncResult, error)
}

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

// SourceSyncResult is deliberately bounded to exact Git identifiers and
// allowlisted classifications, so it can be projected through cleanup records.
type SourceSyncResult struct {
	Status    SourceSyncStatus
	Outcome   SourceSyncOutcome
	Reason    SourceSyncReason
	BeforeSHA string
	AfterSHA  string
	MergeSHA  string
}
