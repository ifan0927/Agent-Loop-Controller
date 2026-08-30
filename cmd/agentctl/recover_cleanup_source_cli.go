package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func controllerRecoverCleanupSource(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentctl controller recover-cleanup-source <preview|apply> <run-id> --replacement-source <absolute-path> <requester flags>")
	}
	mode, args := args[0], args[1:]
	if mode != "preview" && mode != "apply" {
		return errors.New("cleanup source recovery mode must be preview or apply")
	}
	runID, remaining := splitLeadingRunID(args)
	flags := flag.NewFlagSet("controller recover-cleanup-source "+mode, flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := configPathFlag(flags)
	replacement := flags.String("replacement-source", "", "explicit absolute replacement source checkout")
	requestID := flags.String("request-id", "", "stable cleanup recovery request ID")
	previewDigest := flags.String("preview-digest", "", "exact sanitized preview digest")
	confirmed := flags.Bool("source-relocation-confirmed", false, "confirm the source checkout was deliberately relocated")
	if err := flags.Parse(remaining); err != nil {
		return err
	}
	if runID == "" && flags.NArg() == 1 {
		runID = flags.Arg(0)
	}
	if runID == "" || flags.NArg() != 0 || !requester.complete() || strings.TrimSpace(*replacement) == "" {
		return errors.New("run ID, replacement source, and complete requester identity are required")
	}
	if mode == "preview" && (*requestID != "" || *previewDigest != "" || *confirmed) {
		return errors.New("preview does not accept apply authority")
	}
	if mode == "apply" && (*requestID == "" || *previewDigest == "" || !*confirmed) {
		return errors.New("apply requires --request-id, --preview-digest, and --source-relocation-confirmed")
	}
	path, err := resolveConfigPath(*configPath)
	if err != nil {
		return err
	}
	loaded, err := loadManagedConfiguration(path)
	if err != nil {
		return err
	}
	store, err := openManagedConfigurationStore(loaded)
	if err != nil {
		return err
	}
	defer store.Close()
	port := cleanupSourceRecoveryCLIAdapter{adapter: gitadapter.CleanupSourceRecovery{Workspace: gitadapter.Workspace{Process: processadapter.OSRunner{}}}}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return err
	}
	service, err := application.NewCleanupSourceRecoveryService(store, port, authorizer)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(loaded.Controller.RunTimeout)
	defer cancel()
	previewRequest := application.CleanupSourceRecoveryPreviewRequest{Requester: requester.value(), RunID: runID, ReplacementSourcePath: *replacement}
	if mode == "preview" {
		result, err := service.Preview(ctx, previewRequest)
		if err != nil {
			return err
		}
		return printJSON(result)
	}
	result, err := service.Apply(ctx, application.CleanupSourceRecoveryApplyRequest{CleanupSourceRecoveryPreviewRequest: previewRequest, RequestID: *requestID, PreviewDigest: *previewDigest, SourceRelocationConfirmed: *confirmed})
	if err != nil {
		return err
	}
	return printJSON(result)
}

type cleanupSourceRecoveryCLIAdapter struct {
	adapter gitadapter.CleanupSourceRecovery
}

func cleanupSourceGitRequest(value application.CleanupSourceRecoveryGitRequest) gitadapter.CleanupSourceRecoveryRequest {
	return gitadapter.CleanupSourceRecoveryRequest{Repository: value.Repository, FrozenSourcePath: value.FrozenSourcePath, ReplacementSourcePath: value.ReplacementSourcePath, ExpectedOrigin: value.ExpectedOrigin, WorktreePath: value.WorktreePath, Branch: value.Branch, CandidateHead: value.CandidateHead, ExpectedRegistrationDigest: value.ExpectedRegistrationDigest}
}

func (a cleanupSourceRecoveryCLIAdapter) ObserveCleanupSourceRecovery(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) (application.CleanupSourceRecoveryObservation, error) {
	value, err := a.adapter.ObserveCleanupSourceRecovery(ctx, cleanupSourceGitRequest(request))
	return application.CleanupSourceRecoveryObservation{ReplacementSourceDigest: value.ReplacementSourceDigest, ReplacementIdentityDigest: value.ReplacementIdentityDigest, RepositoryOriginDigest: value.RepositoryOriginDigest, RegistrationDigest: value.RegistrationDigest, Branch: value.Branch, CandidateHead: value.CandidateHead, LinkRepaired: value.LinkRepaired, HeadDetached: value.HeadDetached, WorktreePresent: value.WorktreePresent, BranchPresent: value.BranchPresent, WorktreeClean: value.WorktreeClean}, err
}
func (a cleanupSourceRecoveryCLIAdapter) RepairCleanupWorktreeLink(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) error {
	return a.adapter.RepairCleanupWorktreeLink(ctx, cleanupSourceGitRequest(request))
}
func (a cleanupSourceRecoveryCLIAdapter) DetachRecoveredWorktreeHead(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) error {
	return a.adapter.DetachRecoveredWorktreeHead(ctx, cleanupSourceGitRequest(request))
}
func (a cleanupSourceRecoveryCLIAdapter) RemoveRecoveredWorktree(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) error {
	return a.adapter.RemoveRecoveredWorktree(ctx, cleanupSourceGitRequest(request))
}
