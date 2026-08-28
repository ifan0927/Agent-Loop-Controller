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
	if !validUpgradeID(id) {
		return journal{}, "", errors.New("upgrade identifier is invalid")
	}
	bundle := m.bundlePath(id)
	info, err := os.Lstat(bundle)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByUID(info, m.uid) {
		return journal{}, "", errors.New("upgrade bundle is unavailable")
	}
	var active struct {
		UpgradeID string `json:"upgrade_id"`
	}
	if err := readPrivateJSON(m.activePath(), m.uid, &active); err != nil || active.UpgradeID != id {
		return journal{}, "", errors.New("upgrade is not the active managed bundle")
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
	for _, phase := range []string{"prepared", "replacement_intent", "replacement_committed", "rollback_intent", "bootstrap_intent", "healthy", "attention", "rolled_back", "rollback_healthy", "cleanup_intent"} {
		validPhase = validPhase || value.Phase == phase
	}
	if value.SchemaVersion != journalSchemaVersion || value.UpgradeID != id || !validPhase || value.Supervisor != "launchagent" && value.Supervisor != "launchdaemon" || !validRevision(value.Revision) || !canonicalAbsolute(value.BinaryPath) || !canonicalAbsolute(value.ConfigPath) || !canonicalAbsolute(value.DatabasePath) || value.ConfigDigest == "" || !validBuildInfo(value.Candidate.Build) || value.Candidate.Digest == "" || value.Previous.Digest == "" || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return errors.New("upgrade journal is invalid")
	}
	if value.BootstrapIntentAt != nil && value.Phase != "bootstrap_intent" && value.Phase != "healthy" && value.Phase != "attention" && value.Phase != "cleanup_intent" {
		return errors.New("upgrade journal bootstrap authority is contradictory")
	}
	return nil
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
