package sqlite

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOpenConfigurationAuthorityReadOnlyPreservesCurrentStore(t *testing.T) {
	path, configPath, identity := seedReadOnlyConfigurationAuthority(t, schemaVersion)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenConfigurationAuthorityReadOnly(context.Background(), path, configPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	authority, found, err := store.ConfigurationAuthority(context.Background())
	if err != nil || !found || authority.CanonicalConfigPath != configPath {
		store.Close()
		t.Fatalf("authority=%+v found=%t err=%v", authority, found, err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE configuration_authority SET authority_version=authority_version+1 WHERE authority_id=1`); err == nil {
		store.Close()
		t.Fatal("query-only store accepted a write")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != sha256.Sum256(before) {
		t.Fatal("read-only open changed the database file")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("read-only open left auxiliary file %q: %v", suffix, err)
		}
	}
}

func TestOpenConfigurationAuthorityReadOnlyRefusesMigration(t *testing.T) {
	legacyVersion := schemaVersion - 1
	path, configPath, identity := seedReadOnlyConfigurationAuthority(t, legacyVersion)
	if store, err := OpenConfigurationAuthorityReadOnly(context.Background(), path, configPath, identity); err == nil || !strings.Contains(err.Error(), "schema is not current") {
		if store != nil {
			store.Close()
		}
		t.Fatalf("legacy schema open err=%v", err)
	}
	store, err := openWithSupportedSchema(path, legacyVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.SchemaVersion(context.Background()); err != nil || version != legacyVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func seedReadOnlyConfigurationAuthority(t *testing.T, version int) (string, string, application.DatabaseFileIdentity) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "controller.db")
	store, err := openWithSupportedSchema(path, version)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "controller.json")
	input := application.ConfigurationBaselineInput{
		Candidate: application.ValidatedConfigurationCandidate{
			Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: path,
			Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"},
		},
		CanonicalConfigPath: configPath,
		ObservedAt:          time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, _, err := store.AdoptConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	identity := store.DatabaseIdentity()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, configPath, identity
}
