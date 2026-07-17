package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const interruptedRunnerFixtureChildEnvironment = "IFAN_LOOP_TEST_INTERRUPTED_RUNNER_CHILD"

// TestInterruptedTestRunnerSelfReapsManagedLaunch proves that a managed group
// created inside a Go test cannot outlive abrupt loss of that test runner. Its
// cleanup fallback still uses only authenticated attempt evidence.
func TestInterruptedTestRunnerSelfReapsManagedLaunch(t *testing.T) {
	if os.Getenv(interruptedRunnerFixtureChildEnvironment) == "1" {
		runInterruptedRunnerFixtureChild(t)
		return
	}

	root, err := os.MkdirTemp("", "ifan-loop-interrupted-runner-")
	if err != nil {
		t.Fatal(err)
	}
	controlPath := filepath.Join(root, "implementation.process-control.json")
	targetPIDPath := filepath.Join(root, "target.pid")
	descendantPIDPath := filepath.Join(root, "descendant.pid")
	cleanup := &interruptedRunnerCleanup{
		root:              root,
		controlPath:       controlPath,
		targetPIDPath:     targetPIDPath,
		descendantPIDPath: descendantPIDPath,
	}
	t.Cleanup(func() {
		if err := cleanup.finish(); err != nil {
			t.Errorf("interrupted runner cleanup: %v", err)
		}
	})

	child := newInterruptedRunnerFixtureChild(root)
	cleanup.child = child
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}

	waitForProcessControl(t, controlPath)
	targetPID := waitForPIDMarker(t, targetPIDPath)
	descendantPID := waitForPIDMarker(t, descendantPIDPath)
	control, err := readProcessControlFile(controlPath, testProcessControlKey)
	if err != nil {
		t.Fatal(err)
	}
	cleanup.processGroupID = control.ProcessGroupID
	if control.ProcessGroupID == child.Process.Pid {
		t.Fatalf("managed supervisor reused runner-parent pid=%d", child.Process.Pid)
	}
	roster, err := readProcessControlRoster(filepath.Join(root, processControlRosterName), []byte(testProcessControlKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Entries) != 1 || roster.Entries[0] != filepath.Base(controlPath) {
		t.Fatalf("authenticated roster entries=%v", roster.Entries)
	}
	identity, err := os.OpenFile(controlPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(processControlLockPath(controlPath), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		identity.Close()
		t.Fatal(err)
	}
	authenticated, err := readProcessControl(identity, lock, []byte(testProcessControlKey))
	if err != nil {
		identity.Close()
		lock.Close()
		t.Fatal(err)
	}
	active, err := managedProcessControlAuthorized(authenticated, lock)
	if err != nil || !active {
		identity.Close()
		lock.Close()
		t.Fatalf("managed group before interruption active=%t err=%v", active, err)
	}
	observedStart, found, err := observedProcessStartToken(authenticated.ProcessGroupID)
	if err != nil || !found || observedStart != authenticated.ProcessStartToken {
		identity.Close()
		lock.Close()
		t.Fatalf("kernel start identity found=%t match=%t err=%v", found, observedStart == authenticated.ProcessStartToken, err)
	}
	members, err := observedProcessGroupMembers(authenticated.ProcessGroupID)
	if err != nil {
		identity.Close()
		lock.Close()
		t.Fatal(err)
	}
	if !containsPID(members, authenticated.ProcessGroupID) || !containsPID(members, targetPID) || !containsPID(members, descendantPID) {
		identity.Close()
		lock.Close()
		t.Fatalf("authenticated group pgid=%d members=%v target_pid=%d descendant_pid=%d", authenticated.ProcessGroupID, members, targetPID, descendantPID)
	}
	for role, pid := range map[string]int{"target": targetPID, "descendant": descendantPID} {
		if group, err := syscall.Getpgid(pid); err != nil || group != authenticated.ProcessGroupID {
			identity.Close()
			lock.Close()
			t.Fatalf("%s pid=%d pgid=%d expected=%d err=%v", role, pid, group, authenticated.ProcessGroupID, err)
		}
	}
	identity.Close()
	lock.Close()
	t.Logf("armed test-parent lifetime runner_parent_pid=%d authenticated_pgid=%d target_pid=%d descendant_pid=%d roster=%s start_identity_match=true lock_device=%d lock_inode=%d members=%v",
		child.Process.Pid, authenticated.ProcessGroupID, targetPID, descendantPID, roster.Entries[0], authenticated.LockDevice, authenticated.LockInode, members)

	// Interrupt only the dedicated runner-parent test subprocess. The managed
	// supervisor has its own authenticated process group and is not signalled;
	// closing the test-only lifetime pipe must make it reap that exact group.
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := child.Wait()
	cleanup.childWaited = true
	if waitErr == nil {
		t.Fatal("interrupted runner-parent unexpectedly exited successfully")
	}
	if err := waitForProcessGroupAbsent(control.ProcessGroupID, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Logf("test-parent EOF self-reap proved pgid=%d absent without AttemptStopper", control.ProcessGroupID)

	if err := cleanup.finish(); err != nil {
		t.Fatal(err)
	}
	t.Log("isolated reproduction root removed")
}

func TestInterruptedRunnerCleanupOwnsEarlyFailure(t *testing.T) {
	root, err := os.MkdirTemp("", "ifan-loop-interrupted-runner-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	cleanup := &interruptedRunnerCleanup{
		root:              root,
		controlPath:       filepath.Join(root, "implementation.process-control.json"),
		targetPIDPath:     filepath.Join(root, "target.pid"),
		descendantPIDPath: filepath.Join(root, "descendant.pid"),
	}
	t.Cleanup(func() {
		if err := cleanup.finish(); err != nil {
			t.Errorf("early interrupted runner cleanup: %v", err)
		}
	})
	cleanup.child = newInterruptedRunnerFixtureChild(root)
	if err := cleanup.child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("early cleanup root still exists: %v", err)
	}
}

func newInterruptedRunnerFixtureChild(root string) *exec.Cmd {
	child := exec.Command(os.Args[0], "-test.run=^TestInterruptedTestRunnerSelfReapsManagedLaunch$")
	child.Env = append(withoutEnvironment(os.Environ(), []string{interruptedRunnerFixtureChildEnvironment}),
		interruptedRunnerFixtureChildEnvironment+"=1",
		"IFAN_LOOP_TEST_INTERRUPTED_RUNNER_ROOT="+root,
	)
	child.Stdout = os.Stderr
	child.Stderr = os.Stderr
	return child
}

type interruptedRunnerCleanup struct {
	root              string
	controlPath       string
	targetPIDPath     string
	descendantPIDPath string
	child             *exec.Cmd
	childWaited       bool
	processGroupID    int
	finished          bool
}

func (c *interruptedRunnerCleanup) finish() error {
	if c.finished {
		return nil
	}
	if err := c.stopAndWaitChild(2 * time.Second); err != nil {
		return err
	}

	control, err := readProcessControlFile(c.controlPath, testProcessControlKey)
	if err == nil {
		c.processGroupID = control.ProcessGroupID
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read authenticated cleanup control: %w", err)
	}

	if c.processGroupID > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := (AttemptStopper{InterruptGrace: 100 * time.Millisecond}).StopAttempt(ctx, c.root, testProcessControlKey); err != nil {
			return fmt.Errorf("stop authenticated fixture group: %w", err)
		}
		if err := waitForProcessGroupAbsent(c.processGroupID, 2*time.Second); err != nil {
			return err
		}
	} else {
		// Without authenticated control there is no known group to signal. The
		// exact runner parent has exited, closing the unreleased launch gate; a
		// target marker here would contradict that safe pre-release lifecycle.
		for _, marker := range []string{c.targetPIDPath, c.descendantPIDPath} {
			if _, err := os.Lstat(marker); err == nil || !errors.Is(err, os.ErrNotExist) {
				return errors.New("managed target ran without authenticated cleanup control")
			}
		}
	}

	if err := os.RemoveAll(c.root); err != nil {
		return err
	}
	if _, err := os.Stat(c.root); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("isolated fixture root still exists: %w", err)
	}
	c.finished = true
	return nil
}

func (c *interruptedRunnerCleanup) stopAndWaitChild(timeout time.Duration) error {
	if c.child == nil || c.childWaited || c.child.Process == nil {
		return nil
	}
	if err := c.child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill exact fixture runner: %w", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- c.child.Wait() }()
	select {
	case <-waited:
		c.childWaited = true
		return nil
	case <-time.After(timeout):
		return errors.New("exact fixture runner did not exit")
	}
}

func runInterruptedRunnerFixtureChild(t *testing.T) {
	root := os.Getenv("IFAN_LOOP_TEST_INTERRUPTED_RUNNER_ROOT")
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		t.Fatal("reproduction root must be an absolute clean path")
	}
	controlPath := filepath.Join(root, "implementation.process-control.json")
	targetPIDPath := filepath.Join(root, "target.pid")
	descendantPIDPath := filepath.Join(root, "descendant.pid")
	_, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0],
		Args: []string{
			"-test.run=TestProcessHelper", "--", "ignore-with-child-pids", targetPIDPath, descendantPIDPath,
		},
		StdoutPath:  filepath.Join(root, "stdout"),
		StderrPath:  filepath.Join(root, "stderr"),
		ControlPath: controlPath,
		ControlKey:  []byte(testProcessControlKey),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func containsPID(pids []int, target int) bool {
	for _, pid := range pids {
		if pid == target {
			return true
		}
	}
	return false
}

func waitForProcessGroupAbsent(processGroupID int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		exists, err := processGroupExists(processGroupID)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("managed process group remained after test-parent loss")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
