package main

import (
	"context"
	"errors"
	"flag"
	"sort"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func onboardingCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl onboarding <existing|empty|preflight|preview|start|show|cancel|resume> [options]")
	}
	switch args[0] {
	case "existing":
		if len(args) < 2 || args[1] != "open" {
			return errors.New("usage: agentctl onboarding existing open [options]")
		}
		return onboardingOpen(args[2:])
	case "empty":
		if len(args) < 2 || args[1] != "open" {
			return errors.New("usage: agentctl onboarding empty open [options]")
		}
		return onboardingEmptyOpen(args[2:])
	case "preflight", "preview", "show", "cancel", "resume":
		return onboardingReadOrMutate(args[0], args[1:])
	case "start":
		return onboardingStart(args[1:])
	default:
		return errors.New("usage: agentctl onboarding <existing|empty|preflight|preview|start|show|cancel|resume> [options]")
	}
}

func onboardingEmptyOpen(args []string) error {
	flags := flag.NewFlagSet("onboarding empty open", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	requestID := flags.String("request-id", "", "stable caller request identity")
	repository := flags.String("repository", "", "expected canonical owner/repository")
	githubProfile := flags.String("github-app-profile", "", "existing GitHub App profile reference")
	baseBranch := flags.String("base-branch", "", "selected base branch")
	verifiers := flags.String("verifier-ids", "", "comma-separated existing verifier IDs")
	linearSlug := flags.String("linear-label-slug", "", "repository label slug without repo: prefix")
	ciSlowThreshold := flags.Duration("ci-slow-threshold", 0, "optional CI slow threshold")
	if err := flags.Parse(args); err != nil {
		return err
	}
	verifierIDs := splitVerifierIDs(*verifiers)
	if flags.NArg() != 0 || !requester.complete() || strings.TrimSpace(*requestID) == "" || strings.TrimSpace(*repository) == "" || strings.TrimSpace(*githubProfile) == "" || strings.TrimSpace(*baseBranch) == "" || len(verifierIDs) == 0 || strings.TrimSpace(*linearSlug) == "" {
		return errors.New("complete empty-repository input and requester authority are required")
	}
	service, store, err := composeOnboardingCLIService(*configPath, false)
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := service.OpenEmpty(context.Background(), application.EmptyRepositoryOnboardingOpenCommand{Requester: requester.value(), RequestID: *requestID, Input: domain.EmptyRepositoryOnboardingInput{CanonicalRepository: strings.ToLower(*repository), GitHubAppProfileRef: *githubProfile, BaseBranch: *baseBranch, VerifierIDs: verifierIDs, LinearLabelSlug: *linearSlug, CISlowThreshold: *ciSlowThreshold}})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func onboardingOpen(args []string) error {
	flags := flag.NewFlagSet("onboarding existing open", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	requestID := flags.String("request-id", "", "stable caller request identity")
	sourcePath := flags.String("source-path", "", "exact existing checkout path")
	repository := flags.String("repository", "", "expected canonical owner/repository")
	githubProfile := flags.String("github-app-profile", "", "existing GitHub App profile reference")
	baseBranch := flags.String("base-branch", "", "selected base branch")
	verifiers := flags.String("verifier-ids", "", "comma-separated existing verifier IDs")
	linearSlug := flags.String("linear-label-slug", "", "repository label slug without repo: prefix")
	ciSlowThreshold := flags.Duration("ci-slow-threshold", 0, "optional CI slow threshold")
	if err := flags.Parse(args); err != nil {
		return err
	}
	verifierIDs := splitVerifierIDs(*verifiers)
	if flags.NArg() != 0 || !requester.complete() || strings.TrimSpace(*requestID) == "" || strings.TrimSpace(*sourcePath) == "" || strings.TrimSpace(*repository) == "" || strings.TrimSpace(*githubProfile) == "" || strings.TrimSpace(*baseBranch) == "" || len(verifierIDs) == 0 || strings.TrimSpace(*linearSlug) == "" {
		return errors.New("complete existing-checkout input and requester authority are required")
	}
	service, store, err := composeOnboardingCLIService(*configPath, false)
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := service.Open(context.Background(), application.OnboardingOpenCommand{Requester: requester.value(), RequestID: *requestID, Input: domain.ExistingCheckoutOnboardingInput{SourcePath: *sourcePath, CanonicalRepository: strings.ToLower(*repository), GitHubAppProfileRef: *githubProfile, BaseBranch: *baseBranch, VerifierIDs: verifierIDs, LinearLabelSlug: *linearSlug, CISlowThreshold: *ciSlowThreshold}})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func onboardingReadOrMutate(command string, args []string) error {
	onboardingID, remaining := splitLeadingRunID(args)
	flags := flag.NewFlagSet("onboarding "+command, flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	if err := flags.Parse(remaining); err != nil {
		return err
	}
	if onboardingID == "" && flags.NArg() == 1 {
		onboardingID = flags.Arg(0)
	}
	if onboardingID == "" || flags.NArg() != 0 || !requester.complete() {
		return errors.New("one onboarding ID and complete requester identity are required")
	}
	service, store, err := composeOnboardingCLIService(*configPath, false)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	common := application.OnboardingCommand{Requester: requester.value(), OnboardingID: onboardingID}
	var result any
	switch command {
	case "preflight":
		result, err = service.Preflight(ctx, common)
	case "preview":
		result, err = service.Preview(ctx, common)
	case "show":
		result, err = service.Show(ctx, common)
	case "cancel":
		result, err = service.Cancel(ctx, common)
	case "resume":
		result, err = service.Resume(ctx, common)
	}
	if err != nil {
		return err
	}
	return printJSON(result)
}

func onboardingStart(args []string) error {
	onboardingID, remaining := splitLeadingRunID(args)
	flags := flag.NewFlagSet("onboarding start", flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	preflightDigest := flags.String("preflight-digest", "", "exact preflight digest")
	previewDigest := flags.String("preview-digest", "", "exact semantic preview digest")
	if err := flags.Parse(remaining); err != nil {
		return err
	}
	if onboardingID == "" && flags.NArg() == 1 {
		onboardingID = flags.Arg(0)
	}
	if onboardingID == "" || flags.NArg() != 0 || !requester.complete() || len(*preflightDigest) != 64 || len(*previewDigest) != 64 {
		return errors.New("onboarding ID, exact preflight/preview digests, and complete requester identity are required")
	}
	service, store, err := composeOnboardingCLIService(*configPath, false)
	if err != nil {
		return err
	}
	defer store.Close()
	onboarding, receipt, err := service.Start(context.Background(), application.OnboardingStartCommand{Requester: requester.value(), OnboardingID: onboardingID, PreflightDigest: *preflightDigest, PreviewDigest: *previewDigest})
	if err != nil {
		return err
	}
	return printJSON(struct {
		Onboarding application.Onboarding       `json:"onboarding"`
		Receipt    application.OperationReceipt `json:"receipt"`
	}{onboarding, receipt})
}

func composeOnboardingCLIService(configOverride string, withReadiness bool) (*application.OnboardingService, managedStore, error) {
	path, err := resolveConfigPath(configOverride)
	if err != nil {
		return nil, nil, err
	}
	loaded, err := loadManagedConfiguration(path)
	if err != nil {
		return nil, nil, err
	}
	store, err := openBoundConfigurationStore(loaded)
	if err != nil {
		return nil, nil, err
	}
	service, err := composeOnboardingService(loaded, store, withReadiness)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return service, store, nil
}

func splitVerifierIDs(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	sort.Strings(result)
	return result
}
