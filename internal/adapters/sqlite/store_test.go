package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestGitHubV6EvidencePersistsMetadataWithoutSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-gh", IssueID: "IFAN-GH", IdempotencyKey: "key-gh", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/run-gh", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	legacyPR := domain.PullRequest{Number: 1, URL: "https://example.invalid/pr/1", NodeID: "PR", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: "key", State: "open"}
	if err := store.SavePullRequest(ctx, "run-gh", legacyPR); err != nil {
		t.Fatal(err)
	}
	verifiedPR := legacyPR
	verifiedPR.DatabaseID = 101
	if err := store.SavePullRequest(ctx, "run-gh", verifiedPR); err != nil {
		t.Fatal(err)
	}
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	if err := store.SaveGitHubInstallation(ctx, "run-gh", application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: "digest", ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGitHubRequest(ctx, application.GitHubRequestObservation{RunID: "run-gh", Operation: "repository", Category: "REST", HTTPStatus: 200, ResponseDigest: "response-digest", InstallationID: 2, Repository: repo, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	e := domain.GitHubReadEvidence{Repository: repo, PullRequest: domain.PullRequest{Number: 1, HeadSHA: "head", BaseSHA: "base"}, ObservedAt: now}
	if err := store.SaveGitHubEvidence(ctx, "run-gh", e); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGitHubEvidence(ctx, "run-gh", e); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGitHubRequest(ctx, application.GitHubRequestObservation{RunID: "run-gh", Operation: "repository", Category: "REST", HTTPStatus: 200, ResponseDigest: "response-digest", InstallationID: 2, Repository: repo, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM github_read_evidence WHERE run_id='run-gh'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("evidence count=%d err=%v", count, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM github_request_observations WHERE run_id='run-gh'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("request count=%d err=%v", count, err)
	}
	inspection, err := store.Inspect(ctx, "run-gh")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.GitHubInstallation == nil || len(inspection.GitHubRequests) != 2 || inspection.GitHubEvidence == nil {
		t.Fatalf("missing GitHub v6 inspection: %+v", inspection)
	}
	if inspection.PullRequest == nil || inspection.PullRequest.DatabaseID != 101 {
		t.Fatalf("PR database ID was not backfilled: %+v", inspection.PullRequest)
	}
	store.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("fixture-installation-secret"), []byte("BEGIN PRIVATE KEY"), []byte("Bearer "), []byte("eyJhbGci")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("database contains secret marker %q", secret)
		}
	}
}

func TestRetryMergeSideEffectClaimsOnePolicyRetry(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := application.Run{ID: "merge-retry", IssueID: "IFAN-MERGE", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/merge-retry", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	side, _, err := store.BeginSideEffect(context.Background(), application.SideEffectRecord{RunID: run.ID, Kind: "squash_merge", IdempotencyKey: strings.Repeat("a", 40), IntentJSON: `{"pull_request":1}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	side.Status, side.ResultJSON = "failed", `{"category":"merge_policy_pending"}`
	if err := store.FinishSideEffect(context.Background(), side); err != nil {
		t.Fatal(err)
	}
	claimed, changed, err := store.RetryMergeSideEffect(context.Background(), side)
	if err != nil || !changed || claimed.Status != "intent" || claimed.Attempt != 2 || !strings.Contains(claimed.ResultJSON, "merge_policy_retry_claimed") {
		t.Fatalf("claimed=%+v changed=%v err=%v", claimed, changed, err)
	}
	if _, changed, err := store.RetryMergeSideEffect(context.Background(), side); err != nil || changed {
		t.Fatalf("second claim changed=%v err=%v", changed, err)
	}
}

func TestRetryLinearIssueStartSideEffectClaimsOnlySecondAttempt(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := application.Run{ID: "linear-start-retry", IssueID: "IFAN-START", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/linear-start-retry", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	side, _, err := store.BeginSideEffect(context.Background(), application.SideEffectRecord{RunID: run.ID, Kind: "linear_move_to_started", IdempotencyKey: strings.Repeat("b", 64), IntentJSON: `{"issue_id":"123e4567-e89b-42d3-a456-426614174103"}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	side.Status, side.ResultJSON, side.ObservedAt = "failed", `{"category":"todo_after_mutation"}`, time.Now().UTC()
	if err := store.FinishSideEffect(context.Background(), side); err != nil {
		t.Fatal(err)
	}
	claimed, changed, err := store.RetryLinearIssueStartSideEffect(context.Background(), side)
	if err != nil || !changed || claimed.Status != "intent" || claimed.Attempt != 2 || claimed.ResultJSON != "" || !claimed.ObservedAt.IsZero() {
		t.Fatalf("claimed=%+v changed=%v err=%v", claimed, changed, err)
	}
	if _, changed, err := store.RetryLinearIssueStartSideEffect(context.Background(), side); err != nil || changed {
		t.Fatalf("second retry changed=%v err=%v", changed, err)
	}
}

func TestLinearIssueStartExecutionClaimIsSingleOwner(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := application.Run{ID: "linear-start-claim", IssueID: "IFAN-CLAIM", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/linear-start-claim", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: run.ID, Kind: "linear_move_to_started", IdempotencyKey: strings.Repeat("c", 64), IntentJSON: `{"issue_id":"123e4567-e89b-42d3-a456-426614174103"}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	claims := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, claimed, claimErr := store.ClaimLinearIssueStartSideEffect(ctx, side, time.Now().UTC())
			claims <- claimed
			errs <- claimErr
		}()
	}
	claimed := 0
	for range 2 {
		if claimErr := <-errs; claimErr != nil {
			t.Fatal(claimErr)
		}
		if <-claims {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d", claimed)
	}
	claimedSide, found, err := linearIssueStartSideEffectByID(ctx, store.db, side.ID)
	if err != nil || !found || claimedSide.Status != "in_flight" || claimedSide.Attempt != 1 || claimedSide.ClaimedAt.IsZero() {
		t.Fatalf("side=%+v found=%v err=%v", claimedSide, found, err)
	}
	finished := claimedSide
	finished.Status, finished.ResultJSON, finished.ObservedAt = "failed", `{"category":"todo_after_mutation"}`, time.Now().UTC()
	if err := store.FinishLinearIssueStartSideEffect(ctx, finished, "in_flight", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLinearIssueStartSideEffect(ctx, finished, "in_flight", 1); err == nil {
		t.Fatal("stale claimer rewrote the finished effect")
	}
}

func TestRetryLinearIssueStartSideEffectHasOneWinner(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := application.Run{ID: "linear-start-retry-race", IssueID: "IFAN-RETRY", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/linear-start-retry-race", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: run.ID, Kind: "linear_move_to_started", IdempotencyKey: strings.Repeat("d", 64), IntentJSON: `{"issue_id":"123e4567-e89b-42d3-a456-426614174103"}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	side.Status, side.ResultJSON, side.ObservedAt = "failed", `{"category":"todo_after_mutation"}`, time.Now().UTC()
	if err := store.FinishSideEffect(ctx, side); err != nil {
		t.Fatal(err)
	}
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, retried, retryErr := store.RetryLinearIssueStartSideEffect(ctx, side)
			results <- retried
			errs <- retryErr
		}()
	}
	retried := 0
	for range 2 {
		if retryErr := <-errs; retryErr != nil {
			t.Fatal(retryErr)
		}
		if <-results {
			retried++
		}
	}
	if retried != 1 {
		t.Fatalf("retried=%d", retried)
	}
	current, found, err := linearIssueStartSideEffectByID(ctx, store.db, side.ID)
	if err != nil || !found || current.Status != "intent" || current.Attempt != 2 {
		t.Fatalf("current=%+v found=%v err=%v", current, found, err)
	}
}

func TestRetryLinearIssueStartSideEffectRequiresExactConclusiveTodoResult(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := application.Run{ID: "linear-start-non-retry", IssueID: "IFAN-NO-RETRY", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/linear-start-non-retry", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: run.ID, Kind: "linear_move_to_started", IdempotencyKey: strings.Repeat("e", 64), IntentJSON: `{"issue_id":"123e4567-e89b-42d3-a456-426614174103"}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	side.Status, side.ResultJSON, side.ObservedAt = "failed", `{"category":"forbidden"}`, time.Now().UTC()
	if err := store.FinishSideEffect(ctx, side); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.RetryLinearIssueStartSideEffect(ctx, side); err == nil || changed {
		t.Fatalf("non-conclusive failed effect retried: changed=%v err=%v", changed, err)
	}
	current, found, err := linearIssueStartSideEffectByID(ctx, store.db, side.ID)
	if err != nil || !found || current.Status != "failed" || current.Attempt != 1 || current.ResultJSON != side.ResultJSON {
		t.Fatalf("current=%+v found=%v err=%v", current, found, err)
	}
}

func TestRecordMergePolicyPendingAtomicallyPersistsWaitAndTopology(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	head := strings.Repeat("b", 40)
	run := application.Run{ID: "merge-policy-pending", IssueID: "IFAN-POLICY", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/merge-policy-pending", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, head); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateMerging, run.ID); err != nil {
		t.Fatal(err)
	}
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: run.ID, Kind: "squash_merge", IdempotencyKey: head, IntentJSON: `{"pull_request":1}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := json.Marshal(struct {
		Category string                          `json:"category"`
		Head     string                          `json:"head"`
		Threads  []application.MergePolicyThread `json:"threads"`
	}{Category: "merge_policy_pending", Head: head, Threads: []application.MergePolicyThread{{ThreadNodeID: "THREAD", RootCommentNodeID: "ROOT", RootCommentID: 1, ReplyNodeID: "REPLY", ReplyID: 2, TopologyDigest: strings.Repeat("c", 64)}}})
	if err != nil {
		t.Fatal(err)
	}
	side.Status, side.ResultJSON, side.ObservedAt = "failed", string(pending), time.Now().UTC()
	if err := store.RecordMergePolicyPending(ctx, run.ID, side, head); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetRun(ctx, run.ID)
	if err != nil || stored.State != domain.StateAwaitingGitHubMergeability || stored.LastError != "merge_policy_pending" {
		t.Fatalf("run=%+v err=%v", stored, err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || len(inspection.SideEffects) != 1 || inspection.SideEffects[0].Status != "failed" || !strings.Contains(inspection.SideEffects[0].ResultJSON, `"category":"merge_policy_pending"`) {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	var transitions int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE run_id=? AND from_state=? AND to_state=?`, run.ID, domain.StateMerging, domain.StateAwaitingGitHubMergeability).Scan(&transitions); err != nil || transitions != 1 {
		t.Fatalf("transitions=%d err=%v", transitions, err)
	}
}

func TestGitHubReadSuccessPersistsEvidenceAndGateTransitionAtomically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := application.Run{ID: "run-gate", IssueID: "IFAN-GATE", IdempotencyKey: "gate-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", BaseSHA: "base", ArtifactRoot: "/tmp/run-gate", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, "head"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StatePROpen, run.ID); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{Number: 1, DatabaseID: 101, URL: "https://example.invalid/pr/1", NodeID: "PR", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: "gate-key", State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		t.Fatal(err)
	}
	owner := "lease-owner"
	if acquired, err := store.AcquireLease(ctx, run.ID, owner, time.Now().Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	body := "controller-generated required check finding retained only for repair"
	sum := sha256.Sum256([]byte(body))
	evidence := domain.GitHubReadEvidence{Repository: repo, PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: domain.CheckSuccess}}, Findings: []domain.NormalizedFinding{{Source: "github_required_check", SourceID: "finding-1", BodyDigest: hex.EncodeToString(sum[:]), Body: body, HeadSHA: "head", ObservedAt: time.Now().UTC()}}, ObservedAt: time.Now().UTC()}
	metadata := application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: time.Now().Add(time.Hour), PermissionsDigest: "permissions", ObservedAt: time.Now().UTC()}
	if err := store.SaveGitHubReadSuccess(ctx, run.ID, owner, domain.StatePROpen, run.IdempotencyKey, []application.GitHubRequestObservation{{RunID: run.ID, Operation: "read", Category: "REST", HTTPStatus: 200, ResponseDigest: "response", InstallationID: 2, Repository: repo, ObservedAt: time.Now().UTC()}}, pr, metadata, evidence, nil, nil, nil, domain.StateReconcilingReviews, "GitHub evidence collection started"); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.State != domain.StateReconcilingReviews || inspection.GitHubEvidence == nil || len(inspection.GitHubRequests) != 1 || len(inspection.Findings) != 1 || inspection.Findings[0].Body != body || len(inspection.Timeline) != 2 || inspection.Timeline[1].BoundHead != "head" {
		t.Fatalf("incomplete atomic reconciliation: %+v", inspection)
	}
	var evidenceJSON string
	if err := store.db.QueryRowContext(ctx, `SELECT evidence_json FROM github_read_evidence WHERE run_id=?`, run.ID).Scan(&evidenceJSON); err != nil || bytes.Contains([]byte(evidenceJSON), []byte(body)) {
		t.Fatalf("finding body leaked into public GitHub evidence: err=%v evidence=%q", err, evidenceJSON)
	}
	if err := store.SaveGitHubReadSuccess(ctx, run.ID, owner, domain.StateReconcilingReviews, run.IdempotencyKey, nil, pr, metadata, evidence, nil, nil, nil, domain.StateCleaning, "invalid transition"); err == nil {
		t.Fatal("invalid gate transition was accepted")
	}
	var evidenceCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM github_read_evidence WHERE run_id=?`, run.ID).Scan(&evidenceCount); err != nil || evidenceCount != 1 {
		t.Fatalf("rollback evidence count=%d err=%v", evidenceCount, err)
	}
}

func TestManualPRTargetDriftPersistsSanitizedConflictWithoutRewritingBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	run := application.Run{ID: "target-drift", IssueID: "IFAN-44", IdempotencyKey: "target-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{"canonical_repository":"owner/repo","github_app_id":1,"github_installation_id":2,"expected_repository_id":99}`, BaseBranch: "main", WorkingBranch: "feature", BaseSHA: "base", ArtifactRoot: "/tmp/target-drift", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, head); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateAwaitingGitHubMergeability, run.ID); err != nil {
		t.Fatal(err)
	}
	persisted := domain.PullRequest{Number: 44, DatabaseID: 144, URL: "https://example.invalid/pr/44", NodeID: "PR_44", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, BaseSHA: "base", BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, persisted); err != nil {
		t.Fatal(err)
	}
	owner := "target-drift-owner"
	if acquired, err := store.AcquireLease(ctx, run.ID, owner, time.Now().Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	now := time.Date(2026, 7, 14, 6, 0, 0, 0, time.UTC)
	for _, mutate := range []struct {
		name        string
		apply       func(*domain.PullRequest, *domain.RepositoryIdentity, *application.GitHubInstallationMetadata, *application.GitHubRequestObservation)
		caller      func(*domain.PullRequest)
		wantPersist bool
	}{
		{name: "body only", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.BodyDigest = "other-body"
		}},
		{name: "target and body", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.BaseBranch, pr.BodyDigest = "release", "other-body"
		}},
		{name: "url only", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.URL = "https://example.invalid/pr/other"
		}},
		{name: "repository mismatch", apply: func(pr *domain.PullRequest, repo *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.BaseBranch = "release"
			repo.ID = 100
		}},
		{name: "metadata mismatch", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, metadata *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.BaseBranch = "release"
			metadata.InstallationID = 3
		}},
		{name: "tampered caller binding", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.HeadBranch = "other-feature"
		}, caller: func(pr *domain.PullRequest) { pr.BaseBranch = "caller-release" }},
		{name: "base", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.BaseBranch, pr.BaseSHA = "release", "other-base"
		}, wantPersist: true},
		{name: "head", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.HeadBranch, pr.HeadSHA = "other-feature", strings.Repeat("b", 40)
		}, wantPersist: true},
		{name: "ownership", apply: func(pr *domain.PullRequest, _ *domain.RepositoryIdentity, _ *application.GitHubInstallationMetadata, _ *application.GitHubRequestObservation) {
			pr.OwnershipKey = "other-owner"
		}, wantPersist: true},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=?,last_error='' WHERE run_id=?`, domain.StateAwaitingGitHubMergeability, run.ID); err != nil {
				t.Fatal(err)
			}
			before := githubReadRowCounts(t, store, ctx, run.ID)
			observed, evidenceRepo := persisted, repo
			caller := persisted
			metadata := application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: "permissions", ObservedAt: now}
			observation := application.GitHubRequestObservation{RunID: run.ID, Operation: "read_pull_request", Category: "GraphQL", HTTPStatus: 200, ResponseDigest: mutate.name, InstallationID: 2, Repository: repo, ObservedAt: now}
			mutate.apply(&observed, &evidenceRepo, &metadata, &observation)
			if mutate.caller != nil {
				mutate.caller(&caller)
			}
			body := "untrusted external finding must never enter generic evidence"
			digest := domain.TrustedReviewFeedbackDigest(body)
			evidence := domain.GitHubReadEvidence{Repository: evidenceRepo, PullRequest: observed, Findings: []domain.NormalizedFinding{{Source: "github_required_check", SourceID: mutate.name, Body: body, BodyDigest: digest, HeadSHA: observed.HeadSHA, ObservedAt: now}}, ObservedAt: now}
			err := store.SaveGitHubManualPRTargetDrift(ctx, run.ID, owner, domain.StateAwaitingGitHubMergeability, run.IdempotencyKey, repo, caller, []application.GitHubRequestObservation{observation}, metadata, evidence, "GitHub pull request target drifted while awaiting thread resolution")
			inspection, inspectErr := store.Inspect(ctx, run.ID)
			after := githubReadRowCounts(t, store, ctx, run.ID)
			if mutate.wantPersist {
				wantInstallations := before.installations
				if wantInstallations == 0 {
					wantInstallations = 1
				}
				if err != nil || inspectErr != nil || inspection.Run.State != domain.StateManualIntervention || inspection.PullRequest == nil || *inspection.PullRequest != persisted || inspection.GitHubEvidence == nil || inspection.GitHubEvidence.PullRequest != observed || after.requests != before.requests+1 || after.installations != wantInstallations || after.evidence != before.evidence+1 || after.transitions != before.transitions+1 {
					t.Fatalf("inspection=%+v err=%v inspectErr=%v before=%+v after=%+v", inspection, err, inspectErr, before, after)
				}
				var evidenceJSON string
				if err := store.db.QueryRowContext(ctx, `SELECT evidence_json FROM github_read_evidence WHERE run_id=? ORDER BY evidence_id DESC LIMIT 1`, run.ID).Scan(&evidenceJSON); err != nil || bytes.Contains([]byte(evidenceJSON), []byte(body)) {
					t.Fatalf("conflict evidence leaked body or is absent: err=%v evidence=%q", err, evidenceJSON)
				}
				return
			}
			if err == nil || inspectErr != nil || inspection.Run.State != domain.StateAwaitingGitHubMergeability || inspection.PullRequest == nil || *inspection.PullRequest != persisted || after != before {
				t.Fatalf("manual conflict was partially persisted: inspection=%+v err=%v inspectErr=%v before=%+v after=%+v", inspection, err, inspectErr, before, after)
			}
		})
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
	if err != nil || inspection.PullRequest == nil || *inspection.PullRequest != persisted || inspection.Run.State != domain.StateManualIntervention || inspection.GitHubEvidence == nil {
		t.Fatalf("restart inspection=%+v err=%v", inspection, err)
	}
}

type githubReadRows struct{ requests, installations, evidence, transitions int }

func githubReadRowCounts(t *testing.T, store *Store, ctx context.Context, runID string) githubReadRows {
	t.Helper()
	var counts githubReadRows
	for _, query := range []struct {
		name string
		to   *int
	}{
		{name: "github_request_observations", to: &counts.requests},
		{name: "github_installations", to: &counts.installations},
		{name: "github_read_evidence", to: &counts.evidence},
		{name: "transitions", to: &counts.transitions},
	} {
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+query.name+` WHERE run_id=?`, runID).Scan(query.to); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestGitHubReadSuccessRejectsTargetMismatchWithoutPartialWrites(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := application.Run{ID: "non-manual-drift", IssueID: "IFAN-44-FAIL", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{"canonical_repository":"owner/repo","github_app_id":1,"github_installation_id":2,"expected_repository_id":99}`, BaseBranch: "main", WorkingBranch: "feature", BaseSHA: "base", ArtifactRoot: "/tmp/non-manual-drift", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, "head"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateAwaitingGitHubMergeability, run.ID); err != nil {
		t.Fatal(err)
	}
	persisted := domain.PullRequest{Number: 1, DatabaseID: 1, URL: "https://example.invalid/pr/1", NodeID: "PR_1", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: "key", State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, persisted); err != nil {
		t.Fatal(err)
	}
	owner := "non-manual-owner"
	if ok, err := store.AcquireLease(ctx, run.ID, owner, time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	now := time.Now().UTC()
	metadata := application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: "permissions", ObservedAt: now}
	for _, tc := range []struct {
		name  string
		apply func(*domain.PullRequest)
	}{
		{name: "body only", apply: func(pr *domain.PullRequest) { pr.BodyDigest = "other-body" }},
		{name: "target and body", apply: func(pr *domain.PullRequest) { pr.BaseBranch, pr.BodyDigest = "release", "other-body" }},
		{name: "url only", apply: func(pr *domain.PullRequest) { pr.URL = "https://example.invalid/pr/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drifted := persisted
			tc.apply(&drifted)
			before := githubReadRowCounts(t, store, ctx, run.ID)
			evidence := domain.GitHubReadEvidence{Repository: repo, PullRequest: drifted, ObservedAt: now}
			err := store.SaveGitHubReadSuccess(ctx, run.ID, owner, domain.StateAwaitingGitHubMergeability, run.IdempotencyKey, []application.GitHubRequestObservation{{RunID: run.ID, Operation: "read", Category: "REST", HTTPStatus: 200, ResponseDigest: tc.name, InstallationID: 2, Repository: repo, ObservedAt: now}}, drifted, metadata, evidence, nil, nil, nil, domain.StateManualIntervention, "must fail")
			inspection, inspectErr := store.Inspect(ctx, run.ID)
			after := githubReadRowCounts(t, store, ctx, run.ID)
			if err == nil || inspectErr != nil || inspection.Run.State != domain.StateAwaitingGitHubMergeability || inspection.PullRequest == nil || *inspection.PullRequest != persisted || after != before {
				t.Fatalf("non-manual mismatch partially persisted: inspection=%+v err=%v inspectErr=%v before=%+v after=%+v", inspection, err, inspectErr, before, after)
			}
		})
	}
}

func TestGitHubReadFailureRejectsMismatchedAuthorityWithoutWrites(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := application.Run{ID: "read-failure-authority", IssueID: "IFAN-44-READ", IdempotencyKey: "read-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{"canonical_repository":"owner/repo","github_app_id":1,"github_installation_id":2,"expected_repository_id":99}`, BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/read-failure-authority", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateAwaitingGitHubMergeability, run.ID); err != nil {
		t.Fatal(err)
	}
	persisted := domain.PullRequest{Number: 1, DatabaseID: 1, URL: "https://example.invalid/pr/1", NodeID: "PR_1", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, persisted); err != nil {
		t.Fatal(err)
	}
	owner := "failure-owner"
	if ok, err := store.AcquireLease(ctx, run.ID, owner, time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}
	for _, tc := range []struct {
		name string
		obs  application.GitHubRequestObservation
	}{
		{name: "repository", obs: application.GitHubRequestObservation{RunID: run.ID, Operation: "read", Category: "REST", HTTPStatus: 404, ResponseDigest: "repo", InstallationID: 2, Repository: domain.RepositoryIdentity{ID: 100, Owner: "owner", Name: "repo"}, ObservedAt: time.Now().UTC()}},
		{name: "installation", obs: application.GitHubRequestObservation{RunID: run.ID, Operation: "read", Category: "REST", HTTPStatus: 401, ResponseDigest: "installation", InstallationID: 3, Repository: domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}, ObservedAt: time.Now().UTC()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := githubReadRowCounts(t, store, ctx, run.ID)
			err := store.SaveGitHubReadFailure(ctx, run.ID, owner, domain.StateAwaitingGitHubMergeability, run.IdempotencyKey, []application.GitHubRequestObservation{tc.obs})
			inspection, inspectErr := store.Inspect(ctx, run.ID)
			after := githubReadRowCounts(t, store, ctx, run.ID)
			if err == nil || inspectErr != nil || inspection.Run.State != domain.StateAwaitingGitHubMergeability || inspection.PullRequest == nil || *inspection.PullRequest != persisted || after != before {
				t.Fatalf("failure telemetry was partially persisted: inspection=%+v err=%v inspectErr=%v before=%+v after=%+v", inspection, err, inspectErr, before, after)
			}
		})
	}
}

func TestGitHubReadAtomicallySelectsTrustedFeedbackWithRepairTransition(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	run := application.Run{ID: "trusted-repair", IssueID: "IFAN-TR", IdempotencyKey: "trusted-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", BaseSHA: "base", ArtifactRoot: "/tmp/trusted-repair"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, head); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateReconcilingReviews, run.ID); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{Number: 1, DatabaseID: 101, URL: "https://example.invalid/pr/1", NodeID: "PR_101", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, BaseSHA: "base", BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		t.Fatal(err)
	}
	owner := "lease-owner"
	if ok, err := store.AcquireLease(ctx, run.ID, owner, time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("lease ok=%t err=%v", ok, err)
	}
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	now := time.Now().UTC()
	body := "quoted human review body"
	line := 7
	feedback := application.TrustedReviewFeedbackRecord{RunID: run.ID, TrustedReviewFeedback: domain.TrustedReviewFeedback{PRNumber: pr.Number, PRDatabaseID: pr.DatabaseID, PRNodeID: pr.NodeID, ReviewDatabaseID: 3, ReviewNodeID: "REVIEW_3", ThreadNodeID: "THREAD_4", RootCommentDatabaseID: 5, RootCommentNodeID: "COMMENT_5", Author: domain.ActorIdentity{DatabaseID: 6, NodeID: "USER_6", Login: "ifan0927", Type: "User"}, OriginalReviewHeadSHA: head, Path: "internal/example.go", Line: &line, Body: body, BodyDigest: domain.TrustedReviewFeedbackDigest(body), SourceAt: now, ObservedAt: now}}
	evidence := domain.GitHubReadEvidence{Repository: repo, PullRequest: pr, Findings: []domain.NormalizedFinding{{Source: "github_human_review_comment", SourceID: feedback.RootCommentNodeID, ThreadID: feedback.ThreadNodeID, File: feedback.Path, Classification: "trusted_changes_requested", Body: body, BodyDigest: feedback.BodyDigest, HeadSHA: head, SourceAt: now, ObservedAt: now}}, ObservedAt: now}
	metadata := application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: "permissions", ObservedAt: now}
	if err := store.SaveGitHubReadSuccess(ctx, run.ID, owner, domain.StateReconcilingReviews, run.IdempotencyKey, nil, pr, metadata, evidence, []application.TrustedReviewFeedbackRecord{feedback}, nil, nil, domain.StateRepairing, "trusted exact-head inline change request requires repair"); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || inspection.Run.State != domain.StateRepairing || len(inspection.TrustedFeedback) != 1 || inspection.TrustedFeedback[0].Lifecycle != domain.TrustedReviewFeedbackSelectedForRepair || len(inspection.Findings) != 1 {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func TestMigrationAndRunIdempotency(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key-1", SourceRevision: "v1",
		RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run-1", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, created, err := store.CreateRun(context.Background(), input); err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if _, created, err := store.CreateRun(context.Background(), input); err != nil || created {
		t.Fatalf("repeat: created=%v err=%v", created, err)
	}
	drifted := input
	drifted.Run.RegistryVersion = 1
	drifted.Run.RegistryDigest = "changed"
	drifted.Run.RepositoryBindingDigest = "changed"
	if _, _, err := store.CreateRun(context.Background(), drifted); err == nil {
		t.Fatal("idempotent start must reject changed repository authority")
	}
	other := input
	other.Run.ID = "run-2"
	other.Run.IdempotencyKey = "key-2"
	if _, _, err := store.CreateRun(context.Background(), other); err == nil {
		t.Fatal("active issue uniqueness must reject second run")
	}
}

func TestListRunsUsesRepositoryScopedDeterministicCursor(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	for index, id := range []string{"run-a", "run-b", "run-c"} {
		input := application.CreateRunInput{Run: application.Run{ID: id, IssueID: fmt.Sprintf("ISSUE-%d", index), IdempotencyKey: fmt.Sprintf("key-%d", index), SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test"}}
		if _, _, err := store.CreateRun(ctx, input); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE runs SET created_at=? WHERE run_id=?`, formatTime(base.Add(time.Duration(index)*time.Second)), id); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.ListRuns(ctx, "owner/repo", time.Time{}, "", 2)
	if err != nil || len(page) != 2 || page[0].ID != "run-c" || page[1].ID != "run-b" {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	next, err := store.ListRuns(ctx, "owner/repo", page[1].CreatedAt, page[1].ID, 2)
	if err != nil || len(next) != 1 || next[0].ID != "run-a" {
		t.Fatalf("next page=%+v err=%v", next, err)
	}
	if _, err := store.ListRuns(ctx, "owner/repo", time.Time{}, "", 102); err == nil {
		t.Fatal("unbounded list limit was accepted")
	}
}

func TestLinearSourceDriftHaltsTheExactActiveRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-drift", IssueID: "IFAN-42", IdempotencyKey: "key-drift", SourceRevision: "revision-1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/ifan-42", ArtifactRoot: "/tmp/run-drift", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkLinearSourceDrift(context.Background(), "run-drift", domain.StateReceived, "revision-1", "linear-source-drift:digest"); err != nil || !marked {
		t.Fatalf("marked=%t err=%v", marked, err)
	}
	run, found, err := store.GetRunByIssue(context.Background(), "IFAN-42")
	if err != nil || !found || run.ID != "run-drift" || run.State != domain.StateManualIntervention {
		t.Fatalf("run=%+v found=%t err=%v", run, found, err)
	}
	inspection, err := store.Inspect(context.Background(), "run-drift")
	if err != nil || inspection.Run.State != domain.StateManualIntervention || len(inspection.Timeline) != 2 || inspection.Timeline[1].EvidenceReference != "linear-source-drift:digest" {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func TestRunIdempotencyRejectsLocalOwnershipPathDrift(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profile := application.LocalRepository{OriginPath: "/origin-a", SourcePath: "/source-a", RunRoot: "/runs-a", WorktreeRoot: "/worktrees-a"}
	raw, _ := json.Marshal(profile)
	input := application.CreateRunInput{Run: application.Run{ID: "run", IssueID: "issue", IdempotencyKey: "key", TaskHash: "task", SourceRevision: "v1", Repository: "owner/repo", RepositoryConfigJSON: string(raw), ProfileID: "repository-profile:owner/repo", ProfileSnapshotVersion: 1, ProfileDigest: "profile", ProfileSnapshotJSON: `{}`}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	profile.WorktreeRoot = "/worktrees-b"
	raw, _ = json.Marshal(profile)
	input.Run.RepositoryConfigJSON = string(raw)
	if _, _, err := store.CreateRun(context.Background(), input); err == nil {
		t.Fatal("idempotent create accepted local ownership path drift")
	}
}

func TestDatabaseIsPrivateAndMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode=%o", info.Mode().Perm())
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestMigratesLegacyCodeRabbitApprovalColumnWithoutLosingApproval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := application.Run{ID: "run", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/ifan-1", ArtifactRoot: "/tmp/run"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	approval := domain.HumanApproval{PRNumber: 1, Approver: "ifan0927", Actor: domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}, ReviewDatabaseID: 55, ReviewNodeID: "PRR_55", Source: "github_pull_request_review", ApprovedSHA: "head", CIStatus: "pass", ReviewSHA: "head", ApprovedAt: now, ObservedAt: now}
	if err := store.SaveHumanApproval(ctx, run.ID, approval); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE human_approvals ADD COLUMN coderabbit_status TEXT NOT NULL DEFAULT 'legacy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE side_effects DROP COLUMN claimed_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE linear_todo_admission_lease DROP COLUMN expires_at_unix_ns`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE verifications DROP COLUMN process_outcome`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE verifications DROP COLUMN failure_category`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE automatic_retry_schedules`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_attention_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_actions`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE trusted_review_feedback_conflicts`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE trusted_review_feedback`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE trusted_review_reply_evidence`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE attempts DROP COLUMN process_control_key`); err != nil {
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
	var removed, preserved int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('human_approvals') WHERE name='coderabbit_status'`).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM human_approvals WHERE run_id=? AND approved_sha=? AND approver=?`, run.ID, "head", "ifan0927").Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if removed != 0 || preserved != 1 {
		t.Fatalf("removed=%d preserved=%d", removed, preserved)
	}
}

func TestMergeMethodMigrationAcceptsOnlySquashAndExternal(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := application.Run{ID: "external-merge", IssueID: "IFAN-EXT", IdempotencyKey: "external-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/external", ArtifactRoot: "/tmp/external-merge"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	record := application.MergeRecord{RunID: run.ID, PRNumber: 1, PreMergeSHA: "head", BaseSHA: "base", Method: "external", MergeSHA: "merge", MergedAt: time.Now().UTC()}
	if err := store.SaveMerge(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.RunID = "other"
	record.Method = "merge"
	if err := store.SaveMerge(ctx, record); err == nil {
		t.Fatal("unsupported merge method must be rejected")
	}
}

func TestMigratesVersionOneDatabaseToVersionTwo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrationV1 {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(1,'v1')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name IN ('lease_owner','lease_expires_unix','implementation_model','review_model')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatal("current run lease or model columns are missing")
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name='requested_model'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("attempt requested_model column missing: count=%d err=%v", count, err)
	}
}

func TestMigratesVersionFourDatabaseToCurrentVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for version, migration := range [][]string{migrationV1, migrationV2, migrationV3, migrationV4} {
		for _, statement := range migration {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("v%d: %v", version+1, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version+1, fmt.Sprintf("v%d", version+1)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('side_effects','pull_requests','poll_observations','review_findings','human_approvals','human_approval_observations','merge_results','cleanup_results')`).Scan(&count); err != nil || count != 8 {
		t.Fatalf("delivery tables=%d err=%v", count, err)
	}
}

func TestSideEffectIntentSurvivesRestartWithoutDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	intent := application.SideEffectRecord{RunID: "run-1", Kind: "push", IdempotencyKey: "h1", IntentJSON: `{"head":"h1"}`, Attempt: 1}
	first, created, err := store.BeginSideEffect(context.Background(), intent)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	store.Close()
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, created, err := store.BeginSideEffect(context.Background(), intent)
	if err != nil || created || second.ID != first.ID || second.Status != "intent" {
		t.Fatalf("first=%+v second=%+v created=%v err=%v", first, second, created, err)
	}
}

func TestApprovalAndMergeEvidenceAreImmutable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Nanosecond)
	approval := domain.HumanApproval{PRNumber: 1, Approver: "ifan0927", Actor: domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}, ReviewDatabaseID: 55, ReviewNodeID: "PRR_55", Source: "github_pull_request_review", ApprovedSHA: "h1", CIStatus: "pass", ReviewSHA: "h1", ApprovedAt: now, ObservedAt: now}
	if err := store.SaveHumanApproval(ctx, "run-1", approval); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHumanApproval(ctx, "run-1", approval); err != nil {
		t.Fatal(err)
	}
	changed := approval
	changed.ApprovedSHA = "h2"
	changed.ReviewSHA = "h2"
	if err := store.SaveHumanApproval(ctx, "run-1", changed); err != nil {
		t.Fatalf("new HEAD approval: %v", err)
	}
	conflict := changed
	conflict.Approver = "mallory"
	if err := store.SaveHumanApproval(ctx, "run-1", conflict); err == nil {
		t.Fatal("conflicting same-HEAD approval must fail closed")
	}
	if err := store.SetCandidateHead(ctx, "run-1", "h2"); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, "run-1")
	if err != nil || inspection.Approval == nil || inspection.Approval.ApprovedSHA != "h2" {
		t.Fatalf("current approval=%+v err=%v", inspection.Approval, err)
	}
	merge := application.MergeRecord{RunID: "run-1", PRNumber: 1, PreMergeSHA: "h1", BaseSHA: "b1", Method: "squash", MergeSHA: "m1", MergedAt: now}
	if err := store.SaveMerge(ctx, merge); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMerge(ctx, merge); err != nil {
		t.Fatal(err)
	}
	changedMerge := merge
	changedMerge.MergeSHA = "m2"
	if err := store.SaveMerge(ctx, changedMerge); err == nil {
		t.Fatal("conflicting merge must fail closed")
	}
}

func TestHumanApprovalAcceptsOnlyNewerCompatibleObservation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	run := application.Run{ID: "approval-observation", IssueID: "IFAN-APPROVAL", IdempotencyKey: "approval-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/approval-observation", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, head); err != nil {
		t.Fatal(err)
	}
	sourceAt := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	approval := domain.HumanApproval{PRNumber: 1, Approver: "ifan0927", Actor: domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}, ReviewDatabaseID: 55, ReviewNodeID: "PRR_55", Source: "github_pull_request_review", ApprovedSHA: head, CIStatus: "pass", ReviewSHA: head, ApprovedAt: sourceAt, ObservedAt: sourceAt.Add(time.Minute)}
	if err := store.SaveHumanApproval(ctx, run.ID, approval); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHumanApproval(ctx, run.ID, approval); err != nil {
		t.Fatalf("equal observation must be idempotent: %v", err)
	}
	newer := approval
	newer.ObservedAt = newer.ObservedAt.Add(time.Minute)
	if err := store.SaveHumanApproval(ctx, run.ID, newer); err != nil {
		t.Fatalf("newer compatible observation: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.HumanApproval)
	}{
		{name: "older observation", mutate: func(value *domain.HumanApproval) { value.ObservedAt = approval.ObservedAt }},
		{name: "pull request", mutate: func(value *domain.HumanApproval) { value.PRNumber++ }},
		{name: "approver", mutate: func(value *domain.HumanApproval) { value.Approver = "mallory" }},
		{name: "actor database identity", mutate: func(value *domain.HumanApproval) { value.Actor.DatabaseID++ }},
		{name: "actor identity", mutate: func(value *domain.HumanApproval) { value.Actor.NodeID = "USER_OTHER" }},
		{name: "actor login", mutate: func(value *domain.HumanApproval) { value.Actor.Login = "mallory" }},
		{name: "actor type", mutate: func(value *domain.HumanApproval) { value.Actor.Type = "Bot" }},
		{name: "source", mutate: func(value *domain.HumanApproval) { value.Source = "other_source" }},
		{name: "review database identity", mutate: func(value *domain.HumanApproval) { value.ReviewDatabaseID++ }},
		{name: "review identity", mutate: func(value *domain.HumanApproval) { value.ReviewNodeID = "PRR_OTHER" }},
		{name: "review head", mutate: func(value *domain.HumanApproval) { value.ReviewSHA = strings.Repeat("b", 40) }},
		{name: "CI status", mutate: func(value *domain.HumanApproval) { value.CIStatus = "failed" }},
		{name: "approved at", mutate: func(value *domain.HumanApproval) { value.ApprovedAt = value.ApprovedAt.Add(time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conflict := newer
			conflict.ObservedAt = conflict.ObservedAt.Add(time.Minute)
			tc.mutate(&conflict)
			if err := store.SaveHumanApproval(ctx, run.ID, conflict); err == nil {
				t.Fatal("conflicting approval observation was accepted")
			}
		})
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || inspection.Approval == nil || inspection.Approval.ObservedAt != newer.ObservedAt {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM human_approvals WHERE run_id=?`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("approval rows=%d err=%v", count, err)
	}
}

func TestLinearCompletionEvidenceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := application.Run{ID: "run-completion", IssueID: "IFAN-13", IdempotencyKey: "completion-key", SourceRevision: "2026-07-13T00:00:00Z", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/13", ArtifactRoot: "/tmp/run-completion"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 4, 0, 0, 0, time.UTC)
	if err := store.SaveLinearRequestObservation(ctx, run.ID, application.LinearRequestObservation{Operation: "read_issue", HTTPStatus: 200, ResponseDigest: "digest", ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	record := application.LinearCompletionObservation{RunID: run.ID, MergeSHA: "merge", LinearIssueID: "linear-id", Identifier: run.IssueID, SourceRevision: now.Format(time.RFC3339Nano), StateID: "done", StateName: "Done", StateType: "completed", Status: application.LinearCompletionCompleted, ObservedAt: now}
	if err := store.SaveLinearCompletionObservation(ctx, record); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || len(inspection.LinearCompletion) != 1 {
		t.Fatalf("inspection=%+v err=%v", inspection.LinearCompletion, err)
	}
	if got := inspection.LinearCompletion[0]; got.MergeSHA != "merge" || got.Status != application.LinearCompletionCompleted || got.ObservedAt != now {
		t.Fatalf("completion=%+v", got)
	}
}

func TestGitHubApprovalObservationSurvivesRestartWithSourceAndObservationTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := application.Run{ID: "run-approval", IssueID: "IFAN-11", IdempotencyKey: "approval-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", BaseSHA: "base", ArtifactRoot: "/tmp/run-approval", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, "head"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateAwaitingHumanApproval, run.ID); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{Number: 11, DatabaseID: 111, URL: "https://example.invalid/pr/11", NodeID: "PR_11", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		t.Fatal(err)
	}
	owner := "lease-owner"
	if acquired, err := store.AcquireLease(ctx, run.ID, owner, time.Now().Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	sourceAt := time.Date(2026, 7, 13, 1, 0, 0, 0, time.UTC)
	observedAt := sourceAt.Add(time.Minute)
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	evidence := domain.GitHubReadEvidence{Repository: repo, PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: domain.CheckSuccess}}, ObservedAt: observedAt}
	approvalObservation := &domain.HumanApprovalObservation{PRNumber: pr.Number, CandidateHead: "head", Status: domain.HumanApprovalApproved, ReviewDatabaseID: 55, ReviewNodeID: "PRR_55", Actor: domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}, ReviewHeadSHA: "head", SourceAt: sourceAt, ObservedAt: observedAt}
	approval := &domain.HumanApproval{PRNumber: pr.Number, Approver: "ifan0927", Actor: approvalObservation.Actor, ReviewDatabaseID: 55, ReviewNodeID: "PRR_55", Source: "github_pull_request_review", ApprovedSHA: "head", CIStatus: "pass", ReviewSHA: "head", ApprovedAt: sourceAt, ObservedAt: observedAt}
	metadata := application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: observedAt.Add(time.Hour), PermissionsDigest: "permissions", ObservedAt: observedAt}
	if err := store.SaveGitHubReadSuccess(ctx, run.ID, owner, domain.StateAwaitingHumanApproval, run.IdempotencyKey, nil, pr, metadata, evidence, nil, approvalObservation, approval, domain.StateMerging, "trusted human approval is bound to the exact final head"); err != nil {
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
	if err != nil || inspection.Run.State != domain.StateMerging || inspection.Approval == nil || inspection.ApprovalObservation == nil || inspection.Approval.ObservedAt != observedAt || inspection.ApprovalObservation.SourceAt != sourceAt || inspection.ApprovalObservation.Status != domain.HumanApprovalApproved {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func TestResolvedAdvancedApprovalObservationSurvivesSQLiteRestartAndClaimsOneGuardedRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	run := application.Run{ID: "resolved-approval", IssueID: "IFAN-43", IdempotencyKey: "resolved-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", BaseSHA: "base", ArtifactRoot: "/tmp/resolved-approval", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, head); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateMerging, run.ID); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{Number: 43, DatabaseID: 143, URL: "https://example.invalid/pr/43", NodeID: "PR_43", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, BaseSHA: "base", BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		t.Fatal(err)
	}
	sourceAt := time.Date(2026, 7, 14, 2, 0, 0, 0, time.UTC)
	approval := domain.HumanApproval{PRNumber: pr.Number, Approver: "ifan0927", Actor: domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}, ReviewDatabaseID: 55, ReviewNodeID: "PRR_55", Source: "github_pull_request_review", ApprovedSHA: head, CIStatus: "pass", ReviewSHA: head, ApprovedAt: sourceAt, ObservedAt: sourceAt.Add(time.Minute)}
	if err := store.SaveHumanApproval(ctx, run.ID, approval); err != nil {
		t.Fatal(err)
	}
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: run.ID, Kind: "squash_merge", IdempotencyKey: head, IntentJSON: `{"pull_request":43}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	side.Status, side.ResultJSON, side.ObservedAt = "failed", `{"category":"merge_policy_pending"}`, approval.ObservedAt
	if err := store.RecordMergePolicyPending(ctx, run.ID, side, head); err != nil {
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
	owner := "restart-owner"
	if acquired, err := store.AcquireLease(ctx, run.ID, owner, time.Now().Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	resolvedAt := approval.ObservedAt.Add(time.Minute)
	resolvedApproval := approval
	resolvedApproval.ObservedAt = resolvedAt
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	evidence := domain.GitHubReadEvidence{Repository: repo, PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: head, State: domain.CheckSuccess}}, ReviewThreads: []domain.GitHubReviewThread{{NodeID: "THREAD_43", Resolved: true}}, ObservedAt: resolvedAt}
	approvalObservation := &domain.HumanApprovalObservation{PRNumber: pr.Number, CandidateHead: head, Status: domain.HumanApprovalApproved, ReviewDatabaseID: resolvedApproval.ReviewDatabaseID, ReviewNodeID: resolvedApproval.ReviewNodeID, Actor: resolvedApproval.Actor, ReviewHeadSHA: head, SourceAt: sourceAt, ObservedAt: resolvedAt}
	metadata := application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: resolvedAt.Add(time.Hour), PermissionsDigest: "permissions", ObservedAt: resolvedAt}
	if err := store.SaveGitHubReadSuccess(ctx, run.ID, owner, domain.StateAwaitingGitHubMergeability, run.IdempotencyKey, nil, pr, metadata, evidence, nil, approvalObservation, &resolvedApproval, domain.StateMerging, "controller-replied threads resolved; guarded merge may be retried"); err != nil {
		t.Fatalf("resolved advanced observation: %v", err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || inspection.Run.State != domain.StateMerging || inspection.Approval == nil || inspection.Approval.ObservedAt != resolvedAt {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	claimed, changed, err := store.RetryMergeSideEffect(ctx, side)
	if err != nil || !changed || claimed.Status != "intent" || claimed.Attempt != 2 {
		t.Fatalf("claimed=%+v changed=%v err=%v", claimed, changed, err)
	}
	if _, changed, err := store.RetryMergeSideEffect(ctx, side); err != nil || changed {
		t.Fatalf("duplicate guarded retry changed=%v err=%v", changed, err)
	}
}

func TestSideEffectAndPullRequestConflictsFailClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	intent := application.SideEffectRecord{RunID: "run-1", Kind: "push", IdempotencyKey: "h1", IntentJSON: `{"head":"h1"}`, Attempt: 1}
	if _, _, err := store.BeginSideEffect(ctx, intent); err != nil {
		t.Fatal(err)
	}
	changed := intent
	changed.IntentJSON = `{"head":"other"}`
	if _, _, err := store.BeginSideEffect(ctx, changed); err == nil {
		t.Fatal("conflicting side-effect intent must fail")
	}
	pr := domain.PullRequest{Number: 1, URL: "https://fixture/1", NodeID: "n1", HeadBranch: "ifan/one", BaseBranch: "main", HeadSHA: "h1", BaseSHA: "b1", BodyDigest: "d1", OwnershipKey: "key", State: "OPEN"}
	if err := store.SavePullRequest(ctx, "run-1", pr); err != nil {
		t.Fatal(err)
	}
	changedPR := pr
	changedPR.BaseBranch = "other"
	if err := store.SavePullRequest(ctx, "run-1", changedPR); err == nil {
		t.Fatal("conflicting PR evidence must fail")
	}
	updatedPR := pr
	updatedPR.HeadSHA = "h2"
	updatedPR.BodyDigest = "d2"
	if err := store.SavePullRequest(ctx, "run-1", updatedPR); err != nil {
		t.Fatalf("owned PR head update: %v", err)
	}
	inspection, err := store.Inspect(ctx, "run-1")
	if err != nil || inspection.PullRequest == nil || inspection.PullRequest.HeadSHA != "h2" {
		t.Fatalf("updated PR=%+v err=%v", inspection.PullRequest, err)
	}
}

func TestForeignKeysRemainEnabledAfterConnectionRecreation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.db.SetMaxIdleConns(0)
	if err := store.db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO attempts(run_id,number,kind,status,started_at,artifact_dir) VALUES('missing',1,'implementation','started','now','/tmp/missing')`)
	if err == nil {
		t.Fatal("foreign key constraint must survive a recreated connection")
	}
}

func TestRunLeaseUsesOwnerCASAndExpiry(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if ok, err := store.AcquireLease(context.Background(), "run-1", "owner-1", future); err != nil || !ok {
		t.Fatalf("acquire=%v err=%v", ok, err)
	}
	if ok, err := store.AcquireLease(context.Background(), "run-1", "owner-2", future); err != nil || ok {
		t.Fatalf("competing acquire=%v err=%v", ok, err)
	}
	if ok, err := store.RenewLease(context.Background(), "run-1", "owner-1", future.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("renew=%v err=%v", ok, err)
	}
	if err := store.ReleaseLease(context.Background(), "run-1", "owner-2"); err == nil {
		t.Fatal("wrong owner released lease")
	}
	if err := store.ReleaseLease(context.Background(), "run-1", "owner-1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AcquireLease(context.Background(), "run-1", "owner-2", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("reacquire=%v err=%v", ok, err)
	}
}

func TestGitHubFailureAuditUsesLeaseCAS(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := application.Run{ID: "run-audit", IdempotencyKey: "key", Repository: "owner/repo", RepositoryConfigJSON: "{}"}
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AcquireLease(context.Background(), run.ID, "owner", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("acquire=%v err=%v", ok, err)
	}
	observation := application.GitHubRequestObservation{RunID: run.ID, Operation: "read", ErrorClass: "timeout"}
	if err := store.SaveGitHubReadFailure(context.Background(), run.ID, "wrong", domain.StateReceived, "key", []application.GitHubRequestObservation{observation}); err == nil {
		t.Fatal("wrong lease owner persisted failure audit")
	}
	if err := store.SaveGitHubReadFailure(context.Background(), run.ID, "owner", domain.StateReceived, "key", []application.GitHubRequestObservation{observation}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM github_request_observations WHERE run_id=?`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
}

func TestTransitionUsesExpectedStateCompare(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "admit", "snapshot", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "duplicate", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "stale", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateAdmitting, domain.StateProvisioning, "provision", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateAdmitting, domain.StateFailed, "stale", "", ""); err == nil {
		t.Fatal("stale expected state must fail")
	}
}

func TestAttemptArtifactDirectoryCannotBeReused(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.BeginAttempt(context.Background(), "run-1", "implementation", "gpt-5.6-terra", "/tmp/run/attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempt.ProcessControlKey) != 64 {
		t.Fatalf("process control key length=%d", len(attempt.ProcessControlKey))
	}
	if attempt.Status != "prepared" {
		t.Fatalf("attempt status=%q", attempt.Status)
	}
	if committed, err := store.CommitAttemptProcessLaunch(context.Background(), attempt.ID); err != nil || !committed {
		t.Fatalf("commit process launch: committed=%t err=%v", committed, err)
	}
	if committed, err := store.CommitAttemptProcessLaunch(context.Background(), attempt.ID); err != nil || committed {
		t.Fatalf("duplicate process launch commit: committed=%t err=%v", committed, err)
	}
	inspection, err := store.Inspect(context.Background(), "run-1")
	if err != nil || len(inspection.Attempts) != 1 || inspection.Attempts[0].Status != "started" || inspection.Attempts[0].RequestedModel != "gpt-5.6-terra" || inspection.Attempts[0].ProcessControlKey != attempt.ProcessControlKey {
		t.Fatalf("attempt control evidence was not persisted: err=%v", err)
	}
	public, err := json.Marshal(inspection)
	if err != nil || bytes.Contains(public, []byte(attempt.ProcessControlKey)) {
		t.Fatalf("process control key leaked into inspection JSON: err=%v", err)
	}
	if _, err := store.BeginAttempt(context.Background(), "run-1", "resume", "gpt-5.6-terra", "/tmp/run/attempt-1"); err == nil {
		t.Fatal("artifact directory reuse must fail")
	}
}

func TestOwnedResourceCannotBeClaimedByAnotherRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index := 1; index <= 2; index++ {
		id := fmt.Sprintf("run-%d", index)
		input := application.CreateRunInput{Run: application.Run{ID: id, IssueID: fmt.Sprintf("ISSUE-%d", index), IdempotencyKey: fmt.Sprintf("key-%d", index), SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/shared", ArtifactRoot: "/tmp/" + id}}
		if _, _, err := store.CreateRun(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	resource := application.OwnedResource{RunID: "run-1", Kind: "branch", Name: "ifan/shared", CreationEvidence: "{}", Status: "reserved"}
	if err := store.AddOwnedResource(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	resource.RunID = "run-2"
	if err := store.AddOwnedResource(context.Background(), resource); err == nil {
		t.Fatal("second run must not claim an owned branch")
	}
}

func TestBeginRepairAtomicallyRollsCandidateIntoTransition(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	states := []domain.State{domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateVerifying, domain.StateFreshReview, domain.StateRepairing}
	current := domain.StateReceived
	for _, next := range states {
		if err := store.Transition(ctx, "run-1", current, next, "test", "", "h1"); err != nil {
			t.Fatal(err)
		}
		current = next
	}
	if err := store.SetCandidateHead(ctx, "run-1", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRepair(ctx, "run-1", "h1", `{"normalized_prompt":"repair","prompt_hash":"hash"}`); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	latest := inspection.Timeline[len(inspection.Timeline)-1]
	if inspection.Run.State != domain.StateExecuting || inspection.Run.CandidateHead != "" || latest.BoundHead != "h1" {
		t.Fatalf("repair rollover=%+v", inspection)
	}
}
