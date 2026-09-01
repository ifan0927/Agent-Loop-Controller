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

type operatorOverviewLoader interface {
	Load(context.Context, time.Time) (operatorOverviewBatch, error)
}

type operatorOverviewProjectionSource interface {
	Get(context.Context, application.Requester, time.Time) (application.RoutineOverviewProjection, error)
}

type operatorRepositoryProjectionSource interface {
	List(context.Context, application.Requester, int, string, time.Time) (application.RoutineRepositoryPage, error)
}

type productionOperatorOverviewLoader struct {
	overview     operatorOverviewProjectionSource
	repositories operatorRepositoryProjectionSource
	requester    application.Requester
}

func (l *productionOperatorOverviewLoader) Load(ctx context.Context, observedAt time.Time) (operatorOverviewBatch, error) {
	observedAt = observedAt.UTC()
	overview, err := l.overview.Get(ctx, l.requester, observedAt)
	if err != nil {
		return operatorOverviewBatch{}, err
	}
	repositories, err := l.repositories.List(ctx, l.requester, application.RoutineQueryMaximumLimit, "", observedAt)
	if err != nil {
		return operatorOverviewBatch{}, err
	}
	return operatorOverviewBatch{ObservedAt: observedAt, Overview: overview, Repositories: repositories}, nil
}

type operatorComposition struct {
	loader operatorOverviewLoader
	store  *sqlitestore.Store
}

func (c *operatorComposition) Close() error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.Close()
}

// loadOperatorConfigurationReadOnly requires a completed current authority.
// It deliberately does not fall back to the live file, a baseline anchor, a
// schema migration, or any managed composition helper that reconciles state.
func loadOperatorConfigurationReadOnly(ctx context.Context, path string) (bootstrap.Bootstrap, *configurationadapter.Files, *sqlitestore.Store, error) {
	files, err := configurationadapter.NewFiles(path)
	if err != nil {
		return bootstrap.Bootstrap{}, nil, nil, errors.New("configuration authority is unavailable")
	}
	locator, found, err := configurationadapter.ReadLocator(path)
	if err != nil || !found || locator.ConfigPath != path {
		return bootstrap.Bootstrap{}, nil, nil, errors.New("configuration authority locator is unavailable")
	}
	store, err := sqlitestore.OpenConfigurationAuthorityReadOnly(ctx, locator.DatabasePath, path, locator.DatabaseIdentity)
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

func composeOperatorOverview(ctx context.Context, configOverride string) (*operatorComposition, error) {
	path, err := resolveConfigPath(configOverride)
	if err != nil {
		return nil, err
	}
	loaded, files, store, err := loadOperatorConfigurationReadOnly(ctx, path)
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
	operator := loaded.Controller.Operator
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	return &operatorComposition{
		loader: &productionOperatorOverviewLoader{overview: overview, repositories: repositories, requester: requester},
		store:  store,
	}, nil
}
