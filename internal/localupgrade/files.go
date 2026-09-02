package localupgrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func ensurePrivateDirectory(path string, uid int) error {
	if !canonicalAbsolute(path) {
		return errors.New("private upgrade directory is invalid")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errors.New("private upgrade directory is unavailable")
	}
	return validatePrivateDirectory(path, uid)
}

func validatePrivateDirectory(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByUID(info, uid) {
		return errors.New("private upgrade directory is unsafe")
	}
	return nil
}

func safeRegularFile(path string, uid int, executable bool) (os.FileInfo, *syscall.Stat_t, error) {
	if !canonicalAbsolute(path) {
		return nil, nil, errors.New("file path is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ownedByUID(info, uid) {
		return nil, nil, errors.New("file is unsafe")
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return nil, nil, errors.New("file is not executable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return nil, nil, errors.New("file identity is unavailable")
	}
	return info, stat, nil
}

func ownedByUID(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

func safePrivateFileHandle(file *os.File, uid int, expectedSize int64) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByUID(info, uid) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1 && (expectedSize <= 0 || info.Size() == expectedSize)
}

func copyPrivateArtifact(source, destination string, uid int) error {
	sourceFile, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("upgrade artifact source is unavailable")
	}
	defer sourceFile.Close()
	sourceInfo, err := sourceFile.Stat()
	if err != nil || !sourceInfo.Mode().IsRegular() || !ownedByUID(sourceInfo, uid) {
		return errors.New("upgrade artifact source is unsafe")
	}
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("upgrade artifact could not be created")
	}
	remove := true
	defer func() {
		_ = target.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	written, err := io.Copy(target, io.LimitReader(sourceFile, 1<<30))
	if err != nil || written != sourceInfo.Size() || !safePrivateFileHandle(target, uid, written) {
		return errors.New("upgrade artifact copy is incomplete")
	}
	if err := target.Sync(); err != nil || target.Close() != nil {
		return errors.New("upgrade artifact could not be synchronized")
	}
	remove = false
	return syncDirectory(filepath.Dir(destination))
}

func writePrivateJSON(path string, value any, uid int) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return errors.New("upgrade evidence could not be encoded")
	}
	raw = append(raw, '\n')
	if len(raw) > 256<<10 {
		return errors.New("upgrade evidence is too large")
	}
	return atomicWritePrivate(path, raw, uid)
}

func atomicWritePrivate(path string, raw []byte, uid int) error {
	parent := filepath.Dir(path)
	if err := ensurePrivateDirectory(parent, uid); err != nil {
		return err
	}
	temporary := filepath.Join(parent, "."+filepath.Base(path)+".tmp")
	if exists(temporary) {
		info, stat, err := safeRegularFile(temporary, uid, false)
		if err != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
			return errors.New("interrupted durable upgrade evidence is unsafe")
		}
		if err := os.Remove(temporary); err != nil || syncDirectory(parent) != nil {
			return errors.New("interrupted durable upgrade evidence could not be reconciled")
		}
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("durable upgrade evidence could not be created")
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(raw); err != nil || !safePrivateFileHandle(file, uid, int64(len(raw))) {
		return errors.New("durable upgrade evidence could not be written")
	}
	if err := file.Sync(); err != nil || file.Close() != nil {
		return errors.New("durable upgrade evidence could not be synchronized")
	}
	if err := os.Rename(temporary, path); err != nil {
		return errors.New("durable upgrade evidence could not be published")
	}
	remove = false
	return syncDirectory(parent)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return errors.New("upgrade directory synchronization failed")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("upgrade directory synchronization failed")
	}
	return nil
}

func writeJournal(bundle string, value journal, uid int) error {
	if value.Phase == "successor_recovery_intent" || value.DatabaseRecovery != nil {
		return errors.New("historical database relocation recovery evidence is read-only")
	}
	value.UpdatedAt = value.UpdatedAt.UTC()
	return writePrivateJSON(filepath.Join(bundle, "journal.json"), value, uid)
}

func readPrivateJSON(path string, uid int, destination any) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("upgrade evidence is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 2 || info.Size() > 256<<10 || !safePrivateFileHandle(file, uid, info.Size()) {
		return errors.New("upgrade evidence is unsafe")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 256<<10+1))
	if err != nil || len(raw) > 256<<10 {
		return errors.New("upgrade evidence is unreadable")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("upgrade evidence is invalid")
	}
	return nil
}

func (m *Manager) loadJournal(id string) (journal, string, error) {
	value, bundle, err := m.loadBundleJournal(id)
	if err != nil {
		return journal{}, "", err
	}
	active, err := m.readActiveUpgrade()
	if err != nil || active.UpgradeID != id {
		return journal{}, "", errors.New("upgrade is not the active managed bundle")
	}
	return value, bundle, nil
}

type activeUpgrade struct {
	UpgradeID string `json:"upgrade_id"`
}

func (m *Manager) readActiveUpgrade() (activeUpgrade, error) {
	var active activeUpgrade
	if err := readPrivateJSON(m.activePath(), m.uid, &active); err != nil || !validUpgradeID(active.UpgradeID) {
		return activeUpgrade{}, errors.New("active upgrade pointer is unavailable")
	}
	return active, nil
}

func (m *Manager) writeActiveUpgrade(id string) error {
	if !validUpgradeID(id) {
		return errors.New("active upgrade identifier is invalid")
	}
	return writePrivateJSON(m.activePath(), activeUpgrade{UpgradeID: id}, m.uid)
}

func (m *Manager) loadBundleJournal(id string) (journal, string, error) {
	if !validUpgradeID(id) {
		return journal{}, "", errors.New("upgrade identifier is invalid")
	}
	bundle := m.bundlePath(id)
	info, err := os.Lstat(bundle)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByUID(info, m.uid) {
		return journal{}, "", errors.New("upgrade bundle is unavailable")
	}
	var value journal
	if err := readPrivateJSON(filepath.Join(bundle, "journal.json"), m.uid, &value); err != nil {
		return journal{}, "", err
	}
	if err := validateJournal(value, id); err != nil {
		return journal{}, "", err
	}
	return value, bundle, nil
}

func validateJournal(value journal, id string) error {
	validPhase := false
	for _, phase := range []string{"prepared", "replacement_intent", "replacement_committed", "rollback_intent", "bootstrap_intent", "healthy", "attention", "rolled_back", "rollback_healthy", "cleanup_intent", "successor_recovery_intent", "successor_prepare_intent", "superseded"} {
		validPhase = validPhase || value.Phase == phase
	}
	if value.SchemaVersion < 1 || value.SchemaVersion > journalSchemaVersion || value.UpgradeID != id || !validPhase || value.Supervisor != "launchagent" && value.Supervisor != "launchdaemon" || !validRevision(value.Revision) || !canonicalAbsolute(value.BinaryPath) || !canonicalAbsolute(value.ConfigPath) || !canonicalAbsolute(value.DatabasePath) || value.ConfigDigest == "" || !validBuildInfo(value.Candidate.Build) || value.Candidate.Digest == "" || value.Previous.Digest == "" || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return errors.New("upgrade journal is invalid")
	}
	if value.BootstrapIntentAt != nil && value.Phase != "bootstrap_intent" && value.Phase != "healthy" && value.Phase != "attention" && value.Phase != "cleanup_intent" && value.Phase != "successor_recovery_intent" && value.Phase != "successor_prepare_intent" && value.Phase != "superseded" {
		return errors.New("upgrade journal bootstrap authority is contradictory")
	}
	if value.PredecessorID != "" && (!validUpgradeID(value.PredecessorID) || value.PredecessorID == id) {
		return errors.New("upgrade journal predecessor link is invalid")
	}
	if value.SuccessorID != "" && (!validUpgradeID(value.SuccessorID) || value.SuccessorID == id || value.SuccessorID == value.PredecessorID || !validRevision(value.SuccessorRevision)) {
		return errors.New("upgrade journal successor link is invalid")
	}
	if value.SuccessorID == "" && value.SuccessorRevision != "" {
		return errors.New("upgrade journal successor revision is unbound")
	}
	if value.Phase == "successor_prepare_intent" && (value.BootstrapIntentAt == nil || !eligibleSuccessorReason(value.FailureReason) || value.SuccessorID == "" || value.SupersededAt != nil) {
		return errors.New("upgrade journal successor intent is invalid")
	}
	if value.Phase == "successor_recovery_intent" && (value.BootstrapIntentAt == nil || !eligibleSuccessorReason(value.FailureReason) || value.SuccessorID == "" || value.SupersededAt != nil || value.DatabaseRecovery == nil || value.DatabaseRecovery.LocatorPublishedAt != nil) {
		return errors.New("upgrade journal successor recovery intent is invalid")
	}
	if value.Phase == "superseded" && (value.BootstrapIntentAt == nil || !eligibleSuccessorReason(value.FailureReason) || value.SuccessorID == "" || value.SupersededAt == nil) {
		return errors.New("upgrade journal supersession is invalid")
	}
	if value.Phase != "successor_recovery_intent" && value.Phase != "successor_prepare_intent" && value.Phase != "superseded" && value.SuccessorID != "" {
		return errors.New("upgrade journal successor link is contradictory")
	}
	if value.Phase != "superseded" && value.SupersededAt != nil {
		return errors.New("upgrade journal supersession time is contradictory")
	}
	if value.SchemaVersion == 1 && (value.PredecessorID != "" || value.SuccessorID != "" || value.SupersededAt != nil) {
		return errors.New("legacy upgrade journal contains successor evidence")
	}
	if value.DatabaseRecovery != nil {
		recovery := value.DatabaseRecovery
		if value.SchemaVersion < 3 || recovery.Version < 1 || recovery.Version > databaseRecoveryEvidenceVersion || !validSHA256(recovery.PreviewDigest) || recovery.OldDatabase.Device == 0 || recovery.OldDatabase.Inode == 0 || recovery.ReplacementDatabase.Device == 0 || recovery.ReplacementDatabase.Inode == 0 || recovery.OldDatabase == recovery.ReplacementDatabase || recovery.ReplacementDatabase.SchemaVersion != value.Candidate.Build.SupportedControllerSchemaVersion || !validRevision(recovery.SuccessorRevision) || recovery.SuccessorRevision != value.SuccessorRevision || !recovery.DatabaseRelocationConfirmed || !recovery.FullBackupConfirmed || recovery.IntentAt.IsZero() || !validReplacementVerification(recovery.Verification, value.FailureReason, recovery.Version) {
			return errors.New("upgrade journal database recovery evidence is invalid")
		}
		if value.Phase != "successor_recovery_intent" && value.Phase != "successor_prepare_intent" && value.Phase != "superseded" {
			return errors.New("upgrade journal database recovery evidence is contradictory")
		}
		if value.Phase != "successor_recovery_intent" && recovery.LocatorPublishedAt == nil {
			return errors.New("upgrade journal database recovery publication is unavailable")
		}
		if value.Phase == "successor_recovery_intent" && value.Database != recovery.OldDatabase || value.Phase != "successor_recovery_intent" && value.Database != recovery.ReplacementDatabase {
			return errors.New("upgrade journal database recovery identity is contradictory")
		}
	} else if value.Phase == "successor_recovery_intent" {
		return errors.New("upgrade journal database recovery evidence is unavailable")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validReplacementVerification(value replacementDatabaseVerification, reason string, version int) bool {
	if !validSHA256(value.ContentDigest) || !validSHA256(value.AuthorityDigest) || value.SchemaVersion <= 0 || !value.IntegrityOK || !value.ForeignKeysOK || !value.BindingMatches || !value.DesiredConfigurationMatch {
		return false
	}
	if version == 1 {
		return value.LegacyReadinessReason == reason && value.Readiness == (recoveryReadinessVerification{})
	}
	if version != databaseRecoveryEvidenceVersion || value.LegacyReadinessReason != "" {
		return false
	}
	readiness := value.Readiness
	if readiness.PredecessorReason != reason || readiness.CurrentGeneration < 0 || readiness.PublishedGeneration < 0 {
		return false
	}
	if readiness.GenerationRelationship != classifyIntegrityGeneration(readiness.CurrentGeneration, readiness.PublishedGeneration) {
		return false
	}
	switch readiness.Relationship {
	case recoveryReadinessExactMatch:
		return readiness.ReplacementReason == reason
	case recoveryReadinessIntegrityConflictPending:
		return reason == "integrity_conflict" && readiness.ReplacementReason == "integrity_pending" && readiness.GenerationRelationship == integrityGenerationAdvanced && readiness.CurrentGeneration > readiness.PublishedGeneration && readiness.CurrentObservationValid && validIntegrityReadiness(readiness.ObservationReadiness)
	default:
		return false
	}
}

func classifyIntegrityGeneration(current, published int64) integrityGenerationRelationship {
	if current == published {
		return integrityGenerationCurrent
	}
	if current > published {
		return integrityGenerationAdvanced
	}
	return integrityGenerationPublishedAhead
}

func validIntegrityReadiness(value string) bool {
	switch value {
	case "ready", "not_ready", "unknown", "conflict":
		return true
	default:
		return false
	}
}

func validUpgradeID(value string) bool {
	if !stringsHasPrefixLength(value, "upgrade-", 40) {
		return false
	}
	for _, character := range value[len("upgrade-"):] {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func stringsHasPrefixLength(value, prefix string, length int) bool {
	return len(value) == length && len(value) >= len(prefix) && value[:len(prefix)] == prefix
}
