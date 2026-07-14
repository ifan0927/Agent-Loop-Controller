package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
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
