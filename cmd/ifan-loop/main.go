package main

import (
	"context"
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

	codexadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/githubapp"
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

func githubRead(args []string) error {
	flags := flag.NewFlagSet("github-read", flag.ContinueOnError)
	configPath := flags.String("config", "", "GitHub App read-only configuration")
	pr := flags.Int64("pr", 0, "persisted pull request number")
	head := flags.String("expected-head", "", "expected exact PR head SHA")
	dbPath := flags.String("db", "", "optional controller SQLite database")
	runID := flags.String("run-id", "", "run ID required with --db")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *pr < 1 || *head == "" {
		return errors.New("--config, --pr, and --expected-head are required")
	}
	if *dbPath == "" || *runID == "" {
		return errors.New("--db and --run-id are required for persisted ownership reconciliation")
	}
	file, err := os.Open(*configPath)
	if err != nil {
		return err
	}
	cfg, decodeErr := githubapp.DecodeConfig(file)
	file.Close()
	if decodeErr != nil {
		return decodeErr
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
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
	parts := strings.Split(inspection.Run.Repository, "/")
	if len(parts) != 2 || parts[0] != cfg.RepositoryOwner || parts[1] != cfg.RepositoryName {
		return errors.New("configured repository does not match persisted run repository")
	}
	expectedRepository := domain.RepositoryIdentity{ID: cfg.RepositoryID, Owner: cfg.RepositoryOwner, Name: cfg.RepositoryName}
	if inspection.GitHubInstallation != nil {
		expectedRepository = inspection.GitHubInstallation.Repository
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
	evidence, readErr := client.Read(ctx, *pr, *head)
	metadata := client.InstallationMetadata()
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer persistCancel()
	if readErr != nil {
		if saveErr := store.SaveGitHubRequests(persistCtx, observations); saveErr != nil {
			return saveErr
		}
		return readErr
	}
	persistFailure := func(cause error) error {
		failureCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.SaveGitHubRequests(failureCtx, observations); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	if expectedRepository.NodeID == "" {
		expectedRepository.NodeID = evidence.Repository.NodeID
	}
	if err := application.ReconcileGitHubRead(expectedRepository, *inspection.PullRequest, inspection.Run.WorkingBranch, inspection.Run.BaseBranch, inspection.Run.CandidateHead, inspection.Run.BaseSHA, inspection.Run.IdempotencyKey, inspection.PullRequest.BodyDigest, evidence); err != nil {
		return persistFailure(err)
	}
	if inspection.GitHubInstallation != nil && (metadata.InstallationID != inspection.GitHubInstallation.InstallationID || metadata.AppID != inspection.GitHubInstallation.AppID) {
		return persistFailure(errors.New("GitHub App installation binding mismatch"))
	}
	if err := store.SaveGitHubReadSuccess(persistCtx, *runID, observations, evidence.PullRequest, metadata, evidence); err != nil {
		return persistFailure(err)
	}
	return printJSON(struct {
		Installation application.GitHubInstallationMetadata `json:"installation"`
		Requests     []application.GitHubRequestObservation `json:"requests"`
		Evidence     domain.GitHubReadEvidence              `json:"evidence"`
	}{metadata, observations, evidence})
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
	issuePath := flags.String("issue", "", "simulated Linear issue JSON")
	registryPath := flags.String("registry", "", "controller-owned local repository registry JSON")
	dbPath := flags.String("db", "", "SQLite controller database")
	runRoot := flags.String("run-root", "", "absolute run artifact root")
	worktreeRoot := flags.String("worktree-root", "", "absolute dedicated worktree root")
	codexBinary := flags.String("codex-binary", "codex", "Codex CLI binary")
	timeout := flags.Duration("timeout", 30*time.Minute, "local run timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *issuePath == "" || *registryPath == "" || *dbPath == "" || *runRoot == "" || *worktreeRoot == "" {
		return fmt.Errorf("--issue, --registry, --db, --run-root, and --worktree-root are required")
	}
	registry, err := localregistry.Load(*registryPath)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
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
	repo, err := registry.Resolve(snapshot.Task.Repository)
	if err != nil {
		return err
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	controller := newLocalController(store, *codexBinary, *worktreeRoot)
	ctx, cancel := localContext(*timeout)
	defer cancel()
	run, err := controller.Start(ctx, application.LocalStartInput{Task: snapshot.Task, RawIssueJSON: snapshot.RawJSON, RawIssueHash: snapshot.RawHash,
		NormalizedJSON: snapshot.NormalizedJSON, TaskHash: snapshot.TaskHash, IdempotencyKey: snapshot.IdempotencyKey,
		Repository: application.LocalRepository{Label: repo.Label, OriginPath: repo.OriginPath, SourcePath: repo.SourcePath, BaseBranch: repo.BaseBranch, VerifierIDs: repo.VerifierIDs},
		RunRoot:    *runRoot, WorktreeRoot: *worktreeRoot})
	if err != nil {
		return err
	}
	return printJSON(run)
}

func localContinue(args []string) error {
	flags := flag.NewFlagSet("local continue", flag.ContinueOnError)
	dbPath := flags.String("db", "", "SQLite controller database")
	decisionPath := flags.String("decision", "", "optional simulated human decision JSON")
	codexBinary := flags.String("codex-binary", "codex", "Codex CLI binary")
	timeout := flags.Duration("timeout", 30*time.Minute, "local run timeout")
	runID, flagArgs := splitLeadingRunID(args)
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if runID == "" && flags.NArg() == 1 {
		runID = flags.Arg(0)
	}
	if runID == "" || *dbPath == "" {
		return fmt.Errorf("usage: ifan-loop local continue <run-id> --db <controller.db> [--decision <decision.json>]")
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	existing, err := store.GetRun(context.Background(), runID)
	if err != nil {
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
	run, err := controller.Continue(ctx, runID, decision)
	if err != nil {
		return err
	}
	return printJSON(run)
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
	dbPath := flags.String("db", "", "SQLite controller database")
	runID, flagArgs := splitLeadingRunID(args)
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if runID == "" && flags.NArg() == 1 {
		runID = flags.Arg(0)
	}
	if runID == "" || *dbPath == "" {
		return fmt.Errorf("usage: ifan-loop local %s <run-id> --db <controller.db>", command)
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	inspection, err := store.Inspect(context.Background(), runID)
	if err != nil {
		return err
	}
	return printJSON(inspection)
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
