package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type abandonCleanupFake struct {
	calls             []string
	err               error
	afterWorktree     func()
	afterBranch       func()
	beforeRemote      func()
	blockUntilContext bool
	operationStarted  chan struct{}
	reconcileAbsent   bool
	reconcileErr      error
	reconcileCalls    []string
}

type abandonGitHubReader struct {
	evidence     domain.GitHubReadEvidence
	metadata     GitHubInstallationMetadata
	observations []GitHubRequestObservation
	err          error
}

type abandonSequenceGitHubReader struct {
	evidence []domain.GitHubReadEvidence
	errors   []error
	metadata GitHubInstallationMetadata
	calls    int
}

func (r *abandonSequenceGitHubReader) Authority() GitHubInstallationMetadata { return r.metadata }

func (r *abandonSequenceGitHubReader) Read(context.Context, int64, string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	index := r.calls
	r.calls++
	if index >= len(r.evidence) {
		return domain.GitHubReadEvidence{}, domain.InlineReviewBodyHandoff{}, nil, r.metadata, errors.New("unexpected GitHub read")
	}
	var err error
	if index < len(r.errors) {
		err = r.errors[index]
	}
	return r.evidence[index], domain.InlineReviewBodyHandoff{}, nil, r.metadata, err
}

func (r abandonGitHubReader) Authority() GitHubInstallationMetadata { return r.metadata }

func (r abandonGitHubReader) Read(context.Context, int64, string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	return r.evidence, domain.InlineReviewBodyHandoff{}, r.observations, r.metadata, r.err
}

type abandonCoordinatorStore struct {
	*pushTestStore
	abandonCalls int
	action       OperatorActionRecord
	attention    OperatorAttentionEvent
	stopErr      error
}

func (s *abandonCoordinatorStore) Inspect(ctx context.Context, runID string) (RunInspection, error) {
	inspection, err := s.pushTestStore.Inspect(ctx, runID)
	if err == nil && s.action.ActionID != "" {
		inspection.OperatorActions = []OperatorActionRecord{s.action}
	}
	return inspection, err
}

func (s *abandonCoordinatorStore) CurrentOperatorAttention(context.Context, string) (OperatorAttentionEvent, bool, error) {
	if count := len(s.pushTestStore.attention); count > 0 {
		event := s.pushTestStore.attention[count-1]
		return event, true, nil
	}
	return s.attention, s.attention.EventKey != "", nil
}

func (s *abandonCoordinatorStore) BeginOperatorAction(_ context.Context, record OperatorActionRecord) (OperatorActionRecord, bool, error) {
	if s.action.ActionID != "" {
		return s.action, false, nil
	}
	s.action = record
	return record, true, nil
}

func (s *abandonCoordinatorStore) ApplyOperatorActionResult(_ context.Context, result OperatorActionMutationResult) (OperatorActionRecord, bool, error) {
	if s.action.Status != result.ExpectedStatus {
		return s.action, false, nil
	}
	s.action.Status = OperatorActionStatusApplied
	s.action.ResultStatus = result.ResultStatus
	s.action.ResultingState = result.ResultingState
	s.action.ResultingTransitionSequence = result.ResultingTransitionSequence
	s.action.EvidenceDigest = result.EvidenceDigest
	s.action.AppliedAt = result.At
	return s.action, true, nil
}

func (s *abandonCoordinatorStore) ObserveOperatorActionResult(_ context.Context, result OperatorActionMutationResult) (OperatorActionRecord, bool, error) {
	if s.action.Status != result.ExpectedStatus {
		return s.action, false, nil
	}
	s.action.Status = OperatorActionStatusObserved
	s.action.ResultStatus = result.ResultStatus
	s.action.OutcomeDigest = result.EvidenceDigest
	s.action.ObservedAt = result.At
	return s.action, true, nil
}

func (s *abandonCoordinatorStore) StopAutomaticAdmissionAttempts(_ context.Context, runID, _ string, stoppedAt time.Time) (int64, error) {
	if s.stopErr != nil {
		return 0, s.stopErr
	}
	var count int64
	for index := range s.inspection.Attempts {
		attempt := &s.inspection.Attempts[index]
		if attempt.RunID == runID && (attempt.Status == "prepared" || attempt.Status == "started") {
			attempt.Status, attempt.FinishedAt, attempt.ExitCode, attempt.ErrorCategory = "failed", stoppedAt, -1, AutomaticAdmissionAbandonReason
			count++
		}
	}
	return count, nil
}

func (s *abandonCoordinatorStore) AbandonAutomaticAdmission(_ context.Context, request AutomaticAdmissionAbandonment) (Run, bool, error) {
	if s.run.State == domain.StateFailed {
		return s.run, true, nil
	}
	if s.run.State != request.ExpectedState || s.run.IdempotencyKey != request.IdempotencyKey {
		return Run{}, false, errors.New("abandon compare failed")
	}
	s.abandonCalls++
	s.transitions = append(s.transitions, Transition{Sequence: int64(len(s.transitions) + 1), From: s.run.State, To: domain.StateFailed, Reason: AutomaticAdmissionAbandonTransition, CreatedAt: time.Now().UTC()})
	s.run.State = domain.StateFailed
	s.run.LastError = AutomaticAdmissionAbandonTransition
	s.run.UpdatedAt = s.transitions[len(s.transitions)-1].CreatedAt
	return s.run, false, nil
}

func (f *abandonCleanupFake) RemoveWorktree(ctx context.Context, _ string, _ string, _ string, _ string) error {
	f.calls = append(f.calls, "worktree")
	if f.blockUntilContext {
		if f.operationStarted != nil {
			close(f.operationStarted)
		}
		return waitForAbandonCleanupContext(ctx)
	}
	if f.afterWorktree != nil {
		f.afterWorktree()
	}
	return f.err
}

func (f *abandonCleanupFake) DeleteLocalBranch(ctx context.Context, _ string, _ string, _ string) error {
	f.calls = append(f.calls, "branch")
	if f.blockUntilContext {
		if f.operationStarted != nil {
			close(f.operationStarted)
		}
		return waitForAbandonCleanupContext(ctx)
	}
	if f.afterBranch != nil {
		f.afterBranch()
	}
	return f.err
}

func waitForAbandonCleanupContext(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *abandonCleanupFake) DeleteRemoteBranch(context.Context, string, string, string) error {
	if f.beforeRemote != nil {
		f.beforeRemote()
	}
	f.calls = append(f.calls, "remote")
	return f.err
}

func (f *abandonCleanupFake) CleanupResourceAbsent(_ context.Context, _ string, kind, _ string) (bool, error) {
	f.reconcileCalls = append(f.reconcileCalls, kind)
	return f.reconcileAbsent, f.reconcileErr
}

type abandonChildStopperFake struct {
	err          error
	calls        []string
	beforeReturn func()
}

func (f *abandonChildStopperFake) StopAttempt(_ context.Context, artifactDir, _ string) error {
	f.calls = append(f.calls, artifactDir)
	if f.beforeReturn != nil {
		f.beforeReturn()
	}
	return f.err
}

func TestAbandonLocalCleanupRetainsArtifactAndUsesOnlyOwnedLocalResources(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, resources: []OwnedResource{
		{RunID: run.ID, Kind: "artifact_root", Name: run.ArtifactRoot, CreationEvidence: `{"path":"/owned/artifacts","attempts_path":"/owned/artifacts/attempts","run_root":"/owned","nonce":"artifact","task_hash":"task"}`, Status: "owned"},
		{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, CreationEvidence: evidence, Status: "owned"},
		{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
	}}
	cleanup := &abandonCleanupFake{}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err != nil {
		t.Fatal(err)
	}
	if len(cleanup.calls) != 2 || cleanup.calls[0] != "worktree" || cleanup.calls[1] != "branch" {
		t.Fatalf("cleanup calls=%v", cleanup.calls)
	}
	if len(store.cleanup) != 3 {
		t.Fatalf("cleanup audit=%+v", store.cleanup)
	}
	for _, item := range store.cleanup {
		if item.Kind == "artifact_root" && item.Status != "retained" {
			t.Fatalf("artifact cleanup status=%+v", item)
		}
	}
	for _, resource := range store.resources {
		if (resource.Kind == "worktree" || resource.Kind == "branch") && resource.Status == "owned" {
			// The in-memory production fixture intentionally appends ownership updates;
			// the SQLite adapter performs the same update in place.
			continue
		}
		if resource.Kind == "artifact_root" && resource.Status != "owned" {
			t.Fatalf("artifact ownership changed=%+v", resource)
		}
	}
}

func TestRefreshAbandonGitHubEvidenceAdoptsFreshClosedUnmergedState(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	run.IdempotencyKey = "run-key"
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store := &pushTestStore{run: run, pr: &pr, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr}}
	fresh := pr
	fresh.State = "closed"
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	reader := abandonGitHubReader{evidence: domain.GitHubReadEvidence{Repository: repositoryIdentity, PullRequest: fresh, ObservedAt: time.Now().UTC()}, metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity, ObservedAt: time.Now().UTC()}, observations: []GitHubRequestObservation{{Operation: "read_pull_request", Category: "pull_request", ResponseDigest: "digest", ObservedAt: time.Now().UTC()}}}

	updated, err := refreshAbandonGitHubEvidence(context.Background(), store, store.inspection, reader)
	if err != nil || updated.PullRequest == nil || updated.PullRequest.State != "closed" || store.pr == nil || store.pr.State != "closed" || len(store.github) != 1 || len(store.metadata) != 1 || len(store.requests) != 1 || store.requests[0].RunID != run.ID {
		t.Fatalf("updated=%+v pr=%+v github=%d metadata=%d requests=%+v err=%v", updated.PullRequest, store.pr, len(store.github), len(store.metadata), store.requests, err)
	}
}

func TestRefreshAbandonGitHubEvidenceFailsClosedForMergeAndAuthorityDrift(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	run.IdempotencyKey = "run-key"
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	for _, test := range []struct {
		name     string
		fresh    domain.PullRequest
		metadata GitHubInstallationMetadata
	}{
		{name: "merged", fresh: func() domain.PullRequest { value := pr; value.Merged = true; value.State = "closed"; return value }(), metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity}},
		{name: "installation drift", fresh: pr, metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 99, Repository: repositoryIdentity}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &pushTestStore{run: run, pr: &pr, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr}}
			reader := abandonGitHubReader{evidence: domain.GitHubReadEvidence{Repository: repositoryIdentity, PullRequest: test.fresh, ObservedAt: time.Now().UTC()}, metadata: test.metadata, observations: []GitHubRequestObservation{{Operation: "read_pull_request", Category: "pull_request", ResponseDigest: "digest", ObservedAt: time.Now().UTC()}}}
			if _, err := refreshAbandonGitHubEvidence(context.Background(), store, store.inspection, reader); err == nil || len(store.github) != 0 || len(store.metadata) != 0 || len(store.requests) != 1 {
				t.Fatalf("github=%d metadata=%d requests=%d err=%v", len(store.github), len(store.metadata), len(store.requests), err)
			}
		})
	}
}

func TestAbandonLocalCleanupPersistsFailureDetails(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, resources: []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"}}}
	cleanup := &abandonCleanupFake{err: errors.New("branch cleanup failed while removing candidate")}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err == nil {
		t.Fatal("cleanup unexpectedly succeeded")
	}
	progress, err := store.CleanupProgress(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0].Status != "failed" || progress[0].LastError != "branch cleanup failed while removing candidate" {
		t.Fatalf("cleanup failure audit=%+v", progress)
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Cleanup) != 1 || inspection.Cleanup[0].LastError != progress[0].LastError {
		t.Fatalf("inspection cleanup audit=%+v", inspection.Cleanup)
	}
}

func TestAbandonCleanupAdoptsClosedOwnedPullRequestAndRetainsRemoteBranch(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	pr := domain.PullRequest{Number: 7, State: "closed", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, OwnershipKey: "key"}
	store := &pushTestStore{run: run, pr: &pr, side: SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}, resources: []OwnedResource{
		{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	}}
	cleanup := &abandonCleanupFake{}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err == nil {
		t.Fatal("closed PR remote branch was not retained as residue")
	}
	if len(cleanup.calls) != 0 {
		t.Fatalf("cleanup calls=%v", cleanup.calls)
	}
	progress, err := store.CleanupProgress(context.Background(), run.ID)
	if err != nil || !hasAbandonCleanupStatus(progress, "pull_request", "7", "deleted") || !hasAbandonCleanupStatus(progress, "remote_branch", run.WorkingBranch, "retained") {
		t.Fatalf("cleanup progress=%+v err=%v", progress, err)
	}
}

func TestAbandonCleanupDeletesOwnedRemoteBranchWhileOpenPullRequestAwaitsFreshRead(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	pr := domain.PullRequest{Number: 7, State: "open", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, OwnershipKey: "key"}
	store := &pushTestStore{run: run, pr: &pr, side: SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}, resources: []OwnedResource{
		{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	}}
	cleanup := &abandonCleanupFake{}
	err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup)
	if err != nil || len(cleanup.calls) != 1 || cleanup.calls[0] != "remote" {
		t.Fatalf("open PR cleanup err=%v calls=%v", err, cleanup.calls)
	}
	if !hasAbandonCleanupStatus(store.cleanup, "pull_request", "7", "retained") || !hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "deleted") {
		t.Fatalf("cleanup progress=%+v", store.cleanup)
	}
}

func TestAbandonCleanupRetainsRemoteBranchWhenOpenPullRequestOwnershipIsUnproven(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	pr := domain.PullRequest{Number: 7, State: "open", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, OwnershipKey: "key"}
	store := &pushTestStore{run: run, pr: &pr, resources: []OwnedResource{
		{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	}}
	cleanup := &abandonCleanupFake{}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err == nil || len(cleanup.calls) != 0 {
		t.Fatalf("cleanup calls=%v err=%v", cleanup.calls, err)
	}
	if !hasAbandonCleanupStatus(store.cleanup, "pull_request", "7", "retained") || !hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "retained") {
		t.Fatalf("cleanup progress=%+v", store.cleanup)
	}
}

func TestAbandonCleanupAdoptsAbsentResourceAfterPersistedIntentWithoutRepeatingDelete(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, resources: []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"}}, cleanup: []CleanupRecord{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, Status: "intent"}}}
	cleanup := &abandonCleanupFake{reconcileAbsent: true}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err != nil {
		t.Fatal(err)
	}
	if len(cleanup.calls) != 0 || len(cleanup.reconcileCalls) != 1 || cleanup.reconcileCalls[0] != "branch" || !hasAbandonCleanupStatus(store.cleanup, "branch", run.WorkingBranch, "deleted") {
		t.Fatalf("delete calls=%v reconcile=%v progress=%+v", cleanup.calls, cleanup.reconcileCalls, store.cleanup)
	}
}

func TestAbandonCleanupContinuesSafeResourcesWhenUnknownResidueExists(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, resources: []OwnedResource{
		{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		{RunID: run.ID, Kind: "unknown", Name: "external", CreationEvidence: "unknown", Status: "owned"},
	}}
	cleanup := &abandonCleanupFake{}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err == nil || len(cleanup.calls) != 1 || cleanup.calls[0] != "branch" {
		t.Fatalf("partial cleanup err=%v calls=%v", err, cleanup.calls)
	}
}

func hasAbandonCleanupStatus(records []CleanupRecord, kind, name, status string) bool {
	for _, record := range records {
		if record.Kind == kind && record.Name == name && record.Status == status {
			return true
		}
	}
	return false
}

func TestAbandonLocalCleanupReportsFailureAuditPersistenceError(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, cleanupFailAt: 2, resources: []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"}}}
	cleanup := &abandonCleanupFake{err: errors.New("branch cleanup failed while removing candidate")}
	err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup)
	if err == nil || !strings.Contains(err.Error(), "branch cleanup failed while removing candidate") || !strings.Contains(err.Error(), "persist abandon cleanup failure audit") {
		t.Fatalf("cleanup error did not expose audit persistence failure: %v", err)
	}
	progress, progressErr := store.CleanupProgress(context.Background(), run.ID)
	if progressErr != nil || len(progress) != 1 || progress[0].Status != "intent" {
		t.Fatalf("cleanup progress=%+v err=%v", progress, progressErr)
	}
}

func TestAbandonLocalCleanupStopsWhenLeaseIsLostBetweenResources(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, leaseHeld: true, resources: []OwnedResource{
		{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, CreationEvidence: evidence, Status: "owned"},
		{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
	}}
	cleanup := &abandonCleanupFake{afterWorktree: func() {
		store.leaseMu.Lock()
		store.leaseLost = true
		store.leaseMu.Unlock()
	}}
	err := cleanupAbandonedLocalResourcesWithLease(context.Background(), store, run, cleanup, "abandon-owner")
	if err == nil || !strings.Contains(err.Error(), "persist abandon cleanup failure audit") {
		t.Fatal("cleanup unexpectedly crossed a lost lease")
	}
	if len(cleanup.calls) != 1 || cleanup.calls[0] != "worktree" {
		t.Fatalf("cleanup calls after lease loss=%v", cleanup.calls)
	}
	progress, err := store.CleanupProgress(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0].Kind != "worktree" || progress[0].Status != "intent" {
		t.Fatalf("lease loss audit=%+v", progress)
	}
}

func TestAbandonLocalCleanupBoundsGitOperationByLeaseExpiry(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, leaseHeld: true, resources: []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"}}}
	cleanup := &abandonCleanupFake{blockUntilContext: true, operationStarted: make(chan struct{})}
	err := cleanupAbandonedLocalResourcesWithLeaseTTL(context.Background(), store, run, cleanup, "abandon-owner", 20*time.Millisecond, false, false)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup did not stop at the lease deadline: %v", err)
	}
	if len(cleanup.calls) != 1 || cleanup.calls[0] != "branch" {
		t.Fatalf("cleanup calls=%v", cleanup.calls)
	}
	progress, progressErr := store.CleanupProgress(context.Background(), run.ID)
	if progressErr != nil || len(progress) != 1 || progress[0].Status != "failed" {
		t.Fatalf("cleanup progress=%+v err=%v", progress, progressErr)
	}
}

func TestProductionAbandonRevalidatesBeforeDurableMutationAndCleansLocally(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{})
	if err != nil || result.Action != ProductionAbandon || result.Run.State != domain.StateFailed || result.Idempotent || wrapped.abandonCalls != 1 {
		t.Fatalf("result=%+v err=%v abandonCalls=%d", result, err, wrapped.abandonCalls)
	}
	if cleanup.calls == nil || len(cleanup.calls) != 1 || cleanup.calls[0] != "branch" {
		t.Fatalf("cleanup calls=%v", cleanup.calls)
	}
}

func TestProductionAbandonDoesNotRequireStopProofForPreparedAttempt(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	store.inspection.Attempts = []Attempt{{ID: 1, RunID: run.ID, Status: "prepared", ArtifactDir: "/owned/artifacts/attempt-1", ProcessControlKey: "control-key"}}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{}
	stopper := &abandonChildStopperFake{err: errors.New("prepared attempt must not call stopper")}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, stopper)
	if err != nil || result.Run.State != domain.StateFailed || result.ResidueAttention || len(stopper.calls) != 0 || !slices.Contains(cleanup.calls, "branch") {
		t.Fatalf("result=%+v stop=%v cleanup=%v err=%v", result, stopper.calls, cleanup.calls, err)
	}
	if wrapped.inspection.Attempts[0].Status != "failed" || wrapped.inspection.Attempts[0].ErrorCategory != AutomaticAdmissionAbandonReason {
		t.Fatalf("prepared attempt was not terminalized: %+v", wrapped.inspection.Attempts[0])
	}
}

func TestProductionAbandonTerminalizesAfterCleanupBudgetExpires(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	coordinator.abandonCleanupTTL = 20 * time.Millisecond
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{blockUntilContext: true, operationStarted: make(chan struct{})}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{})
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || wrapped.abandonCalls != 1 {
		t.Fatalf("result=%+v abandon=%d cleanup=%v err=%v", result, wrapped.abandonCalls, cleanup.calls, err)
	}
	if wrapped.action.Status != OperatorActionStatusObserved || wrapped.action.ResultStatus != OperatorActionResultSucceeded {
		t.Fatalf("action=%+v", wrapped.action)
	}
}

func TestProductionAbandonSkipsResourceCleanupWhenChildStopIsUnproven(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	store.inspection.Attempts = []Attempt{{ID: 1, RunID: run.ID, Status: "started", ArtifactDir: "/owned/artifacts/attempt-1"}}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{}
	stopper := &abandonChildStopperFake{err: errors.New("managed child stop is unproven")}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, stopper)
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(stopper.calls) != 1 || len(cleanup.calls) != 0 {
		t.Fatalf("stop calls=%v cleanup calls=%v", stopper.calls, cleanup.calls)
	}
	if wrapped.inspection.Attempts[0].Status != "started" {
		t.Fatalf("unproven child stop was recorded as finished: %+v", wrapped.inspection.Attempts[0])
	}
}

func TestProductionAbandonTerminalizesWhenAttemptStopEvidenceWriteFails(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	store.inspection.Attempts = []Attempt{{ID: 1, RunID: run.ID, Status: "started", ArtifactDir: "/owned/artifacts/attempt-1", ProcessControlKey: "control-key"}}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	wrapped.stopErr = errors.New("attempt stop evidence unavailable")
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{})
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || len(cleanup.calls) != 0 || wrapped.abandonCalls != 1 {
		t.Fatalf("result=%+v cleanup=%v abandon=%d err=%v", result, cleanup.calls, wrapped.abandonCalls, err)
	}
}

func TestProductionAbandonTerminalReplayWithoutResidueAttentionIsNoop(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateFailed)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store.side = SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}
	evidence := store.resources[0].CreationEvidence
	store.resources = append(store.resources,
		OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		OwnedResource{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	)
	store.cleanup = append(store.cleanup, CleanupRecord{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "intent"})
	wrapped := &abandonCoordinatorStore{pushTestStore: store, action: OperatorActionRecord{ActionID: "abandon-action", RunID: run.ID, Repository: run.Repository, RunIdempotencyKey: run.IdempotencyKey, ActionType: OperatorActionAbandon, Requester: abandonRequester(), Status: OperatorActionStatusObserved}}
	coordinator.store = wrapped
	reader := &abandonSequenceGitHubReader{}
	cleanup := &abandonCleanupFake{reconcileAbsent: true}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: domain.StateFailed, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{}, reader)
	if err != nil || !result.Idempotent || result.Run.State != domain.StateFailed || reader.calls != 0 || len(cleanup.reconcileCalls) != 0 || len(cleanup.calls) != 0 {
		t.Fatalf("result=%+v reads=%d cleanup=%v reconcile=%v err=%v", result, reader.calls, cleanup.calls, cleanup.reconcileCalls, err)
	}
}

func TestProductionAbandonTerminalReplayRepairsValidatedActionResult(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateFailed)
	wrapped := &abandonCoordinatorStore{pushTestStore: store, action: OperatorActionRecord{
		ActionID: "abandon-action", RunID: run.ID, Repository: run.Repository,
		RunIdempotencyKey: run.IdempotencyKey, ActionType: OperatorActionAbandon,
		Requester: abandonRequester(), Status: OperatorActionStatusValidated,
		ValidatedAt: run.UpdatedAt.Add(-time.Second),
	}}
	store.transitions = []Transition{{Sequence: 2, From: domain.StateManualIntervention, To: domain.StateFailed, Reason: AutomaticAdmissionAbandonTransition, CreatedAt: run.UpdatedAt}}
	coordinator.store = wrapped
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: domain.StateFailed, IdempotencyKey: run.IdempotencyKey}, &abandonCleanupFake{}, &abandonChildStopperFake{})
	if err != nil || !result.Idempotent || wrapped.action.Status != OperatorActionStatusObserved || wrapped.action.ResultStatus != OperatorActionResultSucceeded {
		t.Fatalf("result=%+v action=%+v err=%v", result, wrapped.action, err)
	}
}

func TestProductionAbandonTerminalizesAfterCallerCancellationFollowingIntent(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	store.inspection.Attempts = []Attempt{{ID: 1, RunID: run.ID, Status: "started", ArtifactDir: "/owned/artifacts/attempt-1", ProcessControlKey: "control-key"}}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	ctx, cancel := context.WithCancel(context.Background())
	stopper := &abandonChildStopperFake{beforeReturn: cancel, err: context.Canceled}
	result, err := coordinator.Abandon(ctx, ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &abandonCleanupFake{}, stopper)
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || wrapped.abandonCalls != 1 {
		t.Fatalf("result=%+v abandon=%d err=%v", result, wrapped.abandonCalls, err)
	}
}

func TestAbandonRetainsRemoteBranchWithoutFreshPullRequestAuthority(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, resources: []OwnedResource{{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"}}}
	cleanup := &abandonCleanupFake{}
	err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup)
	if err == nil || len(cleanup.calls) != 0 || !hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "retained") {
		t.Fatalf("cleanup=%v audit=%+v err=%v", cleanup.calls, store.cleanup, err)
	}
}

func TestProductionAbandonReplayRevalidatesBeforeRetryingLocalCleanup(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{err: errors.New("local cleanup is temporarily unavailable")}
	first := ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}
	if result, err := coordinator.Abandon(context.Background(), first, cleanup, &abandonChildStopperFake{}); err != nil || !result.ResidueAttention || len(cleanup.calls) != 1 {
		t.Fatalf("first abandon result=%+v err=%v cleanup calls=%v", result, err, cleanup.calls)
	}
	reader := coordinator.admission.reader.(*admissionReader)
	reader.source.SourceRevision = "2026-07-16T00:00:00Z"
	before := len(cleanup.calls)
	replay := first
	replay.ExpectedState = domain.StateFailed
	if _, err := coordinator.Abandon(context.Background(), replay, cleanup, &abandonChildStopperFake{}); err == nil || len(cleanup.calls) != before {
		t.Fatalf("drifted replay crossed cleanup boundary err=%v cleanup calls=%v", err, cleanup.calls)
	}
}

func TestProductionAbandonRechecksPullRequestAfterCleanupBeforeTerminalCAS(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store.side = SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}
	evidence := store.resources[0].CreationEvidence
	store.resources = append(store.resources,
		OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		OwnedResource{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	)
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	merged := pr
	merged.State, merged.Merged = "closed", true
	reader := &abandonSequenceGitHubReader{evidence: []domain.GitHubReadEvidence{{Repository: repositoryIdentity, PullRequest: pr, ObservedAt: time.Now().UTC()}, {Repository: repositoryIdentity, PullRequest: merged, ObservedAt: time.Now().UTC()}}, metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity}}
	cleanup := &abandonCleanupFake{}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{}, reader)
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || wrapped.abandonCalls != 1 || reader.calls != 2 || slices.Contains(cleanup.calls, "remote") {
		t.Fatalf("result=%+v abandonCalls=%d reads=%d cleanup=%v err=%v", result, wrapped.abandonCalls, reader.calls, cleanup.calls, err)
	}
}

func TestProductionAbandonTerminalizesWhenFinalPullRequestReadFails(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store.side = SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}
	evidence := store.resources[0].CreationEvidence
	store.resources = append(store.resources,
		OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		OwnedResource{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	)
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	reader := &abandonSequenceGitHubReader{
		evidence: []domain.GitHubReadEvidence{{Repository: repositoryIdentity, PullRequest: pr, ObservedAt: time.Now().UTC()}, {}},
		errors:   []error{nil, errors.New("final read unavailable")},
		metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity},
	}
	cleanup := &abandonCleanupFake{}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{}, reader)
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || reader.calls != 2 || slices.Contains(cleanup.calls, "remote") {
		t.Fatalf("result=%+v reads=%d cleanup=%v err=%v", result, reader.calls, cleanup.calls, err)
	}
}

func TestProductionAbandonTerminalizesWhenFreshGitHubReaderIsUnavailable(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{})
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || wrapped.abandonCalls != 1 || slices.Contains(cleanup.calls, "remote") {
		t.Fatalf("result=%+v cleanup=%v abandon=%d err=%v", result, cleanup.calls, wrapped.abandonCalls, err)
	}
}

func TestProductionAbandonBlocksFreshMergedPullRequestEvidence(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	merged := pr
	merged.State, merged.Merged = "closed", true
	reader := abandonGitHubReader{evidence: domain.GitHubReadEvidence{Repository: repositoryIdentity, PullRequest: merged, ObservedAt: time.Now().UTC()}, metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity}}
	cleanup := &abandonCleanupFake{}
	if _, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{}, reader); err == nil || wrapped.run.State != domain.StateManualIntervention || len(cleanup.calls) != 0 {
		t.Fatalf("state=%s cleanup=%v err=%v", wrapped.run.State, cleanup.calls, err)
	}
	if wrapped.action.Status != OperatorActionStatusObserved || wrapped.action.ResultStatus != OperatorActionResultFailed || wrapped.action.OutcomeDigest == "" {
		t.Fatalf("blocked action was not durably concluded: %+v", wrapped.action)
	}
}

func TestProductionAbandonTreatsUnauthenticatedMergedReadErrorAsResidue(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	merged := pr
	merged.State, merged.Merged = "closed", true
	reader := abandonGitHubReader{
		evidence: domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 999, Owner: "attacker", Name: "repo"}, PullRequest: merged, ObservedAt: time.Now().UTC()},
		metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: domain.RepositoryIdentity{ID: 999, Owner: "attacker", Name: "repo"}},
		err:      errors.New("untrusted partial GitHub response"),
	}
	cleanup := &abandonCleanupFake{}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{}, reader)
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || wrapped.abandonCalls != 1 || slices.Contains(cleanup.calls, "remote") {
		t.Fatalf("result=%+v cleanup=%v abandon=%d err=%v", result, cleanup.calls, wrapped.abandonCalls, err)
	}
}

func TestProductionAbandonDeletesRemoteOnlyAfterFinalFreshPullRequestRead(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store.side = SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}
	evidence := store.resources[0].CreationEvidence
	store.resources = append(store.resources,
		OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		OwnedResource{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	)
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	reader := &abandonSequenceGitHubReader{evidence: []domain.GitHubReadEvidence{{Repository: repositoryIdentity, PullRequest: pr, ObservedAt: time.Now().UTC()}, {Repository: repositoryIdentity, PullRequest: pr, ObservedAt: time.Now().UTC()}}, metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity}}
	cleanup := &abandonCleanupFake{beforeRemote: func() {
		if reader.calls != 2 {
			t.Fatalf("remote cleanup began after %d fresh reads", reader.calls)
		}
	}}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{}, reader)
	if err != nil || result.Run.State != domain.StateFailed || reader.calls != 2 || !slices.Contains(cleanup.calls, "remote") {
		t.Fatalf("result=%+v reads=%d cleanup=%v err=%v", result, reader.calls, cleanup.calls, err)
	}
}

func TestProductionAbandonReconcilesRemoteDeleteCrashBeforeHeadDrift(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store.side = SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}
	evidence := store.resources[0].CreationEvidence
	store.resources = append(store.resources,
		OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		OwnedResource{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	)
	store.cleanup = append(store.cleanup, CleanupRecord{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "intent"})
	store.inspection.Attempts = []Attempt{{ID: 1, RunID: run.ID, Status: "started", ArtifactDir: "/owned/artifacts/attempt-1", ProcessControlKey: "control-key"}}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	deletedHead := pr
	deletedHead.HeadSHA = ""
	reader := &abandonSequenceGitHubReader{
		evidence: []domain.GitHubReadEvidence{{Repository: repositoryIdentity, PullRequest: deletedHead, ObservedAt: time.Now().UTC()}, {Repository: repositoryIdentity, PullRequest: deletedHead, ObservedAt: time.Now().UTC()}},
		errors:   []error{errors.New("pull request head repository is absent"), errors.New("pull request head repository is absent")},
		metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity},
	}
	cleanup := &abandonCleanupFake{reconcileAbsent: true}
	stopper := &abandonChildStopperFake{beforeReturn: func() {
		if hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "deleted") {
			t.Fatal("remote deletion was adopted before managed child exit proof")
		}
	}}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, stopper, reader)
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || reader.calls != 2 || slices.Contains(cleanup.calls, "remote") {
		t.Fatalf("result=%+v reads=%d cleanup=%v reconcile=%v err=%v", result, reader.calls, cleanup.calls, cleanup.reconcileCalls, err)
	}
	if !hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "deleted") || len(cleanup.reconcileCalls) != 1 {
		t.Fatalf("cleanup audit=%+v reconcile=%v", store.cleanup, cleanup.reconcileCalls)
	}
	deletedResource := false
	for _, resource := range store.resources {
		if resource.Kind == "remote_branch" && resource.Name == run.WorkingBranch && resource.Status == "deleted" {
			deletedResource = true
		}
	}
	if !deletedResource {
		t.Fatalf("remote resource deletion was not recorded: %+v", store.resources)
	}
}

func TestInspectAbandonRemoteDeletionIntentFailsClosedWhileRefExists(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{
		run: run, pr: &pr, side: SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}, leaseHeld: true,
		resources: []OwnedResource{
			{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
			{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
		},
		cleanup: []CleanupRecord{{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "intent"}},
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := &abandonCleanupFake{reconcileAbsent: false}
	plan, intent, err := inspectAbandonRemoteDeletionIntent(context.Background(), store, inspection)
	if err != nil || plan == nil || !intent || len(cleanup.reconcileCalls) != 0 {
		t.Fatalf("plan=%+v intent=%t reconcile=%v err=%v", plan, intent, cleanup.reconcileCalls, err)
	}
	replay, err := observeAbandonRemoteDeletionReplay(context.Background(), store, run, *plan, cleanup, "lease-owner")
	if err == nil || replay != nil || len(cleanup.calls) != 0 || len(cleanup.reconcileCalls) != 1 {
		t.Fatalf("replay=%+v delete=%v reconcile=%v err=%v", replay, cleanup.calls, cleanup.reconcileCalls, err)
	}
	if !hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "intent") || hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "deleted") {
		t.Fatalf("unresolved intent was mutated: %+v", store.cleanup)
	}
}

func TestProductionAbandonTerminalizesRemoteIntentWhileRefStillExists(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "closed"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store.side = SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}
	evidence := store.resources[0].CreationEvidence
	store.resources = append(store.resources,
		OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		OwnedResource{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	)
	store.cleanup = append(store.cleanup, CleanupRecord{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "intent"})
	store.inspection.Attempts = []Attempt{{ID: 1, RunID: run.ID, Status: "started", ArtifactDir: "/owned/artifacts/attempt-1", ProcessControlKey: "control-key"}}
	wrapped := newAbandonCoordinatorStore(t, store, run)
	coordinator.store = wrapped
	repositoryIdentity := domain.RepositoryIdentity{ID: 33, Owner: "owner", Name: "repo"}
	fresh := pr
	fresh.State = "open"
	reader := &abandonSequenceGitHubReader{
		evidence: []domain.GitHubReadEvidence{{Repository: repositoryIdentity, PullRequest: fresh, ObservedAt: time.Now().UTC()}, {Repository: repositoryIdentity, PullRequest: fresh, ObservedAt: time.Now().UTC()}},
		metadata: GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repositoryIdentity},
	}
	cleanup := &abandonCleanupFake{reconcileAbsent: true}
	stopper := &abandonChildStopperFake{beforeReturn: func() {
		if len(cleanup.reconcileCalls) != 0 {
			t.Fatal("remote absence was probed before child exit proof")
		}
		cleanup.reconcileAbsent = false
	}}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, stopper, reader)
	if err != nil || result.Run.State != domain.StateFailed || !result.ResidueAttention || reader.calls != 2 || slices.Contains(cleanup.calls, "remote") {
		t.Fatalf("result=%+v reads=%d cleanup=%v reconcile=%v err=%v", result, reader.calls, cleanup.calls, cleanup.reconcileCalls, err)
	}
	if !hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "intent") || hasAbandonCleanupStatus(store.cleanup, "remote_branch", run.WorkingBranch, "deleted") {
		t.Fatalf("unresolved remote intent was not retained: %+v", store.cleanup)
	}
}

func TestProductionAbandonDoesNotReconcileRemoteIntentWithoutCurrentAttention(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	store.inspection.RepositoryBinding = &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: 33}
	store.side = SideEffectRecord{ID: 9, RunID: run.ID, Kind: "open_pull_request", Status: "observed"}
	evidence := store.resources[0].CreationEvidence
	store.resources = append(store.resources,
		OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
		OwnedResource{RunID: run.ID, Kind: "pull_request", Name: "7", CreationEvidence: "open_pull_request:9", Status: "owned"},
	)
	store.cleanup = append(store.cleanup, CleanupRecord{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "intent"})
	coordinator.store = &abandonCoordinatorStore{pushTestStore: store}
	cleanup := &abandonCleanupFake{reconcileAbsent: true}
	resourcesBefore, cleanupBefore := len(store.resources), len(store.cleanup)
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: abandonRequester(), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, &abandonChildStopperFake{}, abandonGitHubReader{})
	if err == nil || result.Run.State != "" {
		t.Fatalf("missing attention result=%+v err=%v", result, err)
	}
	if len(store.resources) != resourcesBefore || len(store.cleanup) != cleanupBefore || len(cleanup.reconcileCalls) != 0 {
		t.Fatalf("cleanup mutated before attention authentication: resources=%+v cleanup=%+v reconcile=%v", store.resources, store.cleanup, cleanup.reconcileCalls)
	}
}

func newAbandonCoordinatorStore(t *testing.T, store *pushTestStore, run Run) *abandonCoordinatorStore {
	t.Helper()
	transition := Transition{Sequence: 1, From: domain.StateVerifying, To: domain.StateManualIntervention, Reason: "manual intervention", CreatedAt: run.UpdatedAt}
	store.transitions = []Transition{transition}
	event, err := ManualInterventionAttentionEvent(run, transition)
	if err != nil {
		t.Fatal(err)
	}
	return &abandonCoordinatorStore{pushTestStore: store, attention: event}
}

func abandonRequester() Requester {
	return Requester{ID: "operator", Kind: "github_login", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
}

func TestAbandonLocalCleanupRejectsForgedOwnershipBeforeGit(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	store := &pushTestStore{run: run, resources: []OwnedResource{{RunID: run.ID, Kind: "worktree", Name: "/other/worktree", CreationEvidence: `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/other/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`, Status: "owned"}}}
	cleanup := &abandonCleanupFake{}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err == nil || len(cleanup.calls) != 0 {
		t.Fatalf("forged cleanup err=%v calls=%v", err, cleanup.calls)
	}
}

func TestSelectAbandonLocalResourcesRequiresExactNamesAndSharedNonce(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	reserved, err := json.Marshal(WorktreeSpec{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, Path: run.WorktreePath, Branch: run.WorkingBranch, BaseBranch: run.BaseBranch, Nonce: "nonce"})
	if err != nil {
		t.Fatal(err)
	}
	resources := []OwnedResource{
		{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, CreationEvidence: string(reserved), Status: "reserved"},
		{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: string(reserved), Status: "reserved"},
	}
	if selected, err := selectAbandonLocalResources(run, resources); err != nil || len(selected) != 2 {
		t.Fatalf("reserved ownership selected=%+v err=%v", selected, err)
	}
	resources[1].Name = "ifan/other"
	if _, err := selectAbandonLocalResources(run, resources); err == nil {
		t.Fatal("mismatched local resource name was accepted")
	}
	resources[1].Name = run.WorkingBranch
	resources[1].CreationEvidence = strings.Replace(string(reserved), `"nonce":"nonce"`, `"nonce":"other"`, 1)
	if _, err := selectAbandonLocalResources(run, resources); err == nil {
		t.Fatal("mismatched local ownership nonce was accepted")
	}
}

func TestValidateAbandonInspectionRejectsExternalDeliveryEvidence(t *testing.T) {
	run := Run{ID: "run", State: domain.StateManualIntervention}
	if err := validateAbandonInspection(RunInspection{Run: run, SideEffects: []SideEffectRecord{{Kind: "squash_merge", Status: "intent"}}}); err == nil {
		t.Fatal("merge side effect was accepted")
	}
	if err := validateAbandonInspection(RunInspection{Run: run, PullRequest: &domain.PullRequest{Number: 1, Merged: true}}); err == nil {
		t.Fatal("merged pull request evidence was accepted")
	}
	for _, inspection := range []RunInspection{
		{Run: run, SideEffects: []SideEffectRecord{{Kind: "push", Status: "failed"}}},
		{Run: run, SideEffects: []SideEffectRecord{{Kind: "reply_to_review_comment", Status: "intent"}}},
		{Run: run, PullRequest: &domain.PullRequest{Number: 1, State: "open"}},
		{Run: run, ReviewReplies: []ReviewReplyEvidence{{RunID: run.ID, RootCommentNodeID: "COMMENT"}}},
		{Run: run, Cleanup: []CleanupRecord{{RunID: run.ID, Kind: "remote_branch", Name: "ifan/one", Status: "intent"}}},
	} {
		if err := validateAbandonInspection(inspection); err != nil {
			t.Fatalf("non-merge delivery evidence was rejected: %v", err)
		}
	}
}

func abandonCleanupRun(t *testing.T, repository LocalRepository) Run {
	t.Helper()
	raw, _ := json.Marshal(repository)
	return Run{ID: "run-abandon-cleanup", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), BaseBranch: repository.BaseBranch, WorkingBranch: "ifan/one", BaseSHA: "base", CandidateHead: "candidate", WorktreePath: "/owned/worktree", ArtifactRoot: "/owned/artifacts", TaskHash: "task", State: domain.StateFailed, UpdatedAt: time.Now().UTC()}
}
