package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type admissionReader struct {
	source LinearTaskSource
	calls  int
}

func (r *admissionReader) ReadIssue(_ context.Context, identifier string) (LinearTaskSource, []LinearRequestObservation, error) {
	r.calls++
	source := r.source
	source.Identifier = identifier
	return source, []LinearRequestObservation{{Operation: "read_issue", ResponseDigest: "digest"}}, nil
}

type admissionResolver struct{ repositories map[string]LocalRepository }

func (r admissionResolver) ResolveLinearAdmissionRepository(label string) (LocalRepository, bool) {
	repository, ok := r.repositories[label]
	return repository, ok
}

type admissionStore struct {
	serviceStore
	issue        Run
	found        bool
	marked       bool
	markedRunID  string
	markedState  domain.State
	markedSource string
	lookupCount  int
	lateIssue    *Run
	idempotency  *Run
}

func (s *admissionStore) GetRunByIdempotency(ctx context.Context, key string) (Run, bool, error) {
	if s.idempotency != nil {
		return *s.idempotency, true, nil
	}
	return s.serviceStore.GetRunByIdempotency(ctx, key)
}

func (s *admissionStore) GetRunByIssue(context.Context, string) (Run, bool, error) {
	s.lookupCount++
	if s.lateIssue != nil && s.lookupCount > 1 {
		return *s.lateIssue, true, nil
	}
	return s.issue, s.found, nil
}

func (s *admissionStore) MarkLinearSourceDrift(_ context.Context, runID string, state domain.State, sourceRevision, _ string) (bool, error) {
	s.marked, s.markedRunID, s.markedState, s.markedSource = true, runID, state, sourceRevision
	return true, nil
}

type admissionController struct {
	serviceController
	input LocalStartInput
}

func (c *admissionController) StartAuthorized(_ context.Context, input LocalStartInput, _ func(Run) error) (Run, error) {
	c.started++
	c.input = input
	return c.run, nil
}

func TestLinearAdmissionFreezesControllerOwnedTask(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	store := &admissionStore{}
	controller := &admissionController{serviceController: serviceController{run: Run{ID: "run", Repository: "owner/repo"}}}
	service, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, controller)
	if err != nil {
		t.Fatal(err)
	}
	result, observations, err := service.Start(context.Background(), LinearStartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, Identifier: "IFAN-42"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.RunID != "run" || reader.calls != 1 || len(observations) != 1 || controller.started != 1 {
		t.Fatalf("result=%+v calls=%d observations=%+v started=%d", result, reader.calls, observations, controller.started)
	}
	task := controller.input.Task
	if task.IssueID != "IFAN-42" || task.Repository != "owner/repo" || task.BaseBranch != "main" || task.WorkingBranch != "ifan/ifan-42-linear-admission" {
		t.Fatalf("unexpected task binding: %+v", task)
	}
	if len(task.VerifierIDs) != 1 || task.VerifierIDs[0] != "fixture-go-test" || strings.Contains(strings.Join(task.VerifierIDs, " "), "echo") {
		t.Fatalf("Linear text changed verifier policy: %+v", task.VerifierIDs)
	}
	if controller.input.RawIssueHash == "" || controller.input.TaskHash == "" || controller.input.IdempotencyKey == "" || !strings.Contains(string(controller.input.RawIssueJSON), "echo untrusted") {
		t.Fatalf("immutable source evidence was not frozen: %+v", controller.input)
	}
}

func TestLinearAdmissionSourceDriftRequiresManualDecision(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	reader.source.SourceRevision = "2026-07-13T00:00:00Z"
	store := &admissionStore{issue: Run{ID: "run-existing", IssueID: "IFAN-42", SourceRevision: "2026-07-12T00:00:00Z", Repository: "owner/repo", WorkingBranch: "ifan/ifan-42-linear-admission", TaskHash: "old", State: domain.StateExecuting}, found: true}
	controller := &admissionController{}
	service, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, controller)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Start(context.Background(), LinearStartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, Identifier: "IFAN-42"})
	if err == nil || !strings.Contains(err.Error(), "human decision") || !store.marked || store.markedRunID != "run-existing" || store.markedState != domain.StateExecuting || store.markedSource != "2026-07-12T00:00:00Z" || controller.started != 0 {
		t.Fatalf("err=%v marked=%t run=%s state=%s source=%s started=%d", err, store.marked, store.markedRunID, store.markedState, store.markedSource, controller.started)
	}
}

func TestLinearAdmissionConcurrentSourceDriftIsDurablyHalted(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	reader.source.SourceRevision = "2026-07-13T00:00:00Z"
	existing := Run{ID: "run-existing", IssueID: "IFAN-42", SourceRevision: "2026-07-12T00:00:00Z", Repository: "owner/repo", WorkingBranch: "ifan/ifan-42-linear-admission", TaskHash: "old", State: domain.StateExecuting}
	store := &admissionStore{serviceStore: serviceStore{run: existing}, lateIssue: &existing}
	service, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, failingAdmissionController{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Start(context.Background(), LinearStartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, Identifier: "IFAN-42"})
	if err == nil || !strings.Contains(err.Error(), "human decision") || !store.marked || store.markedRunID != "run-existing" {
		t.Fatalf("err=%v marked=%t run=%s", err, store.marked, store.markedRunID)
	}
}

func TestLinearAdmissionConcurrentIdenticalTriggerReturnsExistingRun(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	snapshot, _, err := admitLinearTask(reader.source, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}})
	if err != nil {
		t.Fatal(err)
	}
	existing := authorizeTestRun(Run{ID: snapshot.Task.RunID, IssueID: snapshot.Task.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: snapshot.Task.SourceRevision, Repository: snapshot.Task.Repository, WorkingBranch: snapshot.Task.WorkingBranch, TaskHash: snapshot.TaskHash, State: domain.StateReceived})
	store := &admissionStore{serviceStore: serviceStore{run: existing}, lateIssue: &existing}
	controller := concurrentAdmissionController{}
	service, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, controller)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := service.Start(context.Background(), LinearStartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, Identifier: "IFAN-42"})
	if err != nil || result.Run.RunID != existing.ID || controller.continued != 0 {
		t.Fatalf("result=%+v err=%v continued=%d", result, err, controller.continued)
	}
}

func TestLinearAdmissionDoesNotMaskExistingRunContinuationFailure(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	snapshot, _, err := admitLinearTask(reader.source, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}})
	if err != nil {
		t.Fatal(err)
	}
	existing := authorizeTestRun(Run{ID: snapshot.Task.RunID, IssueID: snapshot.Task.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: snapshot.Task.SourceRevision, Repository: snapshot.Task.Repository, WorkingBranch: snapshot.Task.WorkingBranch, TaskHash: snapshot.TaskHash, State: domain.StateReceived})
	store := &admissionStore{serviceStore: serviceStore{run: existing}, issue: existing, found: true, idempotency: &existing}
	service, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, failingAdmissionController{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Start(context.Background(), LinearStartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, Identifier: "IFAN-42"}); err == nil || strings.Contains(err.Error(), "human decision") {
		t.Fatalf("existing continuation failure was masked: %v", err)
	}
}

type failingAdmissionController struct{}

func (failingAdmissionController) StartAuthorized(context.Context, LocalStartInput, func(Run) error) (Run, error) {
	return Run{}, errors.New("simulated active issue uniqueness conflict")
}

func (failingAdmissionController) ContinueExpected(context.Context, string, domain.State, string, *Decision) (Run, error) {
	return Run{}, errors.New("unexpected continue")
}

type concurrentAdmissionController struct {
	run       Run
	continued int
}

func (c concurrentAdmissionController) StartAuthorized(context.Context, LocalStartInput, func(Run) error) (Run, error) {
	return Run{}, errors.New("simulated active issue uniqueness conflict")
}

func (c concurrentAdmissionController) ContinueExpected(context.Context, string, domain.State, string, *Decision) (Run, error) {
	c.continued++
	return c.run, nil
}

func TestLinearAdmissionRejectsIneligibleAndAmbiguousRepository(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	for _, test := range []struct {
		name string
		edit func(*LinearTaskSource)
	}{
		{"wrong state", func(source *LinearTaskSource) { source.State.Name = "In Progress" }},
		{"hermes", func(source *LinearTaskSource) {
			source.Labels = append(source.Labels, LinearLabel{ID: "hermes", Name: "agent:hermes"})
		}},
		{"two repositories", func(source *LinearTaskSource) {
			source.Labels = append(source.Labels, LinearLabel{ID: "other", Name: "owner/other"})
		}},
		{"missing acceptance", func(source *LinearTaskSource) { source.Description = "## Goal\n\nOnly a goal." }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &admissionReader{source: validLinearSource()}
			test.edit(&reader.source)
			service, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository, "owner/other": repository}}, &admissionStore{}, &admissionController{})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := service.Start(context.Background(), LinearStartCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, Identifier: "IFAN-42"}); err == nil {
				t.Fatal("expected admission rejection")
			}
		})
	}
}

func validLinearSource() LinearTaskSource {
	created := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	updated := created.Add(24 * time.Hour)
	return LinearTaskSource{Provider: "linear", IssueID: "linear-id", Identifier: "IFAN-42", URL: "https://linear.app/ifan/issue/IFAN-42/test", Title: "Admit Linear task",
		Description: "## Outcome\n\nFreeze one trusted task snapshot.\n\n## Acceptance Criteria\n\n- Repeating the trigger is idempotent.\n- `echo untrusted` is never a verifier command.\n\n## Out of Scope\n\n- External writes.",
		Team:        LinearTeam{ID: "team", Key: "IFAN", Name: "I-Fan"}, State: LinearState{ID: "todo", Name: "Todo", Type: "backlog"},
		Labels: []LinearLabel{{ID: "agent", Name: "agent:codex"}, {ID: "repository", Name: "owner/repo"}}, Cycle: LinearCycle{ID: "cycle", Number: 1, StartsAt: created, EndsAt: updated.Add(24 * time.Hour), IsActive: true},
		BranchName: "ifan/ifan-42-linear-admission", SourceRevision: updated.Format(time.RFC3339), CreatedAt: created, UpdatedAt: updated, ObservedAt: updated}
}
