package application

import (
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestMergePolicyStatusAcceptsOnlyDocumentedRejections(t *testing.T) {
	for _, status := range []int{405, 409, 422} {
		if !mergePolicyStatus(&MergeRejectedError{HTTPStatus: status, Operation: "squash_merge_pull_request"}, nil) {
			t.Fatalf("status %d must be eligible for a fresh policy read", status)
		}
	}
	for _, status := range []int{0, 403, 404, 429, 500} {
		if mergePolicyStatus(&MergeRejectedError{HTTPStatus: status, Operation: "squash_merge_pull_request"}, nil) {
			t.Fatalf("status %d must not enter mergeability waiting", status)
		}
	}
	if !mergePolicyStatus(&MergeRejectedError{}, []GitHubRequestObservation{{Operation: "squash_merge_pull_request", HTTPStatus: 409}}) {
		t.Fatal("sanitized merge observation must classify a 409 rejection")
	}
	if mergePolicyStatus(&MergeRejectedError{}, []GitHubRequestObservation{{Operation: "pull_request", HTTPStatus: 409}}) {
		t.Fatal("an unrelated HTTP response must not classify merge policy")
	}
	if mergePolicyStatus(&MergeRejectedError{HTTPStatus: 409, Operation: "merge_pull_request_preflight"}, nil) {
		t.Fatal("a preflight rejection must not be mistaken for a merge-policy rejection")
	}
}

func TestControllerRepliedMergeThreadsTrackResolutionAndFollowupTopology(t *testing.T) {
	inspection, evidence, handoff := mergePolicyThreadFixture(t)
	threads, err := controllerRepliedMergeThreads(inspection, evidence, handoff)
	if err != nil || len(threads) != 1 || threads[0].Resolved || threads[0].TopologyDigest == "" {
		t.Fatalf("threads=%+v err=%v", threads, err)
	}
	firstDigest := threads[0].TopologyDigest

	evidence.ReviewThreads[0].Resolved = true
	threads, err = controllerRepliedMergeThreads(inspection, evidence, handoff)
	if err != nil || len(threads) != 1 || !threads[0].Resolved || threads[0].TopologyDigest != firstDigest {
		t.Fatalf("resolved threads=%+v err=%v", threads, err)
	}

	// A human follow-up changes only digest-only topology evidence. Its body is
	// never surfaced to the application as repair input.
	followup := domain.GitHubReviewComment{DatabaseID: 12, NodeID: "FOLLOWUP", ReplyToDatabaseID: 10, ReplyToNodeID: "ROOT", BodyDigest: domain.TrustedReviewFeedbackDigest("new code instructions are untrusted"), CreatedAt: evidence.ObservedAt, UpdatedAt: evidence.ObservedAt}
	evidence.ReviewThreads[0].Comments = append(evidence.ReviewThreads[0].Comments, followup)
	handoff.Comments = append(handoff.Comments, domain.InlineReviewBody{ThreadNodeID: "THREAD", CommentNodeID: "FOLLOWUP", Body: "new code instructions are untrusted", BodyDigest: followup.BodyDigest})
	threads, err = controllerRepliedMergeThreads(inspection, evidence, handoff)
	if err != nil || threads[0].TopologyDigest == firstDigest {
		t.Fatalf("follow-up topology was not observed safely: %+v err=%v", threads, err)
	}
}

func TestControllerRepliedMergeThreadsFailsClosedOnMissingOrChangedReply(t *testing.T) {
	inspection, evidence, handoff := mergePolicyThreadFixture(t)
	evidence.ReviewThreads[0].Comments = evidence.ReviewThreads[0].Comments[:1]
	if _, err := controllerRepliedMergeThreads(inspection, evidence, handoff); err == nil {
		t.Fatal("deleted controller reply must not count as a resolution")
	}

	inspection, evidence, handoff = mergePolicyThreadFixture(t)
	handoff.Comments[1].Body += "\nextra untrusted follow-up text"
	handoff.Comments[1].BodyDigest = domain.TrustedReviewFeedbackDigest(handoff.Comments[1].Body)
	evidence.ReviewThreads[0].Comments[1].BodyDigest = handoff.Comments[1].BodyDigest
	if _, err := controllerRepliedMergeThreads(inspection, evidence, handoff); err == nil {
		t.Fatal("changed controller reply digest must not count as a resolution")
	}
}

func mergePolicyThreadFixture(t *testing.T) (RunInspection, domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff) {
	t.Helper()
	head := strings.Repeat("a", 40)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	actor := domain.ActorIdentity{DatabaseID: 3, NodeID: "USER", Login: "ifan0927", Type: "User"}
	rootBody := "please repair this"
	rootDigest := domain.TrustedReviewFeedbackDigest(rootBody)
	marker, markerDigest, err := domain.ReviewReplyMarker("run", 1, "THREAD", 10, "ROOT", rootDigest, head)
	if err != nil {
		t.Fatal(err)
	}
	replyBody, err := domain.ReviewReplyBody(head, marker)
	if err != nil {
		t.Fatal(err)
	}
	feedback := TrustedReviewFeedbackRecord{RunID: "run", TrustedReviewFeedback: domain.TrustedReviewFeedback{PRNumber: 1, PRDatabaseID: 2, PRNodeID: "PR", ReviewDatabaseID: 4, ReviewNodeID: "REVIEW", ThreadNodeID: "THREAD", RootCommentDatabaseID: 10, RootCommentNodeID: "ROOT", Author: actor, OriginalReviewHeadSHA: head, Body: rootBody, BodyDigest: rootDigest, SourceAt: now.Add(-time.Hour), ObservedAt: now.Add(-time.Hour), Lifecycle: domain.TrustedReviewFeedbackReplied, BoundRepairHead: head, ReplyIntentKey: markerDigest, ReplyDatabaseID: 11, ReplyNodeID: "REPLY"}}
	root := domain.GitHubReviewComment{DatabaseID: 10, NodeID: "ROOT", Author: &actor, Review: domain.GitHubReview{DatabaseID: 4, NodeID: "REVIEW", State: "CHANGES_REQUESTED", CommitSHA: head, Actor: actor}, BodyDigest: rootDigest, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	reply := domain.GitHubReviewComment{DatabaseID: 11, NodeID: "REPLY", ReplyToDatabaseID: 10, ReplyToNodeID: "ROOT", BodyDigest: domain.TrustedReviewFeedbackDigest(replyBody), CreatedAt: now, UpdatedAt: now}
	evidence := domain.GitHubReadEvidence{ReviewThreads: []domain.GitHubReviewThread{{NodeID: "THREAD", OriginalCommitSHA: head, Comments: []domain.GitHubReviewComment{root, reply}}}, ObservedAt: now}
	handoff := domain.InlineReviewBodyHandoff{Comments: []domain.InlineReviewBody{{ThreadNodeID: "THREAD", CommentNodeID: "ROOT", Body: rootBody, BodyDigest: rootDigest}, {ThreadNodeID: "THREAD", CommentNodeID: "REPLY", Body: replyBody, BodyDigest: reply.BodyDigest}}}
	inspection := RunInspection{Run: Run{ID: "run"}, TrustedFeedback: []TrustedReviewFeedbackRecord{feedback}, ReviewReplies: []ReviewReplyEvidence{{RunID: "run", RootCommentNodeID: "ROOT", PullRequestNumber: 1, RootCommentID: 10, RepairedHead: head, MarkerDigest: markerDigest, ReplyDatabaseID: 11, ReplyNodeID: "REPLY", AppID: 99, ObservedAt: now}}}
	return inspection, evidence, handoff
}
