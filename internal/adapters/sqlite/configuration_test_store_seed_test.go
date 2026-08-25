package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAdmissionTestStoreSeedIsPrivateCurrentAndAuthorityFree(t *testing.T) {
	if err := validateClosedAdmissionTestStoreSeed(admissionTestStoreSeedPath); err != nil {
		t.Fatal(err)
	}
	seed, err := Open(admissionTestStoreSeedPath)
	if err != nil {
		t.Fatal(err)
	}
	version, err := seed.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	if _, found, err := seed.ConfigurationAuthority(context.Background()); err != nil || found {
		t.Fatalf("configuration authority found=%t err=%v", found, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateClosedAdmissionTestStoreSeed(admissionTestStoreSeedPath); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionTestStoreSeedCopiesAreIsolatedAndExistingPathIsPreserved(t *testing.T) {
	firstPath := filepath.Join(t.TempDir(), "controller.db")
	first, err := openAdmissionTestStore(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	requireAdmissionTestStoreCurrentAndBound(t, first, firstPath)
	if _, err := first.db.ExecContext(context.Background(), `PRAGMA user_version = 127`); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	requirePrivateClosedSQLiteFile(t, firstPath)

	reopened, err := openAdmissionTestStore(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	requireAdmissionTestStoreCurrentAndBound(t, reopened, firstPath)
	var userVersion int
	if err := reopened.db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&userVersion); err != nil || userVersion != 127 {
		t.Fatalf("reopened user version=%d err=%v", userVersion, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	secondPath := filepath.Join(t.TempDir(), "controller.db")
	second, err := openAdmissionTestStore(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	requireAdmissionTestStoreCurrentAndBound(t, second, secondPath)
	if err := second.db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&userVersion); err != nil || userVersion != 0 {
		t.Fatalf("isolated user version=%d err=%v", userVersion, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	requirePrivateClosedSQLiteFile(t, secondPath)

	seedInfo, err := os.Stat(admissionTestStoreSeedPath)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(seedInfo, firstInfo) || os.SameFile(seedInfo, secondInfo) || os.SameFile(firstInfo, secondInfo) {
		t.Fatal("SQLite test seed copies must have independent file identities")
	}
}

func requireAdmissionTestStoreCurrentAndBound(t *testing.T, store *admissionTestStore, path string) {
	t.Helper()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	authority, found, err := store.ConfigurationAuthority(context.Background())
	if err != nil || !found {
		t.Fatalf("configuration authority found=%t err=%v", found, err)
	}
	if authority.DatabasePath != path || authority.CanonicalConfigPath != filepath.Clean(path)+".test-controller.json" {
		t.Fatalf("configuration authority database=%q config=%q", authority.DatabasePath, authority.CanonicalConfigPath)
	}
	if authority.EffectiveID != authority.Desired.GenerationID || !store.authority.Valid() {
		t.Fatalf("configuration authority=%+v admission=%+v", authority, store.authority)
	}
}

func requirePrivateClosedSQLiteFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("SQLite file mode=%v err=%v", info, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("SQLite auxiliary file suffix=%q err=%v", suffix, err)
		}
	}
}
