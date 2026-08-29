package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func managedConfigMigrate(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl config migrate <preview|apply> [options]")
	}
	switch args[0] {
	case "preview":
		return managedConfigMigratePreview(args[1:])
	case "apply":
		return managedConfigMigrateApply(args[1:])
	default:
		return errors.New("usage: agentctl config migrate <preview|apply> [options]")
	}
}

func managedConfigMigratePreview(args []string) error {
	flags := flag.NewFlagSet("config migrate preview", flag.ContinueOnError)
	path := configPathFlag(flags)
	requester := addRequesterFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config migrate preview does not accept positional arguments")
	}
	if !requester.complete() {
		return errors.New("complete requester identity is required")
	}
	service, closeStore, err := openManagedMigrationService(*path)
	if err != nil {
		return err
	}
	defer closeStore()
	preview, err := service.Preview(context.Background(), requester.value())
	if err != nil {
		return err
	}
	return printJSON(preview)
}

func managedConfigMigrateApply(args []string) error {
	flags := flag.NewFlagSet("config migrate apply", flag.ContinueOnError)
	path := configPathFlag(flags)
	requester := addRequesterFlags(flags)
	requestID := flags.String("request-id", "", "stable caller request identity used for exact replay")
	expectedGeneration := flags.Int64("expected-generation-id", 0, "expected desired configuration generation")
	expectedDigest := flags.String("expected-digest", "", "expected desired configuration digest")
	expectedAuthorityVersion := flags.Int64("expected-authority-version", 0, "expected configuration authority version")
	sourceSchema := flags.Int("source-schema-version", 0, "previewed legacy configuration schema")
	targetSchema := flags.Int("target-schema-version", 0, "previewed target configuration schema")
	candidateDigest := flags.String("candidate-digest", "", "previewed current-schema candidate digest")
	migrationDigest := flags.String("migration-digest", "", "previewed semantic migration digest")
	previewDigest := flags.String("preview-digest", "", "exact migration preview digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config migrate apply does not accept positional arguments")
	}
	if !requester.complete() || strings.TrimSpace(*requestID) == "" || *expectedGeneration < 1 || len(*expectedDigest) != 64 || *expectedAuthorityVersion < 1 || *sourceSchema < 2 || *sourceSchema > 4 || *targetSchema != application.ConfigurationMigrationTargetSchemaVersion || len(*candidateDigest) != 64 || len(*migrationDigest) != 64 || len(*previewDigest) != 64 {
		return errors.New("complete configuration migration authority is required")
	}
	service, closeStore, err := openManagedMigrationService(*path)
	if err != nil {
		return err
	}
	defer closeStore()
	result, err := service.Apply(context.Background(), application.ConfigurationMigrationApplyCommand{
		Requester: requester.value(), RequestID: *requestID, ExpectedGenerationID: *expectedGeneration, ExpectedDigest: *expectedDigest,
		ExpectedAuthorityVersion: *expectedAuthorityVersion, SourceSchemaVersion: *sourceSchema, TargetSchemaVersion: *targetSchema,
		CandidateDigest: *candidateDigest, MigrationDigest: *migrationDigest, PreviewDigest: *previewDigest,
	})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func openManagedMigrationService(pathOverride string) (*application.ConfigurationMigrationService, func(), error) {
	path, err := resolveConfigPath(pathOverride)
	if err != nil {
		return nil, nil, err
	}
	loaded, err := loadManagedConfiguration(path)
	if err != nil {
		return nil, nil, err
	}
	store, err := openManagedConfigurationStore(loaded)
	if err != nil {
		return nil, nil, err
	}
	closeStore := func() { _ = store.Close() }
	configuration, err := configuredConvergenceService(store, loaded, 0, false)
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	document, err := configurationadapter.NewFiles(loaded.Path)
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	service, err := application.NewConfigurationMigrationService(configuration, document)
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	return service, closeStore, nil
}
