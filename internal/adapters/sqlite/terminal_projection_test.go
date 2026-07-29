package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
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
	repository := application.LocalRepository{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, AllowedOperatorLogins: []string{"operator"}}
	repositoryJSON, err := json.Marshal(repository)
	if err != nil {
		t.Fatal(err)
	}
	run := application.Run{
		ID: "terminal-projection", IssueID: "IFAN-86", IdempotencyKey: "terminal-projection-key",
		SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}",
		TaskHash: "task", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(repositoryJSON),
		RegistryVersion: 1,
		BaseBranch:      "main", WorkingBranch: "feature", BaseSHA: base, ArtifactRoot: "/private/artifacts",
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
	openSide, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{
		RunID: run.ID, Kind: "open_pull_request", IdempotencyKey: head,
		IntentJSON: `{"head_branch":"feature","base_branch":"main","candidate_sha":"` + head + `","base_sha":"` + base + `","body_digest":"body-digest"}`,
		Attempt:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	openSide.Status = "observed"
	openSide.ResultJSON = `{"pull_request":7,"database_id":70,"node_id":"PR_7","head_sha":"` + head + `","base_sha":"` + base + `","body_digest":"body-digest"}`
	openSide.ObservedAt = now
	if err := store.FinishSideEffect(ctx, openSide); err != nil {
		t.Fatal(err)
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
			Review: domain.GitHubReview{DatabaseID: feedback.ReviewDatabaseID, NodeID: feedback.ReviewNodeID, State: "CHANGES_REQUESTED", CommitSHA: head, Actor: author},
		}},
	}
	evidence := domain.GitHubReadEvidence{
		Repository:  domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"},
		PullRequest: pr, ReviewThreads: []domain.GitHubReviewThread{thread}, ObservedAt: now.Add(time.Minute),
	}
	if err := store.SaveGitHubEvidence(ctx, run.ID, evidence); err != nil {
		t.Fatal(err)
	}
	shadow := evidence
	shadow.ReviewThreads = nil
	shadow.ObservedAt = now.Add(time.Minute + 500*time.Millisecond)
	if err := store.SaveGitHubEvidence(ctx, run.ID, shadow); err != nil {
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
	if result.PullRequestAggregate == nil || result.PullRequestAggregate.AggregateLabel != "mutable_controller_aggregate" || result.PullRequestAggregate.State != "open" || result.PullRequestAggregate.Merged {
		t.Fatalf("PR aggregate changed or was mislabeled across restart: %+v", result.PullRequestAggregate)
	}
	if len(result.PullRequestObservations) != 3 || result.PullRequestObservations[0].ObservationKind != "creation_journal" || result.PullRequestObservations[1].ObservationKind != "github_read" || result.PullRequestObservations[2].ObservationKind != "github_read" {
		t.Fatalf("immutable PR observations missing after restart: %+v", result.PullRequestObservations)
	}
	if !result.PullRequestObservations[1].ObservedAt.Equal(evidence.ObservedAt) || !result.PullRequestObservations[2].ObservedAt.Equal(shadow.ObservedAt) {
		t.Fatalf("immutable GitHub evidence order is not chronological: %+v", result.PullRequestObservations)
	}
	if result.PullRequest == nil || result.PullRequest.Status != "merged" || result.PullRequest.Merged == nil || !*result.PullRequest.Merged || result.PullRequest.MergeSHA != mergeSHA {
		t.Fatalf("effective merged projection missing after restart: run=%+v aggregate=%+v merge=%+v effective=%+v", result.Run, result.PullRequestAggregate, result.Merge, result.PullRequest)
	}
	if len(result.TrustedFeedback) != 1 || result.TrustedFeedback[0].EffectiveThreadStatus.Status != "resolved_outdated" {
		t.Fatalf("effective thread projection missing after restart: %+v", result.TrustedFeedback)
	}
}

func TestCompletedMergeWithoutPullRequestAggregateProjectsUnknownAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	repositoryJSON, _ := json.Marshal(application.LocalRepository{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, AllowedOperatorLogins: []string{"operator"}})
	run := application.Run{ID: "missing-pr-aggregate", IssueID: "IFAN-86", IdempotencyKey: "missing-pr-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: string(repositoryJSON), RegistryVersion: 1, BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=?,base_sha=?,candidate_head=? WHERE run_id=?`, domain.StateCompleted, base, head, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMerge(ctx, application.MergeRecord{RunID: run.ID, PRNumber: 7, PreMergeSHA: head, BaseSHA: base, Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: time.Now().UTC()}); err != nil {
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
	result, err := application.NewQueryService(store).GetRunDetail(ctx, application.RunDetailQuery{Requester: application.Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequest == nil || result.PullRequest.Status != "unknown" || result.PullRequest.EvidenceSource != "missing_pull_request_aggregate" || result.PullRequest.Merged != nil {
		t.Fatalf("missing PR aggregate was omitted or guessed after restart: %+v", result.PullRequest)
	}
}

func TestResolvedFeedbackProjectionOrdersGitHubEvidenceAcrossSQLiteRestart(t *testing.T) {
	resolutionAt := time.Date(2026, 7, 29, 5, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		observedAt   time.Time
		corruptField string
		corruptValue any
		wantStatus   string
		wantSource   string
	}{
		{"earlier unresolved is historical", resolutionAt.Add(-time.Second), "", nil, "resolved", "controller_resolution_observation"},
		{"equal unresolved conflicts", resolutionAt, "", nil, "conflict", "github_read_conflicts_with_controller_lifecycle"},
		{"later unresolved conflicts", resolutionAt.Add(time.Second), "", nil, "conflict", "github_read_conflicts_with_controller_lifecycle"},
		{"missing review identity fails closed", resolutionAt.Add(-time.Second), "review_node_id", "", "conflict", "trusted_review_feedback_authority_conflict"},
		{"impossible resolved lifecycle fails closed", resolutionAt.Add(-time.Second), "lifecycle", string(domain.TrustedReviewFeedbackReplied), "conflict", "trusted_review_feedback_authority_conflict"},
		{"invalid lifecycle timestamp fails closed", resolutionAt.Add(-time.Second), "updated_at", "not-a-time", "conflict", "trusted_review_feedback_authority_conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "controller.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("a", 40)
			base := strings.Repeat("b", 40)
			repositoryJSON, _ := json.Marshal(application.LocalRepository{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, AllowedOperatorLogins: []string{"operator"}})
			run := application.Run{
				ID: "feedback-order", IssueID: "IFAN-86", IdempotencyKey: "feedback-order-key",
				SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
				Repository: "owner/repo", RepositoryConfigJSON: string(repositoryJSON), RegistryVersion: 1,
				BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
			}
			if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=?,base_sha=?,candidate_head=? WHERE run_id=?`, domain.StatePROpen, base, head, run.ID); err != nil {
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
				BodyDigest: domain.TrustedReviewFeedbackDigest(body), SourceAt: resolutionAt.Add(-time.Minute),
				ObservedAt: resolutionAt.Add(-time.Minute), UpdatedAt: resolutionAt.Add(-time.Minute),
			}}
			if _, created, err := store.SaveTrustedReviewFeedback(ctx, feedback); err != nil || !created {
				t.Fatalf("feedback created=%v err=%v", created, err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE trusted_review_feedback SET lifecycle=?,bound_repair_head=?,resolved=1,outdated=0,updated_at=? WHERE run_id=? AND root_comment_node_id=?`, domain.TrustedReviewFeedbackResolved, strings.Repeat("d", 40), formatTime(resolutionAt), run.ID, feedback.RootCommentNodeID); err != nil {
				t.Fatal(err)
			}
			if test.corruptField != "" {
				if _, err := store.db.ExecContext(ctx, `UPDATE trusted_review_feedback SET `+test.corruptField+`=? WHERE run_id=? AND root_comment_node_id=?`, test.corruptValue, run.ID, feedback.RootCommentNodeID); err != nil {
					t.Fatal(err)
				}
			}
			thread := domain.GitHubReviewThread{
				NodeID: feedback.ThreadNodeID, OriginalCommitSHA: head, Path: feedback.Path, Line: &line,
				Comments: []domain.GitHubReviewComment{{
					DatabaseID: feedback.RootCommentDatabaseID, NodeID: feedback.RootCommentNodeID,
					Author: &author, BodyDigest: feedback.BodyDigest,
					Review: domain.GitHubReview{
						DatabaseID: feedback.ReviewDatabaseID, NodeID: feedback.ReviewNodeID,
						State: "CHANGES_REQUESTED", CommitSHA: head, Actor: author,
					},
				}},
			}
			if err := store.SaveGitHubEvidence(ctx, run.ID, domain.GitHubReadEvidence{
				Repository:  domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"},
				PullRequest: pr, ReviewThreads: []domain.GitHubReviewThread{thread}, ObservedAt: test.observedAt,
			}); err != nil {
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
			got := result.TrustedFeedback[0].EffectiveThreadStatus
			if got.Status != test.wantStatus || got.EvidenceSource != test.wantSource {
				t.Fatalf("effective thread status after restart=%+v want status=%s source=%s", got, test.wantStatus, test.wantSource)
			}
		})
	}
}

func TestCorruptPullRequestAggregateFailsClosedAcrossSQLiteRestart(t *testing.T) {
	for _, test := range []struct {
		name   string
		column string
		value  any
	}{
		{"copied ownership", "ownership_key", "another-run"},
		{"corrupt head branch", "head_branch", "another-feature"},
		{"missing database identity", "database_id", int64(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "controller.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("a", 40)
			base := strings.Repeat("b", 40)
			repositoryJSON, _ := json.Marshal(application.LocalRepository{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, AllowedOperatorLogins: []string{"operator"}})
			run := application.Run{
				ID: "corrupt-pr", IssueID: "IFAN-86", IdempotencyKey: "corrupt-pr-key",
				SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
				Repository: "owner/repo", RepositoryConfigJSON: string(repositoryJSON), RegistryVersion: 1,
				BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
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
			if err := store.SaveMerge(ctx, application.MergeRecord{
				RunID: run.ID, PRNumber: pr.Number, PreMergeSHA: head, BaseSHA: base,
				Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE pull_requests SET `+test.column+`=? WHERE run_id=?`, test.value, run.ID); err != nil {
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
			if result.PullRequest == nil || result.PullRequest.Status != "conflict" || result.PullRequest.Merged != nil || result.PullRequest.EvidenceSource != "pull_request_aggregate_authority_conflict" {
				t.Fatalf("corrupt PR aggregate was projected as terminal truth after restart: %+v", result.PullRequest)
			}
		})
	}
}

func TestZeroTimeGitHubObservationFailsClosedAcrossSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	repositoryJSON, _ := json.Marshal(application.LocalRepository{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, AllowedOperatorLogins: []string{"operator"}})
	run := application.Run{
		ID: "zero-time-read", IssueID: "IFAN-86", IdempotencyKey: "zero-time-read-key",
		SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
		Repository: "owner/repo", RepositoryConfigJSON: string(repositoryJSON), RegistryVersion: 1,
		BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
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
	repository := domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"}
	zeroEvidence := domain.GitHubReadEvidence{
		Repository:  repository,
		PullRequest: pr,
	}
	rawZeroEvidence, err := json.Marshal(zeroEvidence)
	if err != nil {
		t.Fatal(err)
	}
	zeroDigest := sha256.Sum256(rawZeroEvidence)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO github_read_evidence(run_id,head_sha,repository_id,evidence_json,evidence_digest,observed_at) VALUES(?,?,?,?,?,?)`, run.ID, head, repository.ID, string(rawZeroEvidence), hex.EncodeToString(zeroDigest[:]), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGitHubEvidence(ctx, run.ID, domain.GitHubReadEvidence{
		Repository:  repository,
		PullRequest: pr,
		ObservedAt:  time.Date(2026, 7, 29, 7, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMerge(ctx, application.MergeRecord{
		RunID: run.ID, PRNumber: pr.Number, PreMergeSHA: head, BaseSHA: base,
		Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: time.Date(2026, 7, 29, 7, 0, 0, 0, time.UTC),
	}); err != nil {
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
	if _, err := application.NewQueryService(store).GetRunDetail(ctx, application.RunDetailQuery{
		Requester: application.Requester{ID: "operator", Kind: "github_login"},
		RunID:     run.ID,
	}); err == nil {
		t.Fatal("later valid GitHub read masked zero-time persisted evidence")
	}
}

func TestGitHubEvidenceSQLAuthorityColumnsFailClosedAcrossSQLiteRestart(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		column string
		value  any
	}{
		{"head SHA mismatch", "head_sha", strings.Repeat("d", 40)},
		{"repository ID mismatch", "repository_id", int64(100)},
		{"observation time mismatch", "observed_at", formatTime(observedAt.Add(time.Second))},
		{"noncanonical equivalent observation time", "observed_at", "2026-07-29T08:00:00+00:00"},
		{"invalid observation time", "observed_at", "not-a-time"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "controller.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			run := application.Run{
				ID: "github-metadata", IssueID: "IFAN-86", IdempotencyKey: "github-metadata-key",
				SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
				Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
			}
			if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
				t.Fatal(err)
			}
			evidence := domain.GitHubReadEvidence{
				Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"},
				PullRequest: domain.PullRequest{
					Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7",
					HeadBranch: "feature", BaseBranch: "main", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
					BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open",
				},
				ObservedAt: observedAt,
			}
			if err := store.SaveGitHubEvidence(ctx, run.ID, evidence); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE github_read_evidence SET `+test.column+`=? WHERE run_id=?`, test.value, run.ID); err != nil {
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
			if _, err := store.Inspect(ctx, run.ID); err == nil {
				t.Fatal("corrupt GitHub evidence SQL authority was accepted")
			}
		})
	}
}

func TestGitHubEvidenceEqualTimesUseEvidenceIDOrderAcrossSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	run := application.Run{
		ID: "github-equal-time", IssueID: "IFAN-86", IdempotencyKey: "github-equal-time-key",
		SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
		Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
	}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	evidence := domain.GitHubReadEvidence{
		Repository:  domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"},
		PullRequest: domain.PullRequest{HeadSHA: strings.Repeat("a", 40)},
		ObservedAt:  observedAt,
	}
	for index := 0; index < 105; index++ {
		evidence.UnknownEvents = []string{fmt.Sprintf("tie-%03d", index)}
		if err := store.SaveGitHubEvidence(ctx, run.ID, evidence); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.GitHubEvidenceTotal != 105 || !inspection.GitHubEvidenceTruncated ||
		len(inspection.GitHubEvidenceHistory) != application.GitHubEvidenceProjectionLimit ||
		len(inspection.GitHubEvidenceHistory[0].UnknownEvents) != 1 || inspection.GitHubEvidenceHistory[0].UnknownEvents[0] != "tie-005" ||
		len(inspection.GitHubEvidenceHistory[99].UnknownEvents) != 1 || inspection.GitHubEvidenceHistory[99].UnknownEvents[0] != "tie-104" {
		t.Fatalf("equal-time evidence did not retain evidence-ID order: %+v", inspection.GitHubEvidenceHistory)
	}
}

func TestGitHubEvidenceLegacyOffsetTimeMatchesCanonicalSQLAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	run := application.Run{
		ID: "github-offset-time", IssueID: "IFAN-86", IdempotencyKey: "github-offset-time-key",
		SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
		Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
	}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	legacyTime := time.Date(2026, 7, 29, 17, 0, 0, 0, time.FixedZone("legacy-offset", 8*60*60))
	evidence := domain.GitHubReadEvidence{
		Repository:  domain.RepositoryIdentity{ID: 99},
		PullRequest: domain.PullRequest{HeadSHA: strings.Repeat("a", 40)},
		ObservedAt:  legacyTime,
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO github_read_evidence(run_id,head_sha,repository_id,evidence_json,evidence_digest,observed_at) VALUES(?,?,?,?,?,?)`, run.ID, evidence.PullRequest.HeadSHA, evidence.Repository.ID, string(raw), hex.EncodeToString(digest[:]), formatTime(legacyTime)); err != nil {
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
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.GitHubEvidenceHistory) != 1 || !inspection.GitHubEvidenceHistory[0].ObservedAt.Equal(legacyTime) {
		t.Fatalf("legacy offset evidence did not match canonical SQL time: %+v", inspection.GitHubEvidenceHistory)
	}
}

func TestCorruptMergeSHAFailsClosedAcrossSQLiteRestart(t *testing.T) {
	for _, test := range []struct {
		name   string
		column string
		value  string
	}{
		{"short pre-merge SHA", "pre_merge_head_sha", "short"},
		{"nonhex pre-merge SHA", "pre_merge_head_sha", strings.Repeat("g", 40)},
		{"short base SHA", "base_sha", "short"},
		{"nonhex base SHA", "base_sha", strings.Repeat("g", 40)},
		{"short merge SHA", "merge_sha", "short"},
		{"nonhex merge SHA", "merge_sha", strings.Repeat("g", 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "controller.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			run := application.Run{
				ID: "corrupt-merge", IssueID: "IFAN-86", IdempotencyKey: "corrupt-merge-key",
				SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
				Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
			}
			if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveMerge(ctx, application.MergeRecord{
				RunID: run.ID, PRNumber: 7, PreMergeSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
				Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE merge_results SET `+test.column+`=? WHERE run_id=?`, test.value, run.ID); err != nil {
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
			if _, err := store.Inspect(ctx, run.ID); err == nil {
				t.Fatal("corrupt persisted merge SHA was accepted")
			}
		})
	}
}

func TestLargeGitHubHistoryUsesBoundedProjectionAndFeedbackSelectionAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	repository := domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"}
	repositoryJSON, _ := json.Marshal(application.LocalRepository{CanonicalRepository: "owner/repo", ExpectedRepositoryID: repository.ID, AllowedOperatorLogins: []string{"operator"}})
	run := application.Run{
		ID: "large-github-history", IssueID: "IFAN-86", IdempotencyKey: "large-history-key",
		SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task",
		Repository: "owner/repo", RepositoryConfigJSON: string(repositoryJSON), RegistryVersion: 1,
		BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/private/artifacts",
	}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=?,base_sha=?,candidate_head=? WHERE run_id=?`, domain.StatePROpen, base, head, run.ID); err != nil {
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
		NodeID: feedback.ThreadNodeID, Resolved: true, OriginalCommitSHA: head, Path: feedback.Path, Line: feedback.Line,
		Comments: []domain.GitHubReviewComment{{
			DatabaseID: feedback.RootCommentDatabaseID, NodeID: feedback.RootCommentNodeID,
			Author: &author, BodyDigest: feedback.BodyDigest,
			Review: domain.GitHubReview{
				DatabaseID: feedback.ReviewDatabaseID, NodeID: feedback.ReviewNodeID,
				State: "CHANGES_REQUESTED", CommitSHA: head, Actor: author,
			},
		}},
	}
	if err := store.SaveGitHubEvidence(ctx, run.ID, domain.GitHubReadEvidence{
		Repository: repository, PullRequest: pr, ReviewThreads: []domain.GitHubReviewThread{thread}, ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 150; index++ {
		if err := store.SaveGitHubEvidence(ctx, run.ID, domain.GitHubReadEvidence{
			Repository: repository, PullRequest: pr, UnknownEvents: []string{fmt.Sprintf("poll-%03d", index)},
			ObservedAt: now.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.GitHubEvidenceTotal != 151 || !inspection.GitHubEvidenceTruncated || len(inspection.GitHubEvidenceHistory) != application.GitHubEvidenceProjectionLimit {
		t.Fatalf("bounded evidence retention shape is wrong: total=%d truncated=%v retained=%d", inspection.GitHubEvidenceTotal, inspection.GitHubEvidenceTruncated, len(inspection.GitHubEvidenceHistory))
	}
	if first := inspection.GitHubEvidenceHistory[0]; len(first.UnknownEvents) != 1 || first.UnknownEvents[0] != "poll-051" {
		t.Fatalf("latest-N history window is not deterministic: %+v", first.UnknownEvents)
	}
	selected, found := inspection.GitHubFeedbackEvidence[feedback.RootCommentNodeID]
	if !found || !selected.ObservedAt.Equal(now) || len(selected.ReviewThreads) != 1 {
		t.Fatalf("older matching feedback evidence disappeared beyond output window: %+v", inspection.GitHubFeedbackEvidence)
	}

	service := application.NewQueryService(store)
	query := application.QueryInput{
		Requester: application.Requester{ID: "operator", Kind: "github_login"},
		RunID:     run.ID, Repository: run.Repository,
	}
	status, err := service.Status(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	inspect, err := service.Inspect(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status, inspect) {
		t.Fatalf("status and inspect bounded projections differ:\nstatus=%+v\ninspect=%+v", status, inspect)
	}
	if len(status.PullRequestObservations) != application.GitHubEvidenceProjectionLimit ||
		status.PullRequestObservationsTotal != 151 || !status.PullRequestObservationsTruncated {
		t.Fatalf("bounded observation metadata is wrong: retained=%d total=%d truncated=%v", len(status.PullRequestObservations), status.PullRequestObservationsTotal, status.PullRequestObservationsTruncated)
	}
	if len(status.TrustedFeedback) != 1 || status.TrustedFeedback[0].EffectiveThreadStatus.Status != "resolved" ||
		status.TrustedFeedback[0].EffectiveThreadStatus.EvidenceSource != "github_read_observation" {
		t.Fatalf("feedback projection lost evidence beyond the output window: %+v", status.TrustedFeedback)
	}
}
