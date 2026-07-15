package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type workerOutput struct {
	WorkerInstanceID    string                               `json:"worker_instance_id"`
	ConfigurationDigest string                               `json:"configuration_digest"`
	Disabled            bool                                 `json:"disabled,omitempty"`
	Cycles              int                                  `json:"cycles,omitempty"`
	LastOutcome         string                               `json:"last_outcome,omitempty"`
	QueueDecision       *application.LinearTodoQueueDecision `json:"queue_decision,omitempty"`
	Stopped             string                               `json:"stopped"`
}

type automaticWorkerDriver struct {
	loaded bootstrap.Bootstrap
	store  *sqlitestore.Store
	policy application.ProductionDriverPolicy
}

func (d automaticWorkerDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	return driveProductionRun(ctx, d.loaded, d.store, command.Requester, command.RunID, d.policy)
}

func controllerWorker(args []string) error {
	flags := flag.NewFlagSet("controller worker", flag.ContinueOnError)
	configPath := configPathFlag(flags)
	once := flags.Bool("once", false, "run exactly one automatic admission cycle")
	maxRuntime := flags.Duration("max-runtime", 24*time.Hour, "maximum worker wall-clock lifetime")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("controller worker does not accept positional arguments")
	}
	if *maxRuntime <= 0 || *maxRuntime > 7*24*time.Hour {
		return errors.New("--max-runtime must be greater than zero and no more than 168h")
	}
	path, err := resolveConfigPath(*configPath)
	if err != nil {
		return err
	}
	loaded, err := bootstrap.Load(path)
	if err != nil {
		return err
	}
	instanceID := uuid.NewString()
	output := workerOutput{WorkerInstanceID: instanceID, ConfigurationDigest: loaded.Digest}
	configured := loaded.Automation.LinearTodoAdmission
	if !configured.Enabled {
		output.Disabled, output.Stopped = true, "disabled"
		return printJSON(output)
	}
	credentials, err := linearCredentialSourceForRef(loaded, configured.CredentialSourceRef)
	if err != nil {
		return errors.New("automatic admission credential source is unavailable")
	}
	checker, ok := credentials.(credentialChecker)
	if !ok || checker.Check(context.Background()) != nil {
		return errors.New("automatic admission credential source is unavailable")
	}
	linearConfig := loaded.Linear
	linearConfig.CredentialSourceRef = configured.CredentialSourceRef
	client, err := linearadapter.New(linearConfig, credentials, nil)
	if err != nil {
		return errors.New("automatic admission configuration is unavailable")
	}
	store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
	if err != nil {
		return errors.New("automatic admission state store is unavailable")
	}
	defer store.Close()
	requester := application.Requester{ID: configured.Requester.Login, Kind: "github_login", DatabaseID: configured.Requester.DatabaseID, NodeID: configured.Requester.NodeID, ActorType: configured.Requester.Type}
	dispatcher, err := application.NewLinearTodoDispatcher(client, client, linearRegistryResolver{registry: loaded.Registry}, client, store, newLocalController(store, loaded.Controller.CodexBinary, ""), automaticWorkerDriver{loaded: loaded, store: store, policy: application.ProductionDriverPolicy{PollInterval: configured.PollInterval, MaxImmediateAction: 32}}, application.LinearTodoDispatchPolicy{
		CandidateAuthority: application.LinearTodoCandidateAuthority{TeamID: configured.TeamID, TeamKey: configured.TeamKey, TodoState: application.LinearState{ID: configured.TodoState.ID, Name: configured.TodoState.Name, Type: configured.TodoState.Type}, InProgressState: application.LinearState{ID: configured.InProgressState.ID, Name: configured.InProgressState.Name, Type: configured.InProgressState.Type}, MaxCandidates: configured.MaxCandidates, MaxPages: configured.MaxPages},
		StartAuthority:     application.LinearIssueStartAuthority{TeamID: configured.TeamID, TeamKey: configured.TeamKey, TodoState: application.LinearState{ID: configured.TodoState.ID, Name: configured.TodoState.Name, Type: configured.TodoState.Type}, InProgressState: application.LinearState{ID: configured.InProgressState.ID, Name: configured.InProgressState.Name, Type: configured.InProgressState.Type}},
		LeaseTTL:           configured.SchedulerLeaseTTL,
		OwnerNonce:         instanceID,
		Requester:          requester,
		AttentionProfile:   application.OperatorAttentionProfile{ID: "automation", Name: "linear-todo-admission"},
	})
	if err != nil {
		return errors.New("automatic admission worker is unavailable")
	}
	fprintfWorkerStart(instanceID, loaded.Digest)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *maxRuntime)
	defer cancel()
	result, err := runAdmissionWorker(ctx, *once, configured.PollInterval, dispatcher.Dispatch, waitAdmissionWorker)
	if err != nil {
		return application.ClassifyError(err)
	}
	output.Cycles, output.LastOutcome, output.QueueDecision, output.Stopped = result.Cycles, result.LastOutcome, result.QueueDecision, result.Stopped
	return printJSON(output)
}

func fprintfWorkerStart(instanceID, configurationDigest string) {
	// Both values are controller-generated or a SHA-256 configuration digest.
	// No source reference, token, requester, path, issue body, or run key is
	// projected while a long-lived driver is running.
	fmt.Fprintf(os.Stderr, "automatic admission worker started instance=%s configuration=%s\n", instanceID, configurationDigest)
}
