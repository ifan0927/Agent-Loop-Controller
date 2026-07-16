package process

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	managedLaunchArgument    = "--ifan-loop-managed-launch"
	managedLaunchEnvironment = "IFAN_LOOP_INTERNAL_MANAGED_LAUNCH"
	managedLaunchGateFD      = 3
)

// init implements the controller-owned half of a launch gate. The helper is
// already the authenticated process-group leader, but it cannot execute the
// requested program until its parent persists lifecycle identity and releases
// the gate. Parent death closes the pipe and makes the helper exit instead.
func init() {
	if os.Getenv(managedLaunchEnvironment) != "1" || len(os.Args) < 3 || os.Args[1] != managedLaunchArgument {
		return
	}
	if !awaitManagedLaunchGate() {
		os.Exit(126)
	}
	environment := withoutEnvironment(os.Environ(), []string{managedLaunchEnvironment})
	program := os.Args[2]
	supervisorSignals := make(chan os.Signal, 2)
	signal.Notify(supervisorSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(supervisorSignals)
	target := exec.Command(program, os.Args[3:]...)
	target.Stdin, target.Stdout, target.Stderr = os.Stdin, os.Stdout, os.Stderr
	target.Env = environment
	if err := target.Start(); err != nil {
		os.Exit(127)
	}
	// The supervisor remains the authenticated group leader while the trusted
	// Codex target and its process-group members run. It exits only after every
	// other member of that controller-owned process group is gone.
	waitErr := target.Wait()
	drainManagedLaunchGroup()
	if waitErr == nil {
		os.Exit(0)
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	os.Exit(125)
}

func awaitManagedLaunchGate() bool {
	gate := os.NewFile(managedLaunchGateFD, "managed-launch-gate")
	if gate == nil {
		return false
	}
	var release [1]byte
	_, err := io.ReadFull(gate, release[:])
	_ = gate.Close()
	return err == nil && release[0] == 1
}

func drainManagedLaunchGroup() {
	group := syscall.Getpgrp()
	for {
		members, err := observedProcessGroupMembers(group)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		remaining := false
		for _, pid := range members {
			if pid == os.Getpid() {
				continue
			}
			remaining = true
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if !remaining {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func observedProcessGroupMembers(group int) ([]int, error) {
	command := exec.Command("/bin/ps", "-axo", "pid=,pgid=,stat=")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var members []int
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || strings.HasPrefix(fields[2], "Z") {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		pgid, groupErr := strconv.Atoi(fields[1])
		if pidErr == nil && groupErr == nil && pgid == group {
			members = append(members, pid)
		}
	}
	return members, nil
}
