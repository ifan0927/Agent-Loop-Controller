package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TrustedChangesRequested is the bounded result of interpreting inline review
// topology. It deliberately contains no remote capability: comment bodies are
// quoted data that can only enter the controller-owned repair prompt.
type TrustedChangesRequested struct {
	Feedback          []TrustedReviewFeedback
	Findings          []NormalizedFinding
	Unsupported       bool
	UnsupportedReason TrustedReviewTopologyReason
}

// TrustedReviewTopologyReason is a finite, prose-free classification for
// trusted review shapes that cannot authorize an automatic repair.
type TrustedReviewTopologyReason string

const (
	TrustedReviewTopologyUnsupported TrustedReviewTopologyReason = "trusted_review_topology_unsupported"
	TrustedReviewTopologySplitReview TrustedReviewTopologyReason = "trusted_review_topology_split_review"
)

// NormalizeTrustedChangesRequested accepts only an unresolved inline root
// comment made by the same configured User that submitted an exact-head
// CHANGES_REQUESTED review. All other GitHub conversation data is inert.
func NormalizeTrustedChangesRequested(pr PullRequest, reviews []GitHubReview, threads []GitHubReviewThread, bodies InlineReviewBodyHandoff, trusted []ActorIdentity, observedAt time.Time) (TrustedChangesRequested, error) {
	if pr.Number < 1 || pr.DatabaseID < 1 || !validTrustedReviewFeedbackNodeID(pr.NodeID) || !fullSHA(pr.HeadSHA) || observedAt.IsZero() {
		return TrustedChangesRequested{}, fmt.Errorf("pull request identity and exact head are required")
	}
	if err := bodies.Validate(); err != nil {
		return TrustedChangesRequested{}, err
	}
	trustedByLogin := make(map[string]ActorIdentity, len(trusted))
	for _, actor := range trusted {
		if actor.DatabaseID < 1 || !validTrustedReviewFeedbackNodeID(actor.NodeID) || strings.TrimSpace(actor.Login) == "" || actor.Type != "User" {
			return TrustedChangesRequested{}, fmt.Errorf("trusted operator identity is incomplete")
		}
		key := strings.ToLower(actor.Login)
		if _, exists := trustedByLogin[key]; exists {
			return TrustedChangesRequested{}, fmt.Errorf("trusted operator identity is ambiguous")
		}
		trustedByLogin[key] = actor
	}
	bodyByComment := make(map[string]InlineReviewBody, len(bodies.Comments))
	for _, body := range bodies.Comments {
		bodyByComment[body.CommentNodeID] = body
	}
	result := TrustedChangesRequested{}
	trustedExactReviews := make(map[string]GitHubReview)
	for _, review := range reviews {
		if review.State == "CHANGES_REQUESTED" && review.CommitSHA == pr.HeadSHA && review.DatabaseID > 0 && validTrustedReviewFeedbackNodeID(review.NodeID) && !review.SourceAt.IsZero() && sameTrustedActor(review.Actor, trustedByLogin) {
			trustedExactReviews[review.NodeID] = review
		}
	}
	trustedExactReview := len(trustedExactReviews) > 0
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			if comment.Author == nil || comment.Review.State != "COMMENTED" || comment.Review.CommitSHA != pr.HeadSHA || !sameTrustedActor(*comment.Author, trustedByLogin) || !sameTrustedActor(comment.Review.Actor, trustedByLogin) || !sameActor(*comment.Author, comment.Review.Actor) {
				continue
			}
			if _, valid := trustedInlineRootObservation(pr, thread, comment, bodyByComment, observedAt); !valid {
				continue
			}
			for _, review := range trustedExactReviews {
				if review.NodeID != comment.Review.NodeID && sameActor(review.Actor, comment.Review.Actor) {
					markUnsupportedTrustedReviewTopology(&result, TrustedReviewTopologySplitReview)
				}
			}
		}
	}
	admittedByReview := make(map[string]bool, len(trustedExactReviews))
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			linked, linkedFound := trustedExactReviews[comment.Review.NodeID]
			if !linkedFound || comment.Review.DatabaseID != linked.DatabaseID || comment.Review.State != linked.State || comment.Review.CommitSHA != linked.CommitSHA || comment.Author == nil || !sameTrustedActor(*comment.Author, trustedByLogin) || !sameActor(*comment.Author, linked.Actor) {
				continue
			}
			feedback, valid := trustedInlineRootObservation(pr, thread, comment, bodyByComment, observedAt)
			if !valid {
				markUnsupportedTrustedReviewTopology(&result, TrustedReviewTopologyUnsupported)
				continue
			}
			result.Feedback = append(result.Feedback, feedback)
			line := 0
			if thread.Line != nil {
				line = *thread.Line
			}
			result.Findings = append(result.Findings, NormalizedFinding{Source: "github_human_review_comment", SourceID: comment.NodeID, ThreadID: thread.NodeID, File: thread.Path, Line: line, Classification: "trusted_changes_requested", BodyDigest: feedback.BodyDigest, Body: feedback.Body, HeadSHA: pr.HeadSHA, SourceAt: comment.CreatedAt.UTC(), ObservedAt: observedAt.UTC()})
			admittedByReview[comment.Review.NodeID] = true
		}
	}
	if trustedExactReview && len(result.Feedback) == 0 {
		markUnsupportedTrustedReviewTopology(&result, TrustedReviewTopologyUnsupported)
	}
	for reviewID := range trustedExactReviews {
		if !admittedByReview[reviewID] {
			markUnsupportedTrustedReviewTopology(&result, TrustedReviewTopologyUnsupported)
		}
	}
	sort.Slice(result.Feedback, func(i, j int) bool {
		return result.Feedback[i].RootCommentNodeID < result.Feedback[j].RootCommentNodeID
	})
	sort.Slice(result.Findings, func(i, j int) bool { return result.Findings[i].SourceID < result.Findings[j].SourceID })
	return result, nil
}

func trustedInlineRootObservation(pr PullRequest, thread GitHubReviewThread, comment GitHubReviewComment, bodies map[string]InlineReviewBody, observedAt time.Time) (TrustedReviewFeedback, bool) {
	if comment.Author == nil || thread.Resolved || thread.Outdated || comment.ReplyToDatabaseID != 0 || comment.ReplyToNodeID != "" || comment.UpdatedAt.IsZero() || comment.UpdatedAt.Before(comment.CreatedAt) || comment.Review.SourceAt.IsZero() || comment.Review.CommitSHA != pr.HeadSHA || thread.OriginalCommitSHA != pr.HeadSHA {
		return TrustedReviewFeedback{}, false
	}
	body, found := bodies[comment.NodeID]
	if !found || body.ThreadNodeID != thread.NodeID || body.BodyDigest != comment.BodyDigest {
		return TrustedReviewFeedback{}, false
	}
	feedback := TrustedReviewFeedback{PRNumber: pr.Number, PRDatabaseID: pr.DatabaseID, PRNodeID: pr.NodeID, ReviewDatabaseID: comment.Review.DatabaseID, ReviewNodeID: comment.Review.NodeID, ThreadNodeID: thread.NodeID, RootCommentDatabaseID: comment.DatabaseID, RootCommentNodeID: comment.NodeID, Author: *comment.Author, OriginalReviewHeadSHA: pr.HeadSHA, Path: thread.Path, Line: thread.Line, Body: body.Body, BodyDigest: body.BodyDigest, SourceAt: comment.CreatedAt.UTC(), ObservedAt: observedAt.UTC()}
	return feedback, feedback.ValidateObservation() == nil
}

func markUnsupportedTrustedReviewTopology(result *TrustedChangesRequested, reason TrustedReviewTopologyReason) {
	result.Unsupported = true
	// The observed split-review shape is more specific than the generic finite
	// fallback and must survive other unsupported facts in the same snapshot.
	if result.UnsupportedReason == "" || reason == TrustedReviewTopologySplitReview {
		result.UnsupportedReason = reason
	}
}

// TrustedFeedbackDrift reports whether the next stable GitHub observation no
// longer proves the immutable authority that was admitted for an unfinished
// feedback item. It compares raw topology/body handoff rather than relying on
// the actionable normalizer, because a resolved or stale item must not simply
// disappear from authority checking.
func TrustedFeedbackDrift(existing []TrustedReviewFeedback, pr PullRequest, reviews []GitHubReview, threads []GitHubReviewThread, bodies InlineReviewBodyHandoff) bool {
	if err := bodies.Validate(); err != nil {
		return true
	}
	reviewsByNode := make(map[string]GitHubReview, len(reviews))
	for _, review := range reviews {
		reviewsByNode[review.NodeID] = review
	}
	bodyByNode := make(map[string]InlineReviewBody, len(bodies.Comments))
	for _, body := range bodies.Comments {
		bodyByNode[body.CommentNodeID] = body
	}
	for _, saved := range existing {
		if saved.Lifecycle != "" && saved.Lifecycle != TrustedReviewFeedbackObserved && saved.Lifecycle != TrustedReviewFeedbackSelectedForRepair {
			continue
		}
		if saved.PRNumber != pr.Number || saved.PRDatabaseID != pr.DatabaseID || saved.PRNodeID != pr.NodeID || saved.OriginalReviewHeadSHA != pr.HeadSHA {
			return true
		}
		found := false
		for _, thread := range threads {
			if thread.NodeID != saved.ThreadNodeID || thread.Resolved || thread.Outdated || thread.OriginalCommitSHA != saved.OriginalReviewHeadSHA || thread.Path != saved.Path || !sameLine(thread.Line, saved.Line) {
				continue
			}
			for _, comment := range thread.Comments {
				review, reviewFound := reviewsByNode[comment.Review.NodeID]
				body, bodyFound := bodyByNode[comment.NodeID]
				if comment.NodeID == saved.RootCommentNodeID && comment.DatabaseID == saved.RootCommentDatabaseID && comment.ReplyToDatabaseID == 0 && comment.ReplyToNodeID == "" && comment.Author != nil && sameActor(*comment.Author, saved.Author) && reviewFound && review.DatabaseID == saved.ReviewDatabaseID && review.NodeID == saved.ReviewNodeID && review.State == "CHANGES_REQUESTED" && review.CommitSHA == saved.OriginalReviewHeadSHA && sameActor(review.Actor, saved.Author) && bodyFound && body.ThreadNodeID == saved.ThreadNodeID && body.BodyDigest == saved.BodyDigest && body.Body == saved.Body {
					found = true
				}
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func sameLine(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameTrustedActor(actor ActorIdentity, trusted map[string]ActorIdentity) bool {
	configured, found := trusted[strings.ToLower(actor.Login)]
	return found && sameActor(actor, configured)
}

func sameActor(a, b ActorIdentity) bool {
	return a.Type == "User" && b.Type == "User" && a.DatabaseID == b.DatabaseID && a.NodeID == b.NodeID && strings.EqualFold(a.Login, b.Login)
}
