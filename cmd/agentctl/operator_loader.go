package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type operatorOverviewBatch struct {
	ObservedAt   time.Time
	Overview     application.RoutineOverviewProjection
	Repositories application.RoutineRepositoryPage
}

type operatorLoader interface {
	LoadOverview(context.Context, time.Time) (operatorOverviewBatch, error)
	LoadRuns(context.Context, application.RunLifecycleFilter, string, string, time.Time) (application.RoutineRunPage, error)
	LoadAttention(context.Context, string, time.Time) (application.RoutineAttentionPage, error)
	LoadRunDetail(context.Context, string, time.Time) (application.RoutineRunDetail, error)
	LoadRepositories(context.Context, string, time.Time) (application.RoutineRepositoryPage, error)
	LoadRepositoryDetail(context.Context, string, time.Time) (application.RoutineRepositoryDetail, error)
	EnableRepository(context.Context, string, string) (application.RepositoryMutationResult, error)
	AcceptDecision(context.Context, string, application.LegalDecisionInput) (application.OperationReceipt, error)
}

type operatorOverviewProjectionSource interface {
	Get(context.Context, application.Requester, time.Time) (application.RoutineOverviewProjection, error)
}

type operatorRepositoryProjectionSource interface {
	ListController(context.Context, application.ControllerReadAuthority, int, string, time.Time) (application.RoutineRepositoryPage, error)
}

type productionOperatorLoader struct {
	overview          operatorOverviewProjectionSource
	repositories      operatorRepositoryProjectionSource
	repositoryQueries *application.RoutineRepositoryQueryService
	runs              *application.RoutineRunQueryService
	attention         *application.RoutineAttentionQueryService
	requester         application.Requester
	reader            application.ControllerReadAuthority
	configPath        string
}

func (l *productionOperatorLoader) LoadOverview(ctx context.Context, observedAt time.Time) (operatorOverviewBatch, error) {
	observedAt = observedAt.UTC()
	overview, err := l.overview.Get(ctx, l.requester, observedAt)
	if err != nil {
		return operatorOverviewBatch{}, err
	}
	repositories, err := l.repositories.ListController(ctx, l.reader, application.RoutineQueryMaximumLimit, "", observedAt)
	if err != nil {
		return operatorOverviewBatch{}, err
	}
	return operatorOverviewBatch{ObservedAt: observedAt, Overview: overview, Repositories: repositories}, nil
}

func (l *productionOperatorLoader) LoadRuns(ctx context.Context, lifecycle application.RunLifecycleFilter, repository, cursor string, observedAt time.Time) (application.RoutineRunPage, error) {
	return l.runs.ListController(ctx, l.reader, application.RunSummaryQuery{Repository: repository, Lifecycle: lifecycle, Limit: application.RoutineQueryDefaultLimit, Cursor: cursor}, observedAt.UTC())
}

func (l *productionOperatorLoader) LoadRunDetail(ctx context.Context, runID string, observedAt time.Time) (application.RoutineRunDetail, error) {
	return l.runs.Detail(ctx, application.RunDetailQuery{Requester: l.requester, RunID: runID}, observedAt.UTC())
}

func (l *productionOperatorLoader) LoadAttention(ctx context.Context, cursor string, observedAt time.Time) (application.RoutineAttentionPage, error) {
	return l.attention.ListController(ctx, l.reader, application.RoutineAttentionQuery{Requester: l.requester, Scope: application.ScopeController, Limit: application.RoutineQueryDefaultLimit, Cursor: cursor}, observedAt.UTC())
}

func (l *productionOperatorLoader) LoadRepositories(ctx context.Context, cursor string, observedAt time.Time) (application.RoutineRepositoryPage, error) {
	return l.repositoryQueries.ListController(ctx, l.reader, application.RoutineQueryDefaultLimit, cursor, observedAt.UTC())
}

func (l *productionOperatorLoader) LoadRepositoryDetail(ctx context.Context, repository string, observedAt time.Time) (application.RoutineRepositoryDetail, error) {
	return l.repositoryQueries.Detail(ctx, l.requester, repository, observedAt.UTC())
}

func (l *productionOperatorLoader) EnableRepository(ctx context.Context, repository, requestID string) (application.RepositoryMutationResult, error) {
	loaded, _, store, err := loadOperatorConfigurationCurrent(ctx, l.configPath)
	if err != nil {
		return application.RepositoryMutationResult{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "repository authority is unavailable"}
	}
	defer store.Close()
	authority, present, err := store.ConfigurationAuthority(ctx)
	if err != nil || !present || authority.Desired.GenerationID < 1 || authority.Version < 1 || loaded.Digest != authority.Desired.Digest {
		return application.RepositoryMutationResult{}, &application.ServiceError{Category: application.ErrorConflict, Message: "repository authority changed"}
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return application.RepositoryMutationResult{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "repository authority is unavailable"}
	}
	repositories, err := application.NewRepositoryService(store, authorizer, loaded.Registry, application.RepositoryObservers{})
	if err != nil {
		return application.RepositoryMutationResult{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "repository authority is unavailable"}
	}
	expected := application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}
	return repositories.Enable(ctx, application.RepositoryMutationCommand{Requester: l.requester, Repository: repository, RequestID: requestID, ExpectedConfigurationAuthority: &expected})
}

func (l *productionOperatorLoader) AcceptDecision(ctx context.Context, offerID string, input application.LegalDecisionInput) (application.OperationReceipt, error) {
	loaded, _, store, err := loadOperatorConfigurationCurrent(ctx, l.configPath)
	if err != nil {
		return application.OperationReceipt{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "decision authority is unavailable"}
	}
	defer store.Close()
	operator := loaded.Controller.Operator
	currentRequester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	if currentRequester != l.requester {
		return application.OperationReceipt{}, &application.ServiceError{Category: application.ErrorConflict, Message: "decision authority changed"}
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return application.OperationReceipt{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "decision authority is unavailable"}
	}
	actions, err := application.NewLegalActionService(store, authorizer)
	if err != nil {
		return application.OperationReceipt{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "decision authority is unavailable"}
	}
	controller := newLocalController(store, loaded.Controller.CodexBinary, "")
	return actions.ExecuteDecision(ctx, application.LegalActionExecutionCommand{Requester: l.requester, OfferID: offerID}, input, controller)
}

type operatorComposition struct {
	loader operatorLoader
	store  *sqlitestore.Store
}

func (c *operatorComposition) Close() error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.Close()
}

// loadOperatorConfigurationCurrent requires a completed current authority.
// It deliberately does not fall back to the live file, a baseline anchor, a
// schema migration, or any managed composition helper that reconciles state.
func loadOperatorConfigurationCurrent(ctx context.Context, path string) (bootstrap.Bootstrap, *configurationadapter.Files, *sqlitestore.Store, error) {
	files, err := configurationadapter.NewFiles(path)
	if err != nil {
		return bootstrap.Bootstrap{}, nil, nil, errors.New("configuration authority is unavailable")
	}
	locator, found, err := configurationadapter.ReadLocator(path)
	if err != nil || !found || locator.ConfigPath != path {
		return bootstrap.Bootstrap{}, nil, nil, errors.New("configuration authority locator is unavailable")
	}
	store, err := sqlitestore.OpenConfigurationAuthorityCurrent(ctx, locator.DatabasePath, path, locator.DatabaseIdentity)
	if err != nil {
		return bootstrap.Bootstrap{}, nil, nil, errors.New("configuration authority store is unavailable")
	}
	closeWith := func(err error) (bootstrap.Bootstrap, *configurationadapter.Files, *sqlitestore.Store, error) {
		_ = store.Close()
		return bootstrap.Bootstrap{}, nil, nil, err
	}
	authority, present, err := store.ConfigurationAuthority(ctx)
	if err != nil || !present || authority.CanonicalConfigPath != path || authority.DatabasePath != locator.DatabasePath || authority.Desired.GenerationID < 1 || !authority.Desired.RawRetained {
		return closeWith(errors.New("configuration authority is unavailable"))
	}
	payload, err := files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil {
		return closeWith(errors.New("configuration desired evidence is unavailable"))
	}
	loaded, err := bootstrap.ValidateCurrentBytes(path, payload)
	if err != nil || loaded.Digest != authority.Desired.Digest || loaded.Controller.DatabasePath != locator.DatabasePath || !loaded.Controller.Operator.Equal(authority.Desired.ConfiguredOperator) {
		return closeWith(errors.New("configuration desired evidence conflicts"))
	}
	if err := files.BindDatabaseIdentity(locator.DatabasePath, locator.DatabaseIdentity); err != nil {
		return closeWith(errors.New("configuration authority locator conflicts"))
	}
	return loaded, files, store, nil
}

func composeOperator(ctx context.Context, configOverride string) (*operatorComposition, error) {
	path, err := resolveConfigPath(configOverride)
	if err != nil {
		return nil, err
	}
	loaded, files, store, err := loadOperatorConfigurationCurrent(ctx, path)
	if err != nil {
		return nil, err
	}
	closeWith := func(err error) (*operatorComposition, error) {
		_ = store.Close()
		return nil, err
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return closeWith(errors.New("configured operator authority is unavailable"))
	}
	operator := loaded.Controller.Operator
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	configured, err := authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return closeWith(errors.New("configured operator authority is unavailable"))
	}
	reader, err := authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		return closeWith(errors.New("controller read authority is unavailable"))
	}
	runtime, err := application.NewRuntimeObservationService(
		workerHeartbeatReader{configPath: loaded.Path, expectedUID: os.Getuid()},
		workerProcessIdentityObserver{},
		authorizer,
	)
	if err != nil {
		return closeWith(errors.New("runtime observation service is unavailable"))
	}
	configuration, err := application.NewConfigurationService(store, files, runtime)
	if err != nil {
		return closeWith(errors.New("configuration projection service is unavailable"))
	}
	settings, err := application.NewRoutineSettingsService(configuration, store, files, nil)
	if err != nil {
		return closeWith(errors.New("settings projection service is unavailable"))
	}
	overview, err := application.NewRoutineOverviewService(store, authorizer, runtime, settings)
	if err != nil {
		return closeWith(errors.New("overview projection service is unavailable"))
	}
	repositoryService, err := application.NewRepositoryService(store, authorizer, loaded.Registry, application.RepositoryObservers{})
	if err != nil {
		return closeWith(errors.New("repository projection service is unavailable"))
	}
	repositories, err := application.NewRoutineRepositoryQueryService(repositoryService, store)
	if err != nil {
		return closeWith(errors.New("repository projection service is unavailable"))
	}
	runs, err := application.NewRoutineRunQueryService(store, authorizer)
	if err != nil {
		return closeWith(errors.New("run projection service is unavailable"))
	}
	attention, err := application.NewRoutineAttentionQueryService(store, store, authorizer)
	if err != nil {
		return closeWith(errors.New("attention projection service is unavailable"))
	}
	return &operatorComposition{
		loader: &productionOperatorLoader{overview: overview, repositories: repositories, repositoryQueries: repositories, runs: runs, attention: attention, requester: requester, reader: reader, configPath: path},
		store:  store,
	}, nil
}
