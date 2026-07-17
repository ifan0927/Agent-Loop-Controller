package main

import (
	"context"
	"errors"
	"flag"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/githubapp"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func controllerRecoverCIWait(args []string) error {
	flags := flag.NewFlagSet("controller recover-ci-wait", flag.ContinueOnError)
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
	linearReader, err := linearadapter.New(loaded.Linear, credentials, nil)
	if err != nil {
		return err
	}
	local := newLocalController(store, loaded.Controller.CodexBinary, "")
	admission, err := application.NewLinearAdmissionService(linearReader, linearRegistryResolver{registry: loaded.Registry}, store, local)
	if err != nil {
		return err
	}
	profile, err := loaded.GitHubProfileForRepository(run.Repository)
	if err != nil {
		return err
	}
	observations := []application.GitHubRequestObservation{}
	client, err := githubapp.New(profile.Config, githubapp.RealClock{}, func(observation application.GitHubRequestObservation) {
		observation.RunID = run.ID
		observations = append(observations, observation)
	})
	if err != nil {
		return err
	}
	service, err := application.NewCIWaitRecoveryService(store, admission)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), maxDuration(loaded.Linear.HTTPTimeout, profile.Config.HTTPTimeout*20))
	defer cancel()
	workspace := gitadapter.Workspace{Process: processadapter.OSRunner{}}
	localAuthority := ciWaitLocalAuthority{commandWorktrees{manager: gitadapter.WorktreeManager{Workspace: workspace}}, workspace}
	result, err := service.Recover(ctx, application.CIWaitRecoveryCommand{Requester: requester.value(), RunID: run.ID}, githubReadAdapter{client: client, observations: &observations}, localAuthority)
	if err != nil {
		return err
	}
	return printJSON(result)
}

type ciWaitLocalAuthority struct {
	commandWorktrees
	workspace gitadapter.Workspace
}

func (a ciWaitLocalAuthority) Head(ctx context.Context, path string) (string, error) {
	return a.workspace.Head(ctx, path)
}

func (a ciWaitLocalAuthority) Branch(ctx context.Context, path string) (string, error) {
	return a.workspace.Branch(ctx, path)
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}
