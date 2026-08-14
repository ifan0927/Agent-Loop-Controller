// Package configuration owns private raw configuration evidence, the trusted
// authority locator, and safe atomic replacement of the canonical live file.
package configuration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const (
	maximumConfigurationBytes = 256 << 10
	locatorVersion            = 1
	baselineBindingVersion    = 1
)

type Files struct {
	configPath       string
	root             string
	rawRoot          string
	uid              int
	databasePath     string
	databaseIdentity application.DatabaseFileIdentity
	beforeSwap       func()
	syncDir          func(string) error
}

type AuthorityLocator struct {
	Version          int                              `json:"version"`
	ConfigPath       string                           `json:"config_path"`
	DatabasePath     string                           `json:"database_path"`
	DatabaseIdentity application.DatabaseFileIdentity `json:"database_identity"`
}

type BaselineBinding struct {
	Version          int                              `json:"version"`
	ConfigPath       string                           `json:"config_path"`
	DatabasePath     string                           `json:"database_path"`
	DatabaseIdentity application.DatabaseFileIdentity `json:"database_identity"`
	Digest           string                           `json:"digest"`
	Size             int64                            `json:"size"`
	Schema           int                              `json:"schema_version"`
}

type replacementLock struct{ file *os.File }

func (l *replacementLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func NewFiles(configPath string) (*Files, error) {
	if !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath {
		return nil, errors.New("configuration authority path is invalid")
	}
	parent := filepath.Dir(configPath)
	if err := inspectPrivateDirectory(parent, os.Getuid(), false); err != nil {
		return nil, errors.New("configuration authority root is unsafe")
	}
	root := filepath.Join(parent, "authority")
	return &Files{configPath: configPath, root: root, rawRoot: filepath.Join(root, "generations"), uid: os.Getuid()}, nil
}

func (f *Files) CanonicalConfigPath() string { return f.configPath }

func (f *Files) BindDatabaseIdentity(databasePath string, identity application.DatabaseFileIdentity) error {
	if !filepath.IsAbs(databasePath) || filepath.Clean(databasePath) != databasePath || !identity.Valid() || !pathHasDatabaseIdentity(databasePath, identity, f.uid) {
		return errors.New("configuration database identity conflicts")
	}
	f.databasePath, f.databaseIdentity = databasePath, identity
	return nil
}

func (f *Files) ValidateBaseline(payload []byte) (application.ValidatedConfigurationCandidate, error) {
	loaded, err := bootstrap.ValidateBytes(f.configPath, payload)
	if err != nil {
		return application.ValidatedConfigurationCandidate{}, err
	}
	return candidateFromBootstrap(loaded, len(payload)), nil
}

func (f *Files) ValidateCurrent(payload []byte) (application.ValidatedConfigurationCandidate, error) {
	loaded, err := bootstrap.ValidateCurrentBytes(f.configPath, payload)
	if err != nil {
		return application.ValidatedConfigurationCandidate{}, err
	}
	return candidateFromBootstrap(loaded, len(payload)), nil
}

func (f *Files) ReadLive() ([]byte, application.ValidatedConfigurationCandidate, error) {
	payload, err := readPrivateRegular(f.configPath, f.uid, maximumConfigurationBytes, false)
	if err != nil {
		return nil, application.ValidatedConfigurationCandidate{}, errors.New("live configuration is unavailable")
	}
	candidate, err := f.ValidateBaseline(payload)
	if err != nil {
		return nil, application.ValidatedConfigurationCandidate{}, err
	}
	return payload, candidate, nil
}

func (f *Files) RetainRaw(digest string, payload []byte) error {
	if configurationDigest(payload) != digest || len(payload) > maximumConfigurationBytes {
		return errors.New("configuration raw evidence is invalid")
	}
	if err := f.ensureRoots(); err != nil {
		return err
	}
	path := f.rawPath(digest)
	if existing, err := readRetainedAfterPublication(path, f.uid); err == nil {
		if bytes.Equal(existing, payload) {
			return nil
		}
		return errors.New("configuration raw evidence conflicts")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration raw evidence is unsafe")
	}
	err := atomicPrivateWrite(path, payload, f.uid, true)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := readRetainedAfterPublication(path, f.uid)
		if readErr == nil && bytes.Equal(existing, payload) {
			return nil
		}
	}
	return err
}

func readRetainedAfterPublication(path string, uid int) ([]byte, error) {
	return readAfterExclusivePublication(path, uid, maximumConfigurationBytes)
}

func readAfterExclusivePublication(path string, uid, limit int) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 32; attempt++ {
		payload, err := readPrivateRegular(path, uid, limit, true)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return payload, err
		}
		last = err
		// Exclusive publication uses a temporary hard link and immediately
		// removes it. Yield only across that bounded two-link window; a durable
		// hard-link attack remains unsafe after the retry bound.
		runtime.Gosched()
	}
	return nil, last
}

func (f *Files) HasRaw(digest string, size int64) bool {
	if !validDigest(digest) || size < 0 || size > maximumConfigurationBytes {
		return false
	}
	payload, err := readPrivateRegular(f.rawPath(digest), f.uid, maximumConfigurationBytes, true)
	return err == nil && int64(len(payload)) == size && configurationDigest(payload) == digest
}

func (f *Files) PublishBaselineBinding(candidate application.ValidatedConfigurationCandidate) error {
	if candidate.DatabasePath != f.databasePath || !pathHasDatabaseIdentity(f.databasePath, f.databaseIdentity, f.uid) {
		return errors.New("configuration baseline binding conflicts")
	}
	value := BaselineBinding{Version: baselineBindingVersion, ConfigPath: f.configPath, DatabasePath: candidate.DatabasePath, DatabaseIdentity: f.databaseIdentity, Digest: candidate.Digest, Size: candidate.Size, Schema: candidate.SchemaVersion}
	if validateBaselineBinding(value) != nil {
		return errors.New("configuration baseline binding is invalid")
	}
	if err := f.ensureRoots(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("configuration baseline binding is invalid")
	}
	payload = append(payload, '\n')
	path := filepath.Join(f.root, "baseline.json")
	if existing, err := readAfterExclusivePublication(path, f.uid, 4096); err == nil {
		current, decodeErr := decodeBaselineBinding(existing)
		if decodeErr != nil || current != value {
			return errors.New("configuration baseline binding conflicts")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration baseline binding is unsafe")
	}
	if err := atomicPrivateWrite(path, payload, f.uid, true); !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, readErr := readAfterExclusivePublication(path, f.uid, 4096)
	current, decodeErr := decodeBaselineBinding(existing)
	if readErr != nil || decodeErr != nil || current != value {
		return errors.New("configuration baseline binding conflicts")
	}
	return nil
}

func (f *Files) ReadRaw(digest string, size int64) ([]byte, error) {
	if !validDigest(digest) || size < 0 || size > maximumConfigurationBytes {
		return nil, errors.New("configuration raw evidence is invalid")
	}
	payload, err := readPrivateRegular(f.rawPath(digest), f.uid, maximumConfigurationBytes, true)
	if err != nil || int64(len(payload)) != size || configurationDigest(payload) != digest {
		return nil, errors.New("configuration raw evidence is unavailable")
	}
	return payload, nil
}

func (f *Files) AcquireReplacement(operationID string) (application.ConfigurationReplacementLock, bool, error) {
	if _, err := f.replacementStagePath(operationID); err != nil {
		return nil, false, err
	}
	return f.AcquireMutation()
}

func (f *Files) AcquireMutation() (application.ConfigurationReplacementLock, bool, error) {
	if err := f.ensureRoots(); err != nil {
		return nil, false, err
	}
	path := filepath.Join(f.root, "mutation.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, errors.New("configuration replacement lock is unavailable")
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedBy(info, f.uid) || linkCount(info) != 1 {
		file.Close()
		return nil, false, errors.New("configuration replacement lock is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, errors.New("configuration replacement lock is unavailable")
	}
	return &replacementLock{file: file}, true, nil
}

func (f *Files) ReplaceLive(operationID string, expected, payload []byte) error {
	if len(payload) > maximumConfigurationBytes || len(expected) > maximumConfigurationBytes {
		return errors.New("configuration payload is too large")
	}
	live, err := readPrivateRegular(f.configPath, f.uid, maximumConfigurationBytes, false)
	if err != nil || !bytes.Equal(live, expected) {
		return errors.New("live configuration replacement is unsafe")
	}
	stage, err := f.replacementStagePath(operationID)
	if err != nil {
		return err
	}
	if err := atomicPrivateWrite(stage, payload, f.uid, true); err != nil {
		return errors.New("configuration replacement staging failed")
	}
	if f.beforeSwap != nil {
		f.beforeSwap()
	}
	// The exchange atomically captures the exact inode being replaced. A
	// concurrent third-party edit becomes the private stage and is restored.
	if err := atomicSwap(stage, f.configPath); err != nil {
		_ = os.Remove(stage)
		return errors.New("configuration atomic exchange failed")
	}
	replaced, readErr := readPrivateRegular(stage, f.uid, maximumConfigurationBytes, true)
	if readErr != nil || !bytes.Equal(replaced, expected) {
		if swapErr := atomicSwap(stage, f.configPath); swapErr != nil {
			return errors.New("configuration replacement conflict is ambiguous")
		}
		if f.syncParent() != nil {
			return errors.New("configuration replacement conflict is ambiguous")
		}
		_ = os.Remove(stage)
		_ = f.syncParent()
		return errors.New("live configuration changed before replacement")
	}
	if err := f.syncParent(); err != nil {
		// Keep the captured parent beside the live target. Reconciliation must
		// prove directory durability before it may commit desired authority.
		return errors.New("configuration directory synchronization is unproven")
	}
	if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration replacement cleanup failed")
	}
	if err := f.syncParent(); err != nil {
		return errors.New("configuration replacement cleanup synchronization is unproven")
	}
	return nil
}

func (f *Files) ReconcileReplacement(operationID string, expectedParent, target []byte) ([]byte, application.ValidatedConfigurationCandidate, error) {
	stage, err := f.replacementStagePath(operationID)
	if err != nil {
		return nil, application.ValidatedConfigurationCandidate{}, err
	}
	live, candidate, liveErr := f.ReadLive()
	staged, stageErr := readPrivateRegular(stage, f.uid, maximumConfigurationBytes, true)
	if errors.Is(stageErr, os.ErrNotExist) {
		if f.syncParent() != nil {
			return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration replacement cleanup synchronization is unproven")
		}
		return live, candidate, liveErr
	}
	if liveErr != nil || stageErr != nil {
		return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration replacement evidence is unsafe")
	}
	parentLive, targetLive := bytes.Equal(live, expectedParent), bytes.Equal(live, target)
	parentStaged, targetStaged := bytes.Equal(staged, expectedParent), bytes.Equal(staged, target)
	switch {
	case targetLive && parentStaged:
		if f.syncParent() != nil {
			return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration directory synchronization is unproven")
		}
	case targetLive && !parentStaged:
		// A crash after exchange captured unexpected external drift. Restore it
		// before returning the third digest for ambiguous settlement.
		if atomicSwap(stage, f.configPath) != nil || f.syncParent() != nil {
			return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration replacement conflict is ambiguous")
		}
		live, candidate, liveErr = f.ReadLive()
		if liveErr != nil {
			return nil, application.ValidatedConfigurationCandidate{}, liveErr
		}
	case (parentLive || !targetLive) && targetStaged:
		// Publication never happened or a detected conflict was already restored.
	default:
		return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration replacement evidence conflicts")
	}
	if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration replacement cleanup failed")
	}
	if f.syncParent() != nil {
		return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration replacement cleanup is unproven")
	}
	return live, candidate, nil
}

func (f *Files) replacementStagePath(operationID string) (string, error) {
	suffix, found := strings.CutPrefix(operationID, "operation-")
	decoded, decodeErr := hex.DecodeString(suffix)
	if !found || len(suffix) != 32 || decodeErr != nil || len(decoded) != 16 {
		return "", errors.New("configuration operation identity is invalid")
	}
	return filepath.Join(filepath.Dir(f.configPath), ".agentctl-config-swap-"+suffix), nil
}

func (f *Files) syncParent() error {
	if f.syncDir != nil {
		return f.syncDir(filepath.Dir(f.configPath))
	}
	return syncDirectory(filepath.Dir(f.configPath))
}

func (f *Files) RemoveRaw(digest string) error {
	if !validDigest(digest) {
		return errors.New("configuration raw digest is invalid")
	}
	path := f.rawPath(digest)
	if _, err := readPrivateRegular(path, f.uid, maximumConfigurationBytes, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("configuration raw evidence is unsafe")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration raw evidence could not be pruned")
	}
	return syncDirectory(f.rawRoot)
}

func (f *Files) PublishLocator(databasePath string) error {
	if !filepath.IsAbs(databasePath) || filepath.Clean(databasePath) != databasePath || databasePath != f.databasePath || !pathHasDatabaseIdentity(databasePath, f.databaseIdentity, f.uid) {
		return errors.New("configuration authority locator is invalid")
	}
	if err := f.ensureRoots(); err != nil {
		return err
	}
	value := AuthorityLocator{Version: locatorVersion, ConfigPath: f.configPath, DatabasePath: databasePath, DatabaseIdentity: f.databaseIdentity}
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("configuration authority locator is invalid")
	}
	payload = append(payload, '\n')
	path := filepath.Join(f.root, "locator.json")
	if existing, err := readAfterExclusivePublication(path, f.uid, 4096); err == nil {
		current, decodeErr := decodeLocator(existing)
		if decodeErr != nil || current != value {
			return errors.New("configuration authority locator conflicts")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration authority locator is unsafe")
	}
	if err := atomicPrivateWrite(path, payload, f.uid, true); !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, readErr := readAfterExclusivePublication(path, f.uid, 4096)
	current, decodeErr := decodeLocator(existing)
	if readErr != nil || decodeErr != nil || current != value {
		return errors.New("configuration authority locator conflicts")
	}
	return nil
}

// ReadLocator resolves only the fixed locator beside the requested canonical
// configuration path. It never follows a caller-provided database redirect.
func ReadLocator(configPath string) (AuthorityLocator, bool, error) {
	files, err := NewFiles(configPath)
	if err != nil {
		return AuthorityLocator{}, false, err
	}
	path := filepath.Join(files.root, "locator.json")
	payload, err := readPrivateRegular(path, files.uid, 4096, true)
	if errors.Is(err, os.ErrNotExist) {
		return AuthorityLocator{}, false, nil
	}
	if err != nil {
		return AuthorityLocator{}, false, errors.New("configuration authority locator is unsafe")
	}
	value, err := decodeLocator(payload)
	if err != nil || value.ConfigPath != configPath {
		return AuthorityLocator{}, false, errors.New("configuration authority locator conflicts")
	}
	return value, true, nil
}

func ReadBaselineBinding(configPath string) (BaselineBinding, bool, error) {
	files, err := NewFiles(configPath)
	if err != nil {
		return BaselineBinding{}, false, err
	}
	payload, err := readPrivateRegular(filepath.Join(files.root, "baseline.json"), files.uid, 4096, true)
	if errors.Is(err, os.ErrNotExist) {
		return BaselineBinding{}, false, nil
	}
	if err != nil {
		return BaselineBinding{}, false, errors.New("configuration baseline binding is unsafe")
	}
	value, err := decodeBaselineBinding(payload)
	if err != nil || value.ConfigPath != configPath {
		return BaselineBinding{}, false, errors.New("configuration baseline binding conflicts")
	}
	return value, true, nil
}

func decodeBaselineBinding(payload []byte) (BaselineBinding, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value BaselineBinding
	if err := decoder.Decode(&value); err != nil || decoder.Decode(&struct{}{}) != io.EOF || validateBaselineBinding(value) != nil {
		return BaselineBinding{}, errors.New("invalid baseline binding")
	}
	return value, nil
}

func validateBaselineBinding(value BaselineBinding) error {
	if value.Version != baselineBindingVersion || !filepath.IsAbs(value.ConfigPath) || filepath.Clean(value.ConfigPath) != value.ConfigPath || !filepath.IsAbs(value.DatabasePath) || filepath.Clean(value.DatabasePath) != value.DatabasePath || !value.DatabaseIdentity.Valid() || !validDigest(value.Digest) || value.Size < 0 || value.Size > maximumConfigurationBytes || value.Schema < 1 || value.Schema > bootstrap.CurrentVersion {
		return errors.New("invalid baseline binding")
	}
	return nil
}

func decodeLocator(payload []byte) (AuthorityLocator, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value AuthorityLocator
	if err := decoder.Decode(&value); err != nil || decoder.Decode(&struct{}{}) != io.EOF || value.Version != locatorVersion || !filepath.IsAbs(value.ConfigPath) || filepath.Clean(value.ConfigPath) != value.ConfigPath || !filepath.IsAbs(value.DatabasePath) || filepath.Clean(value.DatabasePath) != value.DatabasePath || !value.DatabaseIdentity.Valid() {
		return AuthorityLocator{}, errors.New("invalid locator")
	}
	return value, nil
}

func (f *Files) ensureRoots() error {
	if err := ensurePrivateDirectory(f.root, f.uid); err != nil {
		return errors.New("configuration authority directory is unsafe")
	}
	if err := ensurePrivateDirectory(f.rawRoot, f.uid); err != nil {
		return errors.New("configuration raw directory is unsafe")
	}
	return nil
}

func (f *Files) rawPath(digest string) string {
	if !validDigest(digest) {
		return filepath.Join(f.rawRoot, "invalid")
	}
	return filepath.Join(f.rawRoot, digest+".json")
}

func pathHasDatabaseIdentity(path string, expected application.DatabaseFileIdentity, uid int) bool {
	if !expected.Valid() {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedBy(info, uid) || linkCount(info) != 1 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	current := application.DatabaseFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	return current == expected
}

func candidateFromBootstrap(loaded bootstrap.Bootstrap, size int) application.ValidatedConfigurationCandidate {
	repositories := make(map[string]application.ConfigurationRepositoryAuthority)
	for _, binding := range loaded.Registry.Bindings() {
		trusted := make([]application.TrustedActorIdentity, 0, len(binding.OperatorIdentityPolicy.TrustedActors))
		for _, actor := range binding.OperatorIdentityPolicy.TrustedActors {
			trusted = append(trusted, application.TrustedActorIdentity{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: actor.Login, Type: actor.Type})
		}
		repositories[strings.ToLower(binding.CanonicalRepository)] = application.ConfigurationRepositoryAuthority{
			CanonicalRepository: binding.CanonicalRepository, ProfileID: binding.ProfileID, ProfileDigest: binding.ProfileDigest,
			RepositoryBindingDigest: binding.RepositoryBindingDigest, BaseBranch: binding.BaseBranch, OriginPath: binding.OriginPath,
			SourcePath: binding.SourcePath, RunRoot: binding.RunRoot, WorktreeRoot: binding.WorktreeRoot,
			VerifierRegistryRef: binding.VerifierRegistryRef, VerifierIDs: append([]string(nil), binding.VerifierIDs...),
			GitHubAppProfileRef: binding.GitHubAppProfileRef, GitHubAppID: binding.GitHubAppID,
			GitHubInstallationID: binding.GitHubInstallationID, ExpectedRepositoryID: binding.ExpectedRepositoryID,
			AllowedOperatorLogins: append([]string(nil), binding.OperatorIdentityPolicy.AllowedLogins...), TrustedOperatorActors: trusted,
		}
	}
	return application.ValidatedConfigurationCandidate{Digest: loaded.Digest, Size: int64(size), SchemaVersion: loaded.Version, DatabasePath: loaded.Controller.DatabasePath, Operator: loaded.Controller.Operator, Repositories: repositories}
}

func ensurePrivateDirectory(path string, uid int) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return inspectPrivateDirectory(path, uid, true)
}

func inspectPrivateDirectory(path string, uid int, requirePrivateMode bool) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("unsafe directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || !ownedBy(info, uid) {
		return errors.New("unsafe directory")
	}
	if requirePrivateMode && info.Mode().Perm() != 0o700 {
		return errors.New("unsafe directory mode")
	}
	return nil
}

func readPrivateRegular(path string, uid, limit int, privateMode bool) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() > int64(limit) || !ownedBy(before, uid) || linkCount(before) != 1 || privateMode && before.Mode().Perm() != 0o600 {
		return nil, errors.New("unsafe file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("file changed")
	}
	payload, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(payload) > limit {
		return nil, errors.New("file is unreadable")
	}
	after, statErr := file.Stat()
	current, currentErr := os.Lstat(path)
	if statErr != nil || currentErr != nil || !os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || after.Size() != int64(len(payload)) || !opened.ModTime().Equal(after.ModTime()) || linkCount(after) != 1 {
		return nil, errors.New("file changed")
	}
	return payload, nil
}

func atomicPrivateWrite(path string, payload []byte, uid int, requireAbsent bool) error {
	parent := filepath.Dir(path)
	if err := inspectPrivateDirectory(parent, uid, false); err != nil {
		return errors.New("configuration write directory is unsafe")
	}
	temp, err := os.CreateTemp(parent, ".agentctl-config-*")
	if err != nil {
		return errors.New("configuration staging failed")
	}
	tempPath := temp.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	defer cleanup()
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return errors.New("configuration staging failed")
	}
	if _, err := temp.Write(payload); err != nil || temp.Sync() != nil || temp.Close() != nil {
		_ = temp.Close()
		return errors.New("configuration staging failed")
	}
	if requireAbsent {
		// Link publishes the fully synced inode only when the immutable target is
		// absent. Unlike rename, it cannot overwrite a concurrent winner.
		if err := os.Link(tempPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return os.ErrExist
			}
			return errors.New("configuration exclusive publication failed")
		}
		if err := os.Remove(tempPath); err != nil {
			return errors.New("configuration staging cleanup failed")
		}
		tempPath = ""
	} else if err := os.Rename(tempPath, path); err != nil {
		return errors.New("configuration atomic replacement failed")
	} else {
		tempPath = ""
	}
	if err := syncDirectory(parent); err != nil {
		return errors.New("configuration directory synchronization failed")
	}
	if _, err := readPrivateRegular(path, uid, len(payload), true); err != nil {
		return errors.New("configuration replacement verification failed")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func configurationDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' && char < 'a' || char > 'f' {
			return false
		}
	}
	return true
}

func ownedBy(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

func linkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

var _ application.ConfigurationFileAuthority = (*Files)(nil)
