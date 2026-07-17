package process

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
	// ControlPath is a controller-owned authenticated process identity. The
	// controller alone retains the descriptor for its separately bound lock.
	ControlPath string
	// ControlKey authenticates the lifecycle identity without exposing the key
	// to the child process or its environment.
	ControlKey  []byte
	ExcludedEnv []string
	Environment []string
	// EnvironmentAllowlist retains only these inherited names before fixed
	// overrides are applied. PATH is always controller-managed.
	EnvironmentAllowlist []string
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

// ManagedCommandPath is the controller-owned search path for child binaries.
const ManagedCommandPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func (r OSRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	return r.run(ctx, spec, os.Environ())
}

func (r OSRunner) run(ctx context.Context, spec Spec, environment []string) (Result, error) {
	return r.runWithTestParentLifetime(ctx, spec, environment, goTestRuntimeActive())
}

func (r OSRunner) runWithTestParentLifetime(ctx context.Context, spec Spec, environment []string, testParentLifetime bool) (Result, error) {
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

	excludedEnvironment := append(append([]string(nil), spec.ExcludedEnv...), managedLaunchEnvironment, managedTestParentLifetimeEnvironment)
	baseEnvironment := environment
	if len(spec.EnvironmentAllowlist) > 0 {
		baseEnvironment = restrictEnvironment(environment, spec.EnvironmentAllowlist, excludedEnvironment)
	}
	commandEnvironment := controllerEnvironment(baseEnvironment, excludedEnvironment)
	commandEnvironment = applyEnvironmentOverrides(commandEnvironment, excludedEnvironment, spec.Environment)
	program, err := resolveProgram(spec.Program, commandEnvironment)
	if err != nil {
		initial.FailureCategory = FailureStart
		return initial, FailureError{Category: FailureStart}
	}
	control, err := openProcessControl(spec.ControlPath, spec.ControlKey)
	if err != nil {
		initial.FailureCategory = FailureArtifactSetup
		return initial, FailureError{Category: FailureArtifactSetup}
	}
	if control != nil {
		defer control.Close()
	}
	command := exec.Command(program, spec.Args...)
	var launchGateReader, launchGateWriter, testParentLifetimeReader, testParentLifetimeWriter *os.File
	if control != nil {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			initial.FailureCategory = FailureArtifactSetup
			return initial, FailureError{Category: FailureArtifactSetup}
		}
		launchGateReader, launchGateWriter, err = os.Pipe()
		if err != nil {
			initial.FailureCategory = FailureArtifactSetup
			return initial, FailureError{Category: FailureArtifactSetup}
		}
		defer launchGateReader.Close()
		defer launchGateWriter.Close()
		if testParentLifetime {
			testParentLifetimeReader, testParentLifetimeWriter, err = os.Pipe()
			if err != nil {
				initial.FailureCategory = FailureArtifactSetup
				return initial, FailureError{Category: FailureArtifactSetup}
			}
			defer testParentLifetimeReader.Close()
			defer testParentLifetimeWriter.Close()
		}
		command = exec.Command(executable, append([]string{managedLaunchArgument, program}, spec.Args...)...)
		command.ExtraFiles = []*os.File{launchGateReader}
		commandEnvironment = append(withoutEnvironment(commandEnvironment, []string{managedLaunchEnvironment, managedTestParentLifetimeEnvironment}), managedLaunchEnvironment+"=1")
		if testParentLifetimeReader != nil {
			command.ExtraFiles = append(command.ExtraFiles, testParentLifetimeReader)
			commandEnvironment = append(commandEnvironment, managedTestParentLifetimeEnvironment+"=1")
		}
	}
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
	if launchGateReader != nil {
		_ = launchGateReader.Close()
	}
	if testParentLifetimeReader != nil {
		_ = testParentLifetimeReader.Close()
	}
	runtimeControl, err := newRuntimeProcessControl(command.Process.Pid, "", nil, nil)
	if control != nil {
		runtimeControl, err = newRuntimeProcessControl(command.Process.Pid, filepath.Base(control.path), control.lock, spec.ControlKey)
		if err == nil {
			err = persistProcessControl(control.path, runtimeControl)
		}
	}
	if err == nil && launchGateWriter != nil {
		if _, releaseErr := launchGateWriter.Write([]byte{1}); releaseErr != nil {
			err = releaseErr
		}
		_ = launchGateWriter.Close()
	}
	if err != nil {
		if runtimeControl.ProcessStartToken != "" {
			_ = signalRunnerProcessGroup(runtimeControl, control, syscall.SIGKILL)
		}
		setupWait := make(chan error, 1)
		go func() { setupWait <- command.Wait() }()
		exitProven := false
		if runtimeControl.ProcessStartToken != "" {
			exitProven, _ = waitRunnerProcessGroup(context.Background(), runtimeControl, control, runnerInterruptGrace(r.InterruptGrace))
		}
		select {
		case <-setupWait:
		case <-time.After(runnerInterruptGrace(r.InterruptGrace)):
		}
		initial.FailureCategory = FailureArtifactSetup
		failure := error(FailureError{Category: FailureArtifactSetup})
		if !exitProven {
			failure = errors.Join(failure, ProcessGroupExitUnprovenError{})
		}
		return initial, failure
	}

	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case waitErr := <-wait:
		return processResult(command, spec.StdoutPath, spec.StderrPath, waitErr)
	case <-ctx.Done():
		_ = signalRunnerProcessGroup(runtimeControl, control, syscall.SIGINT)
		grace := runnerInterruptGrace(r.InterruptGrace)
		stopped, _ := waitRunnerProcessGroup(context.Background(), runtimeControl, control, grace)
		if !stopped {
			_ = signalRunnerProcessGroup(runtimeControl, control, syscall.SIGKILL)
			stopped, _ = waitRunnerProcessGroup(context.Background(), runtimeControl, control, grace)
		}
		if stopped {
			<-wait
		} else {
			select {
			case <-wait:
			case <-time.After(grace):
			}
		}
		failure := errors.Join(FailureError{Category: FailureInterrupted}, ctx.Err())
		if !stopped {
			failure = errors.Join(failure, ProcessGroupExitUnprovenError{})
		}
		return Result{Outcome: OutcomeInterrupted, FailureCategory: FailureInterrupted, ExitCode: -1, StdoutPath: spec.StdoutPath, StderrPath: spec.StderrPath}, failure
	}
}

// goTestRuntimeActive uses the linker-provided testing runtime marker. Command
// arguments, flag registration, executable names, and environment variables
// cannot enable the test-only parent-lifetime contract in production binaries.
func goTestRuntimeActive() bool {
	return testing.Testing()
}

func runnerInterruptGrace(value time.Duration) time.Duration {
	if value <= 0 {
		return 2 * time.Second
	}
	return value
}

func waitRunnerProcessGroup(ctx context.Context, identity processControl, files *processControlFiles, duration time.Duration) (bool, error) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		exists, err := processGroupExists(identity.ProcessGroupID)
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
		// Re-evaluate the exact leader start identity and current PGID on every
		// proof poll. Any ambiguity while the group remains is a failed proof.
		if _, err := managedProcessControlActive(identity); err != nil {
			if errors.Is(err, errManagedLeaderAbsentGroupLive) {
				select {
				case <-ctx.Done():
					return false, ctx.Err()
				case <-timer.C:
					return false, errors.New("runner process group exit could not be proven")
				case <-ticker.C:
					continue
				}
			}
			return false, err
		}
		if files != nil {
			device, inode, err := processControlFileIdentity(files.lock)
			if err != nil || device != identity.LockDevice || inode != identity.LockInode {
				return false, errors.New("runner process lifecycle lock identity changed")
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, errors.New("runner process group exit could not be proven")
		case <-ticker.C:
		}
	}
}

func signalRunnerProcessGroup(identity processControl, files *processControlFiles, signal syscall.Signal) error {
	if files != nil {
		device, inode, err := processControlFileIdentity(files.lock)
		if err != nil || device != identity.LockDevice || inode != identity.LockInode {
			return errors.New("runner process lifecycle lock identity changed")
		}
	}
	return signalObservedProcessGroup(identity, signal)
}

type processControl struct {
	SchemaVersion     int    `json:"schema_version"`
	ControlName       string `json:"control_name"`
	ProcessGroupID    int    `json:"process_group_id"`
	ProcessStartToken string `json:"process_start_token"`
	LockDevice        uint64 `json:"lock_device"`
	LockInode         uint64 `json:"lock_inode"`
	MAC               string `json:"mac"`
}

type processControlFiles struct {
	path string
	lock *os.File
}

func (f *processControlFiles) Close() {
	_ = f.lock.Close()
}

func processControlLockPath(path string) string { return path + ".lock" }

func openProcessControl(path string, key []byte) (*processControlFiles, error) {
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) || len(key) < 32 {
		return nil, errors.New("process control path must be absolute")
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, errors.New("process control identity already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lockPath := processControlLockPath(path)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		lock.Close()
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		lock.Close()
		return nil, err
	}
	if err := appendProcessControlRoster(filepath.Dir(path), filepath.Base(path), key); err != nil {
		lock.Close()
		_ = os.Remove(lockPath)
		return nil, err
	}
	return &processControlFiles{path: path, lock: lock}, nil
}

func newRuntimeProcessControl(processGroupID int, controlName string, lock *os.File, key []byte) (processControl, error) {
	if processGroupID < 1 {
		return processControl{}, errors.New("managed process group is invalid")
	}
	startToken, err := processStartToken(processGroupID)
	if err != nil {
		return processControl{}, err
	}
	control := processControl{SchemaVersion: 3, ControlName: controlName, ProcessGroupID: processGroupID, ProcessStartToken: startToken}
	if lock != nil {
		device, inode, err := processControlFileIdentity(lock)
		if err != nil {
			return processControl{}, err
		}
		control.LockDevice, control.LockInode = device, inode
		control.MAC = processControlMAC(control, key)
	}
	return control, nil
}

func persistProcessControl(path string, control processControl) error {
	if control.LockDevice == 0 || control.LockInode == 0 || control.MAC == "" {
		return errors.New("managed process control authentication is incomplete")
	}
	data, err := json.Marshal(control)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".process-control-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o400); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func processControlFileIdentity(file *os.File) (uint64, uint64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("managed process lock identity is unavailable")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func processStartToken(pid int) (string, error) {
	token, found, err := observedProcessStartToken(pid)
	if err != nil || !found {
		return "", errors.New("managed process start identity is unavailable")
	}
	return token, nil
}

func processControlMAC(control processControl, key []byte) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%d\n%s\n%d\n%s\n%d\n%d", control.SchemaVersion, control.ControlName, control.ProcessGroupID, control.ProcessStartToken, control.LockDevice, control.LockInode)
	return hex.EncodeToString(mac.Sum(nil))
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

// ProcessGroupExitUnprovenError marks a started managed process whose complete
// process-group exit could not be proven before the runner's bounded return.
// Callers must retain the attempt as active so controller-owned stop recovery
// can still act on its authenticated lifecycle evidence.
type ProcessGroupExitUnprovenError struct{}

func (ProcessGroupExitUnprovenError) Error() string { return "managed process group exit is unproven" }

func (ProcessGroupExitUnprovenError) ProcessGroupExitUnproven() bool { return true }

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
		filtered[index] = "PATH=" + mergePath(ManagedCommandPath, strings.TrimPrefix(value, "PATH="))
		return filtered
	}
	return append(filtered, "PATH="+ManagedCommandPath)
}

func applyEnvironmentOverrides(environment, excluded, overrides []string) []string {
	blocked := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		blocked[name] = struct{}{}
	}
	result := append([]string(nil), environment...)
	for _, value := range overrides {
		name, _, found := strings.Cut(value, "=")
		if !found || name == "" {
			continue
		}
		if name == "PATH" {
			continue
		}
		if _, reject := blocked[name]; reject {
			continue
		}
		filtered := result[:0]
		for _, existing := range result {
			existingName, _, existingFound := strings.Cut(existing, "=")
			if !existingFound || existingName != name {
				filtered = append(filtered, existing)
			}
		}
		result = append(filtered, value)
	}
	return result
}

func restrictEnvironment(environment, allowlist, excluded []string) []string {
	allowed := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		if name != "" && name != "PATH" {
			allowed[name] = struct{}{}
		}
	}
	blocked := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		blocked[name] = struct{}{}
	}
	filtered := make([]string, 0, len(allowed))
	for _, value := range environment {
		name, _, found := strings.Cut(value, "=")
		if !found {
			continue
		}
		if _, ok := allowed[name]; !ok {
			continue
		}
		if _, reject := blocked[name]; reject {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
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
