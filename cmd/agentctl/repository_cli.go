package main

import (
	"context"
	"errors"
	"flag"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/githubapp"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func repositoryCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl repository <list|inspect|recheck|enable|disable|remove> [options]")
	}
	switch args[0] {
	case "list":
		return repositoryList(args[1:])
	case "inspect":
		return repositoryInspect(args[1:])
	case "recheck", "enable", "disable":
		return repositoryMutate(args[0], args[1:])
	case "remove":
		return repositoryRemove(args[1:])
	default:
		return errors.New("usage: agentctl repository <list|inspect|recheck|enable|disable|remove> [options]")
	}
}

func repositoryRemove(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl repository remove <open|show|validate|preview|apply|discard> [options]")
	}
	switch args[0] {
	case "open":
		return repositoryRemoveOpen(args[1:])
	case "show", "validate", "preview", "discard":
		return repositoryRemoveRead(args[0], args[1:])
	case "apply":
		return repositoryRemoveApply(args[1:])
	default:
		return errors.New("usage: agentctl repository remove <open|show|validate|preview|apply|discard> [options]")
	}
}

func repositoryRemoveOpen(args []string) error {
	repository, remaining := splitLeadingRunID(args)
	flags := flag.NewFlagSet("repository remove open", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	if err := flags.Parse(remaining); err != nil {
		return err
	}
	if repository == "" && flags.NArg() == 1 {
		repository = flags.Arg(0)
	}
	if repository == "" || flags.NArg() != 0 || !requester.complete() {
		return errors.New("one repository and complete requester identity are required")
	}
	service, store, err := composeRepositoryRemovalService(*configPath)
	if err != nil {
		return err
	}
	defer store.Close()
	draft, err := service.Open(context.Background(), application.RepositoryRemovalOpenCommand{Requester: requester.value(), Repository: repository})
	if err != nil {
		return err
	}
	return printJSON(draft)
}

func repositoryRemoveRead(command string, args []string) error {
	flags := flag.NewFlagSet("repository remove "+command, flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	draftID := flags.String("draft-id", "", "repository removal draft ID")
	revision := flags.Int64("revision", 0, "exact immutable removal draft revision")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !requester.complete() || strings.TrimSpace(*draftID) == "" || *revision != 1 {
		return errors.New("draft ID, revision 1, and complete requester identity are required")
	}
	service, store, err := composeRepositoryRemovalService(*configPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	common := application.RepositoryRemovalCommand{Requester: requester.value(), DraftID: *draftID, Revision: *revision}
	var result any
	switch command {
	case "show":
		result, err = service.Show(ctx, common)
	case "validate":
		result, err = service.Validate(ctx, common)
	case "preview":
		result, err = service.Preview(ctx, common)
	case "discard":
		result, err = service.Discard(ctx, common)
	}
	if err != nil {
		return err
	}
	return printJSON(result)
}

func repositoryRemoveApply(args []string) error {
	flags := flag.NewFlagSet("repository remove apply", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	draftID := flags.String("draft-id", "", "repository removal draft ID")
	revision := flags.Int64("revision", 0, "exact immutable removal draft revision")
	previewDigest := flags.String("preview-digest", "", "exact semantic preview digest")
	incarnationID := flags.String("incarnation-id", "", "exact repository incarnation identity")
	lifecycleVersion := flags.Int64("lifecycle-version", 0, "exact repository lifecycle version")
	profileID := flags.String("profile-id", "", "exact repository profile identity")
	bindingDigest := flags.String("repository-binding-digest", "", "exact repository profile binding digest")
	expectedGeneration := flags.Int64("expected-generation-id", 0, "expected desired configuration generation")
	expectedDigest := flags.String("expected-digest", "", "expected desired configuration digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !requester.complete() || strings.TrimSpace(*draftID) == "" || *revision != 1 || len(*previewDigest) != 64 || strings.TrimSpace(*incarnationID) == "" || *lifecycleVersion < 1 || strings.TrimSpace(*profileID) == "" || len(*bindingDigest) != 64 || *expectedGeneration < 1 || len(*expectedDigest) != 64 {
		return errors.New("complete removal draft, preview, repository, lifecycle, profile, binding, generation, and requester authority are required")
	}
	service, store, err := composeRepositoryRemovalService(*configPath)
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := service.Apply(context.Background(), application.RepositoryRemovalApplyCommand{Requester: requester.value(), DraftID: *draftID, Revision: *revision, PreviewDigest: *previewDigest, IncarnationID: *incarnationID, LifecycleVersion: *lifecycleVersion, ProfileID: *profileID, RepositoryBindingDigest: *bindingDigest, ExpectedGenerationID: *expectedGeneration, ExpectedDigest: *expectedDigest})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func composeRepositoryRemovalService(configOverride string) (*application.RepositoryRemovalService, managedStore, error) {
	path, err := resolveConfigPath(configOverride)
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
	closeWith := func(err error) (*application.RepositoryRemovalService, managedStore, error) {
		store.Close()
		return nil, nil, err
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return closeWith(err)
	}
	repositories, err := application.NewRepositoryService(store, authorizer, loaded.Registry, application.RepositoryObservers{})
	if err != nil {
		return closeWith(err)
	}
	configuration, err := configuredConvergenceService(store, loaded, 0, false)
	if err != nil {
		return closeWith(err)
	}
	document, err := configurationadapter.NewFiles(loaded.Path)
	if err != nil {
		return closeWith(err)
	}
	service, err := application.NewRepositoryRemovalService(configuration, repositories, store, document)
	if err != nil {
		return closeWith(err)
	}
	return service, store, nil
}

func repositoryList(args []string) error {
	flags := flag.NewFlagSet("repository list", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	limit := flags.Int("limit", 50, "maximum authorized repositories to return")
	cursor := flags.String("cursor", "", "opaque authorized collection cursor")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !requester.complete() {
		return errors.New("complete requester identity is required")
	}
	service, store, err := composeRepositoryService(*configPath, false)
	if err != nil {
		return err
	}
	defer store.Close()
	page, err := service.List(context.Background(), requester.value(), *limit, *cursor)
	if err != nil {
		return err
	}
	return printJSON(page)
}

func repositoryInspect(args []string) error {
	repository, remaining := splitLeadingRunID(args)
	flags := flag.NewFlagSet("repository inspect", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	if err := flags.Parse(remaining); err != nil {
		return err
	}
	if repository == "" && flags.NArg() == 1 {
		repository = flags.Arg(0)
	}
	if repository == "" || flags.NArg() != 0 || !requester.complete() {
		return errors.New("one repository and complete requester identity are required")
	}
	service, store, err := composeRepositoryService(*configPath, false)
	if err != nil {
		return err
	}
	defer store.Close()
	projection, err := service.Inspect(context.Background(), requester.value(), repository)
	if err != nil {
		return err
	}
	return printJSON(projection)
}

func repositoryMutate(command string, args []string) error {
	repository, remaining := splitLeadingRunID(args)
	flags := flag.NewFlagSet("repository "+command, flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	requestID := flags.String("request-id", "", "stable caller request identity used for exact replay")
	if err := flags.Parse(remaining); err != nil {
		return err
	}
	if repository == "" && flags.NArg() == 1 {
		repository = flags.Arg(0)
	}
	if repository == "" || flags.NArg() != 0 || !requester.complete() || strings.TrimSpace(*requestID) == "" {
		return errors.New("one repository, complete requester identity, and --request-id are required")
	}
	service, store, err := composeRepositoryService(*configPath, command == "recheck")
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	mutation := application.RepositoryMutationCommand{Requester: requester.value(), Repository: repository, RequestID: *requestID}
	var result application.RepositoryMutationResult
	switch command {
	case "recheck":
		result, err = service.Recheck(ctx, mutation)
	case "enable":
		result, err = service.Enable(ctx, mutation)
	case "disable":
		result, err = service.Disable(ctx, mutation)
	}
	if err != nil {
		return err
	}
	return printJSON(result)
}

func composeRepositoryService(configOverride string, observations bool) (*application.RepositoryService, managedStore, error) {
	path, err := resolveConfigPath(configOverride)
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
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	var observers application.RepositoryObservers
	if observations {
		convergence, convergenceErr := configuredConvergenceService(store, loaded, 0, false)
		if convergenceErr != nil {
			store.Close()
			return nil, nil, errors.New("configuration convergence authority is unavailable")
		}
		credentials, credentialErr := linearCredentialSource(loaded)
		if credentialErr != nil {
			store.Close()
			return nil, nil, credentialErr
		}
		linear, linearErr := linearadapter.New(loaded.Linear, credentials, nil)
		if linearErr != nil {
			store.Close()
			return nil, nil, linearErr
		}
		observers = application.RepositoryObservers{
			Configuration: repositoryConfigurationObserver{service: convergence, store: store},
			Git:           repositoryGitObserver{observer: gitadapter.ReadinessObserver{}},
			GitHub:        repositoryGitHubObserver{loaded: loaded},
			Linear:        linear,
			Verifier:      loaded.Registry,
		}
	}
	service, err := application.NewRepositoryService(store, authorizer, loaded.Registry, observers)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return service, store, nil
}

type managedStore interface {
	application.RepositoryLifecycleStore
	RepositoryConfigurationAuthority(context.Context) (application.ConfigurationAdmissionAuthority, error)
	ReconcileRepositoryRechecks(context.Context, time.Time) error
	Close() error
}

type repositoryConfigurationObserver struct {
	service *application.ConfigurationService
	store   managedStore
}

type repositoryGitObserver struct{ observer gitadapter.ReadinessObserver }

func (o repositoryGitObserver) ObserveRepositoryGit(ctx context.Context, profile application.LocalRepository) ([2]domain.RepositoryDimensionResult, error) {
	return o.observer.ObserveRepositoryGit(ctx, gitadapter.ReadinessProfile{ProfileDigest: profile.ProfileDigest, RepositoryBindingDigest: profile.RepositoryBindingDigest, SourcePath: profile.SourcePath, OriginPath: profile.OriginPath, BaseBranch: profile.BaseBranch})
}

func (o repositoryConfigurationObserver) ObserveRepositoryConfiguration(ctx context.Context, profile application.LocalRepository) (domain.RepositoryDimensionResult, application.ConfigurationAdmissionAuthority, error) {
	authority, err := o.store.RepositoryConfigurationAuthority(ctx)
	if err != nil {
		return domain.RepositoryDimensionResult{}, application.ConfigurationAdmissionAuthority{}, err
	}
	decision, err := o.service.CheckNewAdmission(ctx)
	if err != nil {
		return domain.RepositoryDimensionResult{}, application.ConfigurationAdmissionAuthority{}, err
	}
	status, reason := domain.RepositoryReady, "configuration_convergence_ready"
	if !decision.Allowed {
		status, reason = domain.RepositoryUnknown, "configuration_"+string(decision.Reason)
	}
	result := domain.RepositoryDimensionResult{Dimension: domain.ReadinessConfigurationConvergence, Status: status, ReasonCode: reason, Identity: authority.Digest, EvidenceDigest: application.ConfigurationEvidenceDigest("repository-configuration-readiness-v1", profile.ProfileDigest, profile.RepositoryBindingDigest, string(status), reason, authority.Digest), ObservedAt: time.Now().UTC()}
	return result, authority, nil
}

type repositoryGitHubObserver struct{ loaded bootstrap.Bootstrap }

func (o repositoryGitHubObserver) ObserveRepositoryGitHub(ctx context.Context, profile application.LocalRepository) ([2]domain.RepositoryDimensionResult, error) {
	configured, err := o.loaded.GitHubProfileForRepository(profile.CanonicalRepository)
	if err == nil {
		client, clientErr := githubapp.New(configured.Config, nil, nil)
		if clientErr == nil {
			return client.ObserveRepositoryGitHub(ctx, profile)
		}
	}
	now := time.Now().UTC()
	unknown := func(dimension domain.RepositoryReadinessDimension, reason string) domain.RepositoryDimensionResult {
		return domain.RepositoryDimensionResult{Dimension: dimension, Status: domain.RepositoryUnknown, ReasonCode: reason, EvidenceDigest: application.ConfigurationEvidenceDigest("repository-github-unavailable-v1", profile.ProfileDigest, profile.RepositoryBindingDigest, string(dimension), reason), ObservedAt: now}
	}
	return [2]domain.RepositoryDimensionResult{unknown(domain.ReadinessGitHubRepository, "github_observation_unavailable"), unknown(domain.ReadinessGitHubApp, "github_app_observation_unavailable")}, nil
}
