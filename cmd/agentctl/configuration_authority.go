package main

import (
	"context"
	"errors"
	"os"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

// loadManagedConfiguration establishes or reopens Controller-owned generation
// authority before production composition. Once the trusted locator exists,
// desired raw evidence and SQLite binding win over an edited live file for
// composition; the live mismatch remains visible to the convergence gate.
func loadManagedConfiguration(path string) (bootstrap.Bootstrap, error) {
	files, err := configurationadapter.NewFiles(path)
	if err != nil {
		return bootstrap.Bootstrap{}, errors.New("configuration authority is unavailable")
	}
	_, locatedDatabase, located, err := configurationadapter.ReadLocator(path)
	if err != nil {
		return bootstrap.Bootstrap{}, err
	}
	if located {
		store, err := sqlitestore.Open(locatedDatabase)
		if err != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration authority store is unavailable")
		}
		defer store.Close()
		authority, found, err := store.ConfigurationAuthority(context.Background())
		if err != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration authority store is unavailable")
		}
		service, err := application.NewConfigurationService(store, files, nil)
		if err != nil {
			return bootstrap.Bootstrap{}, err
		}
		if !found {
			// A crash may publish the exclusive locator immediately before the
			// baseline transaction. The locator still fixes both paths, so only
			// the matching live configuration may finish that first adoption.
			loaded, loadErr := bootstrap.Load(path)
			if loadErr != nil || loaded.Controller.DatabasePath != locatedDatabase {
				return bootstrap.Bootstrap{}, errors.New("configuration authority locator conflicts")
			}
			adopted, adoptErr := service.Initialize(context.Background())
			if adoptErr != nil || adopted.Desired.Digest != loaded.Digest {
				return bootstrap.Bootstrap{}, errors.New("configuration baseline conflicts")
			}
			return loaded, nil
		}
		if authority.CanonicalConfigPath != path || authority.DatabasePath != locatedDatabase {
			return bootstrap.Bootstrap{}, errors.New("configuration authority locator conflicts")
		}
		// Reconciliation may deliberately return conflict after durably marking
		// ambiguous evidence. The retained desired generation still supplies the
		// only safe composition input, while the gate remains closed.
		_, _ = service.Initialize(context.Background())
		authority, found, err = store.ConfigurationAuthority(context.Background())
		if err != nil || !found {
			return bootstrap.Bootstrap{}, errors.New("configuration authority is unavailable")
		}
		payload, err := files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
		if err != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration desired evidence is unavailable")
		}
		loaded, err := bootstrap.ValidateBytes(path, payload)
		if err != nil || loaded.Digest != authority.Desired.Digest || loaded.Controller.DatabasePath != authority.DatabasePath {
			return bootstrap.Bootstrap{}, errors.New("configuration desired evidence conflicts")
		}
		return loaded, nil
	}

	loaded, err := bootstrap.Load(path)
	if err != nil {
		return bootstrap.Bootstrap{}, err
	}
	store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
	if err != nil {
		return bootstrap.Bootstrap{}, err
	}
	defer store.Close()
	service, err := application.NewConfigurationService(store, files, nil)
	if err != nil {
		return bootstrap.Bootstrap{}, err
	}
	authority, err := service.Initialize(context.Background())
	if err != nil {
		return bootstrap.Bootstrap{}, err
	}
	if authority.Desired.Digest != loaded.Digest || authority.DatabasePath != loaded.Controller.DatabasePath {
		return bootstrap.Bootstrap{}, errors.New("configuration baseline conflicts")
	}
	return loaded, nil
}

func configuredConvergenceService(store *sqlitestore.Store, loaded bootstrap.Bootstrap, expectedProcessID int, supervisorRequired bool) (*application.ConfigurationService, error) {
	files, err := configurationadapter.NewFiles(loaded.Path)
	if err != nil {
		return nil, err
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return application.NewConfigurationService(store, files, nil)
	}
	runtime, err := application.NewRuntimeObservationService(workerHeartbeatReader{configPath: loaded.Path, expectedUID: os.Getuid(), expectedProcessID: expectedProcessID, supervisorProcessRequired: supervisorRequired}, workerProcessIdentityObserver{}, authorizer)
	if err != nil {
		return nil, err
	}
	return application.NewConfigurationService(store, files, runtime)
}
