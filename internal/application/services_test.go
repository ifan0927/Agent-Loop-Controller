package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type serviceStore struct {
	RunStore
	run          Run
	inspection   RunInspection
	renewed      *int
	renewOK      bool
	failureSaved *[]GitHubRequestObservation
}

func (s serviceStore) GetRun(context.Context, string) (Run, error) { return s.run, nil }
func (s serviceStore) GetRunByIdempotency(context.Context, string) (Run, bool, error) {
	return Run{}, false, nil
}
func (s serviceStore) Inspect(context.Context, string) (RunInspection, error) {
	return s.inspection, nil
}
func (s serviceStore) SaveGitHubReadSuccess(context.Context, string, string, domain.State, string, []GitHubRequestObservation, domain.PullRequest, GitHubInstallationMetadata, domain.GitHubReadEvidence) error {
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

type serviceGitHubReader struct {
	calls        int
	observations []GitHubRequestObservation
	err          error
}

func (r *serviceGitHubReader) Read(context.Context, int64, string) (domain.GitHubReadEvidence, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	r.calls++
	return domain.GitHubReadEvidence{}, r.observations, GitHubInstallationMetadata{}, r.err
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

func authorizeTestRun(run Run) Run {
	raw, _ := json.Marshal(LocalRepository{AllowedOperatorLogins: []string{"operator"}})
	run.RepositoryConfigJSON = string(raw)
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

func TestCommandServicePassesAuthorityAndSanitizesContinueResult(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StateExecuting, IdempotencyKey: "key", WorktreePath: "/secret/worktree", ArtifactRoot: "/secret/artifacts", ImplementationSession: "secret-session", LastError: "secret-error"})
	controller := &serviceController{run: run}
	result, err := NewCommandService(controller, serviceStore{run: run}).Continue(context.Background(), ContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StateExecuting, IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if controller.expected != domain.StateExecuting || controller.key != "key" || strings.Contains(string(raw), "key") || strings.Contains(string(raw), "secret") {
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

func TestQueryServiceSanitizesInspection(t *testing.T) {
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", WorktreePath: "/secret/worktree", LastError: "secret"})
	store := serviceStore{run: run, inspection: RunInspection{Run: run, Findings: []FindingRecord{{Body: "untrusted", File: "/secret/file"}}}}
	got, err := NewQueryService(store).Inspect(context.Background(), QueryInput{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "untrusted") {
		t.Fatalf("inspection was not sanitized: %s", raw)
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
	binding := &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubInstallationID: 2}
	evidence := domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}, PullRequest: pr}
	metadata := GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: evidence.Repository}
	store := serviceStore{run: run, inspection: RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr}}
	service := NewCommandService(nil, store)
	if _, err := service.Reconcile(context.Background(), ReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", Evidence: evidence, Metadata: metadata}); err != nil {
		t.Fatal(err)
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

func TestGitHubReconcileCancelsReadWhenLeaseIsLost(t *testing.T) {
	originalTTL := reconcileLeaseTTL
	reconcileLeaseTTL = 30 * time.Millisecond
	defer func() { reconcileLeaseTTL = originalTTL }()
	pr := domain.PullRequest{Number: 1}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key", CandidateHead: "head"})
	renewed := 0
	store := serviceStore{run: run, inspection: RunInspection{Run: run, PullRequest: &pr}, renewed: &renewed, renewOK: false}
	reader := blockingGitHubReader{}
	_, err := NewCommandService(nil, store).ReconcileFromGitHub(context.Background(), GitHubReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", PullRequest: 1, ExpectedHead: "head"}, reader)
	if err == nil || renewed == 0 {
		t.Fatalf("lease loss error=%v renewals=%d", err, renewed)
	}
}

type blockingGitHubReader struct{}

func (blockingGitHubReader) Read(ctx context.Context, _ int64, _ string) (domain.GitHubReadEvidence, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	<-ctx.Done()
	return domain.GitHubReadEvidence{}, nil, GitHubInstallationMetadata{}, context.Cause(ctx)
}

func TestGitHubReconcilePersistsPartialFailureObservations(t *testing.T) {
	pr := domain.PullRequest{Number: 1}
	run := authorizeTestRun(Run{ID: "run", Repository: "owner/repo", State: domain.StatePROpen, IdempotencyKey: "key", CandidateHead: "head"})
	observation := GitHubRequestObservation{RunID: "run", Operation: "read", ErrorClass: "timeout"}
	var saved []GitHubRequestObservation
	store := serviceStore{run: run, inspection: RunInspection{Run: run, PullRequest: &pr}, failureSaved: &saved}
	reader := &serviceGitHubReader{observations: []GitHubRequestObservation{observation}, err: errors.New("read failed")}
	_, err := NewCommandService(nil, store).ReconcileFromGitHub(context.Background(), GitHubReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StatePROpen, IdempotencyKey: "key", PullRequest: 1, ExpectedHead: "head"}, reader)
	if err == nil || len(saved) != 1 || saved[0].ErrorClass != "timeout" {
		t.Fatalf("failure error=%v saved=%+v", err, saved)
	}
}
