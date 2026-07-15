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

// Outcome records whether the child process reached a waitable execution
// boundary. ExitCode is meaningful only for OutcomeExited.
type Outcome string

const (
	OutcomeNotStarted  Outcome = "not_started"
	OutcomeExited      Outcome = "exited"
	OutcomeInterrupted Outcome = "interrupted"
)

// FailureCategory is a controller-owned, sanitized classification. It never
// contains the child process error text.
type FailureCategory string

const (
	FailureNone          FailureCategory = ""
	FailureArtifactSetup FailureCategory = "artifact_setup"
	FailureNotStarted    FailureCategory = "not_started"
	FailureStart         FailureCategory = "process_start"
	FailureInterrupted   FailureCategory = "process_interrupted"
	FailureWait          FailureCategory = "process_wait"
	FailureInvalidResult FailureCategory = "invalid_result"
	FailureUnknown       FailureCategory = "unknown"
)

type Result struct {
	Outcome         Outcome
	FailureCategory FailureCategory
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutPath      string
	StderrPath      string
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
	initial := Result{Outcome: OutcomeNotStarted, FailureCategory: FailureUnknown, ExitCode: -1, StdoutPath: spec.StdoutPath, StderrPath: spec.StderrPath}
	if err := ctx.Err(); err != nil {
		initial.FailureCategory = FailureNotStarted
		return initial, errors.Join(FailureError{Category: FailureNotStarted}, err)
	}
	for _, path := range spec.MustNotExist {
		if err := requireAbsent(path); err != nil {
			initial.FailureCategory = FailureArtifactSetup
			return initial, FailureError{Category: FailureArtifactSetup}
		}
	}
	stdoutFile, err := openExclusive(spec.StdoutPath)
	if err != nil {
		initial.FailureCategory = FailureArtifactSetup
		return initial, FailureError{Category: FailureArtifactSetup}
	}
	defer stdoutFile.Close()
	stderrFile, err := openExclusive(spec.StderrPath)
	if err != nil {
		initial.FailureCategory = FailureArtifactSetup
		return initial, FailureError{Category: FailureArtifactSetup}
	}
	defer stderrFile.Close()

	commandEnvironment := controllerEnvironment(environment, spec.ExcludedEnv)
	program, err := resolveProgram(spec.Program, commandEnvironment)
	if err != nil {
		initial.FailureCategory = FailureStart
		return initial, FailureError{Category: FailureStart}
	}
	command := exec.Command(program, spec.Args...)
	command.Dir = spec.WorkingDir
	command.Stdin = bytes.NewBufferString(spec.Stdin)
	command.Stdout = stdoutFile
	command.Stderr = stderrFile
	command.Env = commandEnvironment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		initial.FailureCategory = FailureStart
		return initial, FailureError{Category: FailureStart}
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
		return Result{Outcome: OutcomeInterrupted, FailureCategory: FailureInterrupted, ExitCode: -1, StdoutPath: spec.StdoutPath, StderrPath: spec.StderrPath}, errors.Join(FailureError{Category: FailureInterrupted}, ctx.Err())
	}
}

// FailureError deliberately omits the underlying process error. Controller
// state and evidence must not expose command output, environment values, or
// arbitrary child-process text.
type FailureError struct {
	Category FailureCategory
}

func (e FailureError) Error() string {
	if e.Category == "" {
		return "managed process failure"
	}
	return "managed process failure: " + string(e.Category)
}

func NewFailure(category FailureCategory) error {
	return FailureError{Category: category}
}

// NormalizeResult makes an adapter result fail closed before another layer
// records or authorizes it.
func NormalizeResult(result Result, runErr error) Result {
	if result.Outcome == "" {
		result.Outcome = OutcomeNotStarted
		result.FailureCategory = FailureInvalidResult
	}
	if runErr != nil && result.Outcome == OutcomeExited && result.FailureCategory == FailureNone {
		result.Outcome = OutcomeInterrupted
		result.FailureCategory = FailureUnknown
	}
	if result.Outcome != OutcomeExited {
		result.ExitCode = -1
	}
	return result
}

func (r Result) Valid() bool {
	switch r.Outcome {
	case OutcomeNotStarted, OutcomeInterrupted:
		return r.ExitCode == -1 && r.FailureCategory != FailureNone
	case OutcomeExited:
		return r.FailureCategory == FailureNone
	default:
		return false
	}
}

func (r Result) Succeeded() bool {
	return r.Valid() && r.Outcome == OutcomeExited && r.ExitCode == 0
}

// SanitizeError maps arbitrary adapter errors to a small controller-owned
// vocabulary. Known context errors remain distinguishable without retaining
// their text.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	var failure FailureError
	if errors.As(err, &failure) {
		return failure.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "managed process interrupted: canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "managed process interrupted: deadline exceeded"
	}
	return "managed process failure: unknown"
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
	result := Result{Outcome: OutcomeExited, ExitCode: command.ProcessState.ExitCode(), StdoutPath: stdoutPath, StderrPath: stderrPath}
	if waitErr == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return result, nil
	}
	return Result{Outcome: OutcomeInterrupted, FailureCategory: FailureWait, ExitCode: -1, StdoutPath: stdoutPath, StderrPath: stderrPath}, FailureError{Category: FailureWait}
}
