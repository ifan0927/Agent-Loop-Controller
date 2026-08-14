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
	locator, located, err := configurationadapter.ReadLocator(path)
	if err != nil {
		return bootstrap.Bootstrap{}, err
	}
	if located {
		store, err := sqlitestore.OpenConfigurationAuthority(context.Background(), locator.DatabasePath, path, locator.DatabaseIdentity, false)
		if err != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration authority store is unavailable")
		}
		defer store.Close()
		if err := files.BindDatabaseIdentity(locator.DatabasePath, store.DatabaseIdentity()); err != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration authority locator conflicts")
		}
		authority, found, err := store.ConfigurationAuthority(context.Background())
		if err != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration authority store is unavailable")
		}
		service, err := application.NewConfigurationService(store, files, nil)
		if err != nil {
			return bootstrap.Bootstrap{}, err
		}
		if !found {
			// A crash may publish the exclusive locator after baseline binding
			// preparation but before generation adoption. The locator still fixes
			// both paths, so only the matching live configuration may finish it.
			loaded, loadErr := bootstrap.Load(path)
			if loadErr != nil || loaded.Controller.DatabasePath != locator.DatabasePath {
				return bootstrap.Bootstrap{}, errors.New("configuration authority locator conflicts")
			}
			adopted, adoptErr := service.Initialize(context.Background())
			if adoptErr != nil || adopted.Desired.Digest != loaded.Digest {
				return bootstrap.Bootstrap{}, errors.New("configuration baseline conflicts")
			}
			return loaded, nil
		}
		if authority.CanonicalConfigPath != path || authority.DatabasePath != locator.DatabasePath {
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
	baselineBinding, prepared, err := configurationadapter.ReadBaselineBinding(path)
	if err != nil {
		return bootstrap.Bootstrap{}, err
	}
	if prepared {
		payload, candidate, readErr := files.ReadLive()
		loaded, loadErr := bootstrap.ValidateBytes(path, payload)
		if readErr != nil || loadErr != nil || candidate.DatabasePath != baselineBinding.DatabasePath || candidate.Digest != baselineBinding.Digest || candidate.Size != baselineBinding.Size || candidate.SchemaVersion != baselineBinding.Schema || loaded.Controller.DatabasePath != baselineBinding.DatabasePath || loaded.Digest != baselineBinding.Digest {
			return bootstrap.Bootstrap{}, errors.New("configuration baseline binding conflicts")
		}
		store, openErr := sqlitestore.OpenConfigurationAuthority(context.Background(), baselineBinding.DatabasePath, path, baselineBinding.DatabaseIdentity, true)
		if openErr != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration authority store is unavailable")
		}
		defer store.Close()
		if err := files.BindDatabaseIdentity(baselineBinding.DatabasePath, store.DatabaseIdentity()); err != nil {
			return bootstrap.Bootstrap{}, errors.New("configuration baseline binding conflicts")
		}
		service, serviceErr := application.NewConfigurationService(store, files, nil)
		if serviceErr != nil {
			return bootstrap.Bootstrap{}, serviceErr
		}
		authority, initializeErr := service.Initialize(context.Background())
		if initializeErr != nil || authority.Desired.Digest != baselineBinding.Digest {
			return bootstrap.Bootstrap{}, errors.New("configuration baseline conflicts")
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
	if err := files.BindDatabaseIdentity(loaded.Controller.DatabasePath, store.DatabaseIdentity()); err != nil {
		return bootstrap.Bootstrap{}, err
	}
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

// openManagedConfigurationStore reopens only the exact private database
// identity published by successful authority initialization. Production
// callers must not fall back to a path-only SQLite open after proof.
func openManagedConfigurationStore(loaded bootstrap.Bootstrap) (*sqlitestore.Store, error) {
	locator, found, err := configurationadapter.ReadLocator(loaded.Path)
	if err != nil || !found || locator.DatabasePath != loaded.Controller.DatabasePath {
		return nil, errors.New("configuration authority locator conflicts")
	}
	store, err := sqlitestore.OpenConfigurationAuthority(context.Background(), locator.DatabasePath, loaded.Path, locator.DatabaseIdentity, false)
	if err != nil {
		return nil, errors.New("configuration authority store is unavailable")
	}
	return store, nil
}

func configuredConvergenceService(store *sqlitestore.Store, loaded bootstrap.Bootstrap, expectedProcessID int, supervisorRequired bool) (*application.ConfigurationService, error) {
	files, err := configurationadapter.NewFiles(loaded.Path)
	if err != nil {
		return nil, err
	}
	if err := files.BindDatabaseIdentity(loaded.Controller.DatabasePath, store.DatabaseIdentity()); err != nil {
		return nil, err
	}
	runtime, err := application.NewConfigurationRuntimeObservationService(workerHeartbeatReader{configPath: loaded.Path, expectedUID: os.Getuid(), expectedProcessID: expectedProcessID, supervisorProcessRequired: supervisorRequired}, workerProcessIdentityObserver{})
	if err != nil {
		return nil, err
	}
	return application.NewConfigurationService(store, files, runtime)
}
