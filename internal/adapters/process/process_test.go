package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testProcessControlKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestOSRunnerRejectsExistingOutputLeaf(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "outcome.json")
	if err := os.WriteFile(output, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		MustNotExist: []string{output},
	})
	if err == nil {
		t.Fatal("existing output leaf must be rejected")
	}
}

func TestOSRunnerRejectsSymlinkOutputLeaf(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	output := filepath.Join(directory, "outcome.json")
	if err := os.WriteFile(target, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, output); err != nil {
		t.Fatal(err)
	}
	_, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		MustNotExist: []string{output},
	})
	if err == nil {
		t.Fatal("symlink output leaf must be rejected")
	}
}

func TestOSRunnerKeepsProductionOutputOnlyInArtifactFiles(t *testing.T) {
	directory := t.TempDir()
	stdoutPath := filepath.Join(directory, "stdout")
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: stdoutPath, StderrPath: filepath.Join(directory, "stderr"),
	})
	if err != nil || result.Outcome != OutcomeExited || !result.Succeeded() {
		t.Fatal(err)
	}
	if len(result.Stdout) != 0 || result.StdoutPath != stdoutPath {
		t.Fatalf("production output was copied into memory: %+v", result)
	}
	data, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "done\n" {
		t.Fatalf("stdout artifact = %q", data)
	}
}

func TestOSRunnerCancelsProcessGroupWithBoundedTermination(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(ctx, Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-interrupt"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
	})
	if err == nil {
		t.Fatal("cancelled process must return an error")
	}
	if result.Outcome != OutcomeInterrupted || result.ExitCode == 0 || result.FailureCategory != FailureInterrupted {
		t.Fatalf("interrupted result=%+v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded termination took %s", elapsed)
	}
}

func TestOSRunnerCancellationKillsCompleteUncontrolledProcessGroup(t *testing.T) {
	directory := t.TempDir()
	leaderMarker := filepath.Join(directory, "leader.pid")
	childMarker := filepath.Join(directory, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type runResult struct {
		result Result
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(ctx, Spec{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-with-child-pids", leaderMarker, childMarker},
			StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		})
		done <- runResult{result: result, err: err}
	}()
	leaderPID := waitForPIDMarker(t, leaderMarker)
	childPID := waitForPIDMarker(t, childMarker)
	if leaderPID == childPID {
		t.Fatalf("leader and child unexpectedly share pid=%d", leaderPID)
	}
	for role, pid := range map[string]int{"leader": leaderPID, "child": childPID} {
		group, err := syscall.Getpgid(pid)
		if err != nil || group != leaderPID {
			t.Fatalf("%s pid=%d pgid=%d expected=%d err=%v", role, pid, group, leaderPID, err)
		}
	}
	cancel()
	completed := <-done
	result, err := completed.result, completed.err
	if err == nil || result.Outcome != OutcomeInterrupted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if exists, err := processGroupExists(leaderPID); err != nil || exists {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		t.Fatalf("uncontrolled process group remains: pgid=%d exists=%t err=%v", leaderPID, exists, err)
	}
}

func waitForPIDMarker(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID marker %s was not written", filepath.Base(path))
	return 0
}

func TestOSRunnerCancellationProvesDescendantProcessGroupExited(t *testing.T) {
	directory := t.TempDir()
	controlPath := filepath.Join(directory, "implementation.process-control.json")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := (OSRunner{InterruptGrace: 100 * time.Millisecond}).Run(ctx, Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-with-child"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: controlPath, ControlKey: []byte(testProcessControlKey),
	})
	if err == nil || result.Outcome != OutcomeInterrupted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	control, err := readProcessControlFile(controlPath, testProcessControlKey)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := processGroupExists(control.ProcessGroupID); err != nil || exists {
		t.Fatalf("descendant process group remains: exists=%t err=%v", exists, err)
	}
}

func TestAttemptStopperTerminatesExactManagedProcessGroup(t *testing.T) {
	directory := t.TempDir()
	control := filepath.Join(directory, "implementation.process-control.json")
	done := make(chan error, 1)
	go func() {
		_, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(context.Background(), Spec{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-interrupt"},
			StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: control, ControlKey: []byte(testProcessControlKey),
		})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(control); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("managed process control was not materialized")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := (AttemptStopper{InterruptGrace: 50 * time.Millisecond}).StopAttempt(context.Background(), directory, testProcessControlKey); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("managed process runner did not observe child termination")
	}
	if err := (AttemptStopper{}).StopAttempt(context.Background(), directory, testProcessControlKey); err != nil {
		t.Fatalf("stopped attempt did not reconcile idempotently: %v", err)
	}
}

func TestAttemptStopperRejectsMissingOrCorruptLifecycleEvidence(t *testing.T) {
	directory := t.TempDir()
	stopper := AttemptStopper{}
	if err := stopper.StopAttempt(context.Background(), directory, testProcessControlKey); err == nil {
		t.Fatal("missing lifecycle evidence was accepted")
	}
	if err := os.WriteFile(filepath.Join(directory, "implementation.process-control.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stopper.StopAttempt(context.Background(), directory, testProcessControlKey); err == nil {
		t.Fatal("corrupt unlocked lifecycle evidence was accepted")
	}
}

func TestAttemptStopperRejectsOrphanProcessLockAfterEarlierIdentity(t *testing.T) {
	directory := t.TempDir()
	completed := filepath.Join(directory, "codex-version.process-control.json")
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: completed, ControlKey: []byte(testProcessControlKey),
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	orphan := processControlLockPath(filepath.Join(directory, "implementation.process-control.json"))
	if err := os.WriteFile(orphan, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (AttemptStopper{}).StopAttempt(context.Background(), directory, testProcessControlKey); err == nil {
		t.Fatal("orphan process lock was accepted after an earlier complete identity")
	}
}

func TestAttemptStopperRequiresEveryRosteredProcessControl(t *testing.T) {
	directory := t.TempDir()
	completed := filepath.Join(directory, "codex-version.process-control.json")
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: filepath.Join(directory, "version-stdout"), StderrPath: filepath.Join(directory, "version-stderr"), ControlPath: completed, ControlKey: []byte(testProcessControlKey),
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	active := filepath.Join(directory, "implementation.process-control.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(ctx, Spec{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-interrupt"},
			StdoutPath: filepath.Join(directory, "implementation-stdout"), StderrPath: filepath.Join(directory, "implementation-stderr"), ControlPath: active, ControlKey: []byte(testProcessControlKey),
		})
		done <- runErr
	}()
	waitForProcessControl(t, active)
	if err := os.Remove(active); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := os.Remove(processControlLockPath(active)); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := (AttemptStopper{}).StopAttempt(context.Background(), directory, testProcessControlKey); err == nil {
		cancel()
		t.Fatal("older preflight evidence hid a missing active roster entry")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("test managed process did not stop")
	}
}

func TestManagedChildDoesNotInheritProcessLockDescriptor(t *testing.T) {
	directory := t.TempDir()
	control := filepath.Join(directory, "implementation.process-control.json")
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "check-lock-fd", processControlLockPath(control)},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: control, ControlKey: []byte(testProcessControlKey),
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	output, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(output) != "missing\n" {
		t.Fatalf("output=%q err=%v", output, err)
	}
	if _, err := readProcessControlFile(control, testProcessControlKey); err != nil {
		t.Fatal(err)
	}
}

func TestManagedChildCannotReleaseOrReplaceControllerLifecycleLock(t *testing.T) {
	directory := t.TempDir()
	control := filepath.Join(directory, "implementation.process-control.json")
	done := make(chan error, 1)
	go func() {
		_, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(context.Background(), Spec{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "unlock-and-ignore"},
			StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: control, ControlKey: []byte(testProcessControlKey),
		})
		done <- err
	}()
	waitForProcessControl(t, control)
	if err := (AttemptStopper{InterruptGrace: 50 * time.Millisecond}).StopAttempt(context.Background(), directory, testProcessControlKey); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("child descriptor unlocked the controller-owned lifecycle lock")
	}
}

func TestAttemptStopperAdoptsAuthenticatedLockAfterRunnerCrash(t *testing.T) {
	directory := t.TempDir()
	controlPath := filepath.Join(directory, "implementation.process-control.json")
	files, err := openProcessControl(controlPath, []byte(testProcessControlKey))
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		files.Close()
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], managedLaunchArgument, os.Args[0], "-test.run=TestProcessHelper", "--", "ignore-interrupt")
	command.Env = append(withoutEnvironment(os.Environ(), []string{managedLaunchEnvironment}), managedLaunchEnvironment+"=1")
	command.ExtraFiles = []*os.File{reader}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		files.Close()
		t.Fatal(err)
	}
	reader.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	control, err := newRuntimeProcessControl(command.Process.Pid, filepath.Base(controlPath), files.lock, []byte(testProcessControlKey))
	if err == nil {
		err = persistProcessControl(controlPath, control)
	}
	if err == nil {
		_, err = writer.Write([]byte{1})
	}
	writer.Close()
	files.Close() // Simulate the originating controller process losing its flock.
	if err != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		t.Fatal(err)
	}
	if err := (AttemptStopper{InterruptGrace: 50 * time.Millisecond}).StopAttempt(context.Background(), directory, testProcessControlKey); err != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		t.Fatal("adopted process group did not stop")
	}
}

func TestManagedLaunchGateDoesNotExecTargetAfterParentEOF(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "target-ran")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], managedLaunchArgument, os.Args[0], "-test.run=TestProcessHelper", "--", "write-marker", marker)
	command.Env = append(withoutEnvironment(os.Environ(), []string{managedLaunchEnvironment}), managedLaunchEnvironment+"=1")
	command.ExtraFiles = []*os.File{reader}
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		t.Fatal(err)
	}
	reader.Close()
	writer.Close()
	if err := command.Wait(); err == nil {
		t.Fatal("launch helper accepted EOF as release")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target executed before lifecycle release: %v", err)
	}
}

func TestOSRunnerPersistsIdentityBeforeManagedTargetExecutes(t *testing.T) {
	directory := t.TempDir()
	control := filepath.Join(directory, "implementation.process-control.json")
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "require-file", control},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: control, ControlKey: []byte(testProcessControlKey),
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestManagedSupervisorDrainsDescendantsBeforeLeaderExits(t *testing.T) {
	directory := t.TempDir()
	controlPath := filepath.Join(directory, "implementation.process-control.json")
	done := make(chan error, 1)
	go func() {
		_, err := (OSRunner{}).Run(context.Background(), Spec{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit-with-child"},
			StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: controlPath, ControlKey: []byte(testProcessControlKey),
		})
		done <- err
	}()
	waitForProcessControl(t, controlPath)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("managed leader did not exit")
	}
	control, err := readProcessControlFile(controlPath, testProcessControlKey)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := processGroupExists(control.ProcessGroupID); err != nil || exists {
		_ = syscall.Kill(-control.ProcessGroupID, syscall.SIGKILL)
		t.Fatalf("managed descendant group remains: exists=%t err=%v", exists, err)
	}
	if err := (AttemptStopper{}).StopAttempt(context.Background(), directory, testProcessControlKey); err != nil {
		t.Fatalf("drained process group did not reconcile: %v", err)
	}
}

func TestProcessIdentityMismatchFailsClosedWhileGroupExists(t *testing.T) {
	control := processControl{ProcessGroupID: syscall.Getpgrp(), ProcessStartToken: "mismatched-kernel-start-token"}
	if active, err := managedProcessControlActive(control); err == nil || active {
		t.Fatalf("mismatched identity with live process group: active=%t err=%v", active, err)
	}
}

func TestRunnerSignalRejectsDriftedStartIdentity(t *testing.T) {
	control := processControl{ProcessGroupID: syscall.Getpgrp(), ProcessStartToken: "drifted-runner-start-token"}
	if err := signalRunnerProcessGroup(control, nil, syscall.Signal(0)); err == nil {
		t.Fatal("runner cancellation accepted a drifted process-group identity")
	}
}

func TestManagedSignalRevalidatesAuthorityAfterEarlierSuccessfulCheck(t *testing.T) {
	directory := t.TempDir()
	controlPath := filepath.Join(directory, "implementation.process-control.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(ctx, Spec{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-interrupt"},
			StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: controlPath, ControlKey: []byte(testProcessControlKey),
		})
		done <- err
	}()
	waitForProcessControl(t, controlPath)
	identity, err := os.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	lock, err := os.Open(processControlLockPath(controlPath))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	control, err := readProcessControl(identity, lock, []byte(testProcessControlKey))
	if err != nil {
		t.Fatal(err)
	}
	if active, err := managedProcessControlAuthorized(control, lock); err != nil || !active {
		t.Fatalf("initial authority active=%t err=%v", active, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("managed process did not exit")
	}
	if active, err := managedProcessControlAuthorized(control, lock); err == nil && active {
		t.Fatal("controller authority remained valid after the runner exited")
	}
}

func TestAttemptStopperFailsClosedForCorruptedAuthenticatedIdentity(t *testing.T) {
	directory := t.TempDir()
	control := filepath.Join(directory, "implementation.process-control.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(ctx, Spec{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-interrupt"},
			StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"), ControlPath: control, ControlKey: []byte(testProcessControlKey),
		})
		done <- err
	}()
	waitForProcessControl(t, control)
	if err := os.Chmod(control, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, []byte(`{"schema_version":2,"process_group_id":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (AttemptStopper{}).StopAttempt(context.Background(), directory, testProcessControlKey); err == nil {
		t.Fatal("corrupted authenticated process identity was accepted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("test managed process did not terminate after cleanup cancellation")
	}
}

func waitForProcessControl(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("managed process control was not materialized")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readProcessControlFile(path, key string) (processControl, error) {
	file, err := os.Open(path)
	if err != nil {
		return processControl{}, err
	}
	defer file.Close()
	lock, err := os.Open(processControlLockPath(path))
	if err != nil {
		return processControl{}, err
	}
	defer lock.Close()
	return readProcessControl(file, lock, []byte(key))
}

func TestOSRunnerRecordsMissingExecutableAsNotStarted(t *testing.T) {
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program:    "definitely-missing-verifier",
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
	})
	if err == nil {
		t.Fatal("missing executable must fail")
	}
	if result.Outcome != OutcomeNotStarted || result.FailureCategory != FailureStart || result.ExitCode == 0 {
		t.Fatalf("start failure result=%+v", result)
	}
	if got := SanitizeError(err); got != "managed process failure: process_start" {
		t.Fatalf("sanitized error=%q", got)
	}
	for _, path := range []string{result.StdoutPath, result.StderrPath} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || len(data) != 0 {
			t.Fatalf("capture path=%q data=%q err=%v", path, data, readErr)
		}
	}
}

func TestResultCannotTreatNotStartedZeroAsSuccess(t *testing.T) {
	result := Result{Outcome: OutcomeNotStarted, FailureCategory: FailureStart, ExitCode: 0}
	if NormalizeResult(result, NewFailure(FailureStart)).Succeeded() {
		t.Fatal("not-started process must never be successful")
	}
}

func TestOSRunnerExcludesConfiguredEnvironment(t *testing.T) {
	t.Setenv("IFAN_LOOP_LINEAR_TOKEN", "secret")
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "print-token"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		ExcludedEnv: []string{"IFAN_LOOP_LINEAR_TOKEN"},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "absent\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestOSRunnerAppliesManagedEnvironmentOverrides(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "ambient-config")
	t.Setenv("GIT_AUTHOR_NAME", "ambient-author")
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "managed-environment"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		Environment: []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=managed-author"},
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "/dev/null\nmanaged-author\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestOSRunnerRestrictsEnvironmentToAllowlist(t *testing.T) {
	t.Setenv("HOME", "/managed-home")
	t.Setenv("RETAINED_SECRET", "must-not-enter")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program:              os.Args[0],
		Args:                 []string{"-test.run=TestProcessHelper", "--", "allowlist-environment"},
		StdoutPath:           filepath.Join(directory, "stdout"),
		StderrPath:           filepath.Join(directory, "stderr"),
		EnvironmentAllowlist: []string{"HOME"},
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "/managed-home\nabsent\nabsent\n"+ManagedCommandPath+"\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestControllerEnvironmentPrependsManagedCommandPath(t *testing.T) {
	environment := controllerEnvironment([]string{"PATH=/custom/bin:/usr/bin", "RETAIN=1"}, nil)
	if got, want := environment[0], "PATH="+ManagedCommandPath+":/custom/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if got := environment[1]; got != "RETAIN=1" {
		t.Fatalf("retained environment = %q", got)
	}
}

func TestEnvironmentOverridesCannotReplaceManagedPath(t *testing.T) {
	environment := applyEnvironmentOverrides([]string{"PATH=" + ManagedCommandPath, "VALUE=old"}, nil, []string{"PATH=/unsafe", "VALUE=new"})
	if got := environment[0]; got != "PATH="+ManagedCommandPath {
		t.Fatalf("PATH override escaped managed runtime: %q", got)
	}
	if got := environment[1]; got != "VALUE=new" {
		t.Fatalf("environment override = %q", got)
	}
}

func TestOSRunnerResolvesSimpleProgramFromControllerEnvironment(t *testing.T) {
	directory := t.TempDir()
	program := "controller-managed-fixture"
	if err := os.WriteFile(filepath.Join(directory, program), []byte("#!/bin/sh\nprintf 'managed\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (OSRunner{}).run(context.Background(), Spec{
		Program:     program,
		StdoutPath:  filepath.Join(directory, "stdout"),
		StderrPath:  filepath.Join(directory, "stderr"),
		WorkingDir:  directory,
		ExcludedEnv: []string{"IFAN_LOOP_LINEAR_TOKEN"},
	}, []string{"PATH=" + directory, "IFAN_LOOP_LINEAR_TOKEN=secret"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "managed\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestProcessHelper(t *testing.T) {
	for index, arg := range os.Args {
		if arg != "--" || index+1 >= len(os.Args) {
			continue
		}
		switch os.Args[index+1] {
		case "exit":
			fmt.Println("done")
			os.Exit(0)
		case "ignore-interrupt":
			signal.Ignore(os.Interrupt)
			time.Sleep(10 * time.Second)
			os.Exit(0)
		case "ignore-interrupt-pid":
			if index+2 >= len(os.Args) {
				os.Exit(2)
			}
			signal.Ignore(os.Interrupt)
			if os.WriteFile(os.Args[index+2], []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600) != nil {
				os.Exit(2)
			}
			time.Sleep(10 * time.Second)
			os.Exit(0)
		case "check-lock-fd":
			if index+2 >= len(os.Args) {
				os.Exit(2)
			}
			info, err := os.Stat(os.Args[index+2])
			expected, ok := info.Sys().(*syscall.Stat_t)
			if err != nil || !ok {
				os.Exit(2)
			}
			inherited := false
			for descriptor := 3; descriptor < 256; descriptor++ {
				var observed syscall.Stat_t
				if syscall.Fstat(descriptor, &observed) == nil && observed.Dev == expected.Dev && observed.Ino == expected.Ino {
					inherited = true
					break
				}
			}
			if inherited {
				fmt.Println("inherited")
			} else {
				fmt.Println("missing")
			}
			os.Exit(0)
		case "unlock-and-ignore":
			_ = syscall.Flock(3, syscall.LOCK_UN)
			signal.Ignore(os.Interrupt)
			time.Sleep(10 * time.Second)
			os.Exit(0)
		case "exit-with-child":
			child := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", "ignore-interrupt")
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		case "ignore-with-child":
			child := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", "ignore-interrupt")
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			signal.Ignore(os.Interrupt)
			time.Sleep(10 * time.Second)
			os.Exit(0)
		case "ignore-with-child-pids":
			if index+3 >= len(os.Args) {
				os.Exit(2)
			}
			child := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", "ignore-interrupt-pid", os.Args[index+3])
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			signal.Ignore(os.Interrupt)
			if err := os.WriteFile(os.Args[index+2], []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
				_ = child.Process.Kill()
				os.Exit(2)
			}
			time.Sleep(10 * time.Second)
			os.Exit(0)
		case "write-marker":
			if index+2 >= len(os.Args) || os.WriteFile(os.Args[index+2], []byte("ran\n"), 0o600) != nil {
				os.Exit(2)
			}
			os.Exit(0)
		case "require-file":
			if index+2 >= len(os.Args) {
				os.Exit(2)
			}
			if info, err := os.Stat(os.Args[index+2]); err != nil || info.Size() == 0 {
				os.Exit(3)
			}
			os.Exit(0)
		case "print-token":
			if _, found := os.LookupEnv("IFAN_LOOP_LINEAR_TOKEN"); found {
				fmt.Println("present")
			} else {
				fmt.Println("absent")
			}
			os.Exit(0)
		case "managed-environment":
			fmt.Println(os.Getenv("GIT_CONFIG_GLOBAL"))
			fmt.Println(os.Getenv("GIT_AUTHOR_NAME"))
			os.Exit(0)
		case "allowlist-environment":
			fmt.Println(os.Getenv("HOME"))
			if _, found := os.LookupEnv("RETAINED_SECRET"); found {
				fmt.Println("present")
			} else {
				fmt.Println("absent")
			}
			if _, found := os.LookupEnv("GIT_CONFIG_COUNT"); found {
				fmt.Println("present")
			} else {
				fmt.Println("absent")
			}
			fmt.Println(os.Getenv("PATH"))
			os.Exit(0)
		}
	}
}
