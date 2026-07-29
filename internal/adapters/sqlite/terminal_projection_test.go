package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestTerminalProjectionSurvivesSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	mergeSHA := strings.Repeat("c", 40)
	now := time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC)
	repository := application.LocalRepository{CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{"operator"}}
	repositoryJSON, err := json.Marshal(repository)
	if err != nil {
		t.Fatal(err)
	}
	run := application.Run{
		ID: "terminal-projection", IssueID: "IFAN-86", IdempotencyKey: "terminal-projection-key",
		SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}",
		TaskHash: "task", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(repositoryJSON),
		BaseBranch: "main", WorkingBranch: "feature", BaseSHA: base, ArtifactRoot: "/private/artifacts",
		State: domain.StateCompleted, CandidateHead: head,
	}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=?,base_sha=?,candidate_head=? WHERE run_id=?`, domain.StateCompleted, base, head, run.ID); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{
		Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7",
		HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: head, BaseSHA: base,
		BodyDigest: "body-digest", OwnershipKey: run.IdempotencyKey, State: "open",
	}
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		t.Fatal(err)
	}
	author := domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "operator", Type: "User"}
	body := "Please fix this."
	line := 12
	feedback := application.TrustedReviewFeedbackRecord{RunID: run.ID, TrustedReviewFeedback: domain.TrustedReviewFeedback{
		PRNumber: pr.Number, PRDatabaseID: pr.DatabaseID, PRNodeID: pr.NodeID,
		ReviewDatabaseID: 80, ReviewNodeID: "REVIEW_80", ThreadNodeID: "THREAD_90",
		RootCommentDatabaseID: 100, RootCommentNodeID: "COMMENT_100", Author: author,
		OriginalReviewHeadSHA: head, Path: "internal/example.go", Line: &line, Body: body,
		BodyDigest: domain.TrustedReviewFeedbackDigest(body), SourceAt: now, ObservedAt: now,
	}}
	if _, created, err := store.SaveTrustedReviewFeedback(ctx, feedback); err != nil || !created {
		t.Fatalf("feedback created=%v err=%v", created, err)
	}
	thread := domain.GitHubReviewThread{
		NodeID: feedback.ThreadNodeID, Resolved: true, Outdated: true,
		OriginalCommitSHA: head, Path: feedback.Path,
		Comments: []domain.GitHubReviewComment{{
			DatabaseID: feedback.RootCommentDatabaseID, NodeID: feedback.RootCommentNodeID,
			Author: &author, BodyDigest: feedback.BodyDigest,
			Review: domain.GitHubReview{DatabaseID: feedback.ReviewDatabaseID, NodeID: feedback.ReviewNodeID},
		}},
	}
	evidence := domain.GitHubReadEvidence{
		Repository:  domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"},
		PullRequest: pr, ReviewThreads: []domain.GitHubReviewThread{thread}, ObservedAt: now.Add(time.Minute),
	}
	if err := store.SaveGitHubEvidence(ctx, run.ID, evidence); err != nil {
		t.Fatal(err)
	}
	merge := application.MergeRecord{RunID: run.ID, PRNumber: pr.Number, PreMergeSHA: head, BaseSHA: base, Method: "squash", MergeSHA: mergeSHA, MergedAt: now.Add(2 * time.Minute)}
	if err := store.SaveMerge(ctx, merge); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result, err := application.NewQueryService(store).GetRunDetail(ctx, application.RunDetailQuery{
		Requester: application.Requester{ID: "operator", Kind: "github_login"},
		RunID:     run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequestSnapshot == nil || result.PullRequestSnapshot.State != "open" || result.PullRequestSnapshot.Merged {
		t.Fatalf("historical PR snapshot changed across restart: %+v", result.PullRequestSnapshot)
	}
	if result.PullRequest == nil || result.PullRequest.Status != "merged" || result.PullRequest.Merged == nil || !*result.PullRequest.Merged || result.PullRequest.MergeSHA != mergeSHA {
		t.Fatalf("effective merged projection missing after restart: run=%+v snapshot=%+v merge=%+v effective=%+v", result.Run, result.PullRequestSnapshot, result.Merge, result.PullRequest)
	}
	if len(result.TrustedFeedback) != 1 || result.TrustedFeedback[0].EffectiveThreadStatus.Status != "resolved_outdated" {
		t.Fatalf("effective thread projection missing after restart: %+v", result.TrustedFeedback)
	}
}
