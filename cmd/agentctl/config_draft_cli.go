package main

import (
	"context"
	"errors"
	"flag"
	"strconv"
	"strings"
	"time"

	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func managedConfigStatus(args []string) error {
	flags := flag.NewFlagSet("config status", flag.ContinueOnError)
	pathFlag := configPathFlag(flags)
	requester := addRequesterFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config status does not accept positional arguments")
	}
	if !requester.complete() {
		return errors.New("complete requester identity is required")
	}
	service, closeStore, err := openManagedDraftService(*pathFlag)
	if err != nil {
		return err
	}
	defer closeStore()
	status, err := service.Status(context.Background(), requester.value())
	if err != nil {
		return err
	}
	return printJSON(status)
}

func managedConfigDraft(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl config draft <open|show|set|validate|preview|apply|discard> [options]")
	}
	switch args[0] {
	case "open":
		return managedConfigDraftOpen(args[1:])
	case "show":
		return managedConfigDraftShow(args[1:])
	case "set":
		return managedConfigDraftSet(args[1:])
	case "validate":
		return managedConfigDraftValidate(args[1:])
	case "preview":
		return managedConfigDraftPreview(args[1:])
	case "apply":
		return managedConfigDraftApply(args[1:])
	case "discard":
		return managedConfigDraftDiscard(args[1:])
	default:
		return errors.New("usage: agentctl config draft <open|show|set|validate|preview|apply|discard> [options]")
	}
}

func managedConfigRollback(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl config rollback <sources|open> [options]")
	}
	switch args[0] {
	case "sources":
		return managedConfigRollbackSources(args[1:])
	case "open":
		return managedConfigRollbackOpen(args[1:])
	default:
		return errors.New("usage: agentctl config rollback <sources|open> [options]")
	}
}

func managedConfigRollbackSources(args []string) error {
	flags := flag.NewFlagSet("config rollback sources", flag.ContinueOnError)
	common := addManagedDraftCommonFlags(flags, false)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config rollback sources does not accept positional arguments")
	}
	if err := common.validate(false); err != nil {
		return err
	}
	service, closeStore, err := openManagedDraftService(*common.path)
	if err != nil {
		return err
	}
	defer closeStore()
	result, err := service.RollbackSources(context.Background(), common.requester.value())
	if err != nil {
		return err
	}
	return printJSON(result)
}

func managedConfigRollbackOpen(args []string) error {
	flags := flag.NewFlagSet("config rollback open", flag.ContinueOnError)
	common := addManagedDraftCommonFlags(flags, false)
	sourceGeneration := flags.Int64("source-generation-id", 0, "retained rollback source generation")
	sourceDigest := flags.String("source-digest", "", "exact retained rollback source digest")
	expectedGeneration := flags.Int64("expected-generation-id", 0, "expected desired configuration generation")
	expectedDigest := flags.String("expected-digest", "", "expected desired configuration digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config rollback open does not accept positional arguments")
	}
	if err := common.validate(false); err != nil {
		return err
	}
	if *sourceGeneration < 1 || *expectedGeneration < 1 || len(*sourceDigest) != 64 || len(*expectedDigest) != 64 {
		return errors.New("source and desired generation authority are required")
	}
	service, closeStore, err := openManagedDraftService(*common.path)
	if err != nil {
		return err
	}
	defer closeStore()
	draft, err := service.OpenRollback(context.Background(), application.ConfigurationRollbackOpenCommand{Requester: common.requester.value(), SourceGenerationID: *sourceGeneration, SourceDigest: *sourceDigest, ExpectedGenerationID: *expectedGeneration, ExpectedDigest: *expectedDigest})
	if err != nil {
		return err
	}
	return printJSON(draft)
}

type managedDraftCommonFlags struct {
	path      *string
	requester requesterFlags
	draftID   *string
	revision  *int64
}

func addManagedDraftCommonFlags(flags *flag.FlagSet, requireDraft bool) managedDraftCommonFlags {
	common := managedDraftCommonFlags{path: configPathFlag(flags), requester: addRequesterFlags(flags)}
	if requireDraft {
		common.draftID = flags.String("draft-id", "", "configuration draft ID")
		common.revision = flags.Int64("revision", 0, "exact configuration draft revision")
	}
	return common
}

func (f managedDraftCommonFlags) validate(requireDraft bool) error {
	if !f.requester.complete() {
		return errors.New("complete requester identity is required")
	}
	if requireDraft && (strings.TrimSpace(*f.draftID) == "" || *f.revision < 1) {
		return errors.New("draft ID and positive revision are required")
	}
	return nil
}

func managedConfigDraftOpen(args []string) error {
	flags := flag.NewFlagSet("config draft open", flag.ContinueOnError)
	common := addManagedDraftCommonFlags(flags, false)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config draft open does not accept positional arguments")
	}
	if err := common.validate(false); err != nil {
		return err
	}
	service, closeStore, err := openManagedDraftService(*common.path)
	if err != nil {
		return err
	}
	defer closeStore()
	draft, err := service.Open(context.Background(), common.requester.value())
	if err != nil {
		return err
	}
	return printJSON(draft)
}

func managedConfigDraftShow(args []string) error {
	return runManagedDraftRead("show", args, func(ctx context.Context, service *application.ConfigurationDraftService, command application.ConfigurationDraftCommand) (any, error) {
		return service.Show(ctx, command)
	})
}

func managedConfigDraftValidate(args []string) error {
	return runManagedDraftRead("validate", args, func(ctx context.Context, service *application.ConfigurationDraftService, command application.ConfigurationDraftCommand) (any, error) {
		return service.Validate(ctx, command)
	})
}

func managedConfigDraftPreview(args []string) error {
	return runManagedDraftRead("preview", args, func(ctx context.Context, service *application.ConfigurationDraftService, command application.ConfigurationDraftCommand) (any, error) {
		return service.Preview(ctx, command)
	})
}

func managedConfigDraftDiscard(args []string) error {
	return runManagedDraftRead("discard", args, func(ctx context.Context, service *application.ConfigurationDraftService, command application.ConfigurationDraftCommand) (any, error) {
		return service.Discard(ctx, command)
	})
}

func runManagedDraftRead(name string, args []string, operation func(context.Context, *application.ConfigurationDraftService, application.ConfigurationDraftCommand) (any, error)) error {
	flags := flag.NewFlagSet("config draft "+name, flag.ContinueOnError)
	common := addManagedDraftCommonFlags(flags, true)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config draft " + name + " does not accept positional arguments")
	}
	if err := common.validate(true); err != nil {
		return err
	}
	service, closeStore, err := openManagedDraftService(*common.path)
	if err != nil {
		return err
	}
	defer closeStore()
	result, err := operation(context.Background(), service, application.ConfigurationDraftCommand{Requester: common.requester.value(), DraftID: *common.draftID, Revision: *common.revision})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func managedConfigDraftSet(args []string) error {
	flags := flag.NewFlagSet("config draft set", flag.ContinueOnError)
	common := addManagedDraftCommonFlags(flags, true)
	runTimeout := flags.String("run-timeout", "", "controller run timeout duration")
	enabled := flags.String("automatic-admission-enabled", "", "automatic admission enabled boolean")
	poll := flags.String("admission-poll-interval", "", "automatic admission poll interval")
	delivery := flags.String("delivery-poll-interval", "", "delivery poll interval")
	leaseTTL := flags.String("scheduler-lease-ttl", "", "scheduler lease TTL")
	leaseRenewal := flags.String("scheduler-lease-renewal-interval", "", "scheduler lease renewal interval")
	maxCandidates := flags.String("max-candidates", "", "candidate scan maximum")
	maxPages := flags.String("max-pages", "", "candidate page maximum")
	heavyCapacity := flags.String("heavy-capacity", "", "local heavy-work capacity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config draft set does not accept positional arguments")
	}
	if err := common.validate(true); err != nil {
		return err
	}
	values := []struct {
		field       application.ConfigurationFieldID
		value, kind string
	}{
		{application.ConfigurationFieldRunTimeout, *runTimeout, "duration"},
		{application.ConfigurationFieldAdmissionEnabled, *enabled, "boolean"},
		{application.ConfigurationFieldAdmissionPollInterval, *poll, "duration"},
		{application.ConfigurationFieldDeliveryPollInterval, *delivery, "duration"},
		{application.ConfigurationFieldSchedulerLeaseTTL, *leaseTTL, "duration"},
		{application.ConfigurationFieldSchedulerLeaseRenewalInterval, *leaseRenewal, "duration"},
		{application.ConfigurationFieldAdmissionMaxCandidates, *maxCandidates, "integer"},
		{application.ConfigurationFieldAdmissionMaxPages, *maxPages, "integer"},
		{application.ConfigurationFieldAdmissionHeavyCapacity, *heavyCapacity, "integer"},
	}
	var edit application.ConfigurationEdit
	count := 0
	for _, value := range values {
		if value.value == "" {
			continue
		}
		count++
		edit.Field = value.field
		switch value.kind {
		case "boolean":
			parsed, err := strconv.ParseBool(value.value)
			if err != nil {
				return errors.New("automatic admission enabled must be true or false")
			}
			edit.Boolean = &parsed
		case "duration":
			parsed, err := time.ParseDuration(value.value)
			if err != nil {
				return errors.New("configuration duration is invalid")
			}
			duration := application.ConfigurationDuration(parsed)
			edit.Duration = &duration
		case "integer":
			parsed, err := strconv.Atoi(value.value)
			if err != nil {
				return errors.New("configuration integer is invalid")
			}
			edit.Integer = &parsed
		}
	}
	if count != 1 {
		return errors.New("exactly one typed configuration setting is required")
	}
	service, closeStore, err := openManagedDraftService(*common.path)
	if err != nil {
		return err
	}
	defer closeStore()
	draft, err := service.Edit(context.Background(), application.ConfigurationDraftEditCommand{Requester: common.requester.value(), DraftID: *common.draftID, Revision: *common.revision, Edit: edit})
	if err != nil {
		return err
	}
	return printJSON(draft)
}

func managedConfigDraftApply(args []string) error {
	flags := flag.NewFlagSet("config draft apply", flag.ContinueOnError)
	common := addManagedDraftCommonFlags(flags, true)
	previewDigest := flags.String("preview-digest", "", "exact semantic preview digest")
	expectedGeneration := flags.Int64("expected-generation-id", 0, "expected desired configuration generation")
	expectedDigest := flags.String("expected-digest", "", "expected desired configuration digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config draft apply does not accept positional arguments")
	}
	if err := common.validate(true); err != nil {
		return err
	}
	if *expectedGeneration < 1 || len(*previewDigest) != 64 || len(*expectedDigest) != 64 {
		return errors.New("preview and desired generation authority are required")
	}
	service, closeStore, err := openManagedDraftService(*common.path)
	if err != nil {
		return err
	}
	defer closeStore()
	result, err := service.Apply(context.Background(), application.ConfigurationDraftApplyCommand{Requester: common.requester.value(), DraftID: *common.draftID, Revision: *common.revision, PreviewDigest: *previewDigest, ExpectedGenerationID: *expectedGeneration, ExpectedDigest: *expectedDigest})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func openManagedDraftService(pathOverride string) (*application.ConfigurationDraftService, func(), error) {
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
	configurationService, err := configuredConvergenceService(store, loaded, 0, false)
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	document, err := configurationadapter.NewFiles(loaded.Path)
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	draftService, err := application.NewConfigurationDraftService(configurationService, store, document)
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	return draftService, closeStore, nil
}
