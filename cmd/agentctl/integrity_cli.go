package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	"github.com/google/uuid"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func controllerIntegrity(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl controller integrity <status|findings|recheck> [options]")
	}
	switch args[0] {
	case "status":
		return controllerIntegrityStatus(args[1:])
	case "findings":
		return controllerIntegrityFindings(args[1:])
	case "recheck":
		return controllerIntegrityRecheck(args[1:])
	default:
		return errors.New("usage: agentctl controller integrity <status|findings|recheck> [options]")
	}
}

func controllerIntegrityStatus(args []string) error {
	flags := flag.NewFlagSet("controller integrity status", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !requester.complete() {
		return errors.New("complete requester identity is required")
	}
	query, _, store, err := composeIntegrityServices(*configPath, requester.value())
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := query.Summary(context.Background(), requester.value())
	if err != nil {
		return err
	}
	return printJSON(result)
}

func controllerIntegrityFindings(args []string) error {
	flags := flag.NewFlagSet("controller integrity findings", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	family := flags.String("family", "", "closed integrity family filter")
	scope := flags.String("scope", "", "public scope filter")
	target := flags.String("target", "", "exact public target filter")
	limit := flags.Int("limit", application.IntegrityDefaultLimit, "maximum findings to return")
	cursor := flags.String("cursor", "", "opaque findings cursor")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !requester.complete() {
		return errors.New("complete requester identity is required")
	}
	query, _, store, err := composeIntegrityServices(*configPath, requester.value())
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := query.Findings(context.Background(), application.IntegrityFindingQuery{Requester: requester.value(), Family: application.IntegrityFamily(strings.TrimSpace(*family)), Scope: application.AuthorityScopeKind(strings.TrimSpace(*scope)), TargetID: strings.TrimSpace(*target), Limit: *limit, Cursor: *cursor})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func controllerIntegrityRecheck(args []string) error {
	flags := flag.NewFlagSet("controller integrity recheck", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	requestID := flags.String("request-id", "", "stable caller request identity used for exact replay")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !requester.complete() || strings.TrimSpace(*requestID) == "" {
		return errors.New("complete requester identity and --request-id are required")
	}
	_, recheck, store, err := composeIntegrityServices(*configPath, requester.value())
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := recheck.Recheck(context.Background(), application.IntegrityRecheckCommand{Requester: requester.value(), RequestID: *requestID, Owner: "integrity-cli:" + uuid.NewString()})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func composeIntegrityServices(configOverride string, requester application.Requester) (*application.IntegrityQueryService, *application.IntegrityRecheckService, *sqlitestore.Store, error) {
	path, err := resolveConfigPath(configOverride)
	if err != nil {
		return nil, nil, nil, err
	}
	loaded, err := loadManagedConfiguration(path)
	if err != nil {
		return nil, nil, nil, err
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := authorizer.ResolveConfiguredRequester(requester); err != nil {
		return nil, nil, nil, err
	}
	store, err := openManagedConfigurationStore(loaded)
	if err != nil {
		return nil, nil, nil, err
	}
	closeWith := func(err error) (*application.IntegrityQueryService, *application.IntegrityRecheckService, *sqlitestore.Store, error) {
		_ = store.Close()
		return nil, nil, nil, err
	}
	query, err := application.NewIntegrityQueryService(store, authorizer)
	if err != nil {
		return closeWith(err)
	}
	maintenance, err := application.NewIntegrityMaintenanceService(store)
	if err != nil {
		return closeWith(err)
	}
	recheck, err := application.NewIntegrityRecheckService(store, authorizer, maintenance)
	if err != nil {
		return closeWith(err)
	}
	return query, recheck, store, nil
}
