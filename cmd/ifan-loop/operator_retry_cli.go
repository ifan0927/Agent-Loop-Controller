package main

import (
	"context"
	"errors"
	"flag"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func controllerRetry(args []string) error {
	flags := flag.NewFlagSet("controller retry", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	runID, remaining := splitLeadingRunID(args)
	if err := flags.Parse(remaining); err != nil {
		return err
	}
	if runID == "" && flags.NArg() == 1 {
		runID = flags.Arg(0)
	}
	if runID == "" || flags.NArg() != 0 || !requester.complete() {
		return errors.New("run ID and complete requester identity are required")
	}
	path, err := resolveConfigPath(*configPath)
	if err != nil {
		return err
	}
	loaded, err := bootstrap.Load(path)
	if err != nil {
		return err
	}
	store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	run, err := store.GetRun(context.Background(), runID)
	if err != nil {
		return application.ClassifyError(err)
	}
	if _, err := application.NewQueryService(store).Status(context.Background(), application.QueryInput{Requester: requester.value(), RunID: run.ID, Repository: run.Repository}); err != nil {
		return err
	}
	if err := validateProductionPersistedBinding(run, loaded.Registry); err != nil {
		return application.ClassifyError(err)
	}
	credentials, err := linearCredentialSource(loaded)
	if err != nil {
		return err
	}
	reader, err := linearadapter.New(loaded.Linear, credentials, nil)
	if err != nil {
		return err
	}
	local := newLocalController(store, loaded.Controller.CodexBinary, "")
	admission, err := application.NewLinearAdmissionService(reader, linearRegistryResolver{registry: loaded.Registry}, store, local)
	if err != nil {
		return err
	}
	service, err := application.NewOperatorRetryService(store, admission)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(loaded.Linear.HTTPTimeout)
	defer cancel()
	result, err := service.Retry(ctx, application.OperatorRetryCommand{Requester: requester.value(), RunID: run.ID})
	if err != nil {
		return err
	}
	return printJSON(result)
}
