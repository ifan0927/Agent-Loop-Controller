package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
	Status              string                               `json:"status"`
	PreviousStatus      string                               `json:"previous_status,omitempty"`
}

const (
	workerLogStartupLimit = 8 << 20
	workerProcessLifetime = "indefinite"
	workerLogPolicy       = "startup_truncate_8_mib"
)

type automaticWorkerDriver struct {
	loaded bootstrap.Bootstrap
	store  *sqlitestore.Store
	policy application.ProductionDriverPolicy
}

type automaticWorkerRuntime struct {
	store    *sqlitestore.Store
	dispatch admissionWorkerDispatch
}

var buildAutomaticWorkerRuntime = newAutomaticWorkerRuntime
var emitAutomaticWorkerOutput = func(output workerOutput) error { return printJSON(output) }
var observeAutomaticWorkerStoreClosed = func() {}

func (d automaticWorkerDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	return driveProductionRun(ctx, d.loaded, d.store, command.Requester, command.RunID, d.policy)
}

func controllerWorker(args []string) error {
	flags := flag.NewFlagSet("controller worker", flag.ContinueOnError)
	configPath := configPathFlag(flags)
	once := flags.Bool("once", false, "run exactly one automatic admission cycle")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("controller worker does not accept positional arguments")
	}
	if err := boundWorkerLogStream(os.Stdout, workerLogStartupLimit); err != nil {
		return errors.New("automatic admission stdout log is unsafe")
	}
	if err := boundWorkerLogStream(os.Stderr, workerLogStartupLimit); err != nil {
		return errors.New("automatic admission stderr log is unsafe")
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
	output := workerOutput{WorkerInstanceID: instanceID, ConfigurationDigest: loaded.Digest, Status: workerStatusRunning}
	configured := loaded.Automation.LinearTodoAdmission
	if !configured.Enabled {
		output.Disabled, output.Stopped, output.PreviousStatus, output.Status = true, "disabled", workerStatusRunning, workerStatusStopping
		return emitAutomaticWorkerOutput(output)
	}
	runtime, err := buildAutomaticWorkerRuntime(loaded, instanceID)
	if err != nil {
		return err
	}
	if runtime.store == nil || runtime.dispatch == nil {
		return errors.New("automatic admission worker is unavailable")
	}
	store := runtime.store
	storeOpen := true
	defer func() {
		if storeOpen {
			_ = store.Close()
		}
	}()
	fprintfWorkerStart(instanceID, loaded.Digest)
	reporter, err := newWorkerStatusReporter(path, instanceID)
	if err != nil {
		return errors.New("automatic admission worker status is unavailable")
	}
	ctx, stop := workerSignalContext()
	defer stop()
	result, err := runAdmissionWorkerObserved(ctx, *once, configured.PollInterval, runtime.dispatch, waitAdmissionWorker, reporter.Observe)
	if err != nil {
		return application.ClassifyError(err)
	}
	output.Cycles, output.LastOutcome, output.QueueDecision, output.Stopped, output.Status, output.PreviousStatus = result.Cycles, result.LastOutcome, result.QueueDecision, result.Stopped, result.Status, result.PreviousStatus
	if err := closeWorkerStateStore(store); err != nil {
		return err
	}
	storeOpen = false
	return emitAutomaticWorkerOutput(output)
}

func newAutomaticWorkerRuntime(loaded bootstrap.Bootstrap, instanceID string) (automaticWorkerRuntime, error) {
	configured := loaded.Automation.LinearTodoAdmission
	credentials, err := linearCredentialSourceForRef(loaded, configured.CredentialSourceRef)
	if err != nil {
		return automaticWorkerRuntime{}, errors.New("automatic admission credential source is unavailable")
	}
	checker, ok := credentials.(credentialChecker)
	if !ok || checker.Check(context.Background()) != nil {
		return automaticWorkerRuntime{}, errors.New("automatic admission credential source is unavailable")
	}
	linearConfig := loaded.Linear
	linearConfig.CredentialSourceRef = configured.CredentialSourceRef
	client, err := linearadapter.New(linearConfig, credentials, nil)
	if err != nil {
		return automaticWorkerRuntime{}, errors.New("automatic admission configuration is unavailable")
	}
	store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
	if err != nil {
		return automaticWorkerRuntime{}, errors.New("automatic admission state store is unavailable")
	}
	requester := application.Requester{ID: configured.Requester.Login, Kind: "github_login", DatabaseID: configured.Requester.DatabaseID, NodeID: configured.Requester.NodeID, ActorType: configured.Requester.Type}
	dispatcher, err := application.NewLinearTodoDispatcher(client, client, linearRegistryResolver{registry: loaded.Registry}, client, store, newLocalController(store, loaded.Controller.CodexBinary, ""), automaticWorkerDriver{loaded: loaded, store: store, policy: automaticWorkerDriverPolicy(configured)}, application.LinearTodoDispatchPolicy{
		CandidateAuthority: application.LinearTodoCandidateAuthority{TeamID: configured.TeamID, TeamKey: configured.TeamKey, TodoState: application.LinearState{ID: configured.TodoState.ID, Name: configured.TodoState.Name, Type: configured.TodoState.Type}, InProgressState: application.LinearState{ID: configured.InProgressState.ID, Name: configured.InProgressState.Name, Type: configured.InProgressState.Type}, MaxCandidates: configured.MaxCandidates, MaxPages: configured.MaxPages},
		StartAuthority:     application.LinearIssueStartAuthority{TeamID: configured.TeamID, TeamKey: configured.TeamKey, TodoState: application.LinearState{ID: configured.TodoState.ID, Name: configured.TodoState.Name, Type: configured.TodoState.Type}, InProgressState: application.LinearState{ID: configured.InProgressState.ID, Name: configured.InProgressState.Name, Type: configured.InProgressState.Type}},
		LeaseTTL:           configured.SchedulerLeaseTTL,
		OwnerNonce:         instanceID,
		Requester:          requester,
		AttentionProfile:   application.OperatorAttentionProfile{ID: "automation", Name: "linear-todo-admission"},
	})
	if err != nil {
		_ = store.Close()
		return automaticWorkerRuntime{}, errors.New("automatic admission worker is unavailable")
	}
	return automaticWorkerRuntime{store: store, dispatch: dispatcher.Dispatch}, nil
}

func automaticWorkerDriverPolicy(configured bootstrap.LinearTodoAdmission) application.ProductionDriverPolicy {
	return application.ProductionDriverPolicy{PollInterval: configured.DeliveryPollInterval, MaxImmediateAction: 32}
}

func closeWorkerStateStore(store *sqlitestore.Store) error {
	if store == nil || store.Close() != nil {
		return errors.New("automatic admission state store did not close cleanly")
	}
	observeAutomaticWorkerStoreClosed()
	return nil
}

func workerSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// boundWorkerLogStream prevents the fixed LaunchAgent stdout/stderr leaves from
// accumulating across restarts. Pipes and terminals are unaffected. A regular
// file must retain the same private ownership contract enforced by doctor.
func boundWorkerLogStream(file *os.File, limit int64) error {
	if file == nil || limit <= 0 {
		return errors.New("invalid worker log stream")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !ownedByCurrentUser(info) || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
		return errors.New("unsafe worker log stream")
	}
	if info.Size() < limit {
		return nil
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	_, err = file.Seek(0, 0)
	return err
}

func fprintfWorkerStart(instanceID, configurationDigest string) {
	// Both values are controller-generated or a SHA-256 configuration digest.
	// No source reference, token, requester, path, issue body, or run key is
	// projected while a long-lived driver is running.
	fmt.Fprintf(os.Stderr, "automatic admission worker started status=%s instance=%s configuration=%s\n", workerStatusRunning, instanceID, configurationDigest)
}
