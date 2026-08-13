package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type schedulingQueryFixture struct {
	runs       []SchedulingRun
	lastScopes AuthorizedScopeSet
	capacity   CapacityProjection
}

func (f *schedulingQueryFixture) Capacity(context.Context, time.Time) (CapacityProjection, error) {
	return f.capacity, nil
}

func (f *schedulingQueryFixture) ListRunScopeAuthorities(context.Context) ([]RunScopeAuthority, error) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	result := make([]RunScopeAuthority, 0, len(f.runs))
	for _, run := range f.runs {
		result = append(result, RunScopeAuthority{RunID: run.RunID, Repository: "repository-for-" + run.RunID, BindingDigest: run.RepositoryBindingDigest, AllowedLogins: []string{operator.Login}, TrustedOperators: []domain.GitHubUserIdentity{operator}})
	}
	return result, nil
}

func (f *schedulingQueryFixture) ListSchedulingRuns(_ context.Context, scopes AuthorizedScopeSet, _ int) ([]SchedulingRun, error) {
	f.lastScopes = scopes
	result := []SchedulingRun{}
	for _, run := range f.runs {
		if scopes.AllowsRun(run.RunID, run.RepositoryBindingDigest) {
			result = append(result, run)
		}
	}
	return result, nil
}

func (f *schedulingQueryFixture) GetSchedulingRun(_ context.Context, scopes AuthorizedScopeSet, runID string) (SchedulingRun, error) {
	f.lastScopes = scopes
	for _, run := range f.runs {
		if run.RunID == runID && scopes.AllowsRun(run.RunID, run.RepositoryBindingDigest) {
			return run, nil
		}
	}
	return SchedulingRun{}, ErrRunNotFound
}

func TestSchedulingQueriesSeparateControllerCapacityFromRepositoryRun(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	authority := repositoryAuthority(operator, false)
	fixture := &schedulingQueryFixture{capacity: CapacityProjection{EffectiveCapacity: 2, InUse: 1, Available: 1}, runs: []SchedulingRun{
		{RunID: "own-run", RepositoryBindingDigest: authority.BindingDigest, State: domain.StateExecuting, SupervisorState: "waiting", WaitingForCapacity: true},
		{RunID: "sibling-run", RepositoryBindingDigest: strings.Repeat("b", 64), State: domain.StateExecuting, SupervisorState: "running", HasHeavyPermit: true},
	}}
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	service, err := NewSchedulingQueryService(fixture, authorizer, serviceRepositoryAuthorities{authority: authority, found: true})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RepositoryRun(context.Background(), requesterForUser(operator), authority.Repository, "own-run")
	if err != nil || repository.RunID != "own-run" || !repository.WaitingForCapacity || fixture.lastScopes.HasController() {
		t.Fatalf("repository=%+v scopes=%+v err=%v", repository, fixture.lastScopes, err)
	}
	raw, _ := json.Marshal(repository)
	if strings.Contains(string(raw), "sibling") || strings.Contains(string(raw), "effective_capacity") || strings.Contains(string(raw), `"in_use"`) || strings.Contains(string(raw), "heavy_permit") {
		t.Fatalf("repository projection disclosed controller or sibling state: %s", raw)
	}
	controller, err := service.Controller(context.Background(), requesterForUser(operator), time.Now().UTC(), 10)
	if err != nil || controller.Capacity.EffectiveCapacity != 2 || len(controller.Runs) != 2 || !fixture.lastScopes.HasController() {
		t.Fatalf("controller=%+v scopes=%+v err=%v", controller, fixture.lastScopes, err)
	}
}
