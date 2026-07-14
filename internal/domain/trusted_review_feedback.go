package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	MaxTrustedReviewFeedbackBodyBytes = 16 * 1024
	MaxTrustedReviewFeedbackPerHead   = 50
	MaxTrustedReviewFeedbackTextBytes = 64 * 1024
)

type TrustedReviewFeedbackLifecycle string

const (
	TrustedReviewFeedbackObserved          TrustedReviewFeedbackLifecycle = "observed"
	TrustedReviewFeedbackSelectedForRepair TrustedReviewFeedbackLifecycle = "selected_for_repair"
	TrustedReviewFeedbackRepairVerified    TrustedReviewFeedbackLifecycle = "repair_verified"
	TrustedReviewFeedbackReplyPending      TrustedReviewFeedbackLifecycle = "reply_pending"
	TrustedReviewFeedbackReplied           TrustedReviewFeedbackLifecycle = "replied"
	TrustedReviewFeedbackResolved          TrustedReviewFeedbackLifecycle = "resolved"
	TrustedReviewFeedbackSuperseded        TrustedReviewFeedbackLifecycle = "superseded"
)

// TrustedReviewFeedback is a bounded observation of one trusted human inline
// review root comment. Its identifiers and first accepted body are authority,
// not a mutable cache of a remote conversation.
type TrustedReviewFeedback struct {
	PRNumber              int64                          `json:"pr_number"`
	PRDatabaseID          int64                          `json:"pr_database_id"`
	PRNodeID              string                         `json:"pr_node_id"`
	ReviewDatabaseID      int64                          `json:"review_database_id"`
	ReviewNodeID          string                         `json:"review_node_id"`
	ThreadNodeID          string                         `json:"thread_node_id"`
	RootCommentDatabaseID int64                          `json:"root_comment_database_id"`
	RootCommentNodeID     string                         `json:"root_comment_node_id"`
	Author                ActorIdentity                  `json:"author"`
	OriginalReviewHeadSHA string                         `json:"original_review_head_sha"`
	Path                  string                         `json:"path,omitempty"`
	Line                  *int                           `json:"line,omitempty"`
	Body                  string                         `json:"-"`
	BodyDigest            string                         `json:"body_digest"`
	SourceAt              time.Time                      `json:"source_timestamp"`
	ObservedAt            time.Time                      `json:"observation_timestamp"`
	Lifecycle             TrustedReviewFeedbackLifecycle `json:"lifecycle"`
	BoundRepairHead       string                         `json:"bound_repair_head,omitempty"`
	ReplyIntentKey        string                         `json:"reply_intent_key,omitempty"`
	ReplyDatabaseID       int64                          `json:"reply_database_id,omitempty"`
	ReplyNodeID           string                         `json:"reply_node_id,omitempty"`
	Resolved              bool                           `json:"resolved"`
	Outdated              bool                           `json:"outdated"`
	UpdatedAt             time.Time                      `json:"updated_at"`
}

func (f TrustedReviewFeedback) ValidateObservation() error {
	for _, value := range []struct {
		name string
		id   int64
	}{
		{"pull request", f.PRNumber}, {"pull request database", f.PRDatabaseID}, {"review", f.ReviewDatabaseID}, {"root comment", f.RootCommentDatabaseID}, {"author", f.Author.DatabaseID},
	} {
		if value.id < 1 {
			return fmt.Errorf("%s ID must be positive", value.name)
		}
	}
	for _, value := range []struct{ name, text string }{
		{"pull request node", f.PRNodeID}, {"review node", f.ReviewNodeID}, {"thread node", f.ThreadNodeID}, {"root comment node", f.RootCommentNodeID}, {"author node", f.Author.NodeID}, {"author login", f.Author.Login},
	} {
		if !validTrustedReviewFeedbackNodeID(value.text) {
			return fmt.Errorf("%s ID is required", value.name)
		}
	}
	if f.Author.Type != "User" {
		return errors.New("trusted feedback author must be a User")
	}
	if !fullSHA(f.OriginalReviewHeadSHA) {
		return errors.New("original review head must be a full SHA")
	}
	if f.SourceAt.IsZero() || f.ObservedAt.IsZero() {
		return errors.New("feedback source and observation timestamps are required")
	}
	if strings.ContainsRune(f.Body, '\x00') || len([]byte(f.Body)) == 0 || len([]byte(f.Body)) > MaxTrustedReviewFeedbackBodyBytes {
		return errors.New("feedback body is empty, contains NUL, or exceeds its bound")
	}
	digest := feedbackDigest(f.Body)
	if f.BodyDigest == "" {
		f.BodyDigest = digest
	}
	if f.BodyDigest != digest {
		return errors.New("feedback body digest does not match body")
	}
	if f.Path != "" && !safeRepositoryPath(f.Path) {
		return errors.New("feedback path is not a sanitized repository path")
	}
	if f.Line != nil && (*f.Line < 1 || f.Path == "") {
		return errors.New("feedback line requires a positive value and repository path")
	}
	if f.Lifecycle != "" && f.Lifecycle != TrustedReviewFeedbackObserved {
		return errors.New("new feedback must start observed")
	}
	if f.BoundRepairHead != "" || f.ReplyIntentKey != "" || f.ReplyDatabaseID != 0 || f.ReplyNodeID != "" || f.Resolved || f.Outdated {
		return errors.New("new feedback must not include future lifecycle evidence")
	}
	return nil
}

func (f TrustedReviewFeedback) ImmutableEqual(other TrustedReviewFeedback) bool {
	return f.PRNumber == other.PRNumber && f.PRDatabaseID == other.PRDatabaseID && f.PRNodeID == other.PRNodeID &&
		f.ReviewDatabaseID == other.ReviewDatabaseID && f.ReviewNodeID == other.ReviewNodeID && f.ThreadNodeID == other.ThreadNodeID &&
		f.RootCommentDatabaseID == other.RootCommentDatabaseID && f.RootCommentNodeID == other.RootCommentNodeID &&
		f.Author.DatabaseID == other.Author.DatabaseID && f.Author.NodeID == other.Author.NodeID && f.Author.Login == other.Author.Login && f.Author.Type == other.Author.Type &&
		f.OriginalReviewHeadSHA == other.OriginalReviewHeadSHA && f.BodyDigest == other.BodyDigest && f.Body == other.Body
}

func ValidateTrustedReviewFeedbackTransition(expected, next TrustedReviewFeedbackLifecycle) error {
	switch expected {
	case TrustedReviewFeedbackObserved:
		if next == TrustedReviewFeedbackSelectedForRepair || next == TrustedReviewFeedbackSuperseded {
			return nil
		}
	case TrustedReviewFeedbackSelectedForRepair:
		if next == TrustedReviewFeedbackRepairVerified || next == TrustedReviewFeedbackSuperseded {
			return nil
		}
	case TrustedReviewFeedbackRepairVerified:
		if next == TrustedReviewFeedbackReplyPending || next == TrustedReviewFeedbackSuperseded {
			return nil
		}
	case TrustedReviewFeedbackReplyPending:
		if next == TrustedReviewFeedbackReplied || next == TrustedReviewFeedbackSuperseded {
			return nil
		}
	case TrustedReviewFeedbackReplied:
		if next == TrustedReviewFeedbackResolved || next == TrustedReviewFeedbackSuperseded {
			return nil
		}
	}
	return fmt.Errorf("illegal trusted feedback lifecycle transition: %s -> %s", expected, next)
}

func fullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func feedbackDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// TrustedReviewFeedbackDigest returns the canonical digest stored with a
// bounded feedback body without exposing that body in projections.
func TrustedReviewFeedbackDigest(body string) string { return feedbackDigest(body) }

func safeRepositoryPath(value string) bool {
	return value != "" && value != "." && value != ".." && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00') && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "~") && !strings.Contains(value, "\\") && path.Clean(value) == value && !strings.HasPrefix(value, "../")
}

func validTrustedReviewFeedbackNodeID(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00')
}
