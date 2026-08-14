package configuration

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
	if err := files.PublishLocator(databasePath); err != nil {
		t.Fatal(err)
	}
	config, database, found, err := ReadLocator(configPath)
	if err != nil || !found || config != configPath || database != databasePath {
		t.Fatalf("config=%q database=%q found=%t err=%v", config, database, found, err)
	}
	replacement := []byte("new live bytes")
	if err := files.ReplaceLive(replacement); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("live=%q err=%v", got, err)
	}
	if info, err := os.Lstat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("live mode=%v err=%v", info.Mode(), err)
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
	if err := os.Chmod(files.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := files.PublishLocator(filepath.Join(root, "controller.db")); err == nil {
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
