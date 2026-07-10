package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type Spec struct {
	Program      string
	Args         []string
	WorkingDir   string
	Stdin        string
	StdoutPath   string
	StderrPath   string
	MustNotExist []string
}

type Result struct {
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	StdoutPath string
	StderrPath string
}

type Runner interface {
	Run(context.Context, Spec) (Result, error)
}

type OSRunner struct {
	InterruptGrace time.Duration
}

func (r OSRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	for _, path := range spec.MustNotExist {
		if err := requireAbsent(path); err != nil {
			return Result{}, err
		}
	}
	stdoutFile, err := openExclusive(spec.StdoutPath)
	if err != nil {
		return Result{}, fmt.Errorf("open stdout capture: %w", err)
	}
	defer stdoutFile.Close()
	stderrFile, err := openExclusive(spec.StderrPath)
	if err != nil {
		return Result{}, fmt.Errorf("open stderr capture: %w", err)
	}
	defer stderrFile.Close()

	command := exec.Command(spec.Program, spec.Args...)
	command.Dir = spec.WorkingDir
	command.Stdin = bytes.NewBufferString(spec.Stdin)
	command.Stdout = stdoutFile
	command.Stderr = stderrFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return Result{}, fmt.Errorf("start %s: %w", spec.Program, err)
	}

	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case waitErr := <-wait:
		return processResult(command, spec.StdoutPath, spec.StderrPath, waitErr)
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGINT)
		grace := r.InterruptGrace
		if grace <= 0 {
			grace = 2 * time.Second
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-wait:
		case <-timer.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-wait
		}
		return Result{ExitCode: command.ProcessState.ExitCode(), StdoutPath: spec.StdoutPath, StderrPath: spec.StderrPath}, ctx.Err()
	}
}

func openExclusive(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("capture path must not be empty")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

func requireAbsent(path string) error {
	if path == "" {
		return errors.New("must-not-exist path must not be empty")
	}
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("output leaf already exists: %s", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output leaf %s: %w", path, err)
	}
	return nil
}

func processResult(command *exec.Cmd, stdoutPath, stderrPath string, waitErr error) (Result, error) {
	result := Result{ExitCode: command.ProcessState.ExitCode(), StdoutPath: stdoutPath, StderrPath: stderrPath}
	if waitErr == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return result, nil
	}
	return result, fmt.Errorf("wait for %s: %w", command.Path, waitErr)
}
