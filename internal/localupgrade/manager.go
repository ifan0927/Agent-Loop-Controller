package localupgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	stdbuildinfo "debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	"github.com/ifan0927/Agent-Loop-Controller/internal/buildidentity"
)

type Manager struct {
	home                  string
	launchDaemonDirectory string
	uid                   int
	user                  string
	now                   func() time.Time
	runner                commandRunner
	fail                  func(string) error
}

func (m *Manager) failAt(point string) error {
	if m.fail == nil {
		return nil
	}
	return m.fail(point)
}

func NewManager() (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("operator home is unavailable")
	}
	current, err := user.Current()
	if err != nil || current.Username == "" || os.Geteuid() == 0 {
		return nil, errors.New("upgrade must run as the configured non-root worker user")
	}
	return &Manager{home: home, launchDaemonDirectory: "/Library/LaunchDaemons", uid: os.Getuid(), user: current.Username, now: func() time.Time { return time.Now().UTC() }, runner: osCommandRunner{}}, nil
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, directory, name string, args ...string) (commandResult, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	var stdout, stderr boundedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := commandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return result, nil
		}
		return result, err
	}
	return result, nil
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	const maximum = 1 << 20
	remaining := maximum - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(len(p), remaining)])
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return append([]byte(nil), b.Buffer.Bytes()...) }

func (m *Manager) controllerRoot() string {
	return filepath.Join(m.home, "Library", "Application Support", "agent-loop-controller")
}

func (m *Manager) upgradeRoot() string { return filepath.Join(m.controllerRoot(), "local-upgrades") }
func (m *Manager) activePath() string  { return filepath.Join(m.upgradeRoot(), "active.json") }
func (m *Manager) bundlePath(id string) string {
	return filepath.Join(m.upgradeRoot(), id)
}

func (m *Manager) withGlobalLock(fn func() error) error {
	return m.withCommandLock(true, fn)
}

func (m *Manager) withActiveLock(id string, fn func() error) error {
	if !validUpgradeID(id) {
		return errors.New("upgrade identifier is invalid")
	}
	return m.withCommandLock(false, fn)
}

func (m *Manager) withCommandLock(create bool, fn func() error) error {
	rootCheck, upgradeCheck := validatePrivateDirectory, validatePrivateDirectory
	lockFlags := os.O_RDWR | syscall.O_NOFOLLOW
	if create {
		rootCheck, upgradeCheck = ensurePrivateDirectory, ensurePrivateDirectory
		lockFlags |= os.O_CREATE
	}
	if err := rootCheck(m.controllerRoot(), m.uid); err != nil {
		return err
	}
	if err := upgradeCheck(m.upgradeRoot(), m.uid); err != nil {
		return err
	}
	path := filepath.Join(m.upgradeRoot(), "upgrade.lock")
	file, err := os.OpenFile(path, lockFlags, 0o600)
	if err != nil {
		return errors.New("upgrade lock is unavailable")
	}
	defer file.Close()
	if !safePrivateFileHandle(file, m.uid, 0) {
		return errors.New("upgrade lock is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another upgrade command is active")
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func (m *Manager) Prepare(ctx context.Context, request PrepareRequest) (result Result, finalErr error) {
	if !validRevision(request.Revision) || request.Supervisor != "launchagent" && request.Supervisor != "launchdaemon" || !canonicalAbsolute(request.BinaryPath) {
		return Result{}, errors.New("prepare requires an exact revision, explicit supervisor, and canonical absolute binary")
	}
	if request.ConfigPath == "" {
		request.ConfigPath = filepath.Join(m.controllerRoot(), "controller.json")
	}
	if !canonicalAbsolute(request.ConfigPath) {
		return Result{}, errors.New("configuration path is invalid")
	}
	err := m.withGlobalLock(func() error {
		if exists(m.activePath()) {
			return errors.New("an active managed upgrade already exists")
		}
		loaded, err := bootstrap.Load(request.ConfigPath)
		if err != nil || loaded.Path != request.ConfigPath {
			return errors.New("configuration evidence is unavailable")
		}
		previous, err := inspectBinary(ctx, m.runner, request.BinaryPath, m.uid)
		if err != nil || previous.GoVersion == "" {
			return errors.New("previous binary Go build metadata is unavailable")
		}
		database, err := inspectDatabaseReadOnly(loaded.Controller.DatabasePath, m.uid)
		if err != nil {
			return err
		}
		resolved, err := m.runner.Run(ctx, "", "git", "rev-parse", "--verify", request.Revision+"^{commit}")
		if err != nil || resolved.ExitCode != 0 || strings.TrimSpace(string(resolved.Stdout)) != request.Revision {
			return errors.New("candidate revision is unavailable")
		}
		prepareRoot, err := os.MkdirTemp(m.upgradeRoot(), ".prepare-")
		if err != nil {
			return errors.New("candidate workspace is unavailable")
		}
		defer os.RemoveAll(prepareRoot)
		if err := os.Chmod(prepareRoot, 0o700); err != nil {
			return errors.New("candidate workspace is unavailable")
		}
		worktree := filepath.Join(prepareRoot, "worktree")
		added, err := m.runner.Run(ctx, "", "git", "worktree", "add", "--detach", worktree, request.Revision)
		if err != nil || added.ExitCode != 0 {
			return errors.New("isolated candidate worktree could not be created")
		}
		defer m.runner.Run(context.Background(), "", "git", "worktree", "remove", "--force", worktree)
		if clean, err := gitWorktreeClean(ctx, m.runner, worktree); err != nil || !clean {
			return errors.New("candidate worktree is dirty or unverifiable")
		}
		gate, err := m.runner.Run(ctx, worktree, filepath.Join(worktree, "scripts", "verify-controller.sh"))
		if err != nil || gate.ExitCode != 0 {
			return errors.New("candidate verification gate failed")
		}
		candidatePath := filepath.Join(prepareRoot, "agentctl")
		built, err := m.runner.Run(ctx, worktree, "go", "build", "-trimpath", "-o", candidatePath, "./cmd/agentctl")
		if err != nil || built.ExitCode != 0 {
			return errors.New("candidate build failed")
		}
		if clean, err := gitWorktreeClean(ctx, m.runner, worktree); err != nil || !clean {
			return errors.New("candidate build modified its exact worktree")
		}
		candidate, err := inspectBinary(ctx, m.runner, candidatePath, m.uid)
		if err != nil || candidate.GoVersion == "" || candidate.ModulePath != "github.com/ifan0927/Agent-Loop-Controller/cmd/agentctl" || candidate.GoVCSRevision != request.Revision || candidate.GoVCSModified || candidate.GoVCSTime == "" || !candidate.Structured || candidate.Build.VCSRevision != candidate.GoVCSRevision || candidate.Build.VCSTime != candidate.GoVCSTime || candidate.Build.VCSModified != candidate.GoVCSModified || candidate.Build.SupportedControllerSchemaVersion < database.SchemaVersion {
			return errors.New("candidate build identity is unverifiable or incompatible")
		}
		id := "upgrade-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		bundle := m.bundlePath(id)
		if err := os.Mkdir(bundle, 0o700); err != nil {
			return errors.New("private upgrade bundle could not be created")
		}
		bundleCreated := true
		defer func() {
			if bundleCreated {
				_ = os.RemoveAll(bundle)
			}
		}()
		if !sameFilesystem(bundle, request.BinaryPath) {
			return errors.New("candidate cannot be staged on the installed target filesystem")
		}
		if err := copyPrivateArtifact(candidatePath, filepath.Join(bundle, "candidate.bin"), m.uid); err != nil {
			return err
		}
		if err := copyPrivateArtifact(request.BinaryPath, filepath.Join(bundle, "previous.bin"), m.uid); err != nil {
			return err
		}
		now := m.now()
		manifest := candidateManifest{SchemaVersion: journalSchemaVersion, Revision: request.Revision, Candidate: candidate, Previous: previous, Database: database, ConfigDigest: loaded.Digest, PreparedAt: now}
		if err := writePrivateJSON(filepath.Join(bundle, "candidate-manifest.json"), manifest, m.uid); err != nil {
			return err
		}
		j := journal{SchemaVersion: journalSchemaVersion, UpgradeID: id, Phase: "prepared", Supervisor: request.Supervisor, Revision: request.Revision, BinaryPath: request.BinaryPath, ConfigPath: request.ConfigPath, DatabasePath: loaded.Controller.DatabasePath, ConfigDigest: loaded.Digest, Candidate: candidate, Previous: previous, Database: database, CreatedAt: now, UpdatedAt: now}
		if err := writeJournal(bundle, j, m.uid); err != nil {
			return err
		}
		if err := writePrivateJSON(m.activePath(), struct {
			UpgradeID string `json:"upgrade_id"`
		}{id}, m.uid); err != nil {
			return err
		}
		bundleCreated = false
		result = resultFor(j, "prepared", "candidate_verified", "bootout_selected_supervisor")
		return nil
	})
	if err != nil {
		finalErr = err
		return Result{}, err
	}
	return result, nil
}

func gitWorktreeClean(ctx context.Context, runner commandRunner, path string) (bool, error) {
	result, err := runner.Run(ctx, path, "git", "status", "--porcelain=v1", "--untracked-files=all")
	return err == nil && result.ExitCode == 0 && len(bytes.TrimSpace(result.Stdout)) == 0, err
}

func inspectBinary(ctx context.Context, runner commandRunner, path string, uid int) (binaryEvidence, error) {
	info, stat, err := safeRegularFile(path, uid, true)
	if err != nil || stat.Nlink != 1 || stat.Uid == 0 {
		return binaryEvidence{}, errors.New("installed binary is unsafe or unsupported")
	}
	digest, err := digestFile(path)
	if err != nil {
		return binaryEvidence{}, errors.New("binary digest is unavailable")
	}
	evidence := binaryEvidence{Digest: digest, Size: info.Size(), Mode: uint32(info.Mode().Perm())}
	if metadata, metadataErr := stdbuildinfo.ReadFile(path); metadataErr == nil {
		evidence.GoVersion = metadata.GoVersion
		evidence.ModulePath = metadata.Main.Path
		for _, setting := range metadata.Settings {
			switch setting.Key {
			case "vcs.revision":
				evidence.GoVCSRevision = strings.ToLower(setting.Value)
			case "vcs.time":
				evidence.GoVCSTime = setting.Value
			case "vcs.modified":
				evidence.GoVCSModified = setting.Value == "true"
			}
		}
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	structured, runErr := runner.Run(inspectCtx, "", path, "version", "--json")
	if runErr == nil && structured.ExitCode == 0 {
		decoder := json.NewDecoder(bytes.NewReader(structured.Stdout))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&evidence.Build) == nil && decoder.Decode(&struct{}{}) == io.EOF && validBuildInfo(evidence.Build) {
			evidence.Structured = true
			return evidence, nil
		}
	}
	plain, runErr := runner.Run(inspectCtx, "", path, "version")
	if runErr != nil || plain.ExitCode != 0 {
		return binaryEvidence{}, errors.New("binary build metadata is unavailable")
	}
	value := strings.TrimSpace(string(plain.Stdout))
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
		return binaryEvidence{}, errors.New("legacy binary version evidence is invalid")
	}
	evidence.LegacyVersion = value
	return evidence, nil
}

func validBuildInfo(info buildidentity.Info) bool {
	return info.ProductVersion != "" && strings.HasPrefix(info.BuildIdentity, "sha256:") && len(info.BuildIdentity) == 71 && info.SupportedControllerSchemaVersion > 0
}

func validRevision(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func canonicalAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.TrimSpace(path) == path
}

func resultFor(j journal, state, reason, next string) Result {
	result := Result{UpgradeID: j.UpgradeID, State: state, Reason: reason, NextAction: next, Supervisor: j.Supervisor, CandidateBuild: j.Candidate.Build.BuildIdentity, BootstrapIntent: j.BootstrapIntentAt != nil, UpgradeHealth: "pending", ControllerReadiness: "unknown"}
	switch state {
	case "healthy", "rollback_healthy", "observed_healthy", "cleaned":
		result.UpgradeHealth, result.ControllerReadiness = "healthy", "ready"
	case "pending":
		if reason == "integrity_pending" {
			result.UpgradeHealth, result.ControllerReadiness = "healthy", "pending"
		}
	case "attention":
		result.UpgradeHealth, result.ControllerReadiness = "failed", "conflict"
		if strings.HasPrefix(reason, "integrity_") || reason == "configuration_not_converged" {
			result.UpgradeHealth, result.ControllerReadiness = "healthy", "not_ready"
		}
	}
	return result
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 1<<30)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func sameFilesystem(directory, target string) bool {
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		return false
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return false
	}
	directoryStat, ok1 := directoryInfo.Sys().(*syscall.Stat_t)
	targetStat, ok2 := targetInfo.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && directoryStat.Dev == targetStat.Dev
}
