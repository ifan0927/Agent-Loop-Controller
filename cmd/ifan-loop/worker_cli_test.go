package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestBoundWorkerLogStreamTruncatesOnlyPrivateRegularFileAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := boundWorkerLogStream(file, 64); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("size=%d err=%v", info.Size(), err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := boundWorkerLogStream(file, 64); err == nil {
		t.Fatal("public worker log was accepted")
	}
}

func TestControllerWorkerSubprocessSIGTERMClosesCompleteRuntime(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, dbPath := writeControllerStatusConfig(t, root)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	registryPath, _ := config["repository_registry_file"].(string)
	registryRaw, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry map[string]any
	if err := json.Unmarshal(registryRaw, &registry); err != nil {
		t.Fatal(err)
	}
	config["repositories"] = registry["repositories"]
	delete(config, "repository_registry_file")
	config["version"] = 3
	config["automation"] = map[string]any{"linear_todo_admission": map[string]any{
		"enabled": true, "team_id": "123e4567-e89b-42d3-a456-426614174100", "team_key": "IFAN",
		"todo_state":        map[string]any{"id": offlineAdmissionTodoState.ID, "name": offlineAdmissionTodoState.Name, "type": offlineAdmissionTodoState.Type},
		"in_progress_state": map[string]any{"id": offlineAdmissionInProgressState.ID, "name": offlineAdmissionInProgressState.Name, "type": offlineAdmissionInProgressState.Type},
		"poll_interval":     "1m", "scheduler_lease_ttl": "1m", "scheduler_lease_renewal_interval": "20s",
		"max_candidates": 10, "max_pages": 1, "max_active_runs": 1,
		"requester":         map[string]any{"database_id": 33, "node_id": "MDQ6VXNlcjMz", "login": "ifan0927", "type": "User"},
		"notification_mode": "local_outbox", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN",
	}}
	rewritten, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "managed-child.pid")
	closeMarker := filepath.Join(root, "worker-store.closed")
	stdoutPath, stderrPath := filepath.Join(root, "worker.stdout.log"), filepath.Join(root, "worker.stderr.log")
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		stdout.Close()
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestControllerWorkerSubprocessHelper$")
	command.Env = append(os.Environ(), "IFAN_WORKER_SUBPROCESS=1", "IFAN_WORKER_CONFIG="+configPath, "IFAN_WORKER_CHILD_MARKER="+marker, "IFAN_WORKER_CLOSE_MARKER="+closeMarker, "IFAN_WORKER_ROOT="+root)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		stdout.Close()
		stderr.Close()
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(marker)
		if readErr == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID < 1 {
		_ = command.Process.Kill()
		_ = command.Wait()
		stdout.Close()
		stderr.Close()
		workerOutput, _ := os.ReadFile(stdoutPath)
		workerError, _ := os.ReadFile(stderrPath)
		t.Fatalf("managed child did not start stdout=%s stderr=%s", workerOutput, workerError)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("worker subprocess exit=%v", err)
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("worker subprocess did not stop within bound")
	}
	stdout.Close()
	stderr.Close()
	output, err := os.ReadFile(stdoutPath)
	if err != nil || !strings.Contains(string(output), `"stopped": "canceled"`) || strings.Contains(string(output), "failed") || strings.Contains(string(output), "abandoned") {
		t.Fatalf("terminal output=%s err=%v", output, err)
	}
	if closed, err := os.ReadFile(closeMarker); err != nil || string(closed) != "closed" {
		t.Fatalf("explicit SQLite close marker=%q err=%v", closed, err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("managed child pid=%d remains err=%v", childPID, err)
	}
	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runs, err := store.ListNonterminalRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != domain.StateAwaitingHumanDecision {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "restart-owner", time.Minute, time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("replacement lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	_, _ = store.ReleaseLinearTodoAdmissionLease(context.Background(), lease)
}

func TestControllerWorkerSubprocessHelper(t *testing.T) {
	if os.Getenv("IFAN_WORKER_SUBPROCESS") != "1" {
		return
	}
	closeMarker := os.Getenv("IFAN_WORKER_CLOSE_MARKER")
	observeAutomaticWorkerStoreClosed = func() {
		if err := os.WriteFile(closeMarker, []byte("closed"), 0o600); err != nil {
			panic(err)
		}
	}
	emitAutomaticWorkerOutput = func(output workerOutput) error {
		if closed, err := os.ReadFile(closeMarker); err != nil || string(closed) != "closed" {
			return errors.New("worker terminal output preceded SQLite close")
		}
		return printJSON(output)
	}
	buildAutomaticWorkerRuntime = func(loaded bootstrap.Bootstrap, instanceID string) (automaticWorkerRuntime, error) {
		store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
		if err != nil {
			return automaticWorkerRuntime{}, err
		}
		repository := offlineAdmissionRepository(t)
		candidate := offlineAdmissionCandidate()
		reader := newOfflineAdmissionReader(offlineAdmissionSource(candidate))
		scanner := &offlineAdmissionScanner{scan: application.LinearTodoCandidateScan{Candidates: []application.LinearTodoCandidate{candidate}, Digest: offlineAdmissionDigest("subprocess-scan"), ObservedAt: candidate.UpdatedAt}}
		starter := &offlineAdmissionStarter{reader: reader}
		controller := application.NewLocalController(store, &offlineAdmissionWorktrees{}, &offlineAdmissionCodex{}, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
		driver := workerManagedProcessDriver{root: os.Getenv("IFAN_WORKER_ROOT"), marker: os.Getenv("IFAN_WORKER_CHILD_MARKER")}
		dispatcher, err := newOfflineAdmissionDispatcher(scanner, reader, starter, store, controller, driver, repository, instanceID)
		if err != nil {
			store.Close()
			return automaticWorkerRuntime{}, err
		}
		return automaticWorkerRuntime{store: store, dispatch: dispatcher.Dispatch}, nil
	}
	if err := controllerWorker([]string{"--config", os.Getenv("IFAN_WORKER_CONFIG")}); err != nil {
		t.Fatal(err)
	}
}

type workerManagedProcessDriver struct{ root, marker string }

func (d workerManagedProcessDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	result, err := (processadapter.OSRunner{InterruptGrace: 100 * time.Millisecond}).Run(ctx, processadapter.Spec{
		Program: os.Args[0], Args: []string{"-test.run=^TestControllerWorkerManagedChild$"}, WorkingDir: d.root,
		StdoutPath: filepath.Join(d.root, "managed-child.stdout"), StderrPath: filepath.Join(d.root, "managed-child.stderr"),
		Environment: []string{"IFAN_WORKER_MANAGED_CHILD=1", "IFAN_WORKER_CHILD_MARKER=" + d.marker},
	})
	_ = result
	return application.ProductionDriveResult{Run: application.RunResult{RunID: command.RunID}}, err
}

func TestControllerWorkerManagedChild(t *testing.T) {
	if os.Getenv("IFAN_WORKER_MANAGED_CHILD") != "1" {
		return
	}
	signal.Ignore(syscall.SIGINT)
	if err := os.WriteFile(os.Getenv("IFAN_WORKER_CHILD_MARKER"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestCloseWorkerStateStoreClosesSQLiteBeforeTerminalOutput(t *testing.T) {
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeWorkerStateStore(store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRun(context.Background(), "missing"); err == nil {
		t.Fatal("SQLite remained usable after worker close")
	}
}

func TestWorkerSIGTERMStopsActiveDispatchWithSanitizedTerminalStatus(t *testing.T) {
	ctx, stop := workerSignalContext()
	defer stop()
	done := make(chan admissionWorkerResult, 1)
	started := make(chan struct{})
	go func() {
		result, err := runAdmissionWorker(ctx, false, time.Minute, func(dispatchCtx context.Context) (application.LinearTodoDispatchResult, error) {
			close(started)
			<-dispatchCtx.Done()
			return application.LinearTodoDispatchResult{}, dispatchCtx.Err()
		}, waitAdmissionWorker)
		if err != nil {
			done <- admissionWorkerResult{Stopped: "unexpected_error"}
			return
		}
		done <- result
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter active dispatch")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Cycles != 1 || result.Stopped != "canceled" {
			t.Fatalf("result=%+v", result)
		}
		raw, err := json.Marshal(workerOutput{Cycles: result.Cycles, Stopped: result.Stopped})
		if err != nil || !strings.Contains(string(raw), `"stopped":"canceled"`) || strings.Contains(string(raw), "failed") || strings.Contains(string(raw), "abandoned") {
			t.Fatalf("terminal output=%s err=%v", raw, err)
		}
	case <-time.After(time.Second):
		t.Fatal("SIGTERM did not stop active worker dispatch")
	}
}
