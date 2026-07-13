package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	codexadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/githubapp"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localissue"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "plan":
		err = plan(os.Args[2:])
	case "spike":
		err = spike(os.Args[2:])
	case "local":
		err = local(os.Args[2:])
	case "linear":
		err = linear(os.Args[2:])
	case "controller":
		err = controller(os.Args[2:])
	case "config":
		err = config(os.Args[2:])
	case "github-read":
		err = githubRead(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func controller(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ifan-loop controller <start|continue|push|open-pr|reconcile|merge|reconcile-linear> ...")
	}
	switch args[0] {
	case "start":
		return linearStart(args[1:])
	case "continue":
		return controllerContinue(args[1:])
	case "push":
		return controllerPush(args[1:])
	case "open-pr":
		return controllerOpenPullRequest(args[1:])
	case "reconcile":
		return controllerReconcile(args[1:])
	case "merge":
		return controllerMerge(args[1:])
	case "reconcile-linear":
		return controllerReconcileLinear(args[1:])
	default:
		return fmt.Errorf("unknown controller command: %s", args[0])
	}
}

func linear(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ifan-loop linear start <IFAN-issue> --config <controller.json> --requester <login>")
	}
	switch args[0] {
	case "start":
		return linearStart(args[1:])
	default:
		return fmt.Errorf("unknown Linear command: %s", args[0])
	}
}

func config(args []string) error {
	if len(args) == 0 || (args[0] != "validate" && args[0] != "inspect") {
		return errors.New("usage: ifan-loop config <validate|inspect> --config <controller.json>")
	}
	flags := flag.NewFlagSet("config "+args[0], flag.ContinueOnError)
	path := flags.String("config", "", "controller composition configuration")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("--config is required")
	}
	loaded, err := bootstrap.Load(*path)
	if err != nil {
		return err
	}
	return printJSON(loaded.Readiness())
}

func linearStart(args []string) error {
	identifier, args := splitLinearStartIdentifier(args)
	flags := flag.NewFlagSet("linear start", flag.ContinueOnError)
	requesterIdentity := addRequesterFlags(flags)
	configPath := flags.String("config", "", "controller composition configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if identifier == "" && flags.NArg() == 1 {
		identifier = flags.Arg(0)
	}
	if identifier == "" || flags.NArg() != 0 || *configPath == "" || !requesterIdentity.complete() {
		return errors.New("one IFAN issue identifier plus --config and complete requester identity are required")
	}
	loaded, err := bootstrap.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	reader, err := linearadapter.New(loaded.Linear, linearadapter.EnvironmentCredentialSource{Variable: "IFAN_LOOP_LINEAR_TOKEN"}, nil)
	if err != nil {
		return err
	}
	service, err := application.NewLinearAdmissionService(reader, linearRegistryResolver{registry: loaded.Registry}, store, newLocalController(store, loaded.Controller.CodexBinary, ""))
	if err != nil {
		return err
	}
	ctx, cancel := localContext(loaded.Controller.RunTimeout)
	defer cancel()
	result, _, err := service.Start(ctx, application.LinearStartCommand{Requester: requesterIdentity.value(), Identifier: identifier})
	if err != nil {
		return err
	}
	return printJSON(result.Run)
}

func controllerContinue(args []string) error {
	command, loaded, store, err := productionCommand(args, "controller continue")
	if err != nil {
		return err
	}
	defer store.Close()
	if err := validateProductionPersistedBinding(command.run, loaded.Registry); err != nil {
		return application.ClassifyError(err)
	}
	coordinator, err := newProductionCoordinator(loaded, store, filepath.Dir(command.run.WorktreePath))
	if err != nil {
		return err
	}
	ctx, cancel := localContext(loaded.Controller.RunTimeout)
	defer cancel()
	result, err := coordinator.Continue(ctx, application.ProductionContinueCommand{Requester: command.requester, RunID: command.run.ID, Repository: command.repository, ExpectedState: command.expectedState, IdempotencyKey: command.idempotencyKey, Decision: command.decision})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func controllerReconcile(args []string) error {
	command, loaded, store, err := productionCommand(args, "controller reconcile")
	if err != nil {
		return err
	}
	defer store.Close()
	if err := validateProductionPersistedBinding(command.run, loaded.Registry); err != nil {
		return application.ClassifyError(err)
	}
	coordinator, err := newProductionCoordinator(loaded, store, filepath.Dir(command.run.WorktreePath))
	if err != nil {
		return err
	}
	profile, err := loaded.GitHubProfileForRepository(command.run.Repository)
	if err != nil {
		return err
	}
	if err := profile.Config.Validate(); err != nil {
		return errors.New("configured GitHub App credential source is unavailable")
	}
	observations := []application.GitHubRequestObservation{}
	client, err := githubapp.New(profile.Config, githubapp.RealClock{}, func(o application.GitHubRequestObservation) {
		o.RunID = command.run.ID
		observations = append(observations, o)
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), profile.Config.HTTPTimeout*20)
	defer cancel()
	result, err := coordinator.ReconcileGitHub(ctx, application.ProductionReconcileCommand{Requester: command.requester, RunID: command.run.ID, Repository: command.repository, ExpectedState: command.expectedState, IdempotencyKey: command.idempotencyKey}, githubReadAdapter{client: client, observations: &observations})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func controllerPush(args []string) error {
	command, loaded, store, err := productionCommand(args, "controller push")
	if err != nil {
		return err
	}
	defer store.Close()
	if err := validateProductionPersistedBinding(command.run, loaded.Registry); err != nil {
		return application.ClassifyError(err)
	}
	coordinator, err := newProductionCoordinator(loaded, store, filepath.Dir(command.run.WorktreePath))
	if err != nil {
		return err
	}
	validator := newLocalController(store, loaded.Controller.CodexBinary, filepath.Dir(command.run.WorktreePath))
	publisher := productionPushAdapter{publisher: gitadapter.Publisher{Workspace: gitadapter.Workspace{}, Process: processadapter.OSRunner{}}}
	ctx, cancel := localContext(loaded.Controller.RunTimeout)
	defer cancel()
	result, err := coordinator.Push(ctx, application.ProductionPushCommand{Requester: command.requester, RunID: command.run.ID, Repository: command.repository, ExpectedState: command.expectedState, IdempotencyKey: command.idempotencyKey}, validator, publisher)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func controllerOpenPullRequest(args []string) error {
	command, loaded, store, err := productionCommand(args, "controller open-pr")
	if err != nil {
		return err
	}
	defer store.Close()
	if err := validateProductionPersistedBinding(command.run, loaded.Registry); err != nil {
		return application.ClassifyError(err)
	}
	coordinator, err := newProductionCoordinator(loaded, store, filepath.Dir(command.run.WorktreePath))
	if err != nil {
		return err
	}
	profile, err := loaded.GitHubProfileForRepository(command.run.Repository)
	if err != nil {
		return err
	}
	if !profile.Config.PullRequestsWrite {
		return errors.New("configured GitHub App profile does not enable the narrow pull request write capability")
	}
	if err := profile.Config.Validate(); err != nil {
		return errors.New("configured GitHub App credential source is unavailable")
	}
	client, err := githubapp.New(profile.Config, githubapp.RealClock{}, nil)
	if err != nil {
		return err
	}
	validator := newLocalController(store, loaded.Controller.CodexBinary, filepath.Dir(command.run.WorktreePath))
	ctx, cancel := localContext(loaded.Controller.RunTimeout)
	defer cancel()
	result, err := coordinator.OpenPullRequest(ctx, application.ProductionOpenPullRequestCommand{Requester: command.requester, RunID: command.run.ID, Repository: command.repository, ExpectedState: command.expectedState, IdempotencyKey: command.idempotencyKey}, validator, client)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func controllerMerge(args []string) error {
	command, loaded, store, err := productionCommand(args, "controller merge")
	if err != nil {
		return err
	}
	defer store.Close()
	if err := validateProductionPersistedBinding(command.run, loaded.Registry); err != nil {
		return application.ClassifyError(err)
	}
	coordinator, err := newProductionCoordinator(loaded, store, filepath.Dir(command.run.WorktreePath))
	if err != nil {
		return err
	}
	profile, err := loaded.GitHubProfileForRepository(command.run.Repository)
	if err != nil {
		return err
	}
	if !profile.Config.SquashMergeWrite {
		return errors.New("configured GitHub App profile does not enable the narrow squash merge capability")
	}
	if err := profile.Config.Validate(); err != nil {
		return errors.New("configured GitHub App credential source is unavailable")
	}
	observations := []application.GitHubRequestObservation{}
	client, err := githubapp.New(profile.Config, githubapp.RealClock{}, func(o application.GitHubRequestObservation) {
		o.RunID = command.run.ID
		observations = append(observations, o)
	})
	if err != nil {
		return err
	}
	validator := newLocalController(store, loaded.Controller.CodexBinary, filepath.Dir(command.run.WorktreePath))
	ctx, cancel := context.WithTimeout(context.Background(), profile.Config.HTTPTimeout*30)
	defer cancel()
	adapter := githubReadAdapter{client: client, observations: &observations}
	result, err := coordinator.MergePullRequest(ctx, application.ProductionMergeCommand{Requester: command.requester, RunID: command.run.ID, Repository: command.repository, ExpectedState: command.expectedState, IdempotencyKey: command.idempotencyKey}, validator, adapter, adapter)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func controllerReconcileLinear(args []string) error {
	command, loaded, store, err := productionCommand(args, "controller reconcile-linear")
	if err != nil {
		return err
	}
	defer store.Close()
	if err := validateProductionPersistedBinding(command.run, loaded.Registry); err != nil {
		return application.ClassifyError(err)
	}
	coordinator, err := newProductionCoordinator(loaded, store, filepath.Dir(command.run.WorktreePath))
	if err != nil {
		return err
	}
	ctx, cancel := localContext(loaded.Linear.HTTPTimeout)
	defer cancel()
	result, err := coordinator.ReconcileLinearCompletion(ctx, application.ProductionLinearCompletionCommand{Requester: command.requester, RunID: command.run.ID, Repository: command.repository, ExpectedState: command.expectedState, IdempotencyKey: command.idempotencyKey})
	if err != nil {
		return err
	}
	return printJSON(result)
}

type productionPushAdapter struct{ publisher gitadapter.Publisher }

func (a productionPushAdapter) RemoteSHA(ctx context.Context, workspace, branch string) (string, error) {
	return a.publisher.RemoteSHA(ctx, workspace, branch)
}

func (a productionPushAdapter) Push(ctx context.Context, workspace, branch, candidate, expectedRemote, artifactRoot string) (application.PushEvidence, error) {
	evidence, err := a.publisher.Push(ctx, workspace, branch, candidate, expectedRemote, artifactRoot)
	return application.PushEvidence{RemoteRef: evidence.RemoteRef, SHA: evidence.SHA, ExitCode: evidence.ExitCode, StdoutPath: evidence.Stdout, StderrPath: evidence.Stderr}, err
}

type productionCLICommand struct {
	run            application.Run
	requester      application.Requester
	repository     string
	expectedState  domain.State
	idempotencyKey string
	decision       *application.Decision
}

func productionCommand(args []string, name string) (productionCLICommand, bootstrap.Bootstrap, *sqlitestore.Store, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	requester := addRequesterFlags(flags)
	configPath := flags.String("config", "", "controller composition configuration")
	repository := flags.String("repository", "", "previously observed canonical repository")
	expectedState := flags.String("expected-state", "", "previously observed run state used as a compare-and-swap token")
	idempotencyKey := flags.String("idempotency-key", "", "persisted run idempotency token")
	decisionPath := flags.String("decision", "", "optional human decision JSON")
	runID, remaining := splitLeadingRunID(args)
	if err := flags.Parse(remaining); err != nil {
		return productionCLICommand{}, bootstrap.Bootstrap{}, nil, err
	}
	if runID == "" && flags.NArg() == 1 {
		runID = flags.Arg(0)
	}
	if runID == "" || flags.NArg() != 0 || *configPath == "" || !requester.complete() || *repository == "" || *expectedState == "" || *idempotencyKey == "" {
		return productionCLICommand{}, bootstrap.Bootstrap{}, nil, errors.New("run ID, --config, complete requester identity, --repository, --expected-state, and --idempotency-key are required")
	}
	loaded, err := bootstrap.Load(*configPath)
	if err != nil {
		return productionCLICommand{}, bootstrap.Bootstrap{}, nil, err
	}
	store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
	if err != nil {
		return productionCLICommand{}, bootstrap.Bootstrap{}, nil, err
	}
	run, err := store.GetRun(context.Background(), runID)
	if err != nil {
		store.Close()
		return productionCLICommand{}, bootstrap.Bootstrap{}, nil, application.ClassifyError(err)
	}
	var decision *application.Decision
	if *decisionPath != "" {
		file, err := os.Open(*decisionPath)
		if err != nil {
			store.Close()
			return productionCLICommand{}, bootstrap.Bootstrap{}, nil, err
		}
		value, err := decodeDecision(file)
		file.Close()
		if err != nil {
			store.Close()
			return productionCLICommand{}, bootstrap.Bootstrap{}, nil, err
		}
		decision = &value
	}
	return productionCLICommand{run: run, requester: requester.value(), repository: *repository, expectedState: domain.State(*expectedState), idempotencyKey: *idempotencyKey, decision: decision}, loaded, store, nil
}

func newProductionCoordinator(loaded bootstrap.Bootstrap, store *sqlitestore.Store, worktreeRoot string) (*application.ProductionCoordinator, error) {
	reader, err := linearadapter.New(loaded.Linear, linearadapter.EnvironmentCredentialSource{Variable: "IFAN_LOOP_LINEAR_TOKEN"}, nil)
	if err != nil {
		return nil, err
	}
	local := newLocalController(store, loaded.Controller.CodexBinary, worktreeRoot)
	admission, err := application.NewLinearAdmissionService(reader, linearRegistryResolver{registry: loaded.Registry}, store, local)
	if err != nil {
		return nil, err
	}
	return application.NewProductionCoordinator(admission, local, store)
}

func splitLinearStartIdentifier(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

type linearRegistryResolver struct{ registry localregistry.Registry }

func (r linearRegistryResolver) ResolveLinearAdmissionRepository(label string) (application.LocalRepository, bool) {
	repository, err := r.registry.Resolve(label)
	if err != nil {
		return application.LocalRepository{}, false
	}
	return localRepository(repository), true
}

func githubRead(args []string) error {
	flags := flag.NewFlagSet("github-read", flag.ContinueOnError)
	requesterIdentity := addRequesterFlags(flags)
	configPath := flags.String("config", "", "controller composition configuration")
	pr := flags.Int64("pr", 0, "persisted pull request number")
	head := flags.String("expected-head", "", "expected exact PR head SHA")
	runID := flags.String("run-id", "", "run ID required with --db")
	repository := flags.String("repository", "", "previously observed canonical repository")
	expectedState := flags.String("expected-state", "", "previously observed run state used as a compare-and-swap token")
	idempotencyKey := flags.String("idempotency-key", "", "persisted run idempotency token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *pr < 1 || *head == "" {
		return errors.New("--config, --pr, and --expected-head are required")
	}
	if *runID == "" || !requesterIdentity.complete() || *repository == "" || *expectedState == "" || *idempotencyKey == "" {
		return errors.New("--run-id, --requester, --repository, --expected-state, and --idempotency-key are required for persisted ownership reconciliation")
	}
	loaded, err := bootstrap.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := application.NewQueryService(store).Status(context.Background(), application.QueryInput{Requester: requesterIdentity.value(), RunID: *runID, Repository: *repository}); err != nil {
		return err
	}
	inspection, err := store.Inspect(context.Background(), *runID)
	if err != nil {
		return err
	}
	if inspection.PullRequest == nil {
		return errors.New("persisted PR identity is required")
	}
	if *pr != inspection.PullRequest.Number || *head != inspection.Run.CandidateHead {
		return errors.New("requested PR or expected head does not match persisted run evidence")
	}
	profile, err := loaded.GitHubProfileForRepository(inspection.Run.Repository)
	if err != nil {
		return err
	}
	cfg := profile.Config
	if err := cfg.Validate(); err != nil {
		return errors.New("configured GitHub App credential source is unavailable")
	}
	if inspection.RepositoryBinding != nil && (inspection.RepositoryBinding.ExpectedRepositoryID != cfg.RepositoryID || inspection.RepositoryBinding.GitHubInstallationID != cfg.InstallationID || inspection.RepositoryBinding.GitHubAppID != cfg.AppID) {
		return errors.New("configured GitHub authority does not match persisted repository binding")
	}
	observations := []application.GitHubRequestObservation{}
	observer := func(o application.GitHubRequestObservation) {
		o.RunID = *runID
		observations = append(observations, o)
	}
	client, err := githubapp.New(cfg, githubapp.RealClock{}, observer)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.HTTPTimeout*20)
	defer cancel()
	controller := application.NewCommandService(nil, store)
	result, reconcileErr := controller.ReconcileFromGitHub(ctx, application.GitHubReconcileCommand{
		Requester: requesterIdentity.value(), RunID: *runID, Repository: *repository, ExpectedState: domain.State(*expectedState),
		IdempotencyKey: *idempotencyKey, PullRequest: *pr, ExpectedHead: *head,
	}, githubReadAdapter{client: client, observations: &observations})
	if reconcileErr != nil {
		return reconcileErr
	}
	return printJSON(result)
}

type githubReadAdapter struct {
	client       *githubapp.Client
	observations *[]application.GitHubRequestObservation
}

func (a githubReadAdapter) Authority() application.GitHubInstallationMetadata {
	return a.client.InstallationMetadata()
}

func (a githubReadAdapter) Read(ctx context.Context, pr int64, head string) (domain.GitHubReadEvidence, []application.GitHubRequestObservation, application.GitHubInstallationMetadata, error) {
	start := len(*a.observations)
	evidence, err := a.client.Read(ctx, pr, head)
	return evidence, append([]application.GitHubRequestObservation(nil), (*a.observations)[start:]...), a.client.InstallationMetadata(), err
}

func (a githubReadAdapter) SquashMerge(ctx context.Context, request application.SquashMergeRequest) (domain.PullRequest, []application.GitHubRequestObservation, application.GitHubInstallationMetadata, error) {
	start := len(*a.observations)
	pr, err := a.client.SquashMerge(ctx, request)
	return pr, append([]application.GitHubRequestObservation(nil), (*a.observations)[start:]...), a.client.InstallationMetadata(), err
}

func local(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ifan-loop local <start|continue|status|inspect|fixture-deliver>")
	}
	switch args[0] {
	case "start":
		return localStart(args[1:])
	case "continue":
		return localContinue(args[1:])
	case "status", "inspect":
		return localInspect(args[0], args[1:])
	case "fixture-deliver":
		return localFixtureDeliver(args[1:])
	default:
		return fmt.Errorf("unknown experimental local command: %s", args[0])
	}
}

func localStart(args []string) error {
	flags := flag.NewFlagSet("local start", flag.ContinueOnError)
	requesterIdentity := addRequesterFlags(flags)
	issuePath := flags.String("issue", "", "simulated Linear issue JSON")
	registryPath := flags.String("registry", "", "controller-owned local repository registry JSON")
	dbPath := flags.String("db", "", "SQLite controller database")
	codexBinary := flags.String("codex-binary", "codex", "Codex CLI binary")
	timeout := flags.Duration("timeout", 30*time.Minute, "local run timeout")
	repository := flags.String("repository", "", "caller-selected canonical repository")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *issuePath == "" || *registryPath == "" || *dbPath == "" || !requesterIdentity.complete() || *repository == "" {
		return fmt.Errorf("--issue, --registry, --db, --requester, and --repository are required")
	}
	registry, err := localregistry.Load(*registryPath)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	repo, err := registry.Resolve(*repository)
	if err != nil {
		return err
	}
	if err := application.AuthorizeRequester(requesterIdentity.value(), repo.OperatorIdentityPolicy.AllowedLogins, applicationActors(repo.OperatorIdentityPolicy.TrustedActors)...); err != nil {
		return err
	}
	file, err := os.Open(*issuePath)
	if err != nil {
		return err
	}
	issue, raw, decodeErr := localissue.Decode(file)
	file.Close()
	if decodeErr != nil {
		return decodeErr
	}
	snapshot, err := localissue.Admit(issue, raw, registry)
	if err != nil {
		return fmt.Errorf("simulated admission: %w", err)
	}
	if snapshot.Task.Repository != *repository {
		return application.ClassifyError(errors.New("admitted task repository does not match caller selection"))
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	controller := newLocalController(store, *codexBinary, repo.WorktreeRoot)
	ctx, cancel := localContext(*timeout)
	defer cancel()
	input := application.LocalStartInput{Task: snapshot.Task, RawIssueJSON: snapshot.RawJSON, RawIssueHash: snapshot.RawHash,
		NormalizedJSON: snapshot.NormalizedJSON, TaskHash: snapshot.TaskHash, IdempotencyKey: snapshot.IdempotencyKey,
		Repository: localRepository(repo), RunRoot: repo.RunRoot, WorktreeRoot: repo.WorktreeRoot}
	result, err := application.NewCommandService(controller, store).Start(ctx, application.StartCommand{Requester: requesterIdentity.value(), RepositorySelection: snapshot.Task.Repository, IdempotencyKey: snapshot.IdempotencyKey, Input: input})
	if err != nil {
		return err
	}
	return printJSON(result.Run)
}

func localContinue(args []string) error {
	flags := flag.NewFlagSet("local continue", flag.ContinueOnError)
	requesterIdentity := addRequesterFlags(flags)
	dbPath := flags.String("db", "", "SQLite controller database")
	registryPath := flags.String("registry", "", "versioned repository registry used to create the run")
	decisionPath := flags.String("decision", "", "optional simulated human decision JSON")
	codexBinary := flags.String("codex-binary", "codex", "Codex CLI binary")
	timeout := flags.Duration("timeout", 30*time.Minute, "local run timeout")
	repository := flags.String("repository", "", "previously observed canonical repository")
	expectedState := flags.String("expected-state", "", "previously observed run state used as a compare-and-swap token")
	idempotencyKey := flags.String("idempotency-key", "", "persisted run idempotency token")
	runID, flagArgs := splitLeadingRunID(args)
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if runID == "" && flags.NArg() == 1 {
		runID = flags.Arg(0)
	}
	if runID == "" || *dbPath == "" || *registryPath == "" || !requesterIdentity.complete() || *repository == "" || *expectedState == "" || *idempotencyKey == "" {
		return fmt.Errorf("usage: ifan-loop local continue <run-id> --db <controller.db> --registry <repository-registry.json> --requester <login> --repository <owner/repo> --expected-state <state> --idempotency-key <key> [--decision <decision.json>]")
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	existing, err := store.GetRun(context.Background(), runID)
	if err != nil {
		return application.ClassifyError(err)
	}
	if _, err := application.NewQueryService(store).Status(context.Background(), application.QueryInput{Requester: requesterIdentity.value(), RunID: runID, Repository: *repository}); err != nil {
		return err
	}
	registry, err := localregistry.Load(*registryPath)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	if err := validatePersistedRegistryBinding(existing, registry); err != nil {
		return err
	}
	var decision *application.Decision
	if *decisionPath != "" {
		file, err := os.Open(*decisionPath)
		if err != nil {
			return err
		}
		value, decodeErr := decodeDecision(file)
		file.Close()
		if decodeErr != nil {
			return decodeErr
		}
		decision = &value
	}
	controller := newLocalController(store, *codexBinary, filepath.Dir(existing.WorktreePath))
	ctx, cancel := localContext(*timeout)
	defer cancel()
	result, err := application.NewCommandService(controller, store).Continue(ctx, application.ContinueCommand{Requester: requesterIdentity.value(), RunID: runID, ExpectedState: domain.State(*expectedState), Repository: *repository, IdempotencyKey: *idempotencyKey, Decision: decision})
	if err != nil {
		return err
	}
	return printJSON(result.Run)
}

func localRepository(repo localregistry.Binding) application.LocalRepository {
	return application.LocalRepository{ProfileID: repo.ProfileID, ProfileSnapshotVersion: repo.ProfileSnapshotVersion, ProfileDigest: repo.ProfileDigest,
		ProfileSnapshotJSON: repo.ProfileSnapshotJSON,
		RegistryVersion:     repo.RegistryVersion, RegistryDigest: repo.RegistryDigest,
		RepositoryBindingDigest: repo.RepositoryBindingDigest, CanonicalRepository: repo.CanonicalRepository,
		OriginPath: repo.OriginPath, SourcePath: repo.SourcePath, RunRoot: repo.RunRoot, WorktreeRoot: repo.WorktreeRoot,
		BaseBranch: repo.BaseBranch, VerifierRegistryRef: repo.VerifierRegistryRef, VerifierIDs: append([]string(nil), repo.VerifierIDs...),
		GitHubAppProfileRef: repo.GitHubAppProfileRef, GitHubAppID: repo.GitHubAppID, GitHubInstallationID: repo.GitHubInstallationID,
		ExpectedRepositoryID: repo.ExpectedRepositoryID, AllowedOperatorLogins: append([]string(nil), repo.OperatorIdentityPolicy.AllowedLogins...),
		TrustedOperatorActors: applicationActors(repo.OperatorIdentityPolicy.TrustedActors)}
}

func localBinding(repo application.LocalRepository) localregistry.Binding {
	return localregistry.Binding{ProfileID: repo.ProfileID, ProfileSnapshotVersion: repo.ProfileSnapshotVersion, ProfileDigest: repo.ProfileDigest,
		ProfileSnapshotJSON: repo.ProfileSnapshotJSON,
		RegistryVersion:     repo.RegistryVersion, RegistryDigest: repo.RegistryDigest,
		RepositoryBindingDigest: repo.RepositoryBindingDigest, CanonicalRepository: repo.CanonicalRepository,
		OriginPath: repo.OriginPath, SourcePath: repo.SourcePath, RunRoot: repo.RunRoot, WorktreeRoot: repo.WorktreeRoot,
		BaseBranch: repo.BaseBranch, VerifierRegistryRef: repo.VerifierRegistryRef, VerifierIDs: append([]string(nil), repo.VerifierIDs...),
		GitHubAppProfileRef: repo.GitHubAppProfileRef, GitHubAppID: repo.GitHubAppID, GitHubInstallationID: repo.GitHubInstallationID,
		ExpectedRepositoryID:   repo.ExpectedRepositoryID,
		OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: append([]string(nil), repo.AllowedOperatorLogins...), TrustedActors: registryActors(repo.TrustedOperatorActors)}}
}

func applicationActors(values []localregistry.TrustedActorIdentity) []application.TrustedActorIdentity {
	result := make([]application.TrustedActorIdentity, len(values))
	for i, actor := range values {
		result[i] = application.TrustedActorIdentity{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: actor.Login, Type: actor.Type}
	}
	return result
}

func registryActors(values []application.TrustedActorIdentity) []localregistry.TrustedActorIdentity {
	result := make([]localregistry.TrustedActorIdentity, len(values))
	for i, actor := range values {
		result[i] = localregistry.TrustedActorIdentity{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: actor.Login, Type: actor.Type}
	}
	return result
}

func validatePersistedRegistryBinding(run application.Run, registry localregistry.Registry) error {
	if run.RegistryVersion < 1 || run.ProfileSnapshotVersion < 1 || run.ProfileID == "" || run.ProfileDigest == "" || run.ProfileSnapshotJSON == "" {
		return errors.New("persisted repository binding is legacy-insufficient")
	}
	var persisted application.LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &persisted); err != nil {
		return errors.New("persisted repository binding is invalid")
	}
	persisted.ProfileSnapshotJSON = run.ProfileSnapshotJSON
	rawIssueBytes := []byte(run.RawIssueJSON)
	rawIssueDigest := sha256.Sum256(rawIssueBytes)
	if hex.EncodeToString(rawIssueDigest[:]) != run.RawIssueHash {
		return errors.New("persisted raw issue digest mismatch")
	}
	issue, decodedRaw, err := localissue.Decode(strings.NewReader(run.RawIssueJSON))
	if err != nil {
		return errors.New("persisted raw issue snapshot is invalid")
	}
	snapshot, err := localissue.Admit(issue, decodedRaw, registry)
	if err != nil {
		return fmt.Errorf("re-admit persisted issue snapshot: %w", err)
	}
	if snapshot.RawHash != run.RawIssueHash || snapshot.TaskHash != run.TaskHash || snapshot.IdempotencyKey != run.IdempotencyKey || string(snapshot.NormalizedJSON) != run.NormalizedTaskJSON {
		return errors.New("persisted task snapshot does not match canonical issue admission")
	}
	taskBytes := []byte(run.NormalizedTaskJSON)
	taskDigest := sha256.Sum256(taskBytes)
	if hex.EncodeToString(taskDigest[:]) != run.TaskHash {
		return errors.New("persisted normalized task digest mismatch")
	}
	var task domain.CodingTask
	if err := json.Unmarshal(taskBytes, &task); err != nil || task.Validate() != nil {
		return errors.New("persisted normalized task is invalid")
	}
	if task.RunID != run.ID || task.IssueID != run.IssueID || task.SourceRevision != run.SourceRevision ||
		task.Repository != run.Repository || task.BaseBranch != run.BaseBranch || task.WorkingBranch != run.WorkingBranch {
		return errors.New("persisted run columns do not match immutable task snapshot")
	}
	if run.Repository != persisted.CanonicalRepository || run.BaseBranch != persisted.BaseBranch ||
		run.ProfileID != persisted.ProfileID || run.ProfileSnapshotVersion != persisted.ProfileSnapshotVersion || run.ProfileDigest != persisted.ProfileDigest ||
		run.RegistryVersion != persisted.RegistryVersion || run.RegistryDigest != persisted.RegistryDigest ||
		run.RepositoryBindingDigest != persisted.RepositoryBindingDigest ||
		run.WorktreePath != filepath.Join(persisted.WorktreeRoot, run.ID) || run.ArtifactRoot != filepath.Join(persisted.RunRoot, run.ID) {
		return errors.New("persisted run columns do not match repository authority binding")
	}
	return registry.VerifyPersisted(localBinding(persisted))
}

// validateProductionPersistedBinding verifies the credential-free authority
// frozen on any run. Unlike the local-fixture validator above, production runs
// contain a sanitized Linear source rather than localissue.Issue JSON.
func validateProductionPersistedBinding(run application.Run, registry localregistry.Registry) error {
	if run.RegistryVersion < 1 || run.ProfileSnapshotVersion < 1 || run.ProfileID == "" || run.ProfileDigest == "" || run.ProfileSnapshotJSON == "" {
		return errors.New("persisted repository binding is legacy-insufficient")
	}
	var persisted application.LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &persisted); err != nil {
		return errors.New("persisted repository binding is invalid")
	}
	persisted.ProfileSnapshotJSON = run.ProfileSnapshotJSON
	rawIssueDigest := sha256.Sum256([]byte(run.RawIssueJSON))
	if hex.EncodeToString(rawIssueDigest[:]) != run.RawIssueHash {
		return errors.New("persisted raw issue digest mismatch")
	}
	taskBytes := []byte(run.NormalizedTaskJSON)
	taskDigest := sha256.Sum256(taskBytes)
	if hex.EncodeToString(taskDigest[:]) != run.TaskHash {
		return errors.New("persisted normalized task digest mismatch")
	}
	var task domain.CodingTask
	if err := json.Unmarshal(taskBytes, &task); err != nil || task.Validate() != nil {
		return errors.New("persisted normalized task is invalid")
	}
	if task.RunID != run.ID || task.IssueID != run.IssueID || task.SourceRevision != run.SourceRevision ||
		task.Repository != run.Repository || task.BaseBranch != run.BaseBranch || task.WorkingBranch != run.WorkingBranch {
		return errors.New("persisted run columns do not match immutable task snapshot")
	}
	if run.Repository != persisted.CanonicalRepository || run.BaseBranch != persisted.BaseBranch ||
		run.ProfileID != persisted.ProfileID || run.ProfileSnapshotVersion != persisted.ProfileSnapshotVersion || run.ProfileDigest != persisted.ProfileDigest ||
		run.RegistryVersion != persisted.RegistryVersion || run.RegistryDigest != persisted.RegistryDigest ||
		run.RepositoryBindingDigest != persisted.RepositoryBindingDigest ||
		run.WorktreePath != filepath.Join(persisted.WorktreeRoot, run.ID) || run.ArtifactRoot != filepath.Join(persisted.RunRoot, run.ID) {
		return errors.New("persisted run columns do not match repository authority binding")
	}
	return registry.VerifyPersisted(localBinding(persisted))
}

func decodeDecision(reader io.Reader) (application.Decision, error) {
	var value application.Decision
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return value, errors.New("decision input must contain exactly one JSON value")
		}
		return value, fmt.Errorf("unexpected decision trailing data: %w", err)
	}
	return value, nil
}

func localInspect(command string, args []string) error {
	flags := flag.NewFlagSet("local "+command, flag.ContinueOnError)
	requesterIdentity := addRequesterFlags(flags)
	dbPath := flags.String("db", "", "SQLite controller database")
	runID, flagArgs := splitLeadingRunID(args)
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if runID == "" && flags.NArg() == 1 {
		runID = flags.Arg(0)
	}
	if runID == "" || *dbPath == "" || !requesterIdentity.complete() {
		return fmt.Errorf("usage: ifan-loop local %s <run-id> --db <controller.db>", command)
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	queries := application.NewQueryService(store)
	if command == "status" {
		result, err := queries.GetRunDetail(context.Background(), application.RunDetailQuery{Requester: requesterIdentity.value(), RunID: runID})
		if err != nil {
			return err
		}
		return printJSON(result)
	}
	result, err := queries.GetRunDetail(context.Background(), application.RunDetailQuery{Requester: requesterIdentity.value(), RunID: runID})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func sanitizeInspection(inspection *application.RunInspection) {
	application.SanitizeInspection(inspection)
}

type requesterFlags struct {
	login, nodeID, actorType *string
	databaseID               *int64
}

func addRequesterFlags(flags *flag.FlagSet) requesterFlags {
	return requesterFlags{login: flags.String("requester", "", "authenticated GitHub login"), databaseID: flags.Int64("requester-database-id", 0, "authenticated GitHub actor database ID"), nodeID: flags.String("requester-node-id", "", "authenticated GitHub actor node ID"), actorType: flags.String("requester-type", "", "authenticated GitHub actor type")}
}
func (r requesterFlags) complete() bool {
	return *r.login != "" && *r.databaseID > 0 && *r.nodeID != "" && *r.actorType != ""
}
func (r requesterFlags) value() application.Requester {
	return application.Requester{ID: *r.login, Kind: "github_login", DatabaseID: *r.databaseID, NodeID: *r.nodeID, ActorType: *r.actorType}
}

func cliRequester(login string) application.Requester {
	return application.Requester{ID: login, Kind: "github_login"}
}

func splitLeadingRunID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

type commandWorktrees struct{ manager gitadapter.WorktreeManager }

func (w commandWorktrees) Provision(ctx context.Context, spec application.WorktreeSpec) (application.WorktreeRecord, error) {
	e, err := w.manager.Provision(ctx, gitadapter.WorktreeRequest{SourcePath: spec.SourcePath, OriginPath: spec.OriginPath, BaseBranch: spec.BaseBranch, Branch: spec.Branch, Path: spec.Path})
	if err != nil {
		return application.WorktreeRecord{}, err
	}
	return application.WorktreeRecord{SourcePath: e.SourcePath, OriginPath: e.OriginPath, Path: e.Path, Branch: e.Branch, BaseBranch: e.BaseBranch, BaseSHA: e.BaseSHA}, nil
}
func (w commandWorktrees) ValidateOwned(ctx context.Context, r application.WorktreeRecord) error {
	return w.manager.ValidateOwned(ctx, gitadapter.WorktreeEvidence{SourcePath: r.SourcePath, OriginPath: r.OriginPath, Path: r.Path, Branch: r.Branch, BaseBranch: r.BaseBranch, BaseSHA: r.BaseSHA})
}

func newLocalController(store application.RunStore, codexBinary, worktreeRoot string) *application.LocalController {
	process := processadapter.OSRunner{}
	git := gitadapter.Workspace{}
	registry := verifier.NewRegistry(localregistry.BuiltinVerifierCommands(), process, git)
	executor := codexadapter.NewExecutor(process, codexBinary)
	return application.NewLocalController(store, commandWorktrees{gitadapter.WorktreeManager{}}, executor, registry, git, codexBinary, worktreeRoot)
}
func localContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	timed, cancel := context.WithTimeout(ctx, timeout)
	return timed, func() { cancel(); stop() }
}
func printJSON(value any) error {
	output, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(output))
	return nil
}

func spike(args []string) error {
	flags := flag.NewFlagSet("spike", flag.ContinueOnError)
	taskPath := flags.String("task", "", "path to a disposable fixture CodingTask JSON")
	workspace := flags.String("workspace", "", "absolute disposable fixture repository path")
	artifacts := flags.String("artifacts", "", "absolute new empty attempt directory")
	codexBinary := flags.String("codex-binary", "codex", "Codex CLI binary")
	timeout := flags.Duration("timeout", 30*time.Minute, "overall experimental spike timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *taskPath == "" || *workspace == "" || *artifacts == "" {
		return fmt.Errorf("--task, --workspace, and --artifacts are required")
	}
	file, err := os.Open(*taskPath)
	if err != nil {
		return fmt.Errorf("open task: %w", err)
	}
	defer file.Close()
	task, err := decodeTask(file)
	if err != nil {
		return fmt.Errorf("decode task: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	process := processadapter.OSRunner{}
	git := gitadapter.Workspace{}
	registry := verifier.NewRegistry(map[string]verifier.Command{
		"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}},
	}, process, git)
	executor := codexadapter.NewExecutor(process, *codexBinary)
	result, err := application.NewSpike(*codexBinary, executor, registry, git).Run(ctx, task, *workspace, *artifacts)
	if err != nil {
		return err
	}
	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(output))
	return nil
}

func plan(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	taskPath := flags.String("task", "", "path to a CodingTask JSON snapshot")
	workspace := flags.String("workspace", "", "absolute dedicated worktree path")
	artifacts := flags.String("artifacts", "", "absolute run artifact directory")
	codexBinary := flags.String("codex-binary", "codex", "Codex CLI binary")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *taskPath == "" {
		return fmt.Errorf("--task is required")
	}

	file, err := os.Open(*taskPath)
	if err != nil {
		return fmt.Errorf("open task: %w", err)
	}
	defer file.Close()

	task, err := decodeTask(file)
	if err != nil {
		return fmt.Errorf("decode task: %w", err)
	}

	deliveryPlan, err := application.NewPlanner(*codexBinary).Build(task, *workspace, *artifacts)
	if err != nil {
		return err
	}
	output, err := json.MarshalIndent(deliveryPlan, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}
	fmt.Println(string(output))
	return nil
}

func decodeTask(reader io.Reader) (domain.CodingTask, error) {
	var task domain.CodingTask
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&task); err != nil {
		return domain.CodingTask{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return domain.CodingTask{}, fmt.Errorf("task input must contain exactly one JSON value")
		}
		return domain.CodingTask{}, fmt.Errorf("unexpected trailing data: %w", err)
	}
	return task, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ifan-loop <version|plan|spike|local|github-read> [options]")
}
