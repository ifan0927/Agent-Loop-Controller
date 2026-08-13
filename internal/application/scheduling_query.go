package application

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type SchedulingQueryStore interface {
	Capacity(context.Context, time.Time) (CapacityProjection, error)
	ListRunScopeAuthorities(context.Context) ([]RunScopeAuthority, error)
	ListSchedulingRuns(context.Context, AuthorizedScopeSet, int) ([]SchedulingRun, error)
	GetSchedulingRun(context.Context, AuthorizedScopeSet, string) (SchedulingRun, error)
}

type ControllerSchedulingProjection struct {
	Capacity CapacityProjection `json:"capacity"`
	Runs     []SchedulingRun    `json:"runs"`
}

// RepositorySchedulingProjection deliberately omits controller capacity,
// sibling counts, repository identities, and heavy-permit ownership.
type RepositorySchedulingProjection struct {
	RunID              string       `json:"run_id"`
	State              domain.State `json:"state"`
	SupervisorState    string       `json:"supervisor_state"`
	WaitingForCapacity bool         `json:"waiting_for_capacity"`
	Quarantined        bool         `json:"quarantined"`
}

type SchedulingQueryService struct {
	store        SchedulingQueryStore
	authorizer   *AuthorizationService
	repositories RepositoryAuthoritySource
}

func NewSchedulingQueryService(store SchedulingQueryStore, authorizer *AuthorizationService, repositories RepositoryAuthoritySource) (*SchedulingQueryService, error) {
	if store == nil || authorizer == nil || repositories == nil {
		return nil, errors.New("scheduling query dependencies are required")
	}
	return &SchedulingQueryService{store: store, authorizer: authorizer, repositories: repositories}, nil
}

func (s *SchedulingQueryService) Controller(ctx context.Context, requester Requester, now time.Time, limit int) (ControllerSchedulingProjection, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return ControllerSchedulingProjection{}, hiddenTargetError()
	}
	authorities, err := s.store.ListRunScopeAuthorities(ctx)
	if err != nil {
		return ControllerSchedulingProjection{}, classifyServiceError(err)
	}
	scopes, err := s.authorizer.ControllerRunScopes(configured, authorities)
	if err != nil {
		return ControllerSchedulingProjection{}, hiddenTargetError()
	}
	capacity, err := s.store.Capacity(ctx, now)
	if err != nil {
		return ControllerSchedulingProjection{}, classifyServiceError(err)
	}
	runs, err := s.store.ListSchedulingRuns(ctx, scopes, limit)
	if err != nil {
		return ControllerSchedulingProjection{}, classifyServiceError(err)
	}
	return ControllerSchedulingProjection{Capacity: capacity, Runs: runs}, nil
}

func (s *SchedulingQueryService) RepositoryRun(ctx context.Context, requester Requester, repository, runID string) (RepositorySchedulingProjection, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RepositorySchedulingProjection{}, hiddenTargetError()
	}
	authority, found, err := s.repositories.RepositoryAuthority(ctx, repository)
	if err != nil {
		return RepositorySchedulingProjection{}, classifyServiceError(err)
	}
	if !found || authority.Repository != repository {
		return RepositorySchedulingProjection{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.RepositoryScopes(configured, authority)
	if err != nil {
		return RepositorySchedulingProjection{}, hiddenTargetError()
	}
	return s.repositoryRun(ctx, scopes, runID)
}

func (s *SchedulingQueryService) FrozenRun(ctx context.Context, requester Requester, run Run) (RepositorySchedulingProjection, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RepositorySchedulingProjection{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.ConfiguredFrozenRunScopes(configured, run)
	if err != nil {
		return RepositorySchedulingProjection{}, hiddenTargetError()
	}
	return s.repositoryRun(ctx, scopes, run.ID)
}

func (s *SchedulingQueryService) repositoryRun(ctx context.Context, scopes AuthorizedScopeSet, runID string) (RepositorySchedulingProjection, error) {
	if runID == "" {
		return RepositorySchedulingProjection{}, serviceError(ErrorInvalidInput, "run is required", nil)
	}
	run, err := s.store.GetSchedulingRun(ctx, scopes, runID)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return RepositorySchedulingProjection{}, hiddenTargetError()
		}
		return RepositorySchedulingProjection{}, classifyServiceError(err)
	}
	if run.RunID != runID || !scopes.AllowsRun(run.RunID, run.RepositoryBindingDigest) {
		return RepositorySchedulingProjection{}, serviceError(ErrorInternal, "scheduling run scope mismatch", nil)
	}
	return RepositorySchedulingProjection{RunID: run.RunID, State: run.State, SupervisorState: run.SupervisorState, WaitingForCapacity: run.WaitingForCapacity, Quarantined: run.Quarantined}, nil
}
