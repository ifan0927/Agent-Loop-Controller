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
	"reflect"
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
	syncAuthority    func(string) error
	syncRootEntry    func(string) error
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

type privateDirectorySnapshot struct {
	path string
	info os.FileInfo
}

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

func (f *Files) ProjectEditable(payload []byte) (application.ConfigurationEditableSettings, error) {
	settings, err := bootstrap.ProjectEditableSettings(f.configPath, payload)
	if err != nil {
		return application.ConfigurationEditableSettings{}, err
	}
	return application.ConfigurationEditableSettings{
		RunTimeout: application.ConfigurationDuration(settings.RunTimeout),
		Admission: application.ConfigurationEditableAdmissionSettings{
			Enabled:                       settings.AdmissionEnabled,
			PollInterval:                  application.ConfigurationDuration(settings.AdmissionPollInterval),
			DeliveryPollInterval:          application.ConfigurationDuration(settings.DeliveryPollInterval),
			SchedulerLeaseTTL:             application.ConfigurationDuration(settings.SchedulerLeaseTTL),
			SchedulerLeaseRenewalInterval: application.ConfigurationDuration(settings.SchedulerLeaseRenewalInterval),
			MaxCandidates:                 settings.AdmissionMaxCandidates,
			MaxPages:                      settings.AdmissionMaxPages,
			HeavyCapacity:                 settings.AdmissionHeavyCapacity,
		},
	}, nil
}

func (f *Files) ProjectHistoricalEditable(payload []byte, schemaVersion int) (application.ConfigurationEditableSettings, error) {
	settings, err := bootstrap.ProjectHistoricalEditableSettings(payload, schemaVersion)
	if err != nil {
		return application.ConfigurationEditableSettings{}, err
	}
	return application.ConfigurationEditableSettings{
		RunTimeout: application.ConfigurationDuration(settings.RunTimeout),
		Admission: application.ConfigurationEditableAdmissionSettings{
			Enabled: settings.AdmissionEnabled, PollInterval: application.ConfigurationDuration(settings.AdmissionPollInterval),
			DeliveryPollInterval: application.ConfigurationDuration(settings.DeliveryPollInterval), SchedulerLeaseTTL: application.ConfigurationDuration(settings.SchedulerLeaseTTL),
			SchedulerLeaseRenewalInterval: application.ConfigurationDuration(settings.SchedulerLeaseRenewalInterval), MaxCandidates: settings.AdmissionMaxCandidates,
			MaxPages: settings.AdmissionMaxPages, HeavyCapacity: settings.AdmissionHeavyCapacity,
		},
	}, nil
}

func (f *Files) MaterializeEditable(base []byte, settings application.ConfigurationEditableSettings) ([]byte, error) {
	return bootstrap.MaterializeEditableSettings(f.configPath, base, bootstrap.EditableSettings{
		RunTimeout:                    settings.RunTimeout.Duration(),
		AdmissionEnabled:              settings.Admission.Enabled,
		AdmissionPollInterval:         settings.Admission.PollInterval.Duration(),
		DeliveryPollInterval:          settings.Admission.DeliveryPollInterval.Duration(),
		SchedulerLeaseTTL:             settings.Admission.SchedulerLeaseTTL.Duration(),
		SchedulerLeaseRenewalInterval: settings.Admission.SchedulerLeaseRenewalInterval.Duration(),
		AdmissionMaxCandidates:        settings.Admission.MaxCandidates,
		AdmissionMaxPages:             settings.Admission.MaxPages,
		AdmissionHeavyCapacity:        settings.Admission.HeavyCapacity,
	})
}

func (f *Files) ValidateEditableCandidate(base, candidate []byte) (application.ValidatedConfigurationCandidate, error) {
	baseAuthority, err := f.ValidateCurrent(base)
	if err != nil {
		return application.ValidatedConfigurationCandidate{}, err
	}
	target, err := f.ValidateCurrent(candidate)
	if err != nil {
		return application.ValidatedConfigurationCandidate{}, err
	}
	if !editableAuthorityEqual(base, candidate) || baseAuthority.DatabasePath != target.DatabasePath || baseAuthority.LinearTeamKey != target.LinearTeamKey || !baseAuthority.Operator.Equal(target.Operator) || !reflect.DeepEqual(baseAuthority.Repositories, target.Repositories) {
		return application.ValidatedConfigurationCandidate{}, errors.New("configuration candidate changes non-editable authority")
	}
	return target, nil
}

// MaterializeRepositoryRemoval removes one exact validated inline repository
// profile while preserving every unrelated top-level field and repository
// entry. Raw profile bytes never leave this private configuration adapter.
func (f *Files) MaterializeRepositoryRemoval(base []byte, target application.LocalRepository) ([]byte, int, int, error) {
	loaded, err := bootstrap.ValidateCurrentBytes(f.configPath, base)
	if err != nil {
		return nil, 0, 0, err
	}
	binding, err := loaded.Registry.Resolve(target.CanonicalRepository)
	if err != nil || binding.ProfileID != target.ProfileID || binding.ProfileDigest != target.ProfileDigest || binding.RepositoryBindingDigest != target.RepositoryBindingDigest {
		return nil, 0, 0, errors.New("repository removal target authority conflicts")
	}
	var document map[string]json.RawMessage
	if err := decodeStrictRawDocument(base, &document); err != nil {
		return nil, 0, 0, err
	}
	var repositories []json.RawMessage
	if err := json.Unmarshal(document["repositories"], &repositories); err != nil {
		return nil, 0, 0, errors.New("repository collection is invalid")
	}
	before := len(repositories)
	filtered := make([]json.RawMessage, 0, max(0, before-1))
	removed := false
	for _, raw := range repositories {
		var identity struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			return nil, 0, 0, errors.New("repository identity is invalid")
		}
		canonical := strings.ToLower(strings.TrimSpace(identity.Owner)) + "/" + strings.ToLower(strings.TrimSpace(identity.Name))
		if canonical == target.CanonicalRepository {
			if removed {
				return nil, 0, 0, errors.New("repository removal target is ambiguous")
			}
			removed = true
			continue
		}
		filtered = append(filtered, append(json.RawMessage(nil), raw...))
	}
	if !removed || len(filtered) != before-1 {
		return nil, 0, 0, errors.New("repository removal target is unavailable")
	}
	repositoriesJSON, err := json.Marshal(filtered)
	if err != nil {
		return nil, 0, 0, err
	}
	document["repositories"] = repositoriesJSON
	candidate, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, 0, 0, err
	}
	candidate = append(candidate, '\n')
	return candidate, before, len(filtered), nil
}

func (f *Files) ValidateRepositoryRemovalCandidate(base, candidate []byte, target application.LocalRepository) (application.ValidatedConfigurationCandidate, error) {
	baseLoaded, err := bootstrap.ValidateCurrentBytes(f.configPath, base)
	if err != nil {
		return application.ValidatedConfigurationCandidate{}, err
	}
	targetLoaded, err := bootstrap.ValidateCurrentBytes(f.configPath, candidate)
	if err != nil {
		return application.ValidatedConfigurationCandidate{}, err
	}
	baseBinding, err := baseLoaded.Registry.Resolve(target.CanonicalRepository)
	if err != nil || baseBinding.ProfileID != target.ProfileID || baseBinding.ProfileDigest != target.ProfileDigest || baseBinding.RepositoryBindingDigest != target.RepositoryBindingDigest {
		return application.ValidatedConfigurationCandidate{}, errors.New("repository removal target authority conflicts")
	}
	if targetLoaded.Registry.HasRepository(target.CanonicalRepository) || len(baseLoaded.Registry.Bindings()) != len(targetLoaded.Registry.Bindings())+1 {
		return application.ValidatedConfigurationCandidate{}, errors.New("repository removal candidate has an invalid collection delta")
	}
	baseRepositories := candidateFromBootstrap(baseLoaded, len(base)).Repositories
	targetRepositories := candidateFromBootstrap(targetLoaded, len(candidate)).Repositories
	for repository, binding := range targetRepositories {
		baseCurrent, found := baseRepositories[repository]
		if !found || !reflect.DeepEqual(baseCurrent, binding) {
			return application.ValidatedConfigurationCandidate{}, errors.New("repository removal candidate changes unrelated repository authority")
		}
	}
	baseProtected, err := protectedConfigurationDocument(base)
	if err != nil {
		return application.ValidatedConfigurationCandidate{}, err
	}
	targetProtected, err := protectedConfigurationDocument(candidate)
	if err != nil || !bytes.Equal(baseProtected, targetProtected) {
		return application.ValidatedConfigurationCandidate{}, errors.New("repository removal candidate changes protected configuration authority")
	}
	return candidateFromBootstrap(targetLoaded, len(candidate)), nil
}

func decodeStrictRawDocument(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("configuration document contains trailing values")
	}
	return nil
}

func protectedConfigurationDocument(payload []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := decodeStrictRawDocument(payload, &document); err != nil {
		return nil, err
	}
	delete(document, "repositories")
	return json.Marshal(document)
}

func editableAuthorityEqual(base, candidate []byte) bool {
	normalize := func(payload []byte) ([]byte, bool) {
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.UseNumber()
		var document map[string]any
		if err := decoder.Decode(&document); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return nil, false
		}
		if controller, ok := document["controller"].(map[string]any); ok {
			delete(controller, "run_timeout")
		}
		if automation, ok := document["automation"].(map[string]any); ok {
			if admission, ok := automation["linear_todo_admission"].(map[string]any); ok {
				for _, field := range []string{"enabled", "poll_interval", "delivery_poll_interval", "scheduler_lease_ttl", "scheduler_lease_renewal_interval", "max_candidates", "max_pages", "heavy_capacity"} {
					delete(admission, field)
				}
				if len(admission) == 0 {
					delete(automation, "linear_todo_admission")
				}
			}
			if len(automation) == 0 {
				delete(document, "automation")
			}
		}
		normalized, err := json.Marshal(document)
		return normalized, err == nil
	}
	baseNormalized, baseOK := normalize(base)
	candidateNormalized, candidateOK := normalize(candidate)
	return baseOK && candidateOK && bytes.Equal(baseNormalized, candidateNormalized)
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
	directories, err := f.captureAuthorityDirectories(true)
	if err != nil {
		return err
	}
	syncAuthority := func(string) error { return f.syncAuthorityDirectories(directories) }
	path := f.rawPath(digest)
	if existing, err := f.readAuthorityLeaf(path, maximumConfigurationBytes, directories); err == nil {
		if bytes.Equal(existing, payload) {
			return nil
		}
		return errors.New("configuration raw evidence conflicts")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration raw evidence is unsafe")
	}
	err = atomicPrivateWrite(path, payload, f.uid, true, syncAuthority)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := f.readAuthorityLeaf(path, maximumConfigurationBytes, directories)
		if readErr == nil && bytes.Equal(existing, payload) {
			return nil
		}
	}
	return err
}

func (f *Files) readAuthorityLeaf(path string, limit int, directories []privateDirectorySnapshot) ([]byte, error) {
	return readPrivateRegularWithHook(path, f.uid, limit, true, func() error {
		return f.syncAuthorityDirectories(directories)
	})
}

func (f *Files) HasRaw(digest string, size int64) bool {
	if !validDigest(digest) || size < 0 || size > maximumConfigurationBytes {
		return false
	}
	directories, err := f.captureAuthorityDirectories(true)
	if err != nil {
		return false
	}
	payload, err := f.readAuthorityLeaf(f.rawPath(digest), maximumConfigurationBytes, directories)
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
	directories, err := f.captureAuthorityDirectories(false)
	if err != nil {
		return err
	}
	syncAuthority := func(string) error { return f.syncAuthorityDirectories(directories) }
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("configuration baseline binding is invalid")
	}
	payload = append(payload, '\n')
	path := filepath.Join(f.root, "baseline.json")
	if existing, err := f.readAuthorityLeaf(path, 4096, directories); err == nil {
		current, decodeErr := decodeBaselineBinding(existing)
		if decodeErr != nil || current != value {
			return errors.New("configuration baseline binding conflicts")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration baseline binding is unsafe")
	}
	if err := atomicPrivateWrite(path, payload, f.uid, true, syncAuthority); !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, readErr := f.readAuthorityLeaf(path, 4096, directories)
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
	directories, err := f.captureAuthorityDirectories(true)
	if err != nil {
		return nil, errors.New("configuration raw authority is unsafe")
	}
	payload, err := f.readAuthorityLeaf(f.rawPath(digest), maximumConfigurationBytes, directories)
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
	// Every cooperating Controller process locks the stable filesystem-root
	// inode. A same-UID rename can replace any user-owned descendant pathname,
	// but it cannot give a second Controller a different mutation authority.
	lockRoot := filepath.VolumeName(f.configPath) + string(filepath.Separator)
	file, err := os.Open(lockRoot)
	if err != nil {
		return nil, false, errors.New("configuration replacement lock is unavailable")
	}
	info, statErr := file.Stat()
	current, pathErr := os.Lstat(lockRoot)
	if statErr != nil || pathErr != nil || !info.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
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
	current, pathErr = os.Lstat(lockRoot)
	if pathErr != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, false, errors.New("configuration replacement lock is unsafe")
	}
	if err := f.cleanupPublicationTemps(); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, false, errors.New("configuration publication cleanup is unsafe")
	}
	return &replacementLock{file: file}, true, nil
}

func (f *Files) ReplaceLive(operationID string, expected, payload []byte) error {
	if len(payload) > maximumConfigurationBytes || len(expected) > maximumConfigurationBytes {
		return errors.New("configuration payload is too large")
	}
	if !pathHasDatabaseIdentity(f.databasePath, f.databaseIdentity, f.uid) {
		return errors.New("configuration database authority is unsafe")
	}
	live, err := readPrivateRegular(f.configPath, f.uid, maximumConfigurationBytes, false)
	if err != nil || !bytes.Equal(live, expected) {
		return errors.New("live configuration replacement is unsafe")
	}
	stage, err := f.replacementStagePath(operationID)
	if err != nil {
		return err
	}
	if err := atomicPrivateWrite(stage, payload, f.uid, true, nil); err != nil {
		return errors.New("configuration replacement staging failed")
	}
	if f.beforeSwap != nil {
		f.beforeSwap()
	}
	if !pathHasDatabaseIdentity(f.databasePath, f.databaseIdentity, f.uid) {
		_ = os.Remove(stage)
		_ = f.syncParent()
		return errors.New("configuration database authority changed before replacement")
	}
	// The exchange atomically captures the exact inode being replaced. A
	// concurrent third-party edit becomes the private stage and is restored.
	if err := atomicSwap(stage, f.configPath); err != nil {
		_ = os.Remove(stage)
		return errors.New("configuration atomic exchange failed")
	}
	if !pathHasDatabaseIdentity(f.databasePath, f.databaseIdentity, f.uid) {
		if swapErr := atomicSwap(stage, f.configPath); swapErr != nil || f.syncParent() != nil {
			return errors.New("configuration replacement conflict is ambiguous")
		}
		_ = os.Remove(stage)
		_ = f.syncParent()
		return errors.New("configuration database authority changed during replacement")
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
	if !pathHasDatabaseIdentity(f.databasePath, f.databaseIdentity, f.uid) {
		return nil, application.ValidatedConfigurationCandidate{}, errors.New("configuration database authority is unsafe")
	}
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

// ReconcileRestore resumes only one already-authorized observed digest toward
// the retained desired bytes. It never returns external raw content and keeps
// a third digest live when safe restoration is possible.
func (f *Files) ReconcileRestore(operationID, expectedObservedDigest string, desired []byte) (application.ConfigurationRestoreFileObservation, error) {
	desiredDigest := configurationDigest(desired)
	if !validDigest(expectedObservedDigest) || !validDigest(desiredDigest) || expectedObservedDigest == desiredDigest || len(desired) > maximumConfigurationBytes || !pathHasDatabaseIdentity(f.databasePath, f.databaseIdentity, f.uid) {
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe}, errors.New("configuration restore authority is invalid")
	}
	desiredCandidate, desiredErr := f.ValidateBaseline(desired)
	if desiredErr != nil || desiredCandidate.Digest != desiredDigest || desiredCandidate.DatabasePath != f.databasePath {
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe}, errors.New("configuration desired evidence is invalid")
	}
	stage, err := f.replacementStagePath(operationID)
	if err != nil {
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe}, err
	}
	live, liveCandidate, liveErr := f.ReadLive()
	if liveErr != nil || liveCandidate.DatabasePath != f.databasePath {
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe}, errors.New("configuration restore live evidence is unsafe")
	}
	liveDigest := liveCandidate.Digest
	staged, stageErr := readPrivateRegular(stage, f.uid, maximumConfigurationBytes, true)
	if errors.Is(stageErr, os.ErrNotExist) {
		if f.syncParent() != nil {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe}, errors.New("configuration restore cleanup synchronization is unproven")
		}
		switch liveDigest {
		case desiredDigest:
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileDesired, Digest: desiredDigest}, nil
		case expectedObservedDigest:
			if err := f.ReplaceLive(operationID, live, desired); err != nil {
				return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: liveDigest}, err
			}
			return f.observeRestoredDesired(desiredDigest, desired)
		default:
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileThird, Digest: liveDigest}, nil
		}
	}
	if stageErr != nil {
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: liveDigest}, errors.New("configuration restore stage is unsafe")
	}
	stagedCandidate, validateStageErr := f.ValidateBaseline(staged)
	if validateStageErr != nil || stagedCandidate.DatabasePath != f.databasePath {
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: liveDigest}, errors.New("configuration restore stage conflicts")
	}
	stageDigest := stagedCandidate.Digest
	removeStage := func() error {
		if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return f.syncParent()
	}
	switch {
	case liveDigest == desiredDigest && stageDigest == expectedObservedDigest:
		if f.syncParent() != nil || removeStage() != nil {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: liveDigest}, errors.New("configuration restore settlement is unproven")
		}
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileDesired, Digest: desiredDigest}, nil
	case liveDigest == desiredDigest && stageDigest != desiredDigest:
		// The exchange captured a concurrent third edit. Restore it before
		// classifying the accepted recovery as ambiguous.
		if f.beforeSwap != nil {
			f.beforeSwap()
		}
		if atomicSwap(stage, f.configPath) != nil {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: stageDigest}, errors.New("configuration restore conflict is ambiguous")
		}
		captured, capturedErr := readPrivateRegular(stage, f.uid, maximumConfigurationBytes, true)
		if capturedErr != nil {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: stageDigest}, errors.New("configuration restore conflict evidence is unsafe")
		}
		if !bytes.Equal(captured, desired) {
			capturedCandidate, validateCapturedErr := f.ValidateBaseline(captured)
			if validateCapturedErr != nil || capturedCandidate.DatabasePath != f.databasePath || atomicSwap(stage, f.configPath) != nil || f.syncParent() != nil {
				return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: stageDigest}, errors.New("configuration restore repeated conflict is ambiguous")
			}
			restored, candidate, readErr := f.ReadLive()
			if readErr != nil || candidate.Digest != capturedCandidate.Digest || !bytes.Equal(restored, captured) {
				return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: capturedCandidate.Digest}, errors.New("configuration restore repeated conflict restoration is unproven")
			}
			// The original third edit remains in the private stage. Preserve it
			// together with the newer live edit for a later ambiguous-recovery
			// capability; neither payload is generation history.
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileThird, Digest: capturedCandidate.Digest}, errors.New("configuration restore live file changed during conflict restoration")
		}
		if f.syncParent() != nil || removeStage() != nil {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: stageDigest}, errors.New("configuration restore conflict settlement is unproven")
		}
		restored, candidate, readErr := f.ReadLive()
		if readErr != nil || candidate.Digest != stageDigest || !bytes.Equal(restored, staged) {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: stageDigest}, errors.New("configuration restore conflict restoration is unproven")
		}
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileThird, Digest: stageDigest}, nil
	case liveDigest == expectedObservedDigest && stageDigest == desiredDigest:
		if removeStage() != nil || f.ReplaceLive(operationID, live, desired) != nil {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: liveDigest}, errors.New("configuration restore resume failed")
		}
		return f.observeRestoredDesired(desiredDigest, desired)
	case liveDigest != desiredDigest && liveDigest != expectedObservedDigest && stageDigest == desiredDigest:
		if removeStage() != nil {
			return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe, Digest: liveDigest}, errors.New("configuration restore cleanup is unproven")
		}
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileThird, Digest: liveDigest}, nil
	default:
		// Preserve contradictory private stage evidence for later recovery.
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileThird, Digest: liveDigest}, errors.New("configuration restore evidence conflicts")
	}
}

func (f *Files) observeRestoredDesired(desiredDigest string, desired []byte) (application.ConfigurationRestoreFileObservation, error) {
	live, candidate, err := f.ReadLive()
	if err != nil || candidate.DatabasePath != f.databasePath || candidate.Digest != desiredDigest || !bytes.Equal(live, desired) {
		return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileUnsafe}, errors.New("configuration restored desired evidence is unproven")
	}
	return application.ConfigurationRestoreFileObservation{State: application.ConfigurationRestoreFileDesired, Digest: desiredDigest}, nil
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
	directories, err := f.captureAuthorityDirectories(true)
	if err != nil {
		return errors.New("configuration raw authority is unsafe")
	}
	path := f.rawPath(digest)
	if _, err := readPrivateRegular(path, f.uid, maximumConfigurationBytes, true); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration raw evidence is unsafe")
	} else if err == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("configuration raw evidence could not be pruned")
		}
	}
	if err := f.syncAuthorityDirectories(directories); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration raw evidence removal is unproven")
	}
	return nil
}

func (f *Files) ListRawDigests() ([]string, error) {
	if err := f.ensureRoots(); err != nil {
		return nil, err
	}
	directories, err := f.captureAuthorityDirectories(true)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(f.rawRoot)
	if err != nil {
		return nil, errors.New("configuration raw evidence is unavailable")
	}
	digests := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		digest, found := strings.CutSuffix(name, ".json")
		if !found || !validDigest(digest) {
			return nil, errors.New("configuration raw directory is unsafe")
		}
		payload, err := f.readAuthorityLeaf(filepath.Join(f.rawRoot, name), maximumConfigurationBytes, directories)
		if err != nil || configurationDigest(payload) != digest {
			return nil, errors.New("configuration raw evidence is unsafe")
		}
		digests = append(digests, digest)
	}
	return digests, nil
}

func (f *Files) PublishLocator(databasePath string) error {
	if !filepath.IsAbs(databasePath) || filepath.Clean(databasePath) != databasePath || databasePath != f.databasePath || !pathHasDatabaseIdentity(databasePath, f.databaseIdentity, f.uid) {
		return errors.New("configuration authority locator is invalid")
	}
	if err := f.ensureRoots(); err != nil {
		return err
	}
	directories, err := f.captureAuthorityDirectories(false)
	if err != nil {
		return err
	}
	syncAuthority := func(string) error { return f.syncAuthorityDirectories(directories) }
	value := AuthorityLocator{Version: locatorVersion, ConfigPath: f.configPath, DatabasePath: databasePath, DatabaseIdentity: f.databaseIdentity}
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("configuration authority locator is invalid")
	}
	payload = append(payload, '\n')
	path := filepath.Join(f.root, "locator.json")
	if existing, err := f.readAuthorityLeaf(path, 4096, directories); err == nil {
		current, decodeErr := decodeLocator(existing)
		if decodeErr != nil || current != value {
			return errors.New("configuration authority locator conflicts")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration authority locator is unsafe")
	}
	if err := atomicPrivateWrite(path, payload, f.uid, true, syncAuthority); !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, readErr := f.readAuthorityLeaf(path, 4096, directories)
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
	if _, err := os.Lstat(files.root); errors.Is(err, os.ErrNotExist) {
		return AuthorityLocator{}, false, nil
	} else if err != nil {
		return AuthorityLocator{}, false, errors.New("configuration authority directory is unsafe")
	}
	directories, err := files.captureAuthorityDirectories(false)
	if err != nil {
		return AuthorityLocator{}, false, errors.New("configuration authority directory is unsafe")
	}
	path := filepath.Join(files.root, "locator.json")
	payload, err := files.readAuthorityLeaf(path, 4096, directories)
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
	if _, err := os.Lstat(files.root); errors.Is(err, os.ErrNotExist) {
		return BaselineBinding{}, false, nil
	} else if err != nil {
		return BaselineBinding{}, false, errors.New("configuration authority directory is unsafe")
	}
	directories, err := files.captureAuthorityDirectories(false)
	if err != nil {
		return BaselineBinding{}, false, errors.New("configuration authority directory is unsafe")
	}
	payload, err := files.readAuthorityLeaf(filepath.Join(files.root, "baseline.json"), 4096, directories)
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
	rootIdentity, err := f.syncCreatedDirectoryEntry(f.root, nil)
	if err != nil {
		return errors.New("configuration authority directory durability is unavailable")
	}
	if err := ensurePrivateDirectory(f.rawRoot, f.uid); err != nil {
		return errors.New("configuration raw directory is unsafe")
	}
	if _, err := f.syncCreatedDirectoryEntry(f.rawRoot, rootIdentity); err != nil {
		return errors.New("configuration raw directory durability is unavailable")
	}
	return nil
}

func (f *Files) syncCreatedDirectoryEntry(path string, expectedParent os.FileInfo) (os.FileInfo, error) {
	identity, err := syncPrivateDirectoryEntry(path, f.uid, expectedParent)
	if err != nil {
		return nil, err
	}
	if f.syncRootEntry != nil {
		if err := f.syncRootEntry(path); err != nil {
			return nil, err
		}
	}
	return identity, nil
}

func (f *Files) inspectAuthorityRoots(includeRaw bool) error {
	if err := inspectPrivateDirectory(f.root, f.uid, true); err != nil {
		return err
	}
	if includeRaw {
		return inspectPrivateDirectory(f.rawRoot, f.uid, true)
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
	return application.ValidatedConfigurationCandidate{Digest: loaded.Digest, Size: int64(size), SchemaVersion: loaded.Version, DatabasePath: loaded.Controller.DatabasePath, LinearTeamKey: loaded.Linear.TeamKey, Operator: loaded.Controller.Operator, Repositories: repositories}
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
	return readPrivateRegularWithHook(path, uid, limit, privateMode, nil)
}

func readPrivateRegularWithHook(path string, uid, limit int, privateMode bool, afterRead func() error) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !privateRegularInfoSafe(before, uid, privateMode) || before.Size() > int64(limit) {
		return nil, errors.New("unsafe file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !privateRegularInfoSafe(opened, uid, privateMode) {
		return nil, errors.New("file changed")
	}
	payload, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(payload) > limit {
		return nil, errors.New("file is unreadable")
	}
	if afterRead != nil {
		if err := afterRead(); err != nil {
			return nil, err
		}
	}
	after, statErr := file.Stat()
	current, currentErr := os.Lstat(path)
	if statErr != nil || currentErr != nil || !os.SameFile(opened, after) || !os.SameFile(opened, current) || !privateRegularInfoSafe(after, uid, privateMode) || !privateRegularInfoSafe(current, uid, privateMode) || opened.Size() != after.Size() || after.Size() != int64(len(payload)) || !opened.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("file changed")
	}
	return payload, nil
}

func privateRegularInfoSafe(info os.FileInfo, uid int, privateMode bool) bool {
	return info != nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && ownedBy(info, uid) && linkCount(info) == 1 && (!privateMode || info.Mode().Perm() == 0o600)
}

func atomicPrivateWrite(path string, payload []byte, uid int, requireAbsent bool, syncer func(string) error) error {
	parent := filepath.Dir(path)
	if err := inspectPrivateDirectory(parent, uid, false); err != nil {
		return errors.New("configuration write directory is unsafe")
	}
	temp, err := os.CreateTemp(parent, ".agentctl-config-tmp-*")
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
		// The OS no-replace rename publishes one fully synced, single-link inode
		// atomically. There is no crash window with both temp and final hard links.
		if err := atomicPublishExclusive(tempPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return os.ErrExist
			}
			return errors.New("configuration exclusive publication failed")
		}
		tempPath = ""
	} else if err := os.Rename(tempPath, path); err != nil {
		return errors.New("configuration atomic replacement failed")
	} else {
		tempPath = ""
	}
	if syncer == nil {
		syncer = syncDirectory
	}
	verified, err := readPrivateRegularWithHook(path, uid, len(payload), true, func() error { return syncer(parent) })
	if err != nil || !bytes.Equal(verified, payload) {
		return errors.New("configuration replacement verification failed")
	}
	return nil
}

func (f *Files) cleanupPublicationTemps() error {
	seen := map[string]struct{}{}
	for _, directory := range []string{filepath.Dir(f.configPath), f.root, f.rawRoot} {
		if _, duplicate := seen[directory]; duplicate {
			continue
		}
		seen[directory] = struct{}{}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		removed := false
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".agentctl-config-tmp-") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedBy(info, f.uid) || linkCount(info) != 1 {
				return errors.New("unsafe configuration publication temp")
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			removed = true
		}
		if removed && syncDirectory(directory) != nil {
			return errors.New("configuration publication cleanup is unproven")
		}
	}
	return nil
}

func (f *Files) captureAuthorityDirectories(includeRaw bool) ([]privateDirectorySnapshot, error) {
	paths := []string{f.root}
	if includeRaw {
		paths = append(paths, f.rawRoot)
	}
	return f.capturePrivateDirectories(paths...)
}

func (f *Files) capturePrivateDirectories(paths ...string) ([]privateDirectorySnapshot, error) {
	snapshots := make([]privateDirectorySnapshot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil || !privateDirectoryInfoSafe(info, f.uid) {
			return nil, errors.New("configuration authority directory is unsafe")
		}
		snapshots = append(snapshots, privateDirectorySnapshot{path: path, info: info})
	}
	return snapshots, nil
}

func (f *Files) syncAuthorityDirectories(snapshots []privateDirectorySnapshot) error {
	if len(snapshots) == 0 {
		return errors.New("configuration authority directory is unavailable")
	}
	directories := make([]*os.File, 0, len(snapshots))
	opened := make([]os.FileInfo, 0, len(snapshots))
	defer func() {
		for _, directory := range directories {
			_ = directory.Close()
		}
	}()
	for _, snapshot := range snapshots {
		before, err := os.Lstat(snapshot.path)
		if err != nil || !os.SameFile(snapshot.info, before) || !privateDirectoryInfoSafe(before, f.uid) {
			return errors.New("configuration authority directory is unsafe")
		}
		directory, err := os.OpenFile(snapshot.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return errors.New("configuration authority directory is unsafe")
		}
		directories = append(directories, directory)
		info, err := directory.Stat()
		if err != nil || !os.SameFile(before, info) || !privateDirectoryInfoSafe(info, f.uid) {
			return errors.New("configuration authority directory changed")
		}
		opened = append(opened, info)
	}
	if f.syncAuthority != nil {
		if err := f.syncAuthority(snapshots[len(snapshots)-1].path); err != nil {
			return err
		}
	} else {
		if err := directories[len(directories)-1].Sync(); err != nil {
			return err
		}
	}
	for index, snapshot := range snapshots {
		after, statErr := directories[index].Stat()
		current, currentErr := os.Lstat(snapshot.path)
		if statErr != nil || currentErr != nil || !os.SameFile(opened[index], after) || !os.SameFile(opened[index], current) || !privateDirectoryInfoSafe(after, f.uid) || !privateDirectoryInfoSafe(current, f.uid) {
			return errors.New("configuration authority directory changed")
		}
	}
	return nil
}

func privateDirectoryInfoSafe(info os.FileInfo, uid int) bool {
	return info != nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() && info.Mode().Perm() == 0o700 && ownedBy(info, uid)
}

func syncPrivateDirectoryEntry(path string, uid int, expectedParent os.FileInfo) (os.FileInfo, error) {
	parent := filepath.Dir(path)
	childBefore, err := os.Lstat(path)
	if err != nil || !privateDirectoryInfoSafe(childBefore, uid) {
		return nil, errors.New("configuration directory entry is unsafe")
	}
	child, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, errors.New("configuration directory entry is unsafe")
	}
	defer child.Close()
	childOpened, err := child.Stat()
	if err != nil || !os.SameFile(childBefore, childOpened) || !privateDirectoryInfoSafe(childOpened, uid) {
		return nil, errors.New("configuration directory entry changed")
	}
	parentBefore, err := os.Lstat(parent)
	if err != nil || !ownedDirectoryInfoSafe(parentBefore, uid) || expectedParent != nil && !os.SameFile(expectedParent, parentBefore) {
		return nil, errors.New("configuration directory parent is unsafe")
	}
	parentDirectory, err := os.OpenFile(parent, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, errors.New("configuration directory parent is unsafe")
	}
	defer parentDirectory.Close()
	parentOpened, err := parentDirectory.Stat()
	if err != nil || !os.SameFile(parentBefore, parentOpened) || !ownedDirectoryInfoSafe(parentOpened, uid) {
		return nil, errors.New("configuration directory parent changed")
	}
	if err := parentDirectory.Sync(); err != nil {
		return nil, err
	}
	childAfter, childStatErr := child.Stat()
	childCurrent, childPathErr := os.Lstat(path)
	parentAfter, parentStatErr := parentDirectory.Stat()
	parentCurrent, parentPathErr := os.Lstat(parent)
	if childStatErr != nil || childPathErr != nil || parentStatErr != nil || parentPathErr != nil ||
		!os.SameFile(childOpened, childAfter) || !os.SameFile(childOpened, childCurrent) || !privateDirectoryInfoSafe(childAfter, uid) || !privateDirectoryInfoSafe(childCurrent, uid) ||
		!os.SameFile(parentOpened, parentAfter) || !os.SameFile(parentOpened, parentCurrent) || !ownedDirectoryInfoSafe(parentAfter, uid) || !ownedDirectoryInfoSafe(parentCurrent, uid) {
		return nil, errors.New("configuration directory entry changed")
	}
	return childOpened, nil
}

func ownedDirectoryInfoSafe(info os.FileInfo, uid int) bool {
	return info != nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() && ownedBy(info, uid)
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
