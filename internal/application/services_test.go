package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type serviceStore struct {
	RunStore
	run              Run
	getErr           error
	inspection       RunInspection
	runs             []Run
	listCall         *runListCall
	renewed          *int
	renewOK          bool
	failureSaved     *[]GitHubRequestObservation
	approvalSaved    **domain.HumanApproval
	approvalObserved **domain.HumanApprovalObservation
	nextState        *domain.State
}

type runListCall struct {
	limit  int
	before runSummaryCursor
}

func (s serviceStore) GetRun(context.Context, string) (Run, error) { return s.run, s.getErr }
func (s serviceStore) GetRunByIdempotency(context.Context, string) (Run, bool, error) {
	return Run{}, false, nil
}
func (s serviceStore) Inspect(context.Context, string) (RunInspection, error) {
	return s.inspection, nil
}
func (s serviceStore) ListRuns(_ context.Context, _ string, before time.Time, beforeID string, limit int) ([]Run, error) {
	if s.listCall != nil {
		s.listCall.limit, s.listCall.before = limit, runSummaryCursor{CreatedAt: before, RunID: beforeID}
	}
	return s.runs, nil
}
func (s serviceStore) SaveGitHubReadSuccess(_ context.Context, _ string, _ string, _ domain.State, _ string, _ []GitHubRequestObservation, _ domain.PullRequest, _ GitHubInstallationMetadata, _ domain.GitHubReadEvidence, _ []TrustedReviewFeedbackRecord, observed *domain.HumanApprovalObservation, approval *domain.HumanApproval, next domain.State, _ string) error {
	if s.approvalSaved != nil {
		*s.approvalSaved = approval
	}
	if s.approvalObserved != nil {
		*s.approvalObserved = observed
	}
	if s.nextState != nil {
		*s.nextState = next
	}
	return nil
}
func (s serviceStore) SaveGitHubReadFailure(_ context.Context, _ string, _ string, _ domain.State, _ string, observations []GitHubRequestObservation) error {
	if s.failureSaved != nil {
		*s.failureSaved = append([]GitHubRequestObservation(nil), observations...)
	}
	return nil
}
func (s serviceStore) AcquireLease(context.Context, string, string, time.Time) (bool, error) {
	return true, nil
}
func (s serviceStore) RenewLease(context.Context, string, string, time.Time) (bool, error) {
	if s.renewed != nil {
		(*s.renewed)++
	}
	return s.renewOK, nil
}
func (s serviceStore) ReleaseLease(context.Context, string, string) error { return nil }

type serviceController struct {
	started   int
	continued int
	run       Run
	expected  domain.State
	key       string
}

type foundServiceStore struct {
	serviceStore
	existing Run
}

func (s foundServiceStore) GetRunByIdempotency(context.Context, string) (Run, bool, error) {
	return s.existing, true, nil
}

type serviceGitHubReader struct {
	calls        int
	observations []GitHubRequestObservation
	err          error
	authority    GitHubInstallationMetadata
}

func (r *serviceGitHubReader) Authority() GitHubInstallationMetadata { return r.authority }

func (r *serviceGitHubReader) Read(context.Context, int64, string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	r.calls++
	return domain.GitHubReadEvidence{}, domain.InlineReviewBodyHandoff{}, r.observations, GitHubInstallationMetadata{}, r.err
}

func (c *serviceController) StartAuthorized(_ context.Context, _ LocalStartInput, _ func(Run) error) (Run, error) {
	c.started++
	return c.run, nil
}
func (c *serviceController) ContinueExpected(_ context.Context, _ string, expected domain.State, key string, _ *Decision) (Run, error) {
	c.continued++
	c.expected, c.key = expected, key
	return c.run, nil
}
func (c *serviceController) EnforceRepairDeadline(_ context.Context, _ string) (Run, error) {
	return c.run, nil
}
func (c *serviceController) BoundRepairActionContext(ctx context.Context, _ string) (context.Context, context.CancelFunc, error) {
	return ctx, func() {}, nil
}
func (c *serviceController) RepairFindings(_ context.Context, _ string, _ []FindingRecord) (Run, error) {
	c.continued++
	return c.run, nil
}

func authorizeTestRun(run Run) Run {
	raw, _ := json.Marshal(LocalRepository{AllowedOperatorLogins: []string{"operator"}})
	run.RepositoryConfigJSON = string(raw)
	if run.ProfileSnapshotVersion == 0 {
		run.ProfileID, run.ProfileSnapshotVersion, run.ProfileDigest, run.ProfileSnapshotJSON = "repository-profile:owner/repo", 1, "profile", `{}`
	}
	return run
}

func TestCommandServiceRejectsRepositoryMismatchBeforeStart(t *testing.T) {
	controller := &serviceController{}
	service := NewCommandService(controller, serviceStore{})
	_, err := service.Start(context.Background(), StartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RepositorySelection: "owner/wrong", IdempotencyKey: "key", Input: LocalStartInput{Task: domain.CodingTask{Repository: "owner/repo"}, Repository: LocalRepository{CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{"operator"}}, IdempotencyKey: "key"}})
	var safe *ServiceError
	if !errors.As(err, &safe) || safe.Category != ErrorInvalidInput || controller.started != 0 {
		t.Fatalf("err=%v started=%d", err, controller.started)
	}
}

func TestRequesterRequiresAllowlistAndImmutableIdentity(t *testing.T) {
	actor := TrustedActorIdentity{DatabaseID: 33, NodeID: "node", Login: "operator", Type: "User"}
	requester := Requester{ID: "operator", Kind: "github_login", DatabaseID: 33, NodeID: "node", ActorType: "User"}
	if err := AuthorizeRequester(requester, []string{"other"}, actor); err == nil {
		t.Fatal("trusted actor outside login allowlist was authorized")
	}
	if err := AuthorizeRequester(requester, []string{"OPERATOR"}, actor); err != nil {
		t.Fatal(err)
	}
}

func TestCommandServiceRestartRejectsProfileDrift(t *testing.T) {
	persistedRepository := LocalRepository{ProfileID: "repository-profile:owner/repo", ProfileSnapshotVersion: 1, ProfileDigest: "old", OriginPath: "/origin", SourcePath: "/source", RunRoot: "/runs", WorktreeRoot: "/worktrees", AllowedOperatorLogins: []string{"operator"}}
	raw, _ := json.Marshal(persistedRepository)
	existing := Run{ID: "run", Repository: "owner/repo", IdempotencyKey: "key", TaskHash: "task", ProfileID: persistedRepository.ProfileID, ProfileSnapshotVersion: 1, ProfileDigest: "old", ProfileSnapshotJSON: `{"old":true}`, RepositoryConfigJSON: string(raw)}
	current := persistedRepository
	current.ProfileDigest = "new"
	current.ProfileSnapshotJSON = `{"new":true}`
	controller := &serviceController{run: existing}
	_, err := NewCommandService(controller, foundServiceStore{existing: existing}).Start(context.Background(), StartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RepositorySelection: "owner/repo", IdempotencyKey: "key", Input: LocalStartInput{Task: domain.CodingTask{Repository: "owner/repo"}, TaskHash: "task", Repository: current, IdempotencyKey: "key"}})
	if err == nil || controller.continued != 0 {
		t.Fatalf("profile drift error=%v continued=%d", err, controller.continued)
	}
}

func TestCommandServicePassesAuthorityAndProjectsContinueIdempotencyKey(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateExecuting, IdempotencyKey: "key", WorktreePath: "/secret/worktree", ArtifactRoot: "/secret/artifacts", ImplementationSession: "secret-session", LastError: "secret-error"})
	controller := &serviceController{run: run}
	result, err := NewCommandService(controller, serviceStore{run: run}).Continue(context.Background(), ContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StateExecuting, IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if controller.expected != domain.StateExecuting || controller.key != "key" || result.Run.IdempotencyKey != "key" || !strings.Contains(string(raw), `"idempotency_key":"key"`) || strings.Contains(string(raw), "secret") {
		t.Fatalf("authority or sanitization mismatch: controller=%+v result=%+v", controller, result)
	}
}

func TestCommandServiceContinueUsesExpectedStateAndRepository(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateExecuting, IdempotencyKey: "key"})
	controller := &serviceController{run: run}
	service := NewCommandService(controller, serviceStore{run: run})
	_, err := service.Continue(context.Background(), ContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StateProvisioning, IdempotencyKey: "key"})
	var safe *ServiceError
	if !errors.As(err, &safe) || safe.Category != ErrorConflict || controller.continued != 0 {
		t.Fatalf("err=%v continued=%d", err, controller.continued)
	}
}

func TestQueryServiceSanitizesInspectionAndProjectsIdempotencyKey(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", IdempotencyKey: "resume-key", WorktreePath: "/secret/worktree", LastError: "secret"})
	store := serviceStore{run: run, inspection: RunInspection{Run: run,
		RepositoryBinding: &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", GitHubAppProfileRef: "github-app-profile:secret-holder"},
		PullRequest:       &domain.PullRequest{URL: "https://github.example/owner/repo/pull/1?access_token=not-for-output"},
		Findings:          []FindingRecord{{Body: "Authorization: Bearer super-secret-token; inspect /secret/path", File: "/secret/file"}},
	}}
	got, err := NewQueryService(store).Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if got.Run.IdempotencyKey != "resume-key" || !strings.Contains(string(raw), `"idempotency_key":"resume-key"`) || strings.Contains(string(raw), "super-secret-token") || strings.Contains(string(raw), "secret-holder") || strings.Contains(string(raw), "not-for-output") || strings.Contains(string(raw), "/secret/") || !strings.Contains(string(raw), `"content_trust":"untrusted"`) {
		t.Fatalf("inspection was not sanitized: %s", raw)
	}
}

func TestQueryServiceProjectsDeterministicSourceCheckoutOperatorAttention(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateCompleted})
	observed := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	inspection := RunInspection{Run: run, Cleanup: []CleanupRecord{
		{Kind: "source_checkout", Name: "/private/source", Status: "skipped_attention", ErrorClass: string(SourceSyncReasonWrongBranch), LastError: "token=not-for-output", UpdatedAt: observed},
		{Kind: "source_checkout", Name: "/private/source", Status: "skipped_attention", ErrorClass: string(SourceSyncReasonDirtySource), LastError: "Authorization: Bearer not-for-output", UpdatedAt: observed},
		{Kind: "worktree", Name: "/private/worktree", Status: "deleted", UpdatedAt: observed},
	}}
	service := NewQueryService(serviceStore{run: run, inspection: inspection})
	status, err := service.Status(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	inspect, err := service.Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.OperatorAttention, inspect.OperatorAttention) {
		t.Fatalf("status/inspect attention mismatch: status=%+v inspect=%+v", status.OperatorAttention, inspect.OperatorAttention)
	}
	if status.Run.State != domain.StateCompleted || len(status.OperatorAttention) != 1 {
		t.Fatalf("state=%s attention=%+v", status.Run.State, status.OperatorAttention)
	}
	attention := status.OperatorAttention[0]
	if attention.Code != sourceCheckoutAttentionCode || attention.Component != sourceCheckoutAttentionComponent || attention.Severity != "warning" || attention.Status != "pending" || attention.ReasonCode != string(SourceSyncReasonDirtySource) || !attention.ObservedAt.Equal(observed) {
		t.Fatalf("attention=%+v", attention)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "/private/") || strings.Contains(string(raw), "not-for-output") {
		t.Fatalf("operator attention leaked sensitive cleanup evidence: %s", raw)
	}
}

func TestQueryServiceSourceCheckoutAttentionUsesEmptyArrayAndGenericUnknownReason(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo"})
	service := NewQueryService(serviceStore{run: run, inspection: RunInspection{Run: run}})
	withoutAttention, err := service.Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	if withoutAttention.OperatorAttention == nil || len(withoutAttention.OperatorAttention) != 0 {
		t.Fatalf("missing empty operator attention array: %+v", withoutAttention.OperatorAttention)
	}

	inspection := RunInspection{Run: run, Cleanup: []CleanupRecord{{Kind: "source_checkout", Name: "/secret/checkout", Status: "skipped_attention", ErrorClass: "unexpected /secret/path token=not-for-output", UpdatedAt: time.Now().UTC()}}}
	withUnknownReason, err := NewQueryService(serviceStore{run: run, inspection: inspection}).Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	if len(withUnknownReason.OperatorAttention) != 1 || withUnknownReason.OperatorAttention[0].ReasonCode != sourceCheckoutAttentionReason || withUnknownReason.Cleanup[0].ErrorClass != sourceCheckoutAttentionReason {
		t.Fatalf("unknown source reason was not sanitized: attention=%+v cleanup=%+v", withUnknownReason.OperatorAttention, withUnknownReason.Cleanup)
	}
	raw, _ := json.Marshal(withUnknownReason)
	if strings.Contains(string(raw), "/secret/") || strings.Contains(string(raw), "not-for-output") {
		t.Fatalf("unknown source reason leaked: %s", raw)
	}
}

func TestQueryServiceListsBoundedSummariesWithOpaqueCursor(t *testing.T) {
	now := time.Now().UTC().Round(0)
	runs := []Run{
		authorizeTestRun(Run{ID: "run-3", Repository: "owner/repo", CreatedAt: now, UpdatedAt: now}),
		authorizeTestRun(Run{ID: "run-2", Repository: "owner/repo", CreatedAt: now.Add(-time.Second), UpdatedAt: now}),
		authorizeTestRun(Run{ID: "run-1", Repository: "owner/repo", CreatedAt: now.Add(-2 * time.Second), UpdatedAt: now}),
	}
	call := &runListCall{}
	store := serviceStore{runs: runs, listCall: call}
	page, err := NewQueryService(store).ListRunSummaries(context.Background(), RunSummaryQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, Repository: "owner/repo", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.SchemaVersion != "v1" || len(page.Runs) != 2 || !page.HasMore || page.NextCursor == "" || call.limit != 3 || page.Runs[0].RunID != "run-3" {
		t.Fatalf("page=%+v limit=%d", page, call.limit)
	}
	cursor, err := decodeRunSummaryCursor(page.NextCursor)
	if err != nil || cursor.RunID != "run-2" || !cursor.CreatedAt.Equal(now.Add(-time.Second)) {
		t.Fatalf("cursor=%+v err=%v", cursor, err)
	}
	if _, err := NewQueryService(store).ListRunSummaries(context.Background(), RunSummaryQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, Repository: "owner/repo", Limit: 101}); err == nil {
		t.Fatal("limit above the bounded maximum was accepted")
	}
	if _, err := NewQueryService(store).ListRunSummaries(context.Background(), RunSummaryQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, Repository: "owner/repo", Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("invalid cursor was accepted")
	}
}

func TestGetRunDetailKeepsLegacyAndUnknownEvidenceSafe(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", ImplementationSession: "session", WorktreePath: "/private/worktree", ArtifactRoot: "/private/artifacts"})
	run.ProfileID, run.ProfileSnapshotVersion, run.ProfileDigest, run.ProfileSnapshotJSON = "", 0, "", ""
	inspection := RunInspection{Run: run,
		Attempts:       []Attempt{{SessionID: "session", ArtifactDir: "/private/artifacts", RequestedModel: "model", OutcomeHash: "hash"}},
		Timeline:       []Transition{{To: domain.State("future_state"), Reason: "token=not-for-output"}},
		Findings:       []FindingRecord{{Body: `{"client_secret":"do-not-output"}`, File: "../private/file", BodyDigest: "digest"}},
		GitHubEvidence: &domain.GitHubReadEvidence{UnknownEvents: []string{`{"secret":"do-not-output"}`}, Checks: []domain.GitHubCheck{{Name: "Authorization: Bearer do-not-output", State: domain.CheckState("token=do-not-output")}}},
	}
	store := serviceStore{run: run, inspection: inspection}
	got, err := NewQueryService(store).GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if !got.Attempts[0].SessionRecorded || !got.Attempts[0].ArtifactRecorded || len(got.Telemetry) != 3 || got.Telemetry[2].Value != "[untrusted structured value omitted]" || got.Findings[0].Content != "" || got.Findings[0].File != "" || strings.Contains(string(raw), "do-not-output") || strings.Contains(string(raw), "not-for-output") || strings.Contains(string(raw), "/private/") {
		t.Fatalf("unsafe or incomplete detail projection: %s", raw)
	}
}

func TestGetRunDetailClassifiesNotFound(t *testing.T) {
	store := serviceStore{getErr: ErrRunNotFound}
	_, err := NewQueryService(store).GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "missing"})
	if err == nil {
		t.Fatal("missing run was accepted")
	}
}

func TestKnownDeliveryStatesAreNotUnknownTelemetry(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateCleaning})
	store := serviceStore{run: run, inspection: RunInspection{Run: run, Timeline: []Transition{{From: domain.StateMerging, To: domain.StateCleaning}}}}
	got, err := NewQueryService(store).GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Telemetry) != 0 {
		t.Fatalf("known lifecycle state was reported as telemetry: %+v", got.Telemetry)
	}
}

func TestServiceErrorDoesNotRenderUnderlyingDetails(t *testing.T) {
	err := classifyServiceError(errors.New("/secret/path: token=credential"))
	if err.Error() != "internal: application operation failed" {
		t.Fatalf("unsafe error rendering: %q", err)
	}
}

func TestReconcileUsesPersistedAuthority(t *testing.T) {
	pr := domain.PullRequest{Number: 1, URL: "https://example.invalid/1", NodeID: "PR", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: "key"}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key", WorkingBranch: "feature", BaseBranch: "main", CandidateHead: "head", BaseSHA: "base"})
	binding := &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2}
	evidence := domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}, PullRequest: pr}
	metadata := GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: evidence.Repository}
	store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr}}
	service := NewCommandService(nil, store)
	if _, err := service.Reconcile(context.Background(), ReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", Evidence: evidence, Metadata: metadata}); err != nil {
		t.Fatal(err)
	}
	wrongApp := metadata
	wrongApp.AppID = 3
	if _, err := service.Reconcile(context.Background(), ReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", Evidence: evidence, Metadata: wrongApp}); err == nil {
		t.Fatal("expected GitHub App identity mismatch to be rejected")
	}
	evidence.PullRequest.HeadSHA = "other"
	if _, err := service.Reconcile(context.Background(), ReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", Evidence: evidence, Metadata: metadata}); err == nil {
		t.Fatal("expected evidence detached from persisted head to be rejected")
	}
}

func TestGitHubReconcileAuthorizesBeforeExternalRead(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key"})
	reader := &serviceGitHubReader{}
	service := NewCommandService(nil, serviceStore{run: run})
	_, err := service.ReconcileFromGitHub(context.Background(), GitHubReconcileCommand{Requester: Requester{ID: "intruder", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", PullRequest: 1, ExpectedHead: "head"}, reader)
	if err == nil {
		t.Fatal("expected unauthorized requester rejection")
	}
	if reader.calls != 0 {
		t.Fatalf("external reader called %d times before authorization", reader.calls)
	}
}

func TestGitHubReconcileRechecksCASUnderLeaseBeforeExternalRead(t *testing.T) {
	pr := domain.PullRequest{Number: 1}
	preflight := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key", CandidateHead: "head"})
	changed := preflight
	changed.State = domain.StateExecuting
	reader := &serviceGitHubReader{}
	store := serviceStore{run: preflight, inspection: RunInspection{Run: changed, PullRequest: &pr}}
	_, err := NewCommandService(nil, store).ReconcileFromGitHub(context.Background(), GitHubReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", PullRequest: 1, ExpectedHead: "head"}, reader)
	if err == nil || reader.calls != 0 {
		t.Fatalf("lease-time CAS error=%v reader calls=%d", err, reader.calls)
	}
}

func TestGitHubReconcileRejectsReaderAuthorityBeforeExternalRead(t *testing.T) {
	pr := domain.PullRequest{Number: 1}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key", CandidateHead: "head"})
	binding := &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2}
	reader := &serviceGitHubReader{authority: GitHubInstallationMetadata{AppID: 3, InstallationID: 2, Repository: domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}}}
	store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr}}
	_, err := NewCommandService(nil, store).ReconcileFromGitHub(context.Background(), GitHubReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", PullRequest: 1, ExpectedHead: "head"}, reader)
	if err == nil || reader.calls != 0 {
		t.Fatalf("reader authority error=%v calls=%d", err, reader.calls)
	}
}

func TestGitHubReconcileCancelsReadWhenLeaseIsLost(t *testing.T) {
	originalTTL := reconcileLeaseTTL
	reconcileLeaseTTL = 30 * time.Millisecond
	defer func() { reconcileLeaseTTL = originalTTL }()
	pr := domain.PullRequest{Number: 1}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key", CandidateHead: "head"})
	renewed := 0
	store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2}, PullRequest: &pr}, renewed: &renewed, renewOK: false}
	reader := blockingGitHubReader{}
	_, err := NewCommandService(nil, store).ReconcileFromGitHub(context.Background(), GitHubReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", PullRequest: 1, ExpectedHead: "head"}, reader)
	if err == nil || renewed == 0 {
		t.Fatalf("lease loss error=%v renewals=%d", err, renewed)
	}
}

type blockingGitHubReader struct{}

func (blockingGitHubReader) Authority() GitHubInstallationMetadata {
	return GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}}
}

func (blockingGitHubReader) Read(ctx context.Context, _ int64, _ string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	<-ctx.Done()
	return domain.GitHubReadEvidence{}, domain.InlineReviewBodyHandoff{}, nil, GitHubInstallationMetadata{}, context.Cause(ctx)
}

func TestGitHubReconcilePersistsPartialFailureObservations(t *testing.T) {
	pr := domain.PullRequest{Number: 1}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key", CandidateHead: "head"})
	observation := GitHubRequestObservation{RunID: "run", Operation: "read", ErrorClass: "timeout"}
	var saved []GitHubRequestObservation
	store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2}, PullRequest: &pr}, failureSaved: &saved}
	reader := &serviceGitHubReader{authority: GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}}, observations: []GitHubRequestObservation{observation}, err: errors.New("read failed")}
	_, err := NewCommandService(nil, store).ReconcileFromGitHub(context.Background(), GitHubReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", PullRequest: 1, ExpectedHead: "head"}, reader)
	if err == nil || len(saved) != 1 || saved[0].ErrorClass != "timeout" {
		t.Fatalf("failure error=%v saved=%+v", err, saved)
	}
}

func TestNextGitHubReconciliationStateUsesOnlyLegalFailClosedGates(t *testing.T) {
	passing := domain.GitHubReadEvidence{PullRequest: domain.PullRequest{State: "open", HeadSHA: "head"}, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: domain.CheckSuccess}}}
	actionable := passing
	actionable.Checks[0].State = domain.CheckFailure
	closed := passing
	closed.PullRequest.State = "closed"
	cases := []struct {
		name     string
		current  domain.State
		evidence domain.GitHubReadEvidence
		status   domain.ReconciliationStatus
		want     domain.State
	}{
		{name: "first observation", current: domain.StatePROpen, evidence: passing, status: domain.ReconciliationPass, want: domain.StateReconcilingReviews},
		{name: "passing reconciliation", current: domain.StateReconcilingReviews, evidence: passing, status: domain.ReconciliationPass, want: domain.StateAwaitingHumanApproval},
		{name: "actionable finding", current: domain.StateReconcilingReviews, evidence: actionable, status: domain.ReconciliationActionable, want: domain.StateRepairing},
		{name: "pending evidence revokes approval readiness", current: domain.StateAwaitingHumanApproval, evidence: passing, status: domain.ReconciliationPending, want: domain.StateReconcilingReviews},
		{name: "closed PR", current: domain.StateAwaitingHumanApproval, evidence: closed, status: domain.ReconciliationInfrastructure, want: domain.StateManualIntervention},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := nextGitHubReconciliationState(tc.current, tc.evidence, tc.status)
			if got != tc.want {
				t.Fatalf("state=%s want=%s", got, tc.want)
			}
			if got != tc.current {
				if err := domain.ValidateTransition(tc.current, got); err != nil {
					t.Fatalf("illegal transition %s -> %s: %v", tc.current, got, err)
				}
			}
		})
	}
}

func TestGitHubReconcileRecordsOnlyTrustedExactHeadHumanApproval(t *testing.T) {
	now := time.Now().UTC()
	pr := domain.PullRequest{Number: 1, DatabaseID: 7, URL: "https://example.invalid/1", NodeID: "PR", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: "key", State: "open"}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateAwaitingHumanApproval, IdempotencyKey: "key", WorkingBranch: "feature", BaseBranch: "main", CandidateHead: "head", BaseSHA: "base"})
	trusted := TrustedActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2, TrustedOperatorActors: []TrustedActorIdentity{trusted}}
	evidence := domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}, PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: domain.CheckSuccess}}, Reviews: []domain.GitHubReview{{DatabaseID: 9, NodeID: "PRR", State: "APPROVED", CommitSHA: "head", SourceAt: now, Actor: domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}}}, ObservedAt: now}
	metadata := GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: evidence.Repository}
	var approval *domain.HumanApproval
	var observed *domain.HumanApprovalObservation
	var next domain.State
	store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr}, approvalSaved: &approval, approvalObserved: &observed, nextState: &next}
	result, err := NewCommandService(nil, store).Reconcile(context.Background(), ReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey, Evidence: evidence, Metadata: metadata})
	if err != nil || result.State != domain.StateMerging || next != domain.StateMerging || approval == nil || observed == nil || observed.Status != domain.HumanApprovalApproved {
		t.Fatalf("result=%+v next=%s approval=%+v observed=%+v err=%v", result, next, approval, observed, err)
	}

	evidence.Reviews[0].Actor = domain.ActorIdentity{DatabaseID: 33, NodeID: "BOT_33", Login: "ifan0927", Type: "Bot"}
	approval, observed, next = nil, nil, ""
	result, err = NewCommandService(nil, store).Reconcile(context.Background(), ReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey, Evidence: evidence, Metadata: metadata})
	if err != nil || result.State != domain.StateAwaitingHumanApproval || next != domain.StateAwaitingHumanApproval || approval != nil || observed == nil || observed.Status != domain.HumanApprovalUntrustedActor {
		t.Fatalf("bot result=%+v next=%s approval=%+v observed=%+v err=%v", result, next, approval, observed, err)
	}
}
