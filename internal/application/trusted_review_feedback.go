package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	TrustedReviewFeedbackDriftReason    = "trusted_review_feedback_drift"
	TrustedReviewFeedbackConflictReason = "trusted_review_feedback_conflict"
)

// TrustedReviewFeedbackRecord binds a single immutable root comment to one run.
// Body is only returned to controller-owned callers; public projections omit it.
type TrustedReviewFeedbackRecord struct {
	RunID string `json:"run_id"`
	domain.TrustedReviewFeedback
}

func (r TrustedReviewFeedbackRecord) ValidateObservation() error {
	if strings.TrimSpace(r.RunID) == "" || strings.ContainsRune(r.RunID, '\x00') {
		return errors.New("feedback run ID is required")
	}
	return r.TrustedReviewFeedback.ValidateObservation()
}

// ValidatePersistedAuthority verifies both the immutable observation and the
// lifecycle evidence accumulated by controller-owned compare-and-swap writes.
func (r TrustedReviewFeedbackRecord) ValidatePersistedAuthority(expectedRunID string) error {
	if strings.TrimSpace(expectedRunID) == "" || r.RunID != expectedRunID {
		return errors.New("feedback run authority does not match")
	}
	observation := r
	observation.Lifecycle = domain.TrustedReviewFeedbackObserved
	observation.BoundRepairHead = ""
	observation.ReplyIntentKey = ""
	observation.ReplyDatabaseID = 0
	observation.ReplyNodeID = ""
	observation.Resolved = false
	observation.Outdated = false
	observation.UpdatedAt = time.Time{}
	if err := observation.ValidateObservation(); err != nil {
		return err
	}
	if r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.ObservedAt) {
		return errors.New("feedback lifecycle timestamp is invalid")
	}
	noRepair := r.BoundRepairHead == ""
	validRepair := validFullSHA(r.BoundRepairHead)
	noIntent := strings.TrimSpace(r.ReplyIntentKey) == ""
	noReply := r.ReplyDatabaseID == 0 && r.ReplyNodeID == ""
	validReply := r.ReplyDatabaseID > 0 && validPersistedFeedbackNodeID(r.ReplyNodeID)
	if r.Lifecycle != domain.TrustedReviewFeedbackResolved && (r.Resolved || r.Outdated) {
		return errors.New("feedback lifecycle includes impossible resolution state")
	}
	switch r.Lifecycle {
	case domain.TrustedReviewFeedbackObserved, domain.TrustedReviewFeedbackSelectedForRepair:
		if !noRepair || !noIntent || !noReply {
			return errors.New("feedback lifecycle includes premature repair or reply evidence")
		}
	case domain.TrustedReviewFeedbackRepairVerified:
		if !validRepair || !noIntent || !noReply {
			return errors.New("verified feedback repair evidence is incomplete")
		}
	case domain.TrustedReviewFeedbackReplyPending:
		if !validRepair || noIntent || !noReply {
			return errors.New("pending feedback reply evidence is incomplete")
		}
	case domain.TrustedReviewFeedbackReplied:
		if !validRepair || noIntent || !validReply {
			return errors.New("feedback reply evidence is incomplete")
		}
	case domain.TrustedReviewFeedbackResolved:
		if !r.Resolved || !validRepair || (!noReply && !validReply) || (validReply && noIntent) {
			return errors.New("feedback resolution evidence is inconsistent")
		}
	case domain.TrustedReviewFeedbackSuperseded:
		if !(noRepair && noIntent && noReply) &&
			!(validRepair && noIntent && noReply) &&
			!(validRepair && !noIntent && (noReply || validReply)) {
			return errors.New("superseded feedback lifecycle evidence is inconsistent")
		}
	default:
		return errors.New("feedback lifecycle is unknown")
	}
	return nil
}

func validPersistedFeedbackNodeID(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00')
}

type TrustedReviewFeedbackConflict struct {
	ID                int64     `json:"conflict_id"`
	RunID             string    `json:"run_id"`
	RootCommentNodeID string    `json:"root_comment_node_id"`
	ObservedDigest    string    `json:"observed_body_digest"`
	ReasonCode        string    `json:"reason_code"`
	ObservedAt        time.Time `json:"observed_at"`
}

// TrustedReviewFeedbackStore deliberately provides only immutable observation
// and CAS lifecycle operations. It has no GitHub or side-effect capability.
type TrustedReviewFeedbackStore interface {
	SaveTrustedReviewFeedback(context.Context, TrustedReviewFeedbackRecord) (TrustedReviewFeedbackRecord, bool, error)
	TransitionTrustedReviewFeedback(context.Context, string, string, domain.TrustedReviewFeedbackLifecycle, domain.TrustedReviewFeedbackLifecycle, string, string, int64, string, bool, bool) (TrustedReviewFeedbackRecord, bool, error)
}

// TrustedReviewFeedbackDriftStore atomically records sanitized authority drift
// and halts the exact leased run. It never accepts raw review text.
type TrustedReviewFeedbackDriftStore interface {
	RequireManualInterventionForTrustedFeedbackDrift(context.Context, string, string, domain.State, string, []GitHubRequestObservation, domain.PullRequest, GitHubInstallationMetadata, domain.GitHubReadEvidence, string, string) error
}
