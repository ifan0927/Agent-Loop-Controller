package domain

import (
	"fmt"
	"strings"
	"time"
)

type RepositoryIdentity struct {
	ID     int64  `json:"database_id"`
	NodeID string `json:"node_id"`
	Owner  string `json:"owner"`
	Name   string `json:"name"`
}

type ActorIdentity struct {
	DatabaseID int64  `json:"database_id"`
	NodeID     string `json:"node_id"`
	Login      string `json:"login"`
	Type       string `json:"type"`
	AppID      int64  `json:"app_id,omitempty"`
}

type CheckState string

const (
	CheckQueued         CheckState = "queued"
	CheckInProgress     CheckState = "in_progress"
	CheckPending        CheckState = "pending"
	CheckRequested      CheckState = "requested"
	CheckWaiting        CheckState = "waiting"
	CheckSuccess        CheckState = "success"
	CheckNeutral        CheckState = "neutral"
	CheckSkipped        CheckState = "skipped"
	CheckFailure        CheckState = "failure"
	CheckActionRequired CheckState = "action_required"
	CheckCancelled      CheckState = "cancelled"
	CheckTimedOut       CheckState = "timed_out"
	CheckStale          CheckState = "stale"
	CheckUnknown        CheckState = "unknown"
)

type GitHubCheck struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Required    bool       `json:"required"`
	Source      string     `json:"source"`
	AppID       int64      `json:"app_id,omitempty"`
	State       CheckState `json:"state"`
	ObservedSHA string     `json:"observed_sha"`
	SourceAt    time.Time  `json:"source_timestamp"`
	ObservedAt  time.Time  `json:"observation_timestamp"`
}

type NormalizedFinding struct {
	Source         string `json:"source"`
	SourceID       string `json:"source_id"`
	ThreadID       string `json:"thread_id,omitempty"`
	File           string `json:"file,omitempty"`
	Line           int    `json:"line,omitempty"`
	Classification string `json:"classification"`
	BodyDigest     string `json:"body_digest"`
	// Body is deliberately excluded from GitHub evidence JSON. It is retained
	// only in the bounded, controller-owned finding record used for repair.
	Body       string    `json:"-"`
	Resolved   bool      `json:"resolved"`
	Outdated   bool      `json:"outdated"`
	HeadSHA    string    `json:"observed_head_sha"`
	SourceAt   time.Time `json:"source_timestamp"`
	ObservedAt time.Time `json:"observation_timestamp"`
}

type GitHubReview struct {
	DatabaseID int64         `json:"database_id"`
	NodeID     string        `json:"node_id"`
	State      string        `json:"state"`
	Actor      ActorIdentity `json:"actor"`
	CommitSHA  string        `json:"commit_sha"`
	SourceAt   time.Time     `json:"source_timestamp"`
}

// GitHubReviewThread is the sanitized topology of one inline review thread.
type GitHubReviewThread struct {
	NodeID            string                `json:"node_id"`
	Resolved          bool                  `json:"resolved"`
	Outdated          bool                  `json:"outdated"`
	OriginalCommitSHA string                `json:"original_commit_sha"`
	Path              string                `json:"path,omitempty"`
	Line              *int                  `json:"line,omitempty"`
	Comments          []GitHubReviewComment `json:"comments"`
}

// GitHubReviewComment preserves the immutable remote identities and the
// root/reply relationship without interpreting its untrusted body.
type GitHubReviewComment struct {
	DatabaseID        int64          `json:"database_id"`
	NodeID            string         `json:"node_id"`
	ReplyToDatabaseID int64          `json:"reply_to_database_id,omitempty"`
	ReplyToNodeID     string         `json:"reply_to_node_id,omitempty"`
	Author            *ActorIdentity `json:"author,omitempty"`
	Review            GitHubReview   `json:"review"`
	BodyDigest        string         `json:"body_digest"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

// InlineReviewBodyHandoff is the only in-memory carrier for raw inline review
// bodies. It is deliberately separate from GitHubReadEvidence, which may be
// serialized by generic evidence stores.
type InlineReviewBodyHandoff struct {
	Comments []InlineReviewBody `json:"-"`
}

type InlineReviewBody struct {
	ThreadNodeID  string `json:"-"`
	CommentNodeID string `json:"-"`
	Body          string `json:"-"`
	BodyDigest    string `json:"-"`
}

// Validate enforces the same raw-text limits as the dedicated trusted-feedback
// store before any caller can consider persisting a body.
func (h InlineReviewBodyHandoff) Validate() error {
	if len(h.Comments) > MaxTrustedReviewFeedbackPerHead {
		return fmt.Errorf("inline review body count exceeds bounded limit")
	}
	total := 0
	seen := make(map[string]struct{}, len(h.Comments))
	for _, comment := range h.Comments {
		if strings.TrimSpace(comment.ThreadNodeID) == "" || strings.TrimSpace(comment.CommentNodeID) == "" || strings.ContainsRune(comment.ThreadNodeID, '\x00') || strings.ContainsRune(comment.CommentNodeID, '\x00') {
			return fmt.Errorf("inline review body identity is incomplete")
		}
		if _, exists := seen[comment.CommentNodeID]; exists {
			return fmt.Errorf("duplicate inline review body identity")
		}
		seen[comment.CommentNodeID] = struct{}{}
		bytes := len([]byte(comment.Body))
		if bytes == 0 || bytes > MaxTrustedReviewFeedbackBodyBytes || strings.ContainsRune(comment.Body, '\x00') {
			return fmt.Errorf("inline review body exceeds bounded limit")
		}
		if TrustedReviewFeedbackDigest(comment.Body) != comment.BodyDigest {
			return fmt.Errorf("inline review body digest does not match body")
		}
		total += bytes
		if total > MaxTrustedReviewFeedbackTextBytes {
			return fmt.Errorf("inline review body aggregate exceeds bounded limit")
		}
	}
	return nil
}

type HumanApprovalStatus string

const (
	HumanApprovalPending          HumanApprovalStatus = "pending"
	HumanApprovalApproved         HumanApprovalStatus = "approved"
	HumanApprovalDismissed        HumanApprovalStatus = "dismissed"
	HumanApprovalChangesRequested HumanApprovalStatus = "changes_requested"
	HumanApprovalStaleHead        HumanApprovalStatus = "stale_head"
	HumanApprovalUntrustedActor   HumanApprovalStatus = "untrusted_actor"
	HumanApprovalAmbiguous        HumanApprovalStatus = "ambiguous"
)

// HumanApprovalObservation is a sanitized, immutable-identity interpretation
// of the current GitHub review topology for one candidate head.
type HumanApprovalObservation struct {
	PRNumber         int64               `json:"pr_number"`
	CandidateHead    string              `json:"candidate_head"`
	Status           HumanApprovalStatus `json:"status"`
	ReviewDatabaseID int64               `json:"review_database_id,omitempty"`
	ReviewNodeID     string              `json:"review_node_id,omitempty"`
	Actor            ActorIdentity       `json:"actor,omitempty"`
	ReviewHeadSHA    string              `json:"review_head_sha,omitempty"`
	SourceAt         time.Time           `json:"source_timestamp,omitempty"`
	ObservedAt       time.Time           `json:"observation_timestamp"`
}

// NormalizeHumanApproval accepts only a configured immutable User identity.
// A matching login with different immutable identity is deliberately rejected,
// so bots, Apps, and lookalikes cannot become an approval by name alone.
func NormalizeHumanApproval(pr PullRequest, reviews []GitHubReview, trusted []ActorIdentity, observedAt time.Time) (HumanApprovalObservation, *HumanApproval, error) {
	if pr.Number < 1 || strings.TrimSpace(pr.HeadSHA) == "" || observedAt.IsZero() {
		return HumanApprovalObservation{}, nil, fmt.Errorf("pull request and observation timestamp are required")
	}
	trustedByLogin := make(map[string]ActorIdentity, len(trusted))
	for _, actor := range trusted {
		if actor.DatabaseID < 1 || strings.TrimSpace(actor.NodeID) == "" || strings.TrimSpace(actor.Login) == "" || actor.Type != "User" {
			return HumanApprovalObservation{}, nil, fmt.Errorf("trusted human actor identity is incomplete")
		}
		login := strings.ToLower(actor.Login)
		if _, exists := trustedByLogin[login]; exists {
			return HumanApprovalObservation{}, nil, fmt.Errorf("trusted human actor identity is ambiguous")
		}
		trustedByLogin[login] = actor
	}
	if len(trustedByLogin) == 0 {
		return HumanApprovalObservation{}, nil, fmt.Errorf("trusted human actor identity is required")
	}
	base := HumanApprovalObservation{PRNumber: pr.Number, CandidateHead: pr.HeadSHA, Status: HumanApprovalPending, ObservedAt: observedAt.UTC()}
	var untrusted *GitHubReview
	var ambiguous *GitHubReview
	latest := make(map[string]*GitHubReview, len(trustedByLogin))
	for index := range reviews {
		review := &reviews[index]
		configured, loginKnown := trustedByLogin[strings.ToLower(review.Actor.Login)]
		if !loginKnown {
			continue
		}
		if review.Actor.Type != "User" || review.Actor.DatabaseID != configured.DatabaseID || review.Actor.NodeID != configured.NodeID || !strings.EqualFold(review.Actor.Login, configured.Login) {
			untrusted = review
			continue
		}
		if review.DatabaseID < 1 || strings.TrimSpace(review.NodeID) == "" || review.SourceAt.IsZero() {
			ambiguous = review
			continue
		}
		key := review.Actor.NodeID
		if previous, found := latest[key]; found {
			if review.SourceAt.Equal(previous.SourceAt) {
				ambiguous = review
				continue
			}
			if review.SourceAt.Before(previous.SourceAt) {
				continue
			}
		}
		latest[key] = review
	}
	var approval *GitHubReview
	var stale *GitHubReview
	var dismissal *GitHubReview
	var changes *GitHubReview
	for _, review := range latest {
		switch review.State {
		case "APPROVED":
			if review.CommitSHA == pr.HeadSHA {
				if approval != nil {
					ambiguous = review
					continue
				}
				approval = review
			} else {
				stale = review
			}
		case "DISMISSED":
			dismissal = review
		case "CHANGES_REQUESTED":
			changes = review
		default:
			ambiguous = review
		}
	}
	selected := func(status HumanApprovalStatus, review *GitHubReview) HumanApprovalObservation {
		result := base
		result.Status = status
		if review != nil {
			result.ReviewDatabaseID, result.ReviewNodeID, result.Actor, result.ReviewHeadSHA, result.SourceAt = review.DatabaseID, review.NodeID, review.Actor, review.CommitSHA, review.SourceAt.UTC()
		}
		return result
	}
	switch {
	case changes != nil:
		return selected(HumanApprovalChangesRequested, changes), nil, nil
	case dismissal != nil:
		return selected(HumanApprovalDismissed, dismissal), nil, nil
	case ambiguous != nil:
		return selected(HumanApprovalAmbiguous, ambiguous), nil, nil
	case untrusted != nil:
		return selected(HumanApprovalUntrustedActor, untrusted), nil, nil
	case approval != nil:
		observation := selected(HumanApprovalApproved, approval)
		return observation, &HumanApproval{PRNumber: pr.Number, Approver: approval.Actor.Login, Actor: approval.Actor, ReviewDatabaseID: approval.DatabaseID, ReviewNodeID: approval.NodeID, Source: "github_pull_request_review", ApprovedSHA: pr.HeadSHA, CIStatus: "pass", ReviewSHA: pr.HeadSHA, ApprovedAt: approval.SourceAt.UTC(), ObservedAt: observedAt.UTC()}, nil
	case stale != nil:
		return selected(HumanApprovalStaleHead, stale), nil, nil
	default:
		return base, nil, nil
	}
}

type GitHubReadEvidence struct {
	Repository    RepositoryIdentity   `json:"repository"`
	PullRequest   PullRequest          `json:"pull_request"`
	Checks        []GitHubCheck        `json:"checks"`
	Findings      []NormalizedFinding  `json:"findings"`
	Reviews       []GitHubReview       `json:"reviews"`
	ReviewThreads []GitHubReviewThread `json:"review_threads"`
	UnknownEvents []string             `json:"unknown_telemetry"`
	ObservedAt    time.Time            `json:"observed_at"`
}

func (e GitHubReadEvidence) RequiredChecksStatus() ReconciliationStatus {
	required := 0
	for _, check := range e.Checks {
		if !check.Required {
			continue
		}
		required++
		if check.ObservedSHA != e.PullRequest.HeadSHA {
			return ReconciliationInfrastructure
		}
		switch check.State {
		case CheckSuccess, CheckNeutral, CheckSkipped:
		case CheckQueued, CheckInProgress, CheckPending, CheckRequested, CheckWaiting:
			return ReconciliationPending
		case CheckFailure, CheckActionRequired:
			return ReconciliationActionable
		case CheckCancelled, CheckTimedOut, CheckStale, CheckUnknown:
			return ReconciliationInfrastructure
		default:
			return ReconciliationInfrastructure
		}
	}
	if required == 0 {
		return ReconciliationInfrastructure
	}
	for _, event := range e.UnknownEvents {
		if strings.HasPrefix(event, "missing_required_check:") {
			return ReconciliationInfrastructure
		}
	}
	return ReconciliationPass
}

// DeliveryStatus classifies the GitHub evidence that gates the delivery loop.
// Unknown or incomplete observations deliberately never authorize a later
// lifecycle state.
func (e GitHubReadEvidence) DeliveryStatus() ReconciliationStatus {
	if !strings.EqualFold(e.PullRequest.State, "open") || e.PullRequest.Merged {
		return ReconciliationInfrastructure
	}
	if status := e.RequiredChecksStatus(); status != ReconciliationPass {
		return status
	}
	if len(e.UnknownEvents) > 0 {
		return ReconciliationInfrastructure
	}
	return ReconciliationPass
}
