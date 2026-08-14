package configuration

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestPrivateRawLocatorAndAtomicLiveReplacement(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("private generation bytes")
	digest := configurationDigest(payload)
	if err := files.RetainRaw(digest, payload); err != nil {
		t.Fatal(err)
	}
	if !files.HasRaw(digest, int64(len(payload))) {
		t.Fatal("retained raw evidence was not exact")
	}
	if got, err := files.ReadRaw(digest, int64(len(payload))); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("raw=%q err=%v", got, err)
	}
	for path, mode := range map[string]os.FileMode{files.root: 0o700, files.rawRoot: 0o700, files.rawPath(digest): 0o600} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != mode || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("path=%s mode=%v err=%v", path, info.Mode(), err)
		}
	}
	databasePath := filepath.Join(root, "controller.db")
	bindTestDatabase(t, files, databasePath)
	if err := files.PublishLocator(databasePath); err != nil {
		t.Fatal(err)
	}
	locator, found, err := ReadLocator(configPath)
	if err != nil || !found || locator.ConfigPath != configPath || locator.DatabasePath != databasePath || locator.DatabaseIdentity != files.databaseIdentity {
		t.Fatalf("locator=%+v found=%t err=%v", locator, found, err)
	}
	replacement := []byte("new live bytes")
	if err := files.ReplaceLive("operation-0123456789abcdef0123456789abcdef", []byte("parent"), replacement); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("live=%q err=%v", got, err)
	}
	if info, err := os.Lstat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("live mode=%v err=%v", info.Mode(), err)
	}
}

func TestBaselineBindingIsPrivateExclusiveAndIdempotent(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := application.ValidatedConfigurationCandidate{Digest: configurationDigest([]byte("baseline")), Size: int64(len("baseline")), SchemaVersion: 5, DatabasePath: filepath.Join(root, "controller.db")}
	bindTestDatabase(t, files, candidate.DatabasePath)
	publicationErrors := make(chan error, 8)
	var publications sync.WaitGroup
	for index := 0; index < 8; index++ {
		publications.Add(1)
		go func() {
			defer publications.Done()
			publicationErrors <- files.PublishBaselineBinding(candidate)
		}()
	}
	publications.Wait()
	close(publicationErrors)
	for err := range publicationErrors {
		if err != nil {
			t.Fatalf("idempotent publication: %v", err)
		}
	}
	binding, found, err := ReadBaselineBinding(configPath)
	if err != nil || !found || binding.DatabasePath != candidate.DatabasePath || binding.Digest != candidate.Digest || binding.Size != candidate.Size || binding.Schema != candidate.SchemaVersion {
		t.Fatalf("binding=%+v found=%t err=%v", binding, found, err)
	}
	info, err := os.Lstat(filepath.Join(files.root, "baseline.json"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || linkCount(info) != 1 {
		t.Fatalf("binding info=%+v err=%v", info, err)
	}
	conflict := candidate
	conflict.DatabasePath = filepath.Join(root, "alternate.db")
	if err := files.PublishBaselineBinding(conflict); err == nil {
		t.Fatal("conflicting baseline binding was accepted")
	}
}

func TestDatabaseReplacementInvalidatesBaselineAndLocatorPublication(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "controller.db")
	bindTestDatabase(t, files, databasePath)
	if err := os.Rename(databasePath, filepath.Join(root, "original.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, []byte("replacement database"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := application.ValidatedConfigurationCandidate{
		Digest: configurationDigest([]byte("baseline")), Size: int64(len("baseline")), SchemaVersion: 5, DatabasePath: databasePath,
	}
	if err := files.PublishBaselineBinding(candidate); err == nil {
		t.Fatal("baseline binding accepted a replaced database inode")
	}
	if err := files.PublishLocator(databasePath); err == nil {
		t.Fatal("locator accepted a replaced database inode")
	}
}

func TestAtomicReplacementRestoresConcurrentExternalDrift(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	parent, target, drift := []byte("parent"), []byte("target"), []byte("external drift")
	if err := os.WriteFile(configPath, parent, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	files.beforeSwap = func() {
		if err := os.WriteFile(configPath, drift, 0o600); err != nil {
			t.Error(err)
		}
	}
	err = files.ReplaceLive("operation-fedcba9876543210fedcba9876543210", parent, target)
	if err == nil {
		t.Fatal("concurrent drift was overwritten")
	}
	live, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(live, drift) {
		t.Fatalf("live=%q err=%v", live, readErr)
	}
}

func TestAtomicReplacementRetainsCapturedParentUntilDirectorySyncIsProven(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	parent, target := []byte("parent"), []byte("target")
	if err := os.WriteFile(configPath, parent, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	files.syncDir = func(string) error { return os.ErrInvalid }
	operationID := "operation-00112233445566778899aabbccddeeff"
	if err := files.ReplaceLive(operationID, parent, target); err == nil {
		t.Fatal("unproven directory synchronization committed replacement")
	}
	stage, err := files.replacementStagePath(operationID)
	if err != nil {
		t.Fatal(err)
	}
	if live, err := os.ReadFile(configPath); err != nil || !bytes.Equal(live, target) {
		t.Fatalf("live=%q err=%v", live, err)
	}
	if captured, err := os.ReadFile(stage); err != nil || !bytes.Equal(captured, parent) {
		t.Fatalf("captured parent=%q err=%v", captured, err)
	}
}

func TestReplacementProvesCapturedParentCleanupDirectorySync(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	parent, target := []byte("parent"), []byte("target")
	if err := os.WriteFile(configPath, parent, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	files.syncDir = func(string) error {
		syncCalls++
		if syncCalls == 2 {
			return os.ErrInvalid
		}
		return nil
	}
	operationID := "operation-11223344556677889900aabbccddeeff"
	if err := files.ReplaceLive(operationID, parent, target); err == nil || syncCalls != 2 {
		t.Fatalf("replace sync calls=%d err=%v", syncCalls, err)
	}
	stage, err := files.replacementStagePath(operationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("captured parent stage still exists: %v", err)
	}
	_, _, _ = files.ReconcileReplacement(operationID, parent, target)
	if syncCalls != 3 {
		t.Fatalf("reconciliation did not prove cleanup durability; sync calls=%d", syncCalls)
	}
}

func TestRawRetentionIsConcurrentIdempotentAndRejectsUnsafeEvidence(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("same exact generation")
	digest := configurationDigest(payload)
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsSeen <- files.RetainRaw(digest, payload)
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	linked := filepath.Join(files.rawRoot, "linked.json")
	if err := os.Link(files.rawPath(digest), linked); err != nil {
		t.Fatal(err)
	}
	if files.HasRaw(digest, int64(len(payload))) {
		t.Fatal("hard-linked raw evidence was accepted")
	}
	if err := files.RetainRaw(digest, []byte("different")); err == nil {
		t.Fatal("digest/payload conflict was accepted")
	}
}

func TestRawPruneAndLiveReplacementShareOneFilesystemMutationLock(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	pruneLock, acquired, err := files.AcquireMutation()
	if err != nil || !acquired {
		t.Fatalf("prune lock acquired=%t err=%v", acquired, err)
	}
	if replacement, acquired, err := files.AcquireReplacement("operation-0123456789abcdef0123456789abcdef"); err != nil || acquired || replacement != nil {
		t.Fatalf("replacement crossed prune lock: lock=%v acquired=%t err=%v", replacement, acquired, err)
	}
	if err := pruneLock.Release(); err != nil {
		t.Fatal(err)
	}
	replacement, acquired, err := files.AcquireReplacement("operation-0123456789abcdef0123456789abcdef")
	if err != nil || !acquired {
		t.Fatalf("replacement lock acquired=%t err=%v", acquired, err)
	}
	defer replacement.Release()
	if prune, acquired, err := files.AcquireMutation(); err != nil || acquired || prune != nil {
		t.Fatalf("prune crossed replacement lock: lock=%v acquired=%t err=%v", prune, acquired, err)
	}
}

func TestLocatorRejectsSymlinkAndModeConflicts(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.ensureRoots(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "controller.db")
	bindTestDatabase(t, files, databasePath)
	if err := os.Chmod(files.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := files.PublishLocator(databasePath); err == nil {
		t.Fatal("unsafe authority directory mode was accepted")
	}
}

func canonicalTempDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func bindTestDatabase(t *testing.T, files *Files, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fixture database identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("database stat identity is unavailable")
	}
	identity := application.DatabaseFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	if err := files.BindDatabaseIdentity(path, identity); err != nil {
		t.Fatal(err)
	}
}
