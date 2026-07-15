package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	ExcludedEnv  []string
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

const managedCommandPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func (r OSRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	return r.run(ctx, spec, os.Environ())
}

func (r OSRunner) run(ctx context.Context, spec Spec, environment []string) (Result, error) {
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

	commandEnvironment := controllerEnvironment(environment, spec.ExcludedEnv)
	program, err := resolveProgram(spec.Program, commandEnvironment)
	if err != nil {
		return Result{}, fmt.Errorf("resolve %s: %w", spec.Program, err)
	}
	command := exec.Command(program, spec.Args...)
	command.Dir = spec.WorkingDir
	command.Stdin = bytes.NewBufferString(spec.Stdin)
	command.Stdout = stdoutFile
	command.Stderr = stderrFile
	command.Env = commandEnvironment
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

func withoutEnvironment(environment, excluded []string) []string {
	blocked := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		blocked[name] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, found := strings.Cut(value, "=")
		if found {
			if _, reject := blocked[name]; reject {
				continue
			}
		}
		filtered = append(filtered, value)
	}
	return filtered
}

// controllerEnvironment keeps controller-managed commands independent of the
// sparse PATH supplied by launchd while preserving other required environment
// values after controller-owned exclusions are applied.
func controllerEnvironment(environment, excluded []string) []string {
	filtered := withoutEnvironment(environment, excluded)
	for index, value := range filtered {
		if !strings.HasPrefix(value, "PATH=") {
			continue
		}
		filtered[index] = "PATH=" + mergePath(managedCommandPath, strings.TrimPrefix(value, "PATH="))
		return filtered
	}
	return append(filtered, "PATH="+managedCommandPath)
}

func mergePath(prefix, current string) string {
	parts := strings.Split(prefix, ":")
	for _, entry := range strings.Split(current, ":") {
		found := false
		for _, existing := range parts {
			if entry == existing {
				found = true
				break
			}
		}
		if !found && entry != "" {
			parts = append(parts, entry)
		}
	}
	return strings.Join(parts, ":")
}

// resolveProgram applies the command environment's PATH before exec.Command
// captures its executable path from the controller process environment.
func resolveProgram(program string, environment []string) (string, error) {
	if strings.TrimSpace(program) == "" {
		return "", errors.New("program must not be blank")
	}
	if strings.ContainsRune(program, os.PathSeparator) {
		return program, nil
	}
	path := ""
	for _, value := range environment {
		name, current, found := strings.Cut(value, "=")
		if found && name == "PATH" {
			path = current
			break
		}
	}
	for _, directory := range filepath.SplitList(path) {
		if directory == "" {
			continue
		}
		candidate := filepath.Join(directory, program)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%w: %s", exec.ErrNotFound, program)
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
