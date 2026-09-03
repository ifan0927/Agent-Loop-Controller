package application

import (
	"context"
	"encoding/base64"
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
	renewed          *int
	renewOK          bool
	failureSaved     *[]GitHubRequestObservation
	approvalSaved    **domain.HumanApproval
	approvalObserved **domain.HumanApprovalObservation
	nextState        *domain.State
	transitionReason *string
	attentionEvents  *[]OperatorAttentionEvent
	attentionErr     error
}

type authorityFirstStore struct {
	serviceStore
	authorityReads int
	aggregateReads int
	legacyReads    int
}

type serviceRepositoryAuthorities struct {
	authority RepositoryAuthority
	found     bool
}

func (s serviceRepositoryAuthorities) RepositoryAuthority(context.Context, string) (RepositoryAuthority, bool, error) {
	return s.authority, s.found, nil
}

type controllerRunCollectionFixture struct {
	serviceStore
	runs []Run
}

func (s *controllerRunCollectionFixture) ListControllerRuns(_ context.Context, input ControllerRunQuery) (AuthorizedRunPage, error) {
	filtered := make([]Run, 0, len(s.runs))
	for _, run := range s.runs {
		lifecycleMatch := input.Lifecycle == RunLifecycleAll || input.Lifecycle == RunLifecycleActive && !TerminalRunState(run.State) || input.Lifecycle == RunLifecycleEnded && TerminalRunState(run.State)
		repositoryMatch := input.CanonicalRepository == "" || input.CanonicalRepository == run.Repository
		keysetMatch := input.BeforeUpdatedAt.IsZero() || run.UpdatedAt.Before(input.BeforeUpdatedAt) || run.UpdatedAt.Equal(input.BeforeUpdatedAt) && run.ID < input.BeforeRunID
		if lifecycleMatch && repositoryMatch && keysetMatch {
			filtered = append(filtered, run)
		}
	}
	total := 0
	for _, run := range s.runs {
		lifecycleMatch := input.Lifecycle == RunLifecycleAll || input.Lifecycle == RunLifecycleActive && !TerminalRunState(run.State) || input.Lifecycle == RunLifecycleEnded && TerminalRunState(run.State)
		if lifecycleMatch && (input.CanonicalRepository == "" || input.CanonicalRepository == run.Repository) {
			total++
		}
	}
	if len(filtered) > input.Limit {
		filtered = filtered[:input.Limit]
	}
	return AuthorizedRunPage{Runs: filtered, TotalCount: total}, nil
}

func (s *authorityFirstStore) GetRun(ctx context.Context, id string) (Run, error) {
	s.legacyReads++
	return s.serviceStore.GetRun(ctx, id)
}

func (s *authorityFirstStore) GetRunScopeAuthority(ctx context.Context, id string) (RunScopeAuthority, error) {
	s.authorityReads++
	return s.serviceStore.GetRunScopeAuthority(ctx, id)
}

func (s *authorityFirstStore) GetAuthorizedRun(ctx context.Context, id string, scopes AuthorizedScopeSet) (Run, error) {
	s.aggregateReads++
	return s.serviceStore.GetAuthorizedRun(ctx, id, scopes)
}

func (s serviceStore) GetRun(context.Context, string) (Run, error) { return s.run, s.getErr }
func (s serviceStore) GetRunScopeAuthority(_ context.Context, id string) (RunScopeAuthority, error) {
	if s.getErr != nil {
		return RunScopeAuthority{}, s.getErr
	}
	run := s.run
	if run.ID != id {
		for _, candidate := range s.runs {
			if candidate.ID == id {
				run = candidate
				break
			}
		}
	}
	if run.ID != id {
		return RunScopeAuthority{}, ErrRunNotFound
	}
	return frozenRunScopeAuthority(run)
}
func (s serviceStore) GetAuthorizedRun(_ context.Context, id string, scopes AuthorizedScopeSet) (Run, error) {
	if s.getErr != nil {
		return Run{}, s.getErr
	}
	run := s.run
	if run.ID != id {
		for _, candidate := range s.runs {
			if candidate.ID == id {
				run = candidate
				break
			}
		}
	}
	if run.ID != id || !scopes.AllowsRun(run.ID, run.RepositoryBindingDigest) {
		return Run{}, ErrRunNotFound
	}
	return run, nil
}
func (s serviceStore) GetRunByIdempotency(context.Context, string) (Run, bool, error) {
	return Run{}, false, nil
}
func (s serviceStore) Inspect(context.Context, string) (RunInspection, error) {
	return s.inspection, nil
}
func (s serviceStore) ListOperatorAttention(_ context.Context, input OperatorAttentionQueryInput) ([]OperatorAttentionEvent, error) {
	if input.Limit < 1 || input.Limit > maxOperatorAttentionProjection {
		return nil, errors.New("operator attention projection limit is out of bounds")
	}
	return append([]OperatorAttentionEvent(nil), s.inspection.OperatorAttention...), nil
}
func (s serviceStore) CurrentOperatorAttention(_ context.Context, runID string) (OperatorAttentionEvent, bool, error) {
	events := s.inspection.OperatorAttention
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].RunID == runID {
			return events[index], true, nil
		}
	}
	return OperatorAttentionEvent{}, false, nil
}
func (s serviceStore) AppendOperatorAttention(_ context.Context, event OperatorAttentionEvent) (bool, error) {
	if s.attentionErr != nil {
		return false, s.attentionErr
	}
	if s.attentionEvents != nil {
		for _, current := range *s.attentionEvents {
			if current.EventKey == event.EventKey {
				return false, nil
			}
		}
		*s.attentionEvents = append(*s.attentionEvents, event)
	}
	return true, nil
}
func (s serviceStore) ListControllerRuns(_ context.Context, input ControllerRunQuery) (AuthorizedRunPage, error) {
	if !input.Authority.Valid() {
		return AuthorizedRunPage{}, errors.New("controller read authority is invalid")
	}
	filtered := make([]Run, 0, len(s.runs))
	total := 0
	for _, run := range s.runs {
		lifecycleMatch := input.Lifecycle == RunLifecycleAll || input.Lifecycle == RunLifecycleActive && !TerminalRunState(run.State) || input.Lifecycle == RunLifecycleEnded && TerminalRunState(run.State)
		repositoryMatch := input.CanonicalRepository == "" || input.CanonicalRepository == run.Repository
		if lifecycleMatch && repositoryMatch {
			total++
		}
		keysetMatch := input.BeforeUpdatedAt.IsZero() || run.UpdatedAt.Before(input.BeforeUpdatedAt) || run.UpdatedAt.Equal(input.BeforeUpdatedAt) && run.ID < input.BeforeRunID
		if lifecycleMatch && repositoryMatch && keysetMatch {
			filtered = append(filtered, run)
		}
	}
	if len(filtered) > input.Limit {
		filtered = filtered[:input.Limit]
	}
	return AuthorizedRunPage{Runs: filtered, TotalCount: total}, nil
}
func (s serviceStore) SaveGitHubReadSuccess(_ context.Context, _ string, _ string, _ domain.State, _ string, _ []GitHubRequestObservation, _ domain.PullRequest, _ GitHubInstallationMetadata, _ domain.GitHubReadEvidence, _ []TrustedReviewFeedbackRecord, observed *domain.HumanApprovalObservation, approval *domain.HumanApproval, next domain.State, reason string) error {
	if s.approvalSaved != nil {
		*s.approvalSaved = approval
	}
	if s.approvalObserved != nil {
		*s.approvalObserved = observed
	}
	if s.nextState != nil {
		*s.nextState = next
	}
	if s.transitionReason != nil {
		*s.transitionReason = reason
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
	started        int
	continued      int
	reconciled     int
	reconcileError error
	run            Run
	expected       domain.State
	key            string
}

type foundServiceStore struct {
	serviceStore
	existing Run
}

type foundSchedulingStore struct {
	*serviceSchedulingStore
	existing Run
}

func (s foundSchedulingStore) GetRunByIdempotency(context.Context, string) (Run, bool, error) {
	return s.existing, true, nil
}

type serviceSchedulingStore struct {
	serviceStore
	enabled       bool
	held          bool
	acquireErrors []error
	owners        []string
	acquireCalls  int
	releaseCalls  int
}

func (s *serviceSchedulingStore) HasSchedulingAuthority(context.Context, string) (bool, error) {
	return s.enabled, nil
}

func (s *serviceSchedulingStore) AcquireHeavyPermit(_ context.Context, runID, owner string, now time.Time) (HeavyPermit, bool, error) {
	s.acquireCalls++
	s.owners = append(s.owners, owner)
	if len(s.acquireErrors) > 0 {
		err := s.acquireErrors[0]
		s.acquireErrors = s.acquireErrors[1:]
		return HeavyPermit{}, false, err
	}
	if !s.held {
		return HeavyPermit{}, false, nil
	}
	return HeavyPermit{RunID: runID, OwnerNonce: owner, Version: 1, AcquiredAt: now, UpdatedAt: now}, true, nil
}

func (s *serviceSchedulingStore) ReleaseHeavyPermit(context.Context, HeavyPermit, string, time.Time) (bool, error) {
	s.releaseCalls++
	return true, nil
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

func (c *serviceController) ReconcileInterruptedRun(context.Context, string) error {
	c.reconciled++
	return c.reconcileError
}

func authorizeTestRun(run Run) Run {
	if run.ProfileSnapshotVersion == 0 {
		run.ProfileID, run.ProfileSnapshotVersion, run.ProfileDigest, run.ProfileSnapshotJSON = "repository-profile:owner/repo", 1, "profile", `{}`
	}
	raw, _ := json.Marshal(LocalRepository{CanonicalRepository: run.Repository, ProfileID: run.ProfileID, AllowedOperatorLogins: []string{"operator"}})
	run.RepositoryConfigJSON = string(raw)
	if !validAuthorityDigest(run.RepositoryBindingDigest) {
		run.RepositoryBindingDigest = digestText("legacy-repository-binding:" + strings.ToLower(run.Repository))
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

func TestCommandServiceExistingStartReconcilesBeforeDriverOwnedPermitAdoption(t *testing.T) {
	existing := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateExecuting, IdempotencyKey: "key", TaskHash: "task"})
	controller := &serviceController{run: existing}
	scheduling := &serviceSchedulingStore{
		serviceStore:  serviceStore{run: existing},
		enabled:       true,
		held:          true,
		acquireErrors: []error{ErrHeavyPermitProcessReconciliationRequired},
	}
	store := foundSchedulingStore{serviceSchedulingStore: scheduling, existing: existing}
	var repository LocalRepository
	if err := json.Unmarshal([]byte(existing.RepositoryConfigJSON), &repository); err != nil {
		t.Fatal(err)
	}
	repository.ProfileSnapshotVersion = existing.ProfileSnapshotVersion
	repository.ProfileDigest = existing.ProfileDigest
	repository.ProfileSnapshotJSON = existing.ProfileSnapshotJSON
	ctx := WithHeavyPermitOwner(context.Background(), "direct-owner")
	result, err := NewCommandService(controller, store).Start(ctx, StartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RepositorySelection: existing.Repository, IdempotencyKey: existing.IdempotencyKey, Input: LocalStartInput{Task: domain.CodingTask{Repository: existing.Repository}, TaskHash: existing.TaskHash, Repository: repository, IdempotencyKey: existing.IdempotencyKey}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.RunID != existing.ID || controller.reconciled != 1 || controller.continued != 1 || scheduling.acquireCalls != 2 || scheduling.releaseCalls != 0 {
		t.Fatalf("result=%+v reconciled=%d continued=%d acquires=%d releases=%d", result, controller.reconciled, controller.continued, scheduling.acquireCalls, scheduling.releaseCalls)
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

func TestCommandServiceManualDecisionCannotBypassHeavyPermit(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateAwaitingHumanDecision, IdempotencyKey: "key"})
	heavyRun := run
	heavyRun.State = domain.StateExecuting
	controller := &serviceController{run: heavyRun}
	store := &serviceSchedulingStore{serviceStore: serviceStore{run: run}, enabled: true}
	decision := &Decision{ChoiceID: "continue"}
	command := ContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey, Decision: decision}
	if _, err := NewCommandService(controller, store).Continue(context.Background(), command); err == nil || controller.continued != 1 || store.acquireCalls != 1 {
		t.Fatalf("err=%v continued=%d acquires=%d", err, controller.continued, store.acquireCalls)
	}
	store.held = true
	if _, err := NewCommandService(controller, store).Continue(context.Background(), command); err != nil || controller.continued != 3 || store.acquireCalls != 2 || store.releaseCalls != 1 {
		t.Fatalf("err=%v continued=%d acquires=%d releases=%d", err, controller.continued, store.acquireCalls, store.releaseCalls)
	}
}

func TestCommandServiceAcceptDecisionStopsBeforeHeavyWork(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateAwaitingHumanDecision, IdempotencyKey: "key"})
	accepted := run
	accepted.State = domain.StateExecuting
	controller := &serviceController{run: accepted}
	store := &serviceSchedulingStore{serviceStore: serviceStore{run: run}, enabled: true, held: true}
	decision := &Decision{ChoiceID: "continue", Instructions: "No additional instructions."}
	command := ContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey, Decision: decision}

	result, err := NewCommandService(controller, store).AcceptDecision(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.State != domain.StateExecuting || controller.continued != 1 || store.acquireCalls != 0 || store.releaseCalls != 0 {
		t.Fatalf("result=%+v continued=%d acquires=%d releases=%d", result, controller.continued, store.acquireCalls, store.releaseCalls)
	}
}

func TestCommandServiceManualContinueReconcilesBeforePermitAdoption(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateExecuting, IdempotencyKey: "key"})
	controller := &serviceController{run: run}
	store := &serviceSchedulingStore{
		serviceStore:  serviceStore{run: run},
		enabled:       true,
		held:          true,
		acquireErrors: []error{ErrHeavyPermitProcessReconciliationRequired},
	}
	command := ContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}
	if _, err := NewCommandService(controller, store).Continue(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if controller.reconciled != 1 || controller.continued != 1 || store.acquireCalls != 2 || store.releaseCalls != 1 {
		t.Fatalf("reconciled=%d continued=%d acquires=%d releases=%d", controller.reconciled, controller.continued, store.acquireCalls, store.releaseCalls)
	}
}

func TestCommandServiceUsesUniqueBoundManualSupervisorOwnerAndReleases(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateExecuting, IdempotencyKey: "key"})
	controller := &serviceController{run: run}
	store := &serviceSchedulingStore{serviceStore: serviceStore{run: run}, enabled: true, held: true}
	command := ContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}
	for _, owner := range []string{"manual:run:first", "manual:run:second"} {
		if _, err := NewCommandService(controller, store).Continue(WithManualHeavyPermitOwner(context.Background(), owner), command); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(store.owners, []string{"manual:run:first", "manual:run:second"}) || store.releaseCalls != 2 {
		t.Fatalf("owners=%v releases=%d", store.owners, store.releaseCalls)
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
	event, err := newOperatorAttentionEvent(operatorAttentionEventInput{ScopeID: run.ID, RunID: run.ID, EventType: OperatorAttentionSourceCheckoutSkipped, Profile: OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}, State: domain.StateCleaning, Severity: "warning", ReasonCode: string(SourceSyncReasonDirtySource), EvidenceDigest: strings.Repeat("a", 64), OccurredAt: observed, ObservedAt: observed})
	if err != nil {
		t.Fatal(err)
	}
	inspection := RunInspection{Run: run, Cleanup: []CleanupRecord{
		{Kind: "source_checkout", Name: "/private/source", Status: "skipped_attention", ErrorClass: string(SourceSyncReasonWrongBranch), LastError: "token=not-for-output", UpdatedAt: observed},
		{Kind: "source_checkout", Name: "/private/source", Status: "skipped_attention", ErrorClass: string(SourceSyncReasonDirtySource), LastError: "Authorization: Bearer not-for-output", UpdatedAt: observed},
		{Kind: "worktree", Name: "/private/worktree", Status: "deleted", UpdatedAt: observed},
	}, OperatorAttention: []OperatorAttentionEvent{event}}
	service := NewQueryService(serviceStore{run: run, inspection: inspection})
	status, err := service.Status(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	inspect, err := service.Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.OperatorAttentionEvents, inspect.OperatorAttentionEvents) {
		t.Fatalf("status/inspect attention mismatch: status=%+v inspect=%+v", status.OperatorAttentionEvents, inspect.OperatorAttentionEvents)
	}
	if status.Run.State != domain.StateCompleted || len(status.OperatorAttentionEvents) != 1 {
		t.Fatalf("state=%s attention=%+v", status.Run.State, status.OperatorAttentionEvents)
	}
	attention := status.OperatorAttentionEvents[0]
	if attention.EventType != OperatorAttentionSourceCheckoutSkipped || attention.Severity != "warning" || attention.ReasonCode != string(SourceSyncReasonDirtySource) || !attention.ObservedAt.Equal(observed) {
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
	if withoutAttention.OperatorAttentionEvents == nil || len(withoutAttention.OperatorAttentionEvents) != 0 {
		t.Fatalf("missing empty operator attention array: %+v", withoutAttention.OperatorAttentionEvents)
	}

	inspection := RunInspection{Run: run, Cleanup: []CleanupRecord{{Kind: "source_checkout", Name: "/secret/checkout", Status: "skipped_attention", ErrorClass: "unexpected /secret/path token=not-for-output", UpdatedAt: time.Now().UTC()}}}
	withUnknownReason, err := NewQueryService(serviceStore{run: run, inspection: inspection}).Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	if len(withUnknownReason.OperatorAttentionEvents) != 0 || withUnknownReason.Cleanup[0].ErrorClass != sourceCheckoutAttentionReason {
		t.Fatalf("unknown source reason was not sanitized: attention=%+v cleanup=%+v", withUnknownReason.OperatorAttentionEvents, withUnknownReason.Cleanup)
	}
	raw, _ := json.Marshal(withUnknownReason)
	if strings.Contains(string(raw), "/secret/") || strings.Contains(string(raw), "not-for-output") {
		t.Fatalf("unknown source reason leaked: %s", raw)
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

func TestRunDetailUnknownAndUnauthorizedAreNonDisclosing(t *testing.T) {
	unknown := NewQueryService(serviceStore{getErr: ErrRunNotFound})
	_, unknownErr := unknown.GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "intruder", Kind: "github_login"}, RunID: "private-run"})

	run := authorizeTestRun(Run{ID: "private-run", Repository: "owner/private"})
	unauthorized := NewQueryService(serviceStore{run: run, inspection: RunInspection{Run: run}})
	_, unauthorizedErr := unauthorized.GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "intruder", Kind: "github_login"}, RunID: run.ID})

	var missing, denied *ServiceError
	if !errors.As(unknownErr, &missing) || !errors.As(unauthorizedErr, &denied) || missing.Category != ErrorNotFound || denied.Category != ErrorNotFound || missing.Message != denied.Message || unknownErr.Error() != unauthorizedErr.Error() {
		t.Fatalf("unknown=%v unauthorized=%v", unknownErr, unauthorizedErr)
	}
}

func TestScopedRunDetailAuthorizesBeforeAggregateLookup(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	other := domain.GitHubUserIdentity{Login: "other", DatabaseID: 8, NodeID: "U_8", ActorType: "User"}
	makeRun := func(user domain.GitHubUserIdentity) Run {
		repository := LocalRepository{CanonicalRepository: "owner/private", AllowedOperatorLogins: []string{user.Login}, TrustedOperatorActors: []TrustedActorIdentity{{Login: user.Login, DatabaseID: user.DatabaseID, NodeID: user.NodeID, Type: user.ActorType}}}
		raw, _ := json.Marshal(repository)
		return Run{ID: "private-run", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), RepositoryBindingDigest: strings.Repeat("a", 64)}
	}
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})

	allowedRun := makeRun(operator)
	allowedStore := &authorityFirstStore{serviceStore: serviceStore{run: allowedRun, inspection: RunInspection{Run: allowedRun}}}
	allowed, _ := NewScopedQueryService(allowedStore, authorizer)
	if _, err := allowed.GetRunDetail(context.Background(), RunDetailQuery{Requester: requesterForUser(operator), RunID: allowedRun.ID}); err != nil {
		t.Fatal(err)
	}
	if allowedStore.authorityReads != 1 || allowedStore.aggregateReads != 1 || allowedStore.legacyReads != 0 {
		t.Fatalf("authority=%d aggregate=%d legacy=%d", allowedStore.authorityReads, allowedStore.aggregateReads, allowedStore.legacyReads)
	}

	deniedRun := makeRun(other)
	deniedStore := &authorityFirstStore{serviceStore: serviceStore{run: deniedRun}}
	denied, _ := NewScopedQueryService(deniedStore, authorizer)
	_, deniedErr := denied.GetRunDetail(context.Background(), RunDetailQuery{Requester: requesterForUser(operator), RunID: deniedRun.ID})
	if deniedStore.authorityReads != 1 || deniedStore.aggregateReads != 0 || deniedStore.legacyReads != 0 {
		t.Fatalf("denied authority=%d aggregate=%d legacy=%d", deniedStore.authorityReads, deniedStore.aggregateReads, deniedStore.legacyReads)
	}

	missingStore := &authorityFirstStore{serviceStore: serviceStore{getErr: ErrRunNotFound}}
	missing, _ := NewScopedQueryService(missingStore, authorizer)
	_, missingErr := missing.GetRunDetail(context.Background(), RunDetailQuery{Requester: requesterForUser(operator), RunID: deniedRun.ID})
	if deniedErr == nil || missingErr == nil || deniedErr.Error() != missingErr.Error() || missingStore.aggregateReads != 0 || missingStore.legacyReads != 0 {
		t.Fatalf("denied=%v missing=%v missing aggregate=%d legacy=%d", deniedErr, missingErr, missingStore.aggregateReads, missingStore.legacyReads)
	}
}

func TestControllerRunCollectionUsesStableReaderAcrossMembershipAndBindings(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	now := time.Now().UTC().Round(0)
	frozen := func(repository, binding string) string {
		raw, err := json.Marshal(LocalRepository{CanonicalRepository: repository, RepositoryBindingDigest: binding, AllowedOperatorLogins: []string{operator.Login}, TrustedOperatorActors: []TrustedActorIdentity{{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	newBinding, oldBinding := strings.Repeat("b", 64), strings.Repeat("a", 64)
	store := &controllerRunCollectionFixture{runs: []Run{
		{ID: "run-new-binding", Repository: "owner/repo", RepositoryConfigJSON: frozen("owner/repo", newBinding), RepositoryBindingDigest: newBinding, State: domain.StateReceived, CreatedAt: now, UpdatedAt: now},
		{ID: "run-old-binding", Repository: "owner/repo", RepositoryConfigJSON: frozen("owner/repo", oldBinding), RepositoryBindingDigest: oldBinding, State: domain.StateReceived, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
		{ID: "run-corrupt-unselected", Repository: "owner/repo", RepositoryConfigJSON: "{private-corrupt-row", RepositoryBindingDigest: strings.Repeat("c", 64), State: domain.StateReceived, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute)},
	}}
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(requesterForUser(operator))
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	queries, _ := NewScopedQueryService(store, authorizer)
	first, err := queries.ListControllerRunSummaries(context.Background(), reader, RunSummaryQuery{Repository: "owner/repo", Limit: 1})
	if err != nil || first.TotalCount != 3 || len(first.Runs) != 1 || first.Runs[0].RunID != "run-new-binding" || first.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	store.runs = append(store.runs, Run{ID: "unrelated-added", Repository: "owner/other", RepositoryConfigJSON: frozen("owner/other", strings.Repeat("d", 64)), RepositoryBindingDigest: strings.Repeat("d", 64), State: domain.StateReceived, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)})
	second, err := queries.ListControllerRunSummaries(context.Background(), reader, RunSummaryQuery{Repository: "owner/repo", Limit: 1, Cursor: first.NextCursor})
	if err != nil || second.TotalCount != 3 || len(second.Runs) != 1 || second.Runs[0].RunID != "run-old-binding" || second.NextCursor == "" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if _, err := queries.ListControllerRunSummaries(context.Background(), reader, RunSummaryQuery{Repository: "owner/repo", Limit: 1, Cursor: second.NextCursor}); err == nil || strings.Contains(err.Error(), "private-corrupt-row") {
		t.Fatalf("selected corrupt row did not fail safely: %v", err)
	}
	legacyRaw, _ := json.Marshal(map[string]any{"version": querySchemaVersion, "repository": "owner/repo", "lifecycle": RunLifecycleAll, "scope_digest": reader.Digest(), "updated_at": now, "run_id": "run-new-binding"})
	legacyCursor := base64.RawURLEncoding.EncodeToString(legacyRaw)
	if _, err := queries.ListControllerRunSummaries(context.Background(), reader, RunSummaryQuery{Repository: "owner/repo", Cursor: legacyCursor}); err == nil {
		t.Fatal("pre-v2 Controller Runs cursor was accepted")
	}
	if _, err := queries.ListControllerRunSummaries(context.Background(), reader, RunSummaryQuery{Repository: "owner/other", Limit: 1, Cursor: first.NextCursor}); err == nil {
		t.Fatal("repository-filter-drifted Controller Runs cursor was accepted")
	}
	if _, err := queries.ListControllerRunSummaries(context.Background(), reader, RunSummaryQuery{Repository: "owner/repo", Lifecycle: RunLifecycleEnded, Limit: 1, Cursor: first.NextCursor}); err == nil {
		t.Fatal("lifecycle-drifted Controller Runs cursor was accepted")
	}
}

func TestControllerRunCollectionRejectsSelectedFrozenAuthorityContradictions(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(requesterForUser(operator))
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	binding := strings.Repeat("a", 64)
	validFrozen := func(repository, frozenBinding string) string {
		raw, _ := json.Marshal(LocalRepository{CanonicalRepository: repository, RepositoryBindingDigest: frozenBinding, AllowedOperatorLogins: []string{operator.Login}, TrustedOperatorActors: []TrustedActorIdentity{{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType}}})
		return string(raw)
	}
	for _, test := range []struct {
		name, raw string
	}{
		{name: "malformed", raw: "{private-malformed-authority"},
		{name: "canonical repository mismatch", raw: validFrozen("owner/other", binding)},
		{name: "binding mismatch", raw: validFrozen("owner/repo", strings.Repeat("b", 64))},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC().Round(0)
			store := &controllerRunCollectionFixture{runs: []Run{{ID: "selected-conflict", Repository: "owner/repo", RepositoryConfigJSON: test.raw, RepositoryBindingDigest: binding, State: domain.StateReceived, CreatedAt: now, UpdatedAt: now}}}
			queries, _ := NewScopedQueryService(store, authorizer)
			_, err := queries.ListControllerRunSummaries(context.Background(), reader, RunSummaryQuery{Limit: 1})
			var serviceErr *ServiceError
			if !errors.As(err, &serviceErr) || serviceErr.Category != ErrorInternal || serviceErr.Message != "controller run authority conflicts" || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "owner/other") {
				t.Fatalf("unsafe conflict result: %v", err)
			}
		})
	}
}

func TestMutationReauthorizesFrozenTargetInsteadOfTrustingStaleScope(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	run := Run{ID: "run", Repository: "owner/repo", State: domain.StateAwaitingHumanDecision, IdempotencyKey: "key", RepositoryBindingDigest: strings.Repeat("a", 64)}
	repository := LocalRepository{CanonicalRepository: run.Repository, AllowedOperatorLogins: []string{operator.Login}, TrustedOperatorActors: []TrustedActorIdentity{{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType}}}
	raw, _ := json.Marshal(repository)
	run.RepositoryConfigJSON = string(raw)
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	staleScopes, err := authorizer.FrozenRunScopes(requesterForUser(operator), run)
	if err != nil || staleScopes.Empty() {
		t.Fatalf("stale scope setup=%+v err=%v", staleScopes, err)
	}
	repository.TrustedOperatorActors[0].NodeID = "U_CHANGED"
	raw, _ = json.Marshal(repository)
	run.RepositoryConfigJSON = string(raw)
	controller := &serviceController{}
	_, err = NewCommandService(controller, serviceStore{run: run}).Continue(context.Background(), ContinueCommand{Requester: requesterForUser(operator), RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err == nil || controller.continued != 0 {
		t.Fatalf("stale scope crossed mutation boundary err=%v continued=%d", err, controller.continued)
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

func TestTerminalProjectionSeparatesHistoricalPullRequestSnapshotFromEffectiveMerge(t *testing.T) {
	now := time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC)
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	mergeSHA := strings.Repeat("c", 40)
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateCompleted, IdempotencyKey: "owner", WorkingBranch: "feature", BaseBranch: "main", CandidateHead: head, BaseSHA: base})
	snapshot := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, BaseSHA: base, BodyDigest: "body", OwnershipKey: "owner", State: "open"}
	merge := &MergeRecord{RunID: run.ID, PRNumber: snapshot.Number, PreMergeSHA: head, BaseSHA: base, Method: "squash", MergeSHA: mergeSHA, MergedAt: now}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, ExpectedRepositoryID: 99}
	store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &snapshot, Merge: merge}}

	status, err := NewQueryService(store).Status(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	inspect, err := NewQueryService(store).Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status, inspect) {
		t.Fatalf("status and inspect projections differ:\nstatus=%+v\ninspect=%+v", status, inspect)
	}
	if status.SchemaVersion != "v2" {
		t.Fatalf("terminal projection schema=%q", status.SchemaVersion)
	}
	if status.PullRequestAggregate == nil || status.PullRequestAggregate.AggregateLabel != "mutable_controller_aggregate" || status.PullRequestAggregate.State != "open" || status.PullRequestAggregate.Merged {
		t.Fatalf("pull request aggregate was rewritten or mislabeled: %+v", status.PullRequestAggregate)
	}
	if status.PullRequest == nil || status.PullRequest.Status != "merged" || status.PullRequest.State != "closed" || status.PullRequest.Merged == nil || !*status.PullRequest.Merged || status.PullRequest.MergeSHA != mergeSHA || status.PullRequest.EvidenceSource != "merge_result" {
		t.Fatalf("terminal merge was not projected effectively: %+v", status.PullRequest)
	}
	for _, observedAt := range []time.Time{now.Add(-time.Minute), now} {
		mismatched := snapshot
		mismatched.NodeID = "PR_WRONG"
		evidence := &domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}, PullRequest: mismatched, ObservedAt: observedAt}
		conflict := projectEffectivePullRequest(RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &snapshot, Merge: merge, GitHubEvidence: evidence})
		if conflict == nil || conflict.Status != "conflict" || conflict.EvidenceSource != "github_read_identity_conflict" {
			t.Fatalf("identity mismatch at %s did not fail closed: %+v", observedAt, conflict)
		}
	}
}

func TestTerminalProjectionFailsClosedForMissingOrConflictingMergeEvidence(t *testing.T) {
	now := time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC)
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateCompleted, IdempotencyKey: "owner", WorkingBranch: "feature", BaseBranch: "main", CandidateHead: head, BaseSHA: base})
	snapshot := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, BaseSHA: base, BodyDigest: "body", OwnershipKey: "owner", State: "open"}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, ExpectedRepositoryID: 99}
	service := NewQueryService(serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &snapshot}})
	missing, err := service.GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if missing.PullRequest == nil || missing.PullRequest.Status != "unknown" || missing.PullRequest.State != "unknown" || missing.PullRequest.Merged != nil || missing.PullRequest.EvidenceSource != "missing_terminal_merge_result" {
		t.Fatalf("missing terminal evidence did not fail closed: %+v", missing.PullRequest)
	}

	conflictingMerge := &MergeRecord{RunID: run.ID, PRNumber: snapshot.Number + 1, PreMergeSHA: head, BaseSHA: base, Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: now}
	service = NewQueryService(serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &snapshot, Merge: conflictingMerge}})
	conflict, err := service.GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if conflict.PullRequest == nil || conflict.PullRequest.Status != "conflict" || conflict.PullRequest.State != "conflict" || conflict.PullRequest.Merged != nil {
		t.Fatalf("conflicting terminal evidence was presented as a fact: %+v", conflict.PullRequest)
	}

	service = NewQueryService(serviceStore{run: run, inspection: RunInspection{Run: run, Merge: &MergeRecord{RunID: run.ID, PRNumber: snapshot.Number, PreMergeSHA: head, BaseSHA: base, Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: now}}})
	missingAggregate, err := service.GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if missingAggregate.PullRequest == nil || missingAggregate.PullRequest.Status != "unknown" || missingAggregate.PullRequest.EvidenceSource != "missing_pull_request_aggregate" || missingAggregate.PullRequest.Merged != nil {
		t.Fatalf("missing terminal PR aggregate was omitted or guessed: %+v", missingAggregate.PullRequest)
	}

	for _, test := range []struct {
		name   string
		mutate func(*domain.PullRequest)
	}{
		{"copied ownership", func(value *domain.PullRequest) { value.OwnershipKey = "another-run" }},
		{"wrong head branch", func(value *domain.PullRequest) { value.HeadBranch = "another-feature" }},
		{"wrong base branch", func(value *domain.PullRequest) { value.BaseBranch = "release" }},
		{"missing database identity", func(value *domain.PullRequest) { value.DatabaseID = 0 }},
		{"missing URL identity", func(value *domain.PullRequest) { value.URL = "" }},
		{"wrong base SHA", func(value *domain.PullRequest) { value.BaseSHA = strings.Repeat("d", 40) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			corrupt := snapshot
			test.mutate(&corrupt)
			got := projectEffectivePullRequest(RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &corrupt, Merge: &MergeRecord{
				RunID: run.ID, PRNumber: corrupt.Number, PreMergeSHA: head, BaseSHA: base,
				Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: now,
			}})
			if got == nil || got.Status != "conflict" || got.State != "conflict" || got.Merged != nil || got.EvidenceSource != "pull_request_aggregate_authority_conflict" {
				t.Fatalf("corrupt PR aggregate was projected as terminal truth: %+v", got)
			}
		})
	}
	validMerge := MergeRecord{
		RunID: run.ID, PRNumber: snapshot.Number, PreMergeSHA: head, BaseSHA: base,
		Method: "squash", MergeSHA: strings.Repeat("c", 40), MergedAt: now,
	}
	for _, test := range []struct {
		name   string
		mutate func(*MergeRecord)
	}{
		{"short pre-merge SHA", func(value *MergeRecord) { value.PreMergeSHA = "short" }},
		{"nonhex base SHA", func(value *MergeRecord) { value.BaseSHA = strings.Repeat("g", 40) }},
		{"short merge SHA", func(value *MergeRecord) { value.MergeSHA = "short" }},
		{"nonhex merge SHA", func(value *MergeRecord) { value.MergeSHA = strings.Repeat("z", 40) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			corrupt := validMerge
			test.mutate(&corrupt)
			got := projectEffectivePullRequest(RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &snapshot, Merge: &corrupt})
			if got == nil || got.Status != "conflict" || got.Merged != nil || got.EvidenceSource != "merge_result_conflict" {
				t.Fatalf("malformed merge SHA was projected as terminal truth: %+v", got)
			}
		})
	}
}

func TestEffectivePullRequestBindsRepositoryAndEqualTimeTerminalEvidence(t *testing.T) {
	now := time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC)
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	mergeSHA := strings.Repeat("c", 40)
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateCompleted, IdempotencyKey: "owner", WorkingBranch: "feature", BaseBranch: "main", CandidateHead: head, BaseSHA: base})
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, BaseSHA: base, BodyDigest: "body", OwnershipKey: "owner", State: "open"}
	merge := &MergeRecord{RunID: run.ID, PRNumber: pr.Number, PreMergeSHA: head, BaseSHA: base, Method: "squash", MergeSHA: mergeSHA, MergedAt: now}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, ExpectedRepositoryID: 99}
	repository := domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}

	for _, test := range []struct {
		name       string
		binding    *SanitizedRepositoryBinding
		repo       domain.RepositoryIdentity
		pr         domain.PullRequest
		observed   time.Time
		want       string
		wantSource string
	}{
		{"matching authority without read", binding, domain.RepositoryIdentity{}, domain.PullRequest{}, time.Time{}, "merged", "merge_result"},
		{"missing binding", nil, domain.RepositoryIdentity{}, domain.PullRequest{}, time.Time{}, "unknown", "missing_or_invalid_repository_binding"},
		{"invalid binding", &SanitizedRepositoryBinding{CanonicalRepository: run.Repository}, domain.RepositoryIdentity{}, domain.PullRequest{}, time.Time{}, "unknown", "missing_or_invalid_repository_binding"},
		{"wrong owner", binding, domain.RepositoryIdentity{ID: 99, Owner: "other", Name: "repo"}, pr, now.Add(-time.Minute), "conflict", "github_repository_authority_conflict"},
		{"wrong repository id", binding, domain.RepositoryIdentity{ID: 100, Owner: "owner", Name: "repo"}, pr, now.Add(-time.Minute), "conflict", "github_repository_authority_conflict"},
		{"earlier open is historical", binding, repository, pr, now.Add(-time.Minute), "merged", "merge_result"},
		{"equal open conflicts", binding, repository, pr, now, "conflict", "github_read_state_conflicts_with_merge_result"},
		{"later open conflicts", binding, repository, pr, now.Add(time.Minute), "conflict", "github_read_state_conflicts_with_merge_result"},
		{"equal wrong merge SHA conflicts", binding, repository, func() domain.PullRequest {
			value := pr
			value.State, value.Merged, value.MergeSHA, value.MergedAt = "closed", true, strings.Repeat("d", 40), now
			return value
		}(), now, "conflict", "github_read_state_conflicts_with_merge_result"},
		{"equal merged matches", binding, repository, func() domain.PullRequest {
			value := pr
			value.State, value.Merged, value.MergeSHA, value.MergedAt = "closed", true, mergeSHA, now
			return value
		}(), now, "merged", "merge_result"},
		{"later merged matches", binding, repository, func() domain.PullRequest {
			value := pr
			value.State, value.Merged, value.MergeSHA, value.MergedAt = "closed", true, mergeSHA, now
			return value
		}(), now.Add(time.Minute), "merged", "merge_result"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := RunInspection{Run: run, RepositoryBinding: test.binding, PullRequest: &pr, Merge: merge}
			if !test.observed.IsZero() {
				inspection.GitHubEvidence = &domain.GitHubReadEvidence{Repository: test.repo, PullRequest: test.pr, ObservedAt: test.observed}
			}
			got := projectEffectivePullRequest(inspection)
			if got == nil || got.Status != test.want || got.EvidenceSource != test.wantSource {
				t.Fatalf("effective PR=%+v want status=%s source=%s", got, test.want, test.wantSource)
			}
		})
	}
	zeroTime := projectEffectivePullRequest(RunInspection{
		Run: run, RepositoryBinding: binding, PullRequest: &pr, Merge: merge,
		GitHubEvidence: &domain.GitHubReadEvidence{Repository: repository, PullRequest: pr},
	})
	if zeroTime == nil || zeroTime.Status != "conflict" || zeroTime.EvidenceSource != "github_read_observation_time_conflict" {
		t.Fatalf("zero-time GitHub observation was treated as historical: %+v", zeroTime)
	}
	laterEvidence := domain.GitHubReadEvidence{Repository: repository, PullRequest: pr, ObservedAt: now.Add(time.Minute)}
	maskedZeroTime := projectEffectivePullRequest(RunInspection{
		Run: run, RepositoryBinding: binding, PullRequest: &pr, Merge: merge,
		GitHubEvidence: &laterEvidence,
		GitHubEvidenceHistory: []domain.GitHubReadEvidence{
			{Repository: repository, PullRequest: pr},
			laterEvidence,
		},
	})
	if maskedZeroTime == nil || maskedZeroTime.Status != "conflict" || maskedZeroTime.EvidenceSource != "github_read_observation_time_conflict" {
		t.Fatalf("later GitHub read masked an unorderable observation: %+v", maskedZeroTime)
	}
}

func TestTrustedFeedbackProjectionSeparatesInitialSnapshotFromFinalThreadEvidence(t *testing.T) {
	now := time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC)
	head := strings.Repeat("a", 40)
	author := domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "operator", Type: "User"}
	body := "Please fix this."
	line := 12
	feedback := TrustedReviewFeedbackRecord{RunID: "run", TrustedReviewFeedback: domain.TrustedReviewFeedback{
		PRNumber: 7, PRDatabaseID: 70, PRNodeID: "PR_7", ReviewDatabaseID: 80, ReviewNodeID: "REVIEW_80",
		ThreadNodeID: "THREAD_90", RootCommentDatabaseID: 100, RootCommentNodeID: "COMMENT_100", Author: author,
		OriginalReviewHeadSHA: head, Path: "internal/example.go", Line: &line, BodyDigest: domain.TrustedReviewFeedbackDigest(body),
		Body: body, SourceAt: now, ObservedAt: now, Lifecycle: domain.TrustedReviewFeedbackReplied,
		BoundRepairHead: strings.Repeat("d", 40), ReplyIntentKey: "reply-intent",
		ReplyDatabaseID: 110, ReplyNodeID: "COMMENT_110", UpdatedAt: now.Add(time.Minute),
	}}
	pr := domain.PullRequest{Number: feedback.PRNumber, DatabaseID: feedback.PRDatabaseID, NodeID: feedback.PRNodeID, URL: "https://example.invalid/pull/7", HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, BaseSHA: strings.Repeat("b", 40), BodyDigest: "body", OwnershipKey: "owner", State: "open"}
	thread := domain.GitHubReviewThread{NodeID: feedback.ThreadNodeID, Resolved: true, Outdated: true, OriginalCommitSHA: head, Path: feedback.Path, Line: nil, Comments: []domain.GitHubReviewComment{{
		DatabaseID: feedback.RootCommentDatabaseID, NodeID: feedback.RootCommentNodeID, Author: &author, BodyDigest: feedback.BodyDigest,
		Review: domain.GitHubReview{DatabaseID: feedback.ReviewDatabaseID, NodeID: feedback.ReviewNodeID, State: "CHANGES_REQUESTED", CommitSHA: head, Actor: author},
	}}}
	run := authorizeTestRun(Run{ID: feedback.RunID, Repository: "owner/repo", State: domain.StateCompleted, IdempotencyKey: "owner", WorkingBranch: "feature", BaseBranch: "main", CandidateHead: head, BaseSHA: pr.BaseSHA})
	repository := domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}
	evidence := &domain.GitHubReadEvidence{Repository: repository, PullRequest: pr, ReviewThreads: []domain.GitHubReviewThread{thread}, ObservedAt: now.Add(2 * time.Minute)}
	shadow := *evidence
	shadow.ReviewThreads = nil
	shadow.ObservedAt = now.Add(3 * time.Minute)
	inspection := RunInspection{Run: run, RepositoryBinding: &SanitizedRepositoryBinding{CanonicalRepository: run.Repository, ExpectedRepositoryID: repository.ID}, PullRequest: &pr, TrustedFeedback: []TrustedReviewFeedbackRecord{feedback}, GitHubEvidence: &shadow, GitHubEvidenceHistory: []domain.GitHubReadEvidence{*evidence, shadow}}
	got, err := NewQueryService(serviceStore{run: run, inspection: inspection}).GetRunDetail(context.Background(), RunDetailQuery{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	projected := got.TrustedFeedback[0]
	if projected.SnapshotLabel != "initial_trusted_change_request" || projected.ControllerResolved || projected.ControllerOutdated {
		t.Fatalf("initial/controller snapshot was rewritten: %+v", projected)
	}
	if projected.EffectiveThreadStatus.Status != "resolved_outdated" || projected.EffectiveThreadStatus.Resolved == nil || !*projected.EffectiveThreadStatus.Resolved || projected.EffectiveThreadStatus.Outdated == nil || !*projected.EffectiveThreadStatus.Outdated || projected.EffectiveThreadStatus.EvidenceSource != "github_read_observation" {
		t.Fatalf("final thread evidence was not projected: %+v", projected.EffectiveThreadStatus)
	}
	withoutFinalEvidence := inspection
	withoutFinalEvidence.GitHubEvidence = nil
	withoutFinalEvidence.GitHubEvidenceHistory = nil
	if unknown := projectEffectiveThreadStatus(withoutFinalEvidence, feedback); unknown.Status != "unknown" || unknown.Resolved != nil || unknown.Outdated != nil {
		t.Fatalf("missing final thread evidence was presented as fact: %+v", unknown)
	}
	conflicting := thread
	conflicting.Comments = append([]domain.GitHubReviewComment(nil), thread.Comments...)
	conflicting.Comments[0].DatabaseID++
	conflictEvidence := *evidence
	conflictEvidence.ReviewThreads = []domain.GitHubReviewThread{conflicting}
	inspection.GitHubEvidence = &conflictEvidence
	inspection.GitHubEvidenceHistory = []domain.GitHubReadEvidence{conflictEvidence}
	conflict := projectEffectiveThreadStatus(inspection, feedback)
	if conflict.Status != "conflict" || conflict.Resolved != nil || conflict.Outdated != nil {
		t.Fatalf("conflicting final thread evidence was presented as fact: %+v", conflict)
	}
	wrongAuthority := *evidence
	wrongAuthority.Repository.ID++
	wrongAuthority.PullRequest.Number++
	wrongAuthority.ObservedAt = now.Add(4 * time.Minute)
	inspection.GitHubEvidence = &wrongAuthority
	inspection.GitHubEvidenceHistory = []domain.GitHubReadEvidence{*evidence, wrongAuthority}
	conflict = projectEffectiveThreadStatus(inspection, feedback)
	if conflict.Status != "conflict" || conflict.Resolved != nil || conflict.Outdated != nil {
		t.Fatalf("wrong repository/PR authority was presented as resolved: %+v", conflict)
	}
	corruptAggregate := pr
	corruptAggregate.HeadBranch = "copied-feature"
	corruptEvidence := *evidence
	corruptEvidence.PullRequest = corruptAggregate
	corruptInspection := inspection
	corruptInspection.PullRequest = &corruptAggregate
	corruptInspection.GitHubEvidence = &corruptEvidence
	corruptInspection.GitHubEvidenceHistory = []domain.GitHubReadEvidence{corruptEvidence}
	conflict = projectEffectiveThreadStatus(corruptInspection, feedback)
	if conflict.Status != "conflict" || conflict.EvidenceSource != "feedback_pull_request_authority_conflict" {
		t.Fatalf("mutually consistent evidence detached from the run was trusted: %+v", conflict)
	}

	resolvedFeedback := feedback
	resolvedFeedback.Lifecycle = domain.TrustedReviewFeedbackResolved
	resolvedFeedback.Resolved = true
	resolvedFeedback.UpdatedAt = now.Add(5 * time.Minute)
	maskedThreadInspection := inspection
	maskedThreadInspection.GitHubEvidence = evidence
	maskedThreadInspection.GitHubEvidenceHistory = []domain.GitHubReadEvidence{
		{Repository: repository, PullRequest: pr, ReviewThreads: []domain.GitHubReviewThread{thread}},
		*evidence,
	}
	conflict = projectEffectiveThreadStatus(maskedThreadInspection, resolvedFeedback)
	if conflict.Status != "conflict" || conflict.EvidenceSource != "github_read_observation_time_conflict" {
		t.Fatalf("later thread read masked an unorderable observation: %+v", conflict)
	}
	corruptControllerInspection := corruptInspection
	corruptControllerInspection.GitHubEvidence = nil
	corruptControllerInspection.GitHubEvidenceHistory = nil
	conflict = projectEffectiveThreadStatus(corruptControllerInspection, resolvedFeedback)
	if conflict.Status != "conflict" || conflict.EvidenceSource != "feedback_pull_request_authority_conflict" {
		t.Fatalf("controller resolution detached from the run PR was trusted: %+v", conflict)
	}
	for _, test := range []struct {
		name       string
		observedAt time.Time
		resolved   bool
		outdated   bool
		wantStatus string
		wantSource string
	}{
		{"earlier unresolved is historical", resolvedFeedback.UpdatedAt.Add(-time.Second), false, false, "resolved", "controller_resolution_observation"},
		{"equal unresolved conflicts", resolvedFeedback.UpdatedAt, false, false, "conflict", "github_read_conflicts_with_controller_lifecycle"},
		{"later unresolved conflicts", resolvedFeedback.UpdatedAt.Add(time.Second), false, false, "conflict", "github_read_conflicts_with_controller_lifecycle"},
		{"earlier resolved outdated remains final evidence", resolvedFeedback.UpdatedAt.Add(-time.Second), true, true, "resolved_outdated", "github_read_observation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			matrixThread := thread
			matrixThread.Resolved = test.resolved
			matrixThread.Outdated = test.outdated
			matrixThread.Line = feedback.Line
			matrixEvidence := *evidence
			matrixEvidence.ReviewThreads = []domain.GitHubReviewThread{matrixThread}
			matrixEvidence.ObservedAt = test.observedAt
			matrixInspection := inspection
			matrixInspection.GitHubEvidence = &matrixEvidence
			matrixInspection.GitHubEvidenceHistory = []domain.GitHubReadEvidence{matrixEvidence}
			got := projectEffectiveThreadStatus(matrixInspection, resolvedFeedback)
			if got.Status != test.wantStatus || got.EvidenceSource != test.wantSource {
				t.Fatalf("effective thread status=%+v want status=%s source=%s", got, test.wantStatus, test.wantSource)
			}
		})
	}

	for _, test := range []struct {
		name   string
		mutate func(*TrustedReviewFeedbackRecord)
	}{
		{"wrong run", func(value *TrustedReviewFeedbackRecord) { value.RunID = "another-run" }},
		{"missing review identity", func(value *TrustedReviewFeedbackRecord) { value.ReviewNodeID = "" }},
		{"impossible resolved lifecycle", func(value *TrustedReviewFeedbackRecord) { value.Lifecycle = domain.TrustedReviewFeedbackReplied }},
		{"missing source timestamp", func(value *TrustedReviewFeedbackRecord) { value.SourceAt = time.Time{} }},
		{"missing observation timestamp", func(value *TrustedReviewFeedbackRecord) { value.ObservedAt = time.Time{} }},
		{"missing lifecycle timestamp", func(value *TrustedReviewFeedbackRecord) { value.UpdatedAt = time.Time{} }},
		{"lifecycle timestamp predates observation", func(value *TrustedReviewFeedbackRecord) { value.UpdatedAt = value.ObservedAt.Add(-time.Second) }},
	} {
		t.Run("corrupt feedback "+test.name, func(t *testing.T) {
			corrupt := resolvedFeedback
			test.mutate(&corrupt)
			got := projectEffectiveThreadStatus(inspection, corrupt)
			if got.Status != "conflict" || got.Resolved != nil || got.Outdated != nil || got.EvidenceSource != "trusted_review_feedback_authority_conflict" {
				t.Fatalf("corrupt persisted feedback was projected as resolved: %+v", got)
			}
		})
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

func TestReconcilePersistsFiniteUnsupportedTrustedReviewTopologyReason(t *testing.T) {
	pr := domain.PullRequest{Number: 1, URL: "https://example.invalid/1", NodeID: "PR", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: "body", OwnershipKey: "key", State: "open"}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateReconcilingReviews, IdempotencyKey: "key", WorkingBranch: "feature", BaseBranch: "main", CandidateHead: "head", BaseSHA: "base"})
	binding := &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2}
	evidence := domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}, PullRequest: pr}
	metadata := GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: evidence.Repository}
	conflictExisting := TrustedReviewFeedbackRecord{RunID: run.ID, TrustedReviewFeedback: domain.TrustedReviewFeedback{RootCommentNodeID: "COMMENT_1", Body: "original"}}
	conflictObserved := TrustedReviewFeedbackRecord{RunID: run.ID, TrustedReviewFeedback: domain.TrustedReviewFeedback{RootCommentNodeID: "COMMENT_1", Body: "changed"}}
	for _, tc := range []struct {
		name     string
		command  ReconcileCommand
		existing []TrustedReviewFeedbackRecord
		want     string
	}{
		{name: "split review overrides Linear completion", command: ReconcileCommand{LinearCompleted: true, FeedbackUnsupported: true, FeedbackUnsupportedReason: domain.TrustedReviewTopologySplitReview}, want: string(domain.TrustedReviewTopologySplitReview)},
		{name: "generic unsupported overrides Linear completion", command: ReconcileCommand{LinearCompleted: true, FeedbackUnsupported: true}, want: string(domain.TrustedReviewTopologyUnsupported)},
		{name: "unknown unsupported subtype remains finite", command: ReconcileCommand{LinearCompleted: true, FeedbackUnsupported: true, FeedbackUnsupportedReason: domain.TrustedReviewTopologyReason("actor controlled raw prose")}, want: string(domain.TrustedReviewTopologyUnsupported)},
		{name: "feedback drift overrides Linear completion", command: ReconcileCommand{LinearCompleted: true, FeedbackDrift: true}, want: TrustedReviewFeedbackDriftReason},
		{name: "feedback conflict overrides Linear completion", command: ReconcileCommand{LinearCompleted: true, TrustedFeedback: []TrustedReviewFeedbackRecord{conflictObserved}}, existing: []TrustedReviewFeedbackRecord{conflictExisting}, want: TrustedReviewFeedbackConflictReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var next domain.State
			var reason string
			store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr, TrustedFeedback: tc.existing}, nextState: &next, transitionReason: &reason}
			command := tc.command
			command.Requester, command.RunID, command.Repository = Requester{ID: "operator", Kind: "github_login"}, run.ID, run.Repository
			command.ExpectedState, command.IdempotencyKey, command.Evidence, command.Metadata = run.State, run.IdempotencyKey, evidence, metadata

			result, err := NewCommandService(nil, store).Reconcile(context.Background(), command)
			if err != nil || result.State != domain.StateManualIntervention || next != domain.StateManualIntervention || reason != tc.want {
				t.Fatalf("result=%+v next=%s reason=%q err=%v", result, next, reason, err)
			}
			if strings.Contains(reason, "operator") || strings.Contains(reason, "actor controlled") || strings.Contains(reason, "review body") {
				t.Fatalf("untrusted prose entered reason=%q", reason)
			}
		})
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
