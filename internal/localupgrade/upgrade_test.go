package localupgrade

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/buildidentity"
	_ "modernc.org/sqlite"
)

type fixtureRunner struct {
	selectedTarget string
	selectedPID    int
}

type topologyRunner map[string]bool

func (r topologyRunner) Run(_ context.Context, _ string, name string, args ...string) (commandResult, error) {
	if name != "launchctl" || len(args) != 2 || args[0] != "print" {
		return commandResult{}, errors.New("unexpected command")
	}
	if r[args[1]] {
		return commandResult{ExitCode: 0, Stdout: []byte("state = running\npid = 42\n")}, nil
	}
	return commandResult{ExitCode: 113, Stderr: []byte("service not found")}, nil
}

func (r fixtureRunner) Run(ctx context.Context, directory, name string, args ...string) (commandResult, error) {
	if name != "launchctl" {
		return (osCommandRunner{}).Run(ctx, directory, name, args...)
	}
	if len(args) == 2 && args[0] == "print" && args[1] == r.selectedTarget && r.selectedPID > 0 {
		return commandResult{ExitCode: 0, Stdout: []byte(fmt.Sprintf("state = running\npid = %d\n", r.selectedPID))}, nil
	}
	return commandResult{ExitCode: 113, Stderr: []byte("service not found")}, nil
}

func newFixtureManager(t *testing.T, runner commandRunner) *Manager {
	t.Helper()
	home := t.TempDir()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	manager := &Manager{home: home, launchDaemonDirectory: filepath.Join(home, "LaunchDaemons"), uid: os.Getuid(), user: "fixture-worker", now: func() time.Time { return now }, runner: runner}
	if err := ensurePrivateDirectory(manager.controllerRoot(), manager.uid); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(manager.upgradeRoot(), manager.uid); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(manager.upgradeRoot(), "upgrade.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil || lock.Close() != nil {
		t.Fatal("fixture upgrade lock is unavailable")
	}
	return manager
}

func writeFixtureFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func fixtureBuild(revision string) buildidentity.Info {
	return buildidentity.Info{ProductVersion: "0.1.0-dev", BuildIdentity: "sha256:" + strings.Repeat("a", 64), VCSRevision: revision, VCSTime: "2026-08-28T00:00:00Z", SupportedControllerSchemaVersion: 41}
}

func binaryScript(build buildidentity.Info) string {
	raw, _ := json.Marshal(build)
	return "#!/bin/sh\nif [ \"${1:-}\" = version ] && [ \"${2:-}\" = --json ]; then\n  printf '%s\\n' '" + string(raw) + "'\nelse\n  printf '%s\\n' '" + build.ProductVersion + "'\nfi\n"
}

func createFixtureDatabase(t *testing.T, path string, completeReadiness bool) databaseEvidence {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path, false))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations(version) VALUES(41)`,
	}
	if completeReadiness {
		statements = append(statements,
			`CREATE TABLE configuration_generations(generation_id INTEGER PRIMARY KEY,digest TEXT,lifecycle TEXT)`,
			`CREATE TABLE configuration_authority(authority_id INTEGER PRIMARY KEY,desired_generation_id INTEGER,effective_generation_id INTEGER)`,
			`CREATE TABLE controller_integrity_generation(singleton INTEGER PRIMARY KEY,generation INTEGER)`,
			`CREATE TABLE controller_integrity_current(singleton INTEGER PRIMARY KEY,observation_id TEXT,published_generation INTEGER)`,
			`CREATE TABLE controller_integrity_observations(observation_id TEXT PRIMARY KEY,effective_readiness TEXT)`,
		)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := inspectDatabaseReadOnly(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func seedBundle(t *testing.T, manager *Manager, completeReadiness bool) (journal, string) {
	t.Helper()
	revision := strings.Repeat("b", 40)
	build := fixtureBuild(revision)
	configPath := filepath.Join(manager.controllerRoot(), "controller.json")
	configRaw := "{\"fixture\":true}\n"
	writeFixtureFile(t, configPath, configRaw, 0o600)
	configDigest, _ := digestFile(configPath)
	databasePath := filepath.Join(manager.controllerRoot(), "controller.db")
	database := createFixtureDatabase(t, databasePath, completeReadiness)
	target := filepath.Join(manager.controllerRoot(), "agentctl")
	previousRaw := "#!/bin/sh\nprintf '%s\\n' 'legacy-v1'\n"
	candidateRaw := binaryScript(build)
	writeFixtureFile(t, target, previousRaw, 0o755)
	previousDigest, _ := digestFile(target)
	candidateSource := filepath.Join(manager.controllerRoot(), "candidate-source")
	writeFixtureFile(t, candidateSource, candidateRaw, 0o755)
	candidateDigest, _ := digestFile(candidateSource)
	id := "upgrade-" + strings.Repeat("c", 32)
	bundle := manager.bundlePath(id)
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyPrivateArtifact(candidateSource, filepath.Join(bundle, "candidate.bin"), manager.uid); err != nil {
		t.Fatal(err)
	}
	if err := copyPrivateArtifact(target, filepath.Join(bundle, "previous.bin"), manager.uid); err != nil {
		t.Fatal(err)
	}
	now := manager.now()
	j := journal{SchemaVersion: 1, UpgradeID: id, Phase: "prepared", Supervisor: "launchagent", Revision: revision, BinaryPath: target, ConfigPath: configPath, DatabasePath: databasePath, ConfigDigest: configDigest, Candidate: binaryEvidence{Digest: candidateDigest, Size: int64(len(candidateRaw)), Mode: 0o755, Build: build, Structured: true}, Previous: binaryEvidence{Digest: previousDigest, Size: int64(len(previousRaw)), Mode: 0o755, LegacyVersion: "legacy-v1"}, Database: database, CreatedAt: now, UpdatedAt: now}
	manifest := candidateManifest{SchemaVersion: 1, Revision: revision, Candidate: j.Candidate, Previous: j.Previous, Database: database, ConfigDigest: configDigest, PreparedAt: now}
	if err := writePrivateJSON(filepath.Join(bundle, "candidate-manifest.json"), manifest, manager.uid); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(bundle, j, manager.uid); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(manager.activePath(), struct {
		UpgradeID string `json:"upgrade_id"`
	}{id}, manager.uid); err != nil {
		t.Fatal(err)
	}
	return j, bundle
}

func TestConsistentSnapshotUsesSQLiteBackupAPIWithWAL(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "controller.db")
	db, err := sql.Open("sqlite", sqliteURI(source, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`, `INSERT INTO schema_migrations VALUES(41)`, `CREATE TABLE values_fixture(value TEXT)`, `INSERT INTO values_fixture VALUES('before')`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := inspectDatabaseReadOnly(source, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO values_fixture VALUES('wal-row')`); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "snapshot.db")
	digest, err := createConsistentSnapshot(context.Background(), source, destination, os.Getuid(), evidence)
	if err != nil || len(digest) != 64 {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	snapshot, err := sql.Open("sqlite", sqliteURI(destination, true))
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var count int
	if err := snapshot.QueryRow(`SELECT COUNT(*) FROM values_fixture`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestReplaceIsRestartSafeAndBootstrapIntentPermanentlyForbidsRollback(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedBundle(t, manager, false)
	result, err := manager.Replace(context.Background(), j.UpgradeID)
	if err != nil || result.State != "replaced" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	committed, _, err := manager.loadJournal(j.UpgradeID)
	if err != nil || committed.Phase != "replacement_committed" || committed.SnapshotDigest == "" {
		t.Fatalf("journal=%+v err=%v", committed, err)
	}
	if digest, _ := digestFile(j.BinaryPath); digest != j.Candidate.Digest {
		t.Fatalf("installed digest=%s", digest)
	}
	before, _ := os.ReadFile(filepath.Join(bundle, "journal.json"))
	status, err := manager.Status(context.Background(), j.UpgradeID)
	after, _ := os.ReadFile(filepath.Join(bundle, "journal.json"))
	if err != nil || status.State != "replaced" || string(before) != string(after) {
		t.Fatalf("status=%+v err=%v journal_mutated=%t", status, err, string(before) != string(after))
	}
	authorized, err := manager.AuthorizeBootstrap(context.Background(), j.UpgradeID)
	if err != nil || !authorized.BootstrapIntent || authorized.RequiresSudo || len(authorized.BootstrapInstruction) == 0 {
		t.Fatalf("authorized=%+v err=%v", authorized, err)
	}
	if _, err := manager.Rollback(context.Background(), j.UpgradeID); err == nil || !strings.Contains(err.Error(), "permanently forbidden") {
		t.Fatalf("post-intent rollback err=%v", err)
	}
}

func TestRollbackRestoresOnlyPreviousBinaryAndNeverSnapshot(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, _ := seedBundle(t, manager, false)
	if _, err := manager.Replace(context.Background(), j.UpgradeID); err != nil {
		t.Fatal(err)
	}
	databaseBefore, _ := digestFile(j.DatabasePath)
	result, err := manager.Rollback(context.Background(), j.UpgradeID)
	if err != nil || result.State != "rolled_back" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if digest, _ := digestFile(j.BinaryPath); digest != j.Previous.Digest {
		t.Fatalf("restored digest=%s", digest)
	}
	if databaseAfter, _ := digestFile(j.DatabasePath); databaseAfter != databaseBefore {
		t.Fatal("rollback changed Controller database")
	}
}

func TestReplacementDurablePhaseFailuresReconcileAndResume(t *testing.T) {
	for _, test := range []struct {
		point string
		state string
	}{
		{point: "after_snapshot_journal", state: "prepared"},
		{point: "after_replacement_intent", state: "replacement_interrupted"},
		{point: "after_binary_replacement", state: "replacement_interrupted"},
	} {
		t.Run(test.point, func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			j, _ := seedBundle(t, manager, false)
			manager.fail = func(point string) error {
				if point == test.point {
					return errors.New("injected interruption")
				}
				return nil
			}
			if _, err := manager.Replace(context.Background(), j.UpgradeID); err == nil {
				t.Fatal("injected replacement interruption was ignored")
			}
			manager.fail = nil
			status, err := manager.Status(context.Background(), j.UpgradeID)
			if err != nil || status.State != test.state {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			resumed, err := manager.Replace(context.Background(), j.UpgradeID)
			if err != nil || resumed.State != "replaced" {
				t.Fatalf("resumed=%+v err=%v", resumed, err)
			}
		})
	}
}

func TestInstalledTargetDriftAfterReplacementIntentFailsClosed(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, _ := seedBundle(t, manager, false)
	manager.fail = func(point string) error {
		if point == "after_replacement_intent" {
			return errors.New("injected interruption")
		}
		return nil
	}
	if _, err := manager.Replace(context.Background(), j.UpgradeID); err == nil {
		t.Fatal("replacement interruption was ignored")
	}
	manager.fail = nil
	writeFixtureFile(t, j.BinaryPath, "#!/bin/sh\necho drift\n", 0o755)
	status, err := manager.Status(context.Background(), j.UpgradeID)
	if err != nil || status.State != "attention" || status.Reason != "replacement_identity_ambiguous" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, err := manager.Replace(context.Background(), j.UpgradeID); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("replace err=%v", err)
	}
}

func TestRollbackDurablePhaseFailuresReconcileAndResume(t *testing.T) {
	for _, point := range []string{"after_rollback_intent", "after_binary_rollback"} {
		t.Run(point, func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			j, _ := seedBundle(t, manager, false)
			if _, err := manager.Replace(context.Background(), j.UpgradeID); err != nil {
				t.Fatal(err)
			}
			manager.fail = func(observed string) error {
				if observed == point {
					return errors.New("injected interruption")
				}
				return nil
			}
			if _, err := manager.Rollback(context.Background(), j.UpgradeID); err == nil {
				t.Fatal("injected rollback interruption was ignored")
			}
			manager.fail = nil
			status, err := manager.Status(context.Background(), j.UpgradeID)
			if err != nil || status.State != "rollback_interrupted" {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			resumed, err := manager.Rollback(context.Background(), j.UpgradeID)
			if err != nil || resumed.State != "rolled_back" {
				t.Fatalf("resumed=%+v err=%v", resumed, err)
			}
		})
	}
}

func TestLostResponseAfterBootstrapIntentCannotReauthorizeRollback(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, _ := seedBundle(t, manager, false)
	if _, err := manager.Replace(context.Background(), j.UpgradeID); err != nil {
		t.Fatal(err)
	}
	manager.fail = func(point string) error {
		if point == "after_bootstrap_intent" {
			return errors.New("lost caller response")
		}
		return nil
	}
	if _, err := manager.AuthorizeBootstrap(context.Background(), j.UpgradeID); err == nil {
		t.Fatal("injected response loss was ignored")
	}
	manager.fail = nil
	persisted, _, err := manager.loadJournal(j.UpgradeID)
	if err != nil || persisted.BootstrapIntentAt == nil || persisted.Phase != "bootstrap_intent" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	if _, err := manager.Rollback(context.Background(), j.UpgradeID); err == nil || !strings.Contains(err.Error(), "permanently forbidden") {
		t.Fatalf("rollback err=%v", err)
	}
}

func TestObserveSeparatesPendingIntegrityFromHealthyCompletion(t *testing.T) {
	pid := os.Getpid()
	manager := newFixtureManager(t, fixtureRunner{selectedTarget: fmt.Sprintf("gui/%d/%s", os.Getuid(), neutralLaunchdLabel), selectedPID: pid})
	j, bundle := seedBundle(t, manager, true)
	if err := atomicallyInstall(filepath.Join(bundle, "candidate.bin"), j.BinaryPath, 0o755, manager.uid, j.Previous.Digest, j.Candidate.Digest, j.UpgradeID); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteURI(j.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO configuration_generations VALUES(1,'` + j.ConfigDigest + `','effective')`,
		`INSERT INTO configuration_authority VALUES(1,1,1)`,
		`INSERT INTO controller_integrity_generation VALUES(1,2)`,
		`INSERT INTO controller_integrity_observations VALUES('observation-1','ready')`,
		`INSERT INTO controller_integrity_current VALUES(1,'observation-1',1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	now := manager.now()
	j.Phase, j.BootstrapIntentAt, j.UpdatedAt = "bootstrap_intent", &now, now
	if err := writeJournal(bundle, j, manager.uid); err != nil {
		t.Fatal(err)
	}
	started, err := processStartIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := heartbeatEvidence{SchemaVersion: 3, WorkerInstanceID: "fixture-worker", ProcessID: pid, ProcessStartID: started, BuildIdentity: j.Candidate.Build.BuildIdentity, ConfigurationDigest: j.ConfigDigest, Status: "parked", ObservedAt: now}
	if err := writePrivateJSON(j.ConfigPath+".worker-status.json", heartbeat, manager.uid); err != nil {
		t.Fatal(err)
	}
	pending, err := manager.Observe(context.Background(), j.UpgradeID)
	if err != nil || pending.State != "pending" || pending.Reason != "integrity_pending" || pending.UpgradeHealth != "healthy" || pending.ControllerReadiness != "pending" {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	db, _ = sql.Open("sqlite", sqliteURI(j.DatabasePath, false))
	if _, err := db.Exec(`UPDATE controller_integrity_current SET published_generation=2`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	healthy, err := manager.Observe(context.Background(), j.UpgradeID)
	if err != nil || healthy.State != "healthy" || healthy.NextAction != "cleanup" || healthy.UpgradeHealth != "healthy" || healthy.ControllerReadiness != "ready" {
		t.Fatalf("healthy=%+v err=%v", healthy, err)
	}
}

func TestCleanupRejectsUnownedBundleArtifacts(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedBundle(t, manager, false)
	writeFixtureFile(t, filepath.Join(bundle, "snapshot.db"), "snapshot", 0o600)
	writeFixtureFile(t, filepath.Join(bundle, "unrelated"), "do not delete", 0o600)
	now := manager.now()
	j.Phase, j.CompletedAt, j.UpdatedAt = "healthy", &now, now
	if err := writeJournal(bundle, j, manager.uid); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Cleanup(context.Background(), j.UpgradeID); err == nil || !strings.Contains(err.Error(), "unowned") {
		t.Fatalf("cleanup err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "unrelated")); err != nil {
		t.Fatal("cleanup removed unrelated evidence")
	}
}

func TestCleanupResumesAfterCurrentInstallationCommit(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedBundle(t, manager, false)
	writeFixtureFile(t, filepath.Join(bundle, "snapshot.db"), "snapshot", 0o600)
	now := manager.now()
	j.Phase, j.BootstrapIntentAt, j.CompletedAt, j.UpdatedAt = "healthy", &now, &now, now
	if err := writeJournal(bundle, j, manager.uid); err != nil {
		t.Fatal(err)
	}
	manager.fail = func(point string) error {
		if point == "after_current_installation" {
			return errors.New("injected cleanup interruption")
		}
		return nil
	}
	if _, err := manager.Cleanup(context.Background(), j.UpgradeID); err == nil {
		t.Fatal("injected cleanup interruption was ignored")
	}
	manager.fail = nil
	resumed, err := manager.Cleanup(context.Background(), j.UpgradeID)
	if err != nil || resumed.State != "cleaned" {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
	if exists(bundle) || exists(manager.activePath()) {
		t.Fatal("completed cleanup retained active artifacts")
	}
}

func TestCleanupResumesAfterJournalRemoval(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedBundle(t, manager, false)
	writeFixtureFile(t, filepath.Join(bundle, "snapshot.db"), "snapshot", 0o600)
	now := manager.now()
	j.Phase, j.BootstrapIntentAt, j.CompletedAt, j.UpdatedAt = "cleanup_intent", &now, &now, now
	if err := writeJournal(bundle, j, manager.uid); err != nil {
		t.Fatal(err)
	}
	current := currentInstallation{SchemaVersion: 1, UpgradeID: j.UpgradeID, Supervisor: j.Supervisor, BinaryDigest: j.Candidate.Digest, BuildIdentity: j.Candidate.Build.BuildIdentity, VCSRevision: j.Revision, DatabaseSchema: 41, VerifiedAt: now}
	if err := writePrivateJSON(filepath.Join(manager.controllerRoot(), "current-installation.json"), current, manager.uid); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "candidate.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "journal.json")); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Cleanup(context.Background(), j.UpgradeID)
	if err != nil || result.State != "cleaned" || exists(bundle) || exists(manager.activePath()) {
		t.Fatalf("result=%+v err=%v bundle=%t active=%t", result, err, exists(bundle), exists(manager.activePath()))
	}
}

func TestSupervisorTopologyRejectsLegacyAndOppositeLabels(t *testing.T) {
	uid := os.Getuid()
	tests := []struct {
		name       string
		supervisor string
		target     string
		reason     string
	}{
		{name: "launchagent legacy", supervisor: "launchagent", target: fmt.Sprintf("gui/%d/%s", uid, legacyLaunchdLabel), reason: "legacy_supervisor_conflict"},
		{name: "launchagent opposite", supervisor: "launchagent", target: "system/" + neutralLaunchdLabel, reason: "opposite_supervisor_conflict"},
		{name: "launchdaemon legacy", supervisor: "launchdaemon", target: "system/" + legacyLaunchdLabel, reason: "legacy_supervisor_conflict"},
		{name: "launchdaemon opposite", supervisor: "launchdaemon", target: fmt.Sprintf("gui/%d/%s", uid, neutralLaunchdLabel), reason: "opposite_supervisor_conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newFixtureManager(t, topologyRunner{test.target: true})
			topology := manager.observeSupervisorTopology(context.Background(), test.supervisor)
			if topology.Reason != test.reason {
				t.Fatalf("topology=%+v", topology)
			}
		})
	}
}

func TestActiveCommandLockRejectsConcurrentUpgradeCommand(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, _ := seedBundle(t, manager, false)
	var nestedErr error
	err := manager.withGlobalLock(func() error {
		_, nestedErr = manager.Status(context.Background(), j.UpgradeID)
		return nil
	})
	if err != nil || nestedErr == nil || !strings.Contains(nestedErr.Error(), "another upgrade command") {
		t.Fatalf("outer=%v nested=%v", err, nestedErr)
	}
}

func TestUnsafeBinaryLinksAreRejected(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "agentctl")
	writeFixtureFile(t, target, "binary", 0o755)
	hardlink := filepath.Join(directory, "hardlink")
	if err := os.Link(target, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectBinary(context.Background(), fixtureRunner{}, target, os.Getuid()); err == nil {
		t.Fatal("hard-linked binary was accepted")
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "symlink")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectBinary(context.Background(), fixtureRunner{}, symlink, os.Getuid()); err == nil {
		t.Fatal("symlink binary was accepted")
	}
	if _, err := inspectBinary(context.Background(), fixtureRunner{}, target, os.Getuid()+1); err == nil {
		t.Fatal("wrong-owner binary was accepted")
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectBinary(context.Background(), fixtureRunner{}, target, os.Getuid()); err == nil {
		t.Fatal("non-executable binary was accepted")
	}
}

func TestLegacyGoBinaryWithoutStructuredVersionRetainsSafeBuildMetadata(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "main.go")
	writeFixtureFile(t, source, "package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"legacy-v1\")}\n", 0o600)
	binary := filepath.Join(directory, "legacy-agentctl")
	result, err := (osCommandRunner{}).Run(context.Background(), directory, "go", "build", "-o", binary, source)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("build exit=%d err=%v", result.ExitCode, err)
	}
	evidence, err := inspectBinary(context.Background(), osCommandRunner{}, binary, os.Getuid())
	if err != nil || evidence.Structured || evidence.LegacyVersion != "legacy-v1" || evidence.GoVersion == "" {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}
