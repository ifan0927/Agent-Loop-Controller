package localupgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	"github.com/ifan0927/Agent-Loop-Controller/internal/buildidentity"
	_ "modernc.org/sqlite"
)

type fixtureRunner struct {
	selectedTarget string
	selectedPID    int
}

type topologyRunner map[string]bool

type successorRunner struct {
	fixtureRunner
	sourceRoot string
	revisions  map[string]bool
}

func (r successorRunner) Run(ctx context.Context, directory, name string, args ...string) (commandResult, error) {
	if name == "git" && len(args) == 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel" {
		return commandResult{ExitCode: 0, Stdout: []byte(r.sourceRoot + "\n")}, nil
	}
	if name == "git" && len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" {
		revision := strings.TrimSuffix(args[2], "^{commit}")
		if r.revisions[revision] {
			return commandResult{ExitCode: 0, Stdout: []byte(revision + "\n")}, nil
		}
		return commandResult{ExitCode: 1}, nil
	}
	return r.fixtureRunner.Run(ctx, directory, name, args...)
}

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
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	return buildidentity.Info{ProductVersion: "0.1.0-dev", BuildIdentity: "sha256:" + strings.Repeat("a", 64), VCSRevision: revision, VCSTime: "2026-08-28T00:00:00Z", SupportedControllerSchemaVersion: 42}
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
		`INSERT INTO schema_migrations(version) VALUES(42)`,
	}
	if completeReadiness {
		statements = append(statements,
			`CREATE TABLE configuration_generations(generation_id INTEGER PRIMARY KEY,digest TEXT,lifecycle TEXT)`,
			`CREATE TABLE configuration_authority(authority_id INTEGER PRIMARY KEY,desired_generation_id INTEGER,effective_generation_id INTEGER)`,
			`CREATE TABLE controller_integrity_generation(singleton INTEGER PRIMARY KEY,generation INTEGER)`,
			`CREATE TABLE controller_integrity_current(singleton INTEGER PRIMARY KEY,observation_id TEXT,observation_digest TEXT,published_generation INTEGER)`,
			`CREATE TABLE controller_integrity_observations(observation_id TEXT PRIMARY KEY,schema_version TEXT,registry_version TEXT,observation_digest TEXT,target_generation INTEGER,published_generation INTEGER,effective_readiness TEXT)`,
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

func seedCleanupReady(t *testing.T, manager *Manager, rollback bool) (journal, string) {
	t.Helper()
	j, bundle := seedBundle(t, manager, false)
	writeFixtureFile(t, filepath.Join(bundle, "snapshot.db"), "fixture snapshot", 0o600)
	snapshotDigest, err := digestFile(filepath.Join(bundle, "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := manager.now()
	j.SnapshotDigest, j.CompletedAt, j.UpdatedAt = snapshotDigest, &now, now
	if rollback {
		j.Phase = "rollback_healthy"
	} else {
		j.Phase, j.BootstrapIntentAt = "healthy", &now
	}
	if err := writeJournal(bundle, j, manager.uid); err != nil {
		t.Fatal(err)
	}
	return j, bundle
}

func removeFixtureBundle(t *testing.T, manager *Manager, bundle string) {
	t.Helper()
	for _, name := range []string{"candidate-manifest.json", "candidate.bin", "previous.bin", "snapshot.db", "journal.json"} {
		if err := os.Remove(filepath.Join(bundle, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(bundle); err != nil || syncDirectory(manager.upgradeRoot()) != nil {
		t.Fatal(err)
	}
}

func writeRetainedCleanupPredecessor(t *testing.T, manager *Manager, successor journal, idCharacter string, recovery bool) journal {
	t.Helper()
	now := manager.now()
	predecessor := successor
	predecessor.SchemaVersion = journalSchemaVersion
	predecessor.UpgradeID = "upgrade-" + strings.Repeat(idCharacter, 32)
	predecessor.Phase = "superseded"
	predecessor.Revision = strings.Repeat(idCharacter, 40)
	predecessor.FailureReason = "integrity_not_ready"
	predecessor.PredecessorID = ""
	predecessor.SuccessorID = successor.UpgradeID
	predecessor.SuccessorRevision = successor.Revision
	predecessor.BootstrapIntentAt = &now
	predecessor.SupersededAt = &now
	predecessor.UpdatedAt = now
	predecessor.DatabaseRecovery = nil
	build := fixtureBuild(predecessor.Revision)
	build.BuildIdentity = "sha256:" + strings.Repeat(idCharacter, 64)
	raw := binaryScript(build)
	digest := sha256.Sum256([]byte(raw))
	predecessor.Candidate = binaryEvidence{Digest: fmt.Sprintf("%x", digest), Size: int64(len(raw)), Mode: 0o755, Build: build, Structured: true}
	if recovery {
		oldDatabase := predecessor.Database
		oldDatabase.Inode++
		predecessor.DatabaseRecovery = &databaseRecoveryEvidence{
			Version: databaseRecoveryEvidenceVersion, PreviewDigest: strings.Repeat("1", 64),
			OldDatabase: oldDatabase, ReplacementDatabase: predecessor.Database,
			Verification: replacementDatabaseVerification{
				ContentDigest: strings.Repeat("2", 64), AuthorityDigest: strings.Repeat("3", 64), SchemaVersion: predecessor.Database.SchemaVersion,
				IntegrityOK: true, ForeignKeysOK: true, BindingMatches: true, DesiredConfigurationMatch: true,
				Readiness: recoveryReadinessVerification{Relationship: recoveryReadinessExactMatch, PredecessorReason: predecessor.FailureReason, ReplacementReason: predecessor.FailureReason, GenerationRelationship: integrityGenerationCurrent, CurrentGeneration: 1, PublishedGeneration: 1},
			},
			SuccessorRevision: successor.Revision, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true, IntentAt: now, LocatorPublishedAt: &now,
		}
	}
	bundle := manager.bundlePath(predecessor.UpgradeID)
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(bundle, predecessor, manager.uid); err != nil {
		t.Fatal(err)
	}
	if err := validateJournal(predecessor, predecessor.UpgradeID); err != nil {
		t.Fatalf("retained predecessor fixture is invalid: %v", err)
	}
	return predecessor
}

func bindCleanupSuccessorToPredecessor(t *testing.T, manager *Manager, successor *journal, successorBundle string, predecessor journal) {
	t.Helper()
	previousRaw := binaryScript(predecessor.Candidate.Build)
	if err := os.WriteFile(filepath.Join(successorBundle, "previous.bin"), []byte(previousRaw), 0o600); err != nil || os.Chmod(filepath.Join(successorBundle, "previous.bin"), 0o600) != nil {
		t.Fatal("successor previous artifact fixture could not be replaced")
	}
	successor.Previous = predecessor.Candidate
	manifest := candidateManifest{SchemaVersion: successor.SchemaVersion, Revision: successor.Revision, Candidate: successor.Candidate, Previous: successor.Previous, Database: successor.Database, ConfigDigest: successor.ConfigDigest, PreparedAt: successor.CreatedAt}
	if err := writePrivateJSON(filepath.Join(successorBundle, "candidate-manifest.json"), manifest, manager.uid); err != nil || writeJournal(successorBundle, *successor, manager.uid) != nil {
		t.Fatal("successor cleanup fixture could not be updated")
	}
}

func fixtureTreeFingerprint(t *testing.T, root string) string {
	t.Helper()
	var records []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest := "-"
		if info.Mode().IsRegular() {
			digest, err = digestFile(path)
			if err != nil {
				return err
			}
		}
		records = append(records, fmt.Sprintf("%s|%o|%d|%d|%s", relative, info.Mode(), info.Size(), info.ModTime().UnixNano(), digest))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(records)
	return strings.Join(records, "\n")
}

func commandOutput(t *testing.T, runner commandRunner, directory, name string, args ...string) string {
	t.Helper()
	result, err := runner.Run(context.Background(), directory, name, args...)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("command %s %v exit=%d err=%v stderr=%s", name, args, result.ExitCode, err, result.Stderr)
	}
	return strings.TrimSpace(string(result.Stdout))
}

func gitObjectDirectory(t *testing.T, runner commandRunner, repository string) string {
	t.Helper()
	path := commandOutput(t, runner, repository, "git", "rev-parse", "--git-path", "objects")
	if !filepath.IsAbs(path) {
		path = filepath.Join(repository, path)
	}
	return filepath.Clean(path)
}

func assertObjectStorageIsNotHardLinked(t *testing.T, source, candidate string) {
	t.Helper()
	foundComparableObject := false
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		candidateInfo, err := os.Stat(filepath.Join(candidate, relative))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !candidateInfo.Mode().IsRegular() {
			return nil
		}
		foundComparableObject = true
		if os.SameFile(info, candidateInfo) {
			t.Fatalf("candidate object %s is hard-linked to the source repository", relative)
		}
		return filepath.SkipAll
	})
	if err != nil {
		t.Fatal(err)
	}
	if !foundComparableObject {
		t.Fatal("source and candidate repositories had no comparable Git object")
	}
}

func TestIndependentCandidateRepositoryPreservesVCSBuildIdentity(t *testing.T) {
	runner := osCommandRunner{}
	revision := commandOutput(t, runner, "", "git", "rev-parse", "HEAD")
	sourceRoot, err := resolveCandidateSource(context.Background(), runner, revision)
	if err != nil {
		t.Fatal(err)
	}
	beforeStatus := commandOutput(t, runner, sourceRoot, "git", "status", "--porcelain=v1", "--untracked-files=all")
	beforeRefs := commandOutput(t, runner, sourceRoot, "git", "show-ref", "--head")

	directory := t.TempDir()
	candidateRepository := filepath.Join(directory, "repository")
	if err := cloneCandidateRepository(context.Background(), runner, sourceRoot, candidateRepository, revision); err != nil {
		t.Fatal(err)
	}
	if origin := commandOutput(t, runner, candidateRepository, "git", "remote", "get-url", "origin"); origin != sourceRoot {
		t.Fatalf("candidate origin=%q want=%q", origin, sourceRoot)
	}
	if _, err := os.Lstat(filepath.Join(candidateRepository, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("candidate repository uses Git alternates")
	}
	assertObjectStorageIsNotHardLinked(t, gitObjectDirectory(t, runner, sourceRoot), gitObjectDirectory(t, runner, candidateRepository))

	binary := filepath.Join(directory, "agentctl")
	built, err := runner.Run(context.Background(), candidateRepository, "go", "build", "-trimpath", "-o", binary, "./cmd/agentctl")
	if err != nil || built.ExitCode != 0 {
		t.Fatalf("candidate build exit=%d err=%v stderr=%s", built.ExitCode, err, built.Stderr)
	}
	evidence, err := inspectBinary(context.Background(), runner, binary, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if !candidateCompatible(evidence, revision, evidence.Build.SupportedControllerSchemaVersion) {
		t.Fatalf("candidate evidence=%+v", evidence)
	}
	if evidence.GoVCSTime == "" || evidence.Build.VCSTime != evidence.GoVCSTime || evidence.GoVCSModified || evidence.Build.VCSModified {
		t.Fatalf("candidate VCS evidence=%+v", evidence)
	}
	if clean, err := gitCheckoutClean(context.Background(), runner, candidateRepository); err != nil || !clean {
		t.Fatalf("candidate repository clean=%t err=%v", clean, err)
	}
	if after := commandOutput(t, runner, sourceRoot, "git", "status", "--porcelain=v1", "--untracked-files=all"); after != beforeStatus {
		t.Fatalf("source status changed: before=%q after=%q", beforeStatus, after)
	}
	if after := commandOutput(t, runner, sourceRoot, "git", "show-ref", "--head"); after != beforeRefs {
		t.Fatal("source references changed")
	}
}

func TestCandidateCompatibilityRejectsUnverifiableBuildIdentity(t *testing.T) {
	revision := strings.Repeat("b", 40)
	build := fixtureBuild(revision)
	candidate := binaryEvidence{
		GoVersion:     "go1.26.1",
		ModulePath:    "github.com/ifan0927/Agent-Loop-Controller/cmd/agentctl",
		GoVCSRevision: revision,
		GoVCSTime:     build.VCSTime,
		Build:         build,
		Structured:    true,
	}
	if !candidateCompatible(candidate, revision, build.SupportedControllerSchemaVersion) {
		t.Fatal("valid candidate was rejected")
	}
	tests := []struct {
		name   string
		mutate func(*binaryEvidence)
	}{
		{name: "missing Go version", mutate: func(value *binaryEvidence) { value.GoVersion = "" }},
		{name: "wrong module", mutate: func(value *binaryEvidence) { value.ModulePath = "example.invalid/agentctl" }},
		{name: "missing Go revision", mutate: func(value *binaryEvidence) { value.GoVCSRevision = "" }},
		{name: "modified Go build", mutate: func(value *binaryEvidence) { value.GoVCSModified = true }},
		{name: "missing Go commit time", mutate: func(value *binaryEvidence) { value.GoVCSTime = "" }},
		{name: "missing structured identity", mutate: func(value *binaryEvidence) { value.Structured = false }},
		{name: "mismatched structured revision", mutate: func(value *binaryEvidence) { value.Build.VCSRevision = strings.Repeat("c", 40) }},
		{name: "mismatched structured time", mutate: func(value *binaryEvidence) { value.Build.VCSTime = "2026-08-27T00:00:00Z" }},
		{name: "mismatched structured modified state", mutate: func(value *binaryEvidence) { value.Build.VCSModified = true }},
		{name: "incompatible schema", mutate: func(value *binaryEvidence) { value.Build.SupportedControllerSchemaVersion-- }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := candidate
			test.mutate(&mutated)
			if candidateCompatible(mutated, revision, build.SupportedControllerSchemaVersion) {
				t.Fatal("unverifiable candidate was accepted")
			}
		})
	}
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
	result, err := manager.Replace(context.Background(), j.UpgradeID, true)
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
	if _, err := manager.Replace(context.Background(), j.UpgradeID, true); err != nil {
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
			if _, err := manager.Replace(context.Background(), j.UpgradeID, true); err == nil {
				t.Fatal("injected replacement interruption was ignored")
			}
			manager.fail = nil
			status, err := manager.Status(context.Background(), j.UpgradeID)
			if err != nil || status.State != test.state {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			resumed, err := manager.Replace(context.Background(), j.UpgradeID, true)
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
	if _, err := manager.Replace(context.Background(), j.UpgradeID, true); err == nil {
		t.Fatal("replacement interruption was ignored")
	}
	manager.fail = nil
	writeFixtureFile(t, j.BinaryPath, "#!/bin/sh\necho drift\n", 0o755)
	status, err := manager.Status(context.Background(), j.UpgradeID)
	if err != nil || status.State != "attention" || status.Reason != "replacement_identity_ambiguous" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, err := manager.Replace(context.Background(), j.UpgradeID, true); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("replace err=%v", err)
	}
}

func TestRollbackDurablePhaseFailuresReconcileAndResume(t *testing.T) {
	for _, point := range []string{"after_rollback_intent", "after_binary_rollback"} {
		t.Run(point, func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			j, _ := seedBundle(t, manager, false)
			if _, err := manager.Replace(context.Background(), j.UpgradeID, true); err != nil {
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
	if _, err := manager.Replace(context.Background(), j.UpgradeID, true); err != nil {
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
		`INSERT INTO controller_integrity_observations VALUES('observation-1','v1','v1','` + strings.Repeat("1", 64) + `',1,1,'ready')`,
		`INSERT INTO controller_integrity_current VALUES(1,'observation-1','` + strings.Repeat("1", 64) + `',1)`,
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
	j, bundle := seedCleanupReady(t, manager, false)
	writeFixtureFile(t, filepath.Join(bundle, "unrelated"), "do not delete", 0o600)
	if _, err := manager.Cleanup(context.Background(), j.UpgradeID); err == nil || !strings.Contains(err.Error(), "unowned") {
		t.Fatalf("cleanup err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "unrelated")); err != nil {
		t.Fatal("cleanup removed unrelated evidence")
	}
}

func TestCleanupResumesAfterCurrentInstallationCommit(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedCleanupReady(t, manager, false)
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

func TestCleanupCommitAndReclamationResumeEveryDurableBoundary(t *testing.T) {
	tests := []struct {
		point              string
		wantActive         bool
		wantBundle         bool
		wantArtifactCount  int
		commitMustBeIntact bool
	}{
		{point: "after_cleanup_intent", wantActive: true, wantBundle: true, wantArtifactCount: 5, commitMustBeIntact: true},
		{point: "after_current_installation", wantActive: true, wantBundle: true, wantArtifactCount: 5, commitMustBeIntact: true},
		{point: "after_cleanup_active_unlink", wantBundle: true, wantArtifactCount: 5, commitMustBeIntact: true},
		{point: "after_cleanup_active_sync", wantBundle: true, wantArtifactCount: 5, commitMustBeIntact: true},
		{point: "after_cleanup_artifact_candidate_manifest", wantBundle: true, wantArtifactCount: 4},
		{point: "after_cleanup_artifact_candidate", wantBundle: true, wantArtifactCount: 3},
		{point: "after_cleanup_artifact_previous", wantBundle: true, wantArtifactCount: 2},
		{point: "after_cleanup_artifact_snapshot", wantBundle: true, wantArtifactCount: 1},
		{point: "after_cleanup_artifact_journal", wantBundle: true, wantArtifactCount: 0},
		{point: "after_cleanup_bundle_removal"},
	}
	for _, test := range tests {
		t.Run(test.point, func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			j, bundle := seedCleanupReady(t, manager, false)
			manager.fail = func(point string) error {
				if point == test.point {
					return errors.New("injected cleanup interruption")
				}
				return nil
			}
			if _, err := manager.Cleanup(context.Background(), j.UpgradeID); err == nil {
				t.Fatal("injected cleanup interruption was ignored")
			}
			if exists(manager.activePath()) != test.wantActive || exists(bundle) != test.wantBundle {
				t.Fatalf("active=%t bundle=%t", exists(manager.activePath()), exists(bundle))
			}
			if test.wantBundle {
				entries, err := os.ReadDir(bundle)
				if err != nil || len(entries) != test.wantArtifactCount {
					t.Fatalf("artifact count=%d want=%d err=%v", len(entries), test.wantArtifactCount, err)
				}
			}
			if test.commitMustBeIntact && test.wantArtifactCount != 5 {
				t.Fatal("cleanup removed a bundle artifact before pointer commit")
			}
			manager.fail = nil
			resumed, err := manager.Cleanup(context.Background(), j.UpgradeID)
			if err != nil || resumed.State != "cleaned" || exists(manager.activePath()) || exists(bundle) {
				t.Fatalf("resumed=%+v err=%v active=%t bundle=%t", resumed, err, exists(manager.activePath()), exists(bundle))
			}
		})
	}
}

func TestPostCommitReclamationDoesNotReconstructRetainedLineage(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedCleanupReady(t, manager, false)
	manager.fail = func(point string) error {
		if point == "after_cleanup_active_sync" {
			return errors.New("injected post-commit interruption")
		}
		return nil
	}
	if _, err := manager.Cleanup(context.Background(), j.UpgradeID); err == nil || exists(manager.activePath()) || !exists(bundle) {
		t.Fatalf("commit interruption err=%v active=%t bundle=%t", err, exists(manager.activePath()), exists(bundle))
	}
	writeFixtureFile(t, filepath.Join(bundle, "journal.json"), "invalid post-commit journal\n", 0o600)
	invalidBundle := manager.bundlePath("upgrade-" + strings.Repeat("d", 32))
	if err := os.Mkdir(invalidBundle, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(invalidBundle, "journal.json"), "invalid retained evidence\n", 0o600)
	manager.fail = nil
	resumed, err := manager.Cleanup(context.Background(), j.UpgradeID)
	if err != nil || resumed.State != "cleaned" || exists(bundle) || !exists(invalidBundle) {
		t.Fatalf("resumed=%+v err=%v target=%t retained=%t", resumed, err, exists(bundle), exists(invalidBundle))
	}
}

func TestRollbackCleanupCommitsPreviousInstallationBeforeReclamation(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedCleanupReady(t, manager, true)
	result, err := manager.Cleanup(context.Background(), j.UpgradeID)
	if err != nil || result.State != "cleaned" || exists(manager.activePath()) || exists(bundle) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	current, err := manager.readCurrentInstallation(j.UpgradeID)
	if err != nil || current != currentInstallationFor(j) || current.BinaryDigest != j.Previous.Digest || current.VCSRevision != "" {
		t.Fatalf("current=%+v err=%v", current, err)
	}
}

func TestHistoricalRecoverySuccessorCleanupRetainsExactPredecessor(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	successor, bundle := seedCleanupReady(t, manager, false)
	successor.SchemaVersion = journalSchemaVersion
	successor.PredecessorID = "upgrade-" + strings.Repeat("d", 32)
	predecessor := writeRetainedCleanupPredecessor(t, manager, successor, "d", true)
	bindCleanupSuccessorToPredecessor(t, manager, &successor, bundle, predecessor)
	result, err := manager.Cleanup(context.Background(), successor.UpgradeID)
	if err != nil || result.State != "cleaned" || exists(manager.activePath()) || exists(bundle) || !exists(manager.bundlePath(predecessor.UpgradeID)) {
		t.Fatalf("result=%+v err=%v active=%t successor=%t predecessor=%t", result, err, exists(manager.activePath()), exists(bundle), exists(manager.bundlePath(predecessor.UpgradeID)))
	}
	persisted, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil || persisted.DatabaseRecovery == nil || persisted.SuccessorID != successor.UpgradeID {
		t.Fatalf("predecessor=%+v err=%v", persisted, err)
	}
}

func TestLiveCleanupRejectsContradictoryPredecessorBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Manager, *journal, *journal, string)
	}{
		{name: "successor revision conflicts", mutate: func(_ *testing.T, _ *Manager, _ *journal, predecessor *journal, _ string) {
			predecessor.SuccessorRevision = strings.Repeat("e", 40)
		}},
		{name: "predecessor revision equals successor", mutate: func(_ *testing.T, _ *Manager, successor *journal, predecessor *journal, _ string) {
			predecessor.Revision = successor.Revision
			predecessor.Candidate.Build.VCSRevision = successor.Revision
		}},
		{name: "predecessor candidate equals successor", mutate: func(t *testing.T, manager *Manager, successor *journal, predecessor *journal, bundle string) {
			predecessor.Candidate = successor.Candidate
			bindCleanupSuccessorToPredecessor(t, manager, successor, bundle, *predecessor)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			successor, bundle := seedCleanupReady(t, manager, false)
			successor.SchemaVersion = journalSchemaVersion
			successor.PredecessorID = "upgrade-" + strings.Repeat("d", 32)
			predecessor := writeRetainedCleanupPredecessor(t, manager, successor, "d", false)
			bindCleanupSuccessorToPredecessor(t, manager, &successor, bundle, predecessor)
			test.mutate(t, manager, &successor, &predecessor, bundle)
			if err := writeJournal(manager.bundlePath(predecessor.UpgradeID), predecessor, manager.uid); err != nil {
				t.Fatal(err)
			}
			before := fixtureTreeFingerprint(t, manager.controllerRoot())
			if _, err := manager.Cleanup(context.Background(), successor.UpgradeID); err == nil {
				t.Fatal("contradictory predecessor lineage was accepted")
			}
			after := fixtureTreeFingerprint(t, manager.controllerRoot())
			if before != after || !exists(manager.activePath()) || !exists(bundle) {
				t.Fatal("contradictory predecessor cleanup changed durable evidence")
			}
		})
	}
}

func TestLegacyBundleMissingCleanupCompletesOnlyAfterCurrentInstallationValidation(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedCleanupReady(t, manager, false)
	current := currentInstallationFor(j)
	if err := writePrivateJSON(filepath.Join(manager.controllerRoot(), "current-installation.json"), current, manager.uid); err != nil {
		t.Fatal(err)
	}
	removeFixtureBundle(t, manager, bundle)
	result, err := manager.Cleanup(context.Background(), j.UpgradeID)
	if err != nil || result.State != "cleaned" || exists(bundle) || exists(manager.activePath()) {
		t.Fatalf("result=%+v err=%v bundle=%t active=%t", result, err, exists(bundle), exists(manager.activePath()))
	}
}

func TestLegacyBundleMissingOrdinarySuccessorCleanupIsUnambiguous(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	j, bundle := seedCleanupReady(t, manager, false)
	current := currentInstallationFor(j)
	if err := writePrivateJSON(filepath.Join(manager.controllerRoot(), "current-installation.json"), current, manager.uid); err != nil {
		t.Fatal(err)
	}
	removeFixtureBundle(t, manager, bundle)
	predecessor := writeRetainedCleanupPredecessor(t, manager, j, "d", false)
	result, err := manager.Cleanup(context.Background(), j.UpgradeID)
	if err != nil || result.State != "cleaned" || exists(manager.activePath()) || !exists(manager.bundlePath(predecessor.UpgradeID)) {
		t.Fatalf("result=%+v err=%v active=%t predecessor=%t", result, err, exists(manager.activePath()), exists(manager.bundlePath(predecessor.UpgradeID)))
	}
}

func TestLegacyBundleMissingUnsafeEvidenceFailsWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Manager, journal)
	}{
		{name: "invalid retained journal", mutate: func(t *testing.T, manager *Manager, _ journal) {
			bundle := manager.bundlePath("upgrade-" + strings.Repeat("d", 32))
			if err := os.Mkdir(bundle, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFixtureFile(t, filepath.Join(bundle, "journal.json"), "not-json\n", 0o600)
		}},
		{name: "multiple ordinary claims", mutate: func(t *testing.T, manager *Manager, successor journal) {
			writeRetainedCleanupPredecessor(t, manager, successor, "d", false)
			writeRetainedCleanupPredecessor(t, manager, successor, "e", false)
		}},
		{name: "recovery-bearing retained journal", mutate: func(t *testing.T, manager *Manager, successor journal) {
			predecessor := writeRetainedCleanupPredecessor(t, manager, successor, "d", true)
			predecessor.SuccessorID = "upgrade-" + strings.Repeat("e", 32)
			predecessor.SuccessorRevision = strings.Repeat("e", 40)
			predecessor.DatabaseRecovery.SuccessorRevision = predecessor.SuccessorRevision
			if err := writeJournal(manager.bundlePath(predecessor.UpgradeID), predecessor, manager.uid); err != nil || validateJournal(predecessor, predecessor.UpgradeID) != nil {
				t.Fatalf("recovery fixture err=%v", err)
			}
		}},
		{name: "unproven ordinary relation", mutate: func(t *testing.T, manager *Manager, successor journal) {
			predecessor := writeRetainedCleanupPredecessor(t, manager, successor, "d", false)
			predecessor.SuccessorRevision = strings.Repeat("e", 40)
			if err := writeJournal(manager.bundlePath(predecessor.UpgradeID), predecessor, manager.uid); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "same predecessor and successor revision", mutate: func(t *testing.T, manager *Manager, successor journal) {
			predecessor := writeRetainedCleanupPredecessor(t, manager, successor, "d", false)
			predecessor.Revision = successor.Revision
			predecessor.Candidate.Build.VCSRevision = successor.Revision
			if err := writeJournal(manager.bundlePath(predecessor.UpgradeID), predecessor, manager.uid); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "same predecessor and installed candidate", mutate: func(t *testing.T, manager *Manager, successor journal) {
			predecessor := writeRetainedCleanupPredecessor(t, manager, successor, "d", false)
			predecessor.Candidate = successor.Candidate
			if err := writeJournal(manager.bundlePath(predecessor.UpgradeID), predecessor, manager.uid); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid current installation", mutate: func(t *testing.T, manager *Manager, successor journal) {
			current := currentInstallationFor(successor)
			current.DatabaseSchema = 0
			if err := writePrivateJSON(filepath.Join(manager.controllerRoot(), "current-installation.json"), current, manager.uid); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			j, bundle := seedCleanupReady(t, manager, false)
			current := currentInstallationFor(j)
			if err := writePrivateJSON(filepath.Join(manager.controllerRoot(), "current-installation.json"), current, manager.uid); err != nil {
				t.Fatal(err)
			}
			removeFixtureBundle(t, manager, bundle)
			test.mutate(t, manager, j)
			before := fixtureTreeFingerprint(t, manager.controllerRoot())
			if _, err := manager.Cleanup(context.Background(), j.UpgradeID); err == nil {
				t.Fatal("unsafe legacy cleanup evidence was accepted")
			}
			after := fixtureTreeFingerprint(t, manager.controllerRoot())
			if before != after || !exists(manager.activePath()) {
				t.Fatal("rejected legacy cleanup changed bytes or metadata")
			}
		})
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
	if err != nil || evidence.Structured || evidence.LegacyVersion != "legacy-v1" || evidence.GoVersion == "" || evidence.ModulePath != "command-line-arguments" {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}

func seedEligibleSuccessor(t *testing.T) (*Manager, journal, string, string, *int) {
	t.Helper()
	pid := os.Getpid()
	manager := newFixtureManager(t, fixtureRunner{selectedTarget: fmt.Sprintf("gui/%d/%s", os.Getuid(), neutralLaunchdLabel), selectedPID: pid})
	predecessor, predecessorBundle := seedBundle(t, manager, true)
	repositoryRoot := commandOutput(t, osCommandRunner{}, "", "git", "rev-parse", "--show-toplevel")
	successorRevision := strings.Repeat("d", 40)
	manager.runner = successorRunner{fixtureRunner: fixtureRunner{selectedTarget: fmt.Sprintf("gui/%d/%s", os.Getuid(), neutralLaunchdLabel), selectedPID: pid}, sourceRoot: repositoryRoot, revisions: map[string]bool{successorRevision: true, strings.Repeat("e", 40): true}}
	if err := atomicallyInstall(filepath.Join(predecessorBundle, "candidate.bin"), predecessor.BinaryPath, 0o755, manager.uid, predecessor.Previous.Digest, predecessor.Candidate.Digest, predecessor.UpgradeID); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO configuration_generations VALUES(1,'` + predecessor.ConfigDigest + `','effective')`,
		`INSERT INTO configuration_authority VALUES(1,1,1)`,
		`INSERT INTO controller_integrity_generation VALUES(1,1)`,
		`INSERT INTO controller_integrity_observations VALUES('observation-1','v1','v1','` + strings.Repeat("1", 64) + `',1,1,'not_ready')`,
		`INSERT INTO controller_integrity_current VALUES(1,'observation-1','` + strings.Repeat("1", 64) + `',1)`,
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
	predecessor.SchemaVersion = journalSchemaVersion
	predecessor.Phase = "attention"
	predecessor.FailureReason = "integrity_not_ready"
	predecessor.BootstrapIntentAt = &now
	predecessor.UpdatedAt = now
	snapshotDigest, err := createConsistentSnapshot(context.Background(), predecessor.DatabasePath, filepath.Join(predecessorBundle, "snapshot.db"), manager.uid, predecessor.Database)
	if err != nil {
		t.Fatal(err)
	}
	predecessor.SnapshotDigest = snapshotDigest
	if err := writeJournal(predecessorBundle, predecessor, manager.uid); err != nil {
		t.Fatal(err)
	}
	started, err := processStartIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := heartbeatEvidence{SchemaVersion: 3, WorkerInstanceID: "fixture-worker", ProcessID: pid, ProcessStartID: started, BuildIdentity: predecessor.Candidate.Build.BuildIdentity, ConfigurationDigest: predecessor.ConfigDigest, Status: "parked", ObservedAt: now}
	if err := writePrivateJSON(predecessor.ConfigPath+".worker-status.json", heartbeat, manager.uid); err != nil {
		t.Fatal(err)
	}

	buildCount := 0
	manager.buildCandidate = func(_ context.Context, revision string, databaseSchema int) (preparedCandidate, error) {
		buildCount++
		build := fixtureBuild(revision)
		build.BuildIdentity = "sha256:" + strings.Repeat("d", 64)
		raw := binaryScript(build)
		path := filepath.Join(manager.controllerRoot(), fmt.Sprintf("successor-candidate-%d", buildCount))
		writeFixtureFile(t, path, raw, 0o755)
		digest, _ := digestFile(path)
		evidence := binaryEvidence{Digest: digest, Size: int64(len(raw)), Mode: 0o755, GoVersion: "go1.26.1", ModulePath: "github.com/ifan0927/Agent-Loop-Controller/cmd/agentctl", GoVCSRevision: revision, GoVCSTime: build.VCSTime, Build: build, Structured: true}
		if !candidateCompatible(evidence, revision, databaseSchema) {
			t.Fatal("fixture successor candidate is incompatible")
		}
		return preparedCandidate{Evidence: evidence, Path: path, Cleanup: func() { _ = os.Remove(path) }}, nil
	}
	return manager, predecessor, predecessorBundle, successorRevision, &buildCount
}

func seedAuthorizedDatabaseRelocation(t *testing.T) (*Manager, journal, string, string, databaseEvidence) {
	t.Helper()
	manager, predecessor, predecessorBundle, revision, _ := seedEligibleSuccessor(t)
	runner := manager.runner.(successorRunner)
	runner.fixtureRunner = fixtureRunner{}
	manager.runner = runner
	workerLock, err := os.OpenFile(filepath.Join(filepath.Dir(predecessor.DatabasePath), "worker.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil || workerLock.Close() != nil {
		t.Fatal("fixture worker lock is unavailable")
	}

	db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE configuration_authority ADD COLUMN canonical_config_path TEXT`,
		`ALTER TABLE configuration_authority ADD COLUMN database_path TEXT`,
		`ALTER TABLE configuration_authority ADD COLUMN authority_version INTEGER`,
		`UPDATE configuration_authority SET canonical_config_path='` + predecessor.ConfigPath + `',database_path='` + predecessor.DatabasePath + `',authority_version=1 WHERE authority_id=1`,
		`CREATE TABLE configuration_apply_intents(status TEXT NOT NULL)`,
		`CREATE TABLE configuration_recovery_intents(status TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := configurationadapter.NewFiles(predecessor.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.BindDatabaseIdentity(predecessor.DatabasePath, databaseIdentity(predecessor.Database)); err != nil {
		t.Fatal(err)
	}
	if err := files.PublishLocator(predecessor.DatabasePath); err != nil {
		t.Fatal(err)
	}
	replacementPath := predecessor.DatabasePath + ".relocated"
	if _, err := createConsistentSnapshot(context.Background(), predecessor.DatabasePath, replacementPath, manager.uid, predecessor.Database); err != nil {
		t.Fatal(err)
	}
	displacedPath := predecessor.DatabasePath + ".displaced"
	if err := os.Rename(predecessor.DatabasePath, displacedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, predecessor.DatabasePath); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectory(filepath.Dir(predecessor.DatabasePath)); err != nil {
		t.Fatal(err)
	}
	replacement, err := inspectDatabaseReadOnly(predecessor.DatabasePath, manager.uid)
	if err != nil || replacement == predecessor.Database {
		t.Fatalf("replacement=%+v old=%+v err=%v", replacement, predecessor.Database, err)
	}
	return manager, predecessor, predecessorBundle, revision, replacement
}

func seedMonotonicIntegrityPendingRelocation(t *testing.T) (*Manager, journal, string, string, databaseEvidence) {
	t.Helper()
	manager, predecessor, predecessorBundle, revision, replacement := seedAuthorizedDatabaseRelocation(t)
	db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE controller_integrity_generation SET generation=2 WHERE singleton=1`,
		`UPDATE controller_integrity_observations SET effective_readiness='conflict' WHERE observation_id='observation-1'`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	predecessor.FailureReason = "integrity_conflict"
	if err := writeJournal(predecessorBundle, predecessor, manager.uid); err != nil {
		t.Fatal(err)
	}
	return manager, predecessor, predecessorBundle, revision, replacement
}

func TestSuccessorRecoveryPreviewAndPrepareRebindOnlyVerifiedDatabaseIdentity(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, replacement := seedAuthorizedDatabaseRelocation(t)
	preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil || preview.State != "eligible" || !validSHA256(preview.PreviewDigest) || len(preview.RequiredConfirmations) != 2 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	rawPreview, _ := json.Marshal(preview)
	for _, forbidden := range []string{predecessor.ConfigPath, predecessor.DatabasePath, fmt.Sprint(predecessor.Database.Inode), fmt.Sprint(replacement.Inode), predecessor.ConfigDigest} {
		if strings.Contains(string(rawPreview), forbidden) {
			t.Fatalf("preview exposed private evidence %q: %s", forbidden, rawPreview)
		}
	}
	result, err := manager.RecoverPrepareSuccessor(context.Background(), SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true})
	if err != nil || result.State != "prepared" || result.Reason != "verified_recovered_successor_activated" || result.NextAction != "replace" || !validUpgradeID(result.UpgradeID) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	locator, found, err := configurationadapter.ReadLocator(predecessor.ConfigPath)
	if err != nil || !found || locator.DatabaseIdentity != databaseIdentity(replacement) {
		t.Fatalf("locator=%+v found=%t err=%v", locator, found, err)
	}
	persisted, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil || persisted.Phase != "superseded" || persisted.Database != replacement || persisted.DatabaseRecovery == nil || persisted.DatabaseRecovery.OldDatabase != predecessor.Database || persisted.DatabaseRecovery.ReplacementDatabase != replacement || persisted.DatabaseRecovery.PreviewDigest != preview.PreviewDigest || persisted.DatabaseRecovery.LocatorPublishedAt == nil {
		t.Fatalf("predecessor=%+v err=%v", persisted, err)
	}
	if _, err := os.Stat(filepath.Join(predecessorBundle, "snapshot.db")); err != nil {
		t.Fatal("failed predecessor evidence was not preserved")
	}
}

func TestSuccessorRecoveryAcceptsMonotonicIntegrityPendingAfterRecordedConflict(t *testing.T) {
	manager, predecessor, _, revision, replacement := seedMonotonicIntegrityPendingRelocation(t)
	preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil || preview.State != "eligible" || !validSHA256(preview.PreviewDigest) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	rawPreview, _ := json.Marshal(preview)
	for _, forbidden := range []string{"integrity_conflict", "integrity_pending", "current_newer_than_published", "observation-1", predecessor.ConfigDigest, fmt.Sprint(predecessor.Database.Inode), fmt.Sprint(replacement.Inode)} {
		if strings.Contains(string(rawPreview), forbidden) {
			t.Fatalf("preview exposed private readiness evidence %q: %s", forbidden, rawPreview)
		}
	}
	result, err := manager.RecoverPrepareSuccessor(context.Background(), SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true})
	if err != nil || result.State != "prepared" || result.Reason != "verified_recovered_successor_activated" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	persisted, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil || persisted.DatabaseRecovery == nil {
		t.Fatalf("predecessor=%+v err=%v", persisted, err)
	}
	readiness := persisted.DatabaseRecovery.Verification.Readiness
	if readiness.Relationship != recoveryReadinessIntegrityConflictPending || readiness.PredecessorReason != "integrity_conflict" || readiness.ReplacementReason != "integrity_pending" || readiness.GenerationRelationship != integrityGenerationAdvanced || readiness.CurrentGeneration != 2 || readiness.PublishedGeneration != 1 || !readiness.CurrentObservationValid || readiness.ObservationReadiness != "conflict" {
		t.Fatalf("readiness=%+v", readiness)
	}
}

func TestSuccessorRecoveryRejectsUnverifiedIntegrityPendingRelationships(t *testing.T) {
	for _, scenario := range []struct {
		name              string
		predecessorReason string
		statements        []string
	}{
		{name: "generation did not advance", predecessorReason: "integrity_conflict", statements: []string{`UPDATE controller_integrity_observations SET effective_readiness='unknown' WHERE observation_id='observation-1'`}},
		{name: "published generation is newer", predecessorReason: "integrity_conflict", statements: []string{`UPDATE controller_integrity_generation SET generation=0 WHERE singleton=1`, `UPDATE controller_integrity_observations SET effective_readiness='conflict' WHERE observation_id='observation-1'`}},
		{name: "current observation is missing", predecessorReason: "integrity_conflict", statements: []string{`UPDATE controller_integrity_generation SET generation=2 WHERE singleton=1`, `DELETE FROM controller_integrity_current WHERE singleton=1`}},
		{name: "current observation digest conflicts", predecessorReason: "integrity_conflict", statements: []string{`UPDATE controller_integrity_generation SET generation=2 WHERE singleton=1`, `UPDATE controller_integrity_current SET observation_digest='` + strings.Repeat("2", 64) + `' WHERE singleton=1`}},
		{name: "current observation generation conflicts", predecessorReason: "integrity_conflict", statements: []string{`UPDATE controller_integrity_generation SET generation=2 WHERE singleton=1`, `UPDATE controller_integrity_observations SET target_generation=0 WHERE observation_id='observation-1'`}},
		{name: "predecessor reason is unrelated", predecessorReason: "integrity_not_ready", statements: []string{`UPDATE controller_integrity_generation SET generation=2 WHERE singleton=1`}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager, predecessor, predecessorBundle, revision, _ := seedAuthorizedDatabaseRelocation(t)
			db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range scenario.statements {
				if _, err := db.Exec(statement); err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			predecessor.FailureReason = scenario.predecessorReason
			if err := writeJournal(predecessorBundle, predecessor, manager.uid); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
			if _, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision}); err == nil {
				t.Fatal("unverified integrity-pending relationship produced a recovery preview")
			}
			after, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
			if string(before) != string(after) {
				t.Fatal("rejected preview changed predecessor evidence")
			}
		})
	}
}

func TestSuccessorRecoveryRejectsIntegrityGenerationDriftAfterPreview(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedMonotonicIntegrityPendingRelocation(t)
	preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE controller_integrity_generation SET generation=3 WHERE singleton=1`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	request := SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true}
	if _, err := manager.RecoverPrepareSuccessor(context.Background(), request); err == nil || !strings.Contains(err.Error(), "preview changed") {
		t.Fatalf("generation drift err=%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	if string(before) != string(after) {
		t.Fatal("generation drift created recovery intent")
	}
}

func TestMonotonicIntegrityPendingRecoveryReplaysEveryDurableTransition(t *testing.T) {
	for _, point := range []string{"after_successor_recovery_intent", "after_successor_recovery_locator", "after_successor_recovery_journal", "after_successor_bundle", "after_predecessor_superseded", "after_successor_activation"} {
		t.Run(point, func(t *testing.T) {
			manager, predecessor, _, revision, replacement := seedMonotonicIntegrityPendingRelocation(t)
			preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
			if err != nil {
				t.Fatal(err)
			}
			request := SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true}
			manager.fail = func(observed string) error {
				if observed == point {
					return errors.New("injected monotonic recovery interruption")
				}
				return nil
			}
			if _, err := manager.RecoverPrepareSuccessor(context.Background(), request); err == nil {
				t.Fatal("injected monotonic recovery interruption was ignored")
			}
			intent, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
			if err != nil || intent.SuccessorID == "" {
				t.Fatalf("intent=%+v err=%v", intent, err)
			}
			manager.fail = nil
			resumed, err := manager.RecoverPrepareSuccessor(context.Background(), request)
			if err != nil || resumed.UpgradeID != intent.SuccessorID || resumed.State != "prepared" {
				t.Fatalf("resumed=%+v expected=%s err=%v", resumed, intent.SuccessorID, err)
			}
			entries, _ := os.ReadDir(manager.upgradeRoot())
			bundleCount := 0
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "upgrade-") {
					bundleCount++
				}
			}
			locator, _, _ := configurationadapter.ReadLocator(predecessor.ConfigPath)
			if bundleCount != 2 || locator.DatabaseIdentity != databaseIdentity(replacement) {
				t.Fatalf("bundle_count=%d locator=%+v", bundleCount, locator)
			}
		})
	}
}

func TestExactRecoveryVersionOneIntentRemainsReplayable(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, replacement := seedAuthorizedDatabaseRelocation(t)
	contentDigest, err := replacementDatabaseContentDigest(predecessor.DatabasePath, manager.uid, replacement)
	if err != nil {
		t.Fatal(err)
	}
	legacyAuthority := struct {
		CanonicalConfigPath string `json:"canonical_config_path"`
		DatabasePath        string `json:"database_path"`
		DesiredID           int64  `json:"desired_id"`
		EffectiveID         int64  `json:"effective_id"`
		AuthorityVersion    int64  `json:"authority_version"`
		DesiredDigest       string `json:"desired_digest"`
		DesiredLifecycle    string `json:"desired_lifecycle"`
		EffectiveDigest     string `json:"effective_digest"`
		EffectiveLifecycle  string `json:"effective_lifecycle"`
		IntegrityGeneration int64  `json:"integrity_generation"`
		PublishedGeneration int64  `json:"published_generation"`
		IntegrityReadiness  string `json:"integrity_readiness"`
	}{
		CanonicalConfigPath: predecessor.ConfigPath, DatabasePath: predecessor.DatabasePath,
		DesiredID: 1, EffectiveID: 1, AuthorityVersion: 1,
		DesiredDigest: predecessor.ConfigDigest, DesiredLifecycle: "effective",
		EffectiveDigest: predecessor.ConfigDigest, EffectiveLifecycle: "effective",
		IntegrityGeneration: 1, PublishedGeneration: 1, IntegrityReadiness: "not_ready",
	}
	legacyAuthorityRaw, err := json.Marshal(legacyAuthority)
	if err != nil {
		t.Fatal(err)
	}
	legacyAuthorityDigest := sha256Hex(legacyAuthorityRaw)
	legacyVerification := legacyReplacementDatabaseVerification{
		ContentDigest: contentDigest, AuthorityDigest: legacyAuthorityDigest, SchemaVersion: replacement.SchemaVersion,
		IntegrityOK: true, ForeignKeysOK: true, BindingMatches: true, DesiredConfigurationMatch: true,
		ReadinessReason: predecessor.FailureReason,
	}
	installed, err := inspectBinary(context.Background(), manager.runner, predecessor.BinaryPath, manager.uid)
	if err != nil {
		t.Fatal(err)
	}
	locator, found, err := configurationadapter.ReadLocator(predecessor.ConfigPath)
	if err != nil || !found {
		t.Fatalf("locator found=%t err=%v", found, err)
	}
	legacyPreview := legacyRecoveryPreviewEvidence{
		UpgradeID: predecessor.UpgradeID, Revision: revision, Supervisor: predecessor.Supervisor, FailureReason: predecessor.FailureReason,
		ConfigDigest: predecessor.ConfigDigest, Installed: installed, OldDatabase: predecessor.Database, Replacement: replacement,
		Verification: legacyVerification, LocatorVersion: locator.Version, LocatorConfigPath: locator.ConfigPath,
		LocatorDBPath: locator.DatabasePath, SupervisorsAbsent: true,
	}
	legacyRaw, err := json.Marshal(legacyPreview)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest := sha256Hex(legacyRaw)
	preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	manager.fail = func(point string) error {
		if point == "after_successor_recovery_intent" {
			return errors.New("stop before legacy replay")
		}
		return nil
	}
	request := SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true}
	if _, err := manager.RecoverPrepareSuccessor(context.Background(), request); err == nil {
		t.Fatal("recovery intent interruption was ignored")
	}
	intent, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil || intent.DatabaseRecovery == nil {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	if intent.DatabaseRecovery.Verification.AuthorityDigest == legacyAuthorityDigest {
		t.Fatal("version-two fixture did not prove its expanded authority digest differs from version one")
	}
	intent.DatabaseRecovery.Version = 1
	intent.DatabaseRecovery.PreviewDigest = legacyDigest
	intent.DatabaseRecovery.Verification = replacementDatabaseVerification{
		ContentDigest: legacyVerification.ContentDigest, AuthorityDigest: legacyVerification.AuthorityDigest,
		SchemaVersion: legacyVerification.SchemaVersion, IntegrityOK: legacyVerification.IntegrityOK,
		ForeignKeysOK: legacyVerification.ForeignKeysOK, BindingMatches: legacyVerification.BindingMatches,
		DesiredConfigurationMatch: legacyVerification.DesiredConfigurationMatch, LegacyReadinessReason: legacyVerification.ReadinessReason,
	}
	if err := writeJournal(predecessorBundle, intent, manager.uid); err != nil {
		t.Fatal(err)
	}
	manager.fail = nil
	request.PreviewDigest = legacyDigest
	resumed, err := manager.RecoverPrepareSuccessor(context.Background(), request)
	if err != nil || resumed.State != "prepared" || resumed.UpgradeID != intent.SuccessorID {
		t.Fatalf("resumed=%+v expected=%s err=%v", resumed, intent.SuccessorID, err)
	}
	replayedLocator, replayedFound, err := configurationadapter.ReadLocator(predecessor.ConfigPath)
	if err != nil || !replayedFound || replayedLocator.DatabaseIdentity != databaseIdentity(replacement) {
		t.Fatalf("locator=%+v found=%t err=%v", replayedLocator, replayedFound, err)
	}
}

func TestSuccessorRecoveryDurableTransitionsReplayOneSuccessor(t *testing.T) {
	for _, point := range []string{"after_successor_recovery_intent", "after_successor_recovery_locator", "after_successor_recovery_journal", "after_successor_bundle", "after_predecessor_superseded", "after_successor_activation"} {
		t.Run(point, func(t *testing.T) {
			manager, predecessor, _, revision, replacement := seedAuthorizedDatabaseRelocation(t)
			preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
			if err != nil {
				t.Fatal(err)
			}
			request := SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true}
			manager.fail = func(observed string) error {
				if observed == point {
					return errors.New("injected recovery interruption")
				}
				return nil
			}
			if _, err := manager.RecoverPrepareSuccessor(context.Background(), request); err == nil {
				t.Fatal("injected recovery interruption was ignored")
			}
			intent, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
			if err != nil || intent.SuccessorID == "" {
				t.Fatalf("intent=%+v err=%v", intent, err)
			}
			successorID := intent.SuccessorID
			status, statusErr := manager.Status(context.Background(), predecessor.UpgradeID)
			if statusErr != nil || status.NextAction != "successor-recover-prepare" && status.NextAction != "status_successor" {
				t.Fatalf("status=%+v err=%v", status, statusErr)
			}
			manager.fail = nil
			resumed, err := manager.RecoverPrepareSuccessor(context.Background(), request)
			if err != nil || resumed.UpgradeID != successorID || resumed.State != "prepared" {
				t.Fatalf("resumed=%+v expected=%s err=%v", resumed, successorID, err)
			}
			entries, _ := os.ReadDir(manager.upgradeRoot())
			bundleCount := 0
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "upgrade-") {
					bundleCount++
				}
			}
			locator, _, _ := configurationadapter.ReadLocator(predecessor.ConfigPath)
			if bundleCount != 2 || locator.DatabaseIdentity != databaseIdentity(replacement) {
				t.Fatalf("bundle_count=%d locator=%+v", bundleCount, locator)
			}
		})
	}
}

func TestSuccessorRecoveryRejectsPreviewDriftAndThirdIdentityWithoutTransfer(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedAuthorizedDatabaseRelocation(t)
	preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	badDigest := strings.Repeat("f", 64)
	if _, err := manager.RecoverPrepareSuccessor(context.Background(), SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: badDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true}); err == nil || !strings.Contains(err.Error(), "preview changed") {
		t.Fatalf("preview drift err=%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	if string(before) != string(after) {
		t.Fatal("preview drift changed managed upgrade evidence")
	}
	manager.fail = func(point string) error {
		if point == "after_successor_recovery_intent" {
			return errors.New("stop after intent")
		}
		return nil
	}
	request := SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true}
	if _, err := manager.RecoverPrepareSuccessor(context.Background(), request); err == nil {
		t.Fatal("recovery intent interruption was ignored")
	}
	manager.fail = nil
	locator, _, err := configurationadapter.ReadLocator(predecessor.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	files, err := configurationadapter.NewFiles(predecessor.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, acquired, err := files.AcquireMutation()
	if err != nil || !acquired {
		t.Fatalf("lock acquired=%t err=%v", acquired, err)
	}
	third := locator.DatabaseIdentity
	third.Inode++
	// A syntactically valid but unobserved third identity is persisted directly
	// only to model an attacker or unsupported manual edit after durable intent.
	raw, _ := json.Marshal(configurationadapter.AuthorityLocator{Version: locator.Version, ConfigPath: locator.ConfigPath, DatabasePath: locator.DatabasePath, DatabaseIdentity: third})
	raw = append(raw, '\n')
	if err := atomicWritePrivate(filepath.Join(filepath.Dir(predecessor.ConfigPath), "authority", "locator.json"), raw, manager.uid); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RecoverPrepareSuccessor(context.Background(), request); err == nil || !strings.Contains(err.Error(), "unexpected database identity") {
		t.Fatalf("third identity err=%v", err)
	}
}

func TestReplacementDatabaseVerifierCoversWALIntegrityBindingAndSchema(t *testing.T) {
	t.Run("WAL", func(t *testing.T) {
		manager, predecessor, _, _, replacement := seedAuthorizedDatabaseRelocation(t)
		db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, statement := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`, `CREATE TABLE recovery_wal_fixture(value TEXT)`, `INSERT INTO recovery_wal_fixture VALUES('durable')`} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if !exists(predecessor.DatabasePath + "-wal") {
			t.Fatal("WAL fixture was not retained")
		}
		observed, verification, err := verifyReplacementDatabase(context.Background(), predecessor.DatabasePath, predecessor.ConfigPath, predecessor.ConfigDigest, predecessor.FailureReason, predecessor.Candidate.Build.SupportedControllerSchemaVersion, manager.uid)
		if err != nil || observed.Device != replacement.Device || observed.Inode != replacement.Inode || !verification.IntegrityOK || !verification.ForeignKeysOK || !validSHA256(verification.ContentDigest) {
			t.Fatalf("observed=%+v verification=%+v err=%v", observed, verification, err)
		}
	})

	for _, scenario := range []struct {
		name   string
		mutate func(*testing.T, *Manager, journal)
	}{
		{name: "binding mismatch", mutate: func(t *testing.T, _ *Manager, predecessor journal) {
			db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE configuration_authority SET canonical_config_path='/unexpected/config.json' WHERE authority_id=1`); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "schema drift", mutate: func(t *testing.T, _ *Manager, predecessor journal) {
			db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO schema_migrations(version) VALUES(43)`); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hard link", mutate: func(t *testing.T, _ *Manager, predecessor journal) {
			if err := os.Link(predecessor.DatabasePath, predecessor.DatabasePath+".link"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corruption", mutate: func(t *testing.T, _ *Manager, predecessor journal) {
			file, err := os.OpenFile(predecessor.DatabasePath, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt([]byte("not-sqlite"), 0); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager, predecessor, _, revision, _ := seedAuthorizedDatabaseRelocation(t)
			scenario.mutate(t, manager, predecessor)
			if _, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision}); err == nil {
				t.Fatal("unsafe replacement database produced a recovery preview")
			}
		})
	}
}

func TestSuccessorRecoveryRejectsDatabaseContentAndSupervisorDriftBeforeIntent(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedAuthorizedDatabaseRelocation(t)
	preview, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE post_preview_drift(value TEXT)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	request := SuccessorRecoverPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision, PreviewDigest: preview.PreviewDigest, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true}
	if _, err := manager.RecoverPrepareSuccessor(context.Background(), request); err == nil || !strings.Contains(err.Error(), "preview changed") {
		t.Fatalf("content drift err=%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	if string(before) != string(after) {
		t.Fatal("content drift created recovery intent")
	}

	manager, predecessor, _, revision, _ = seedAuthorizedDatabaseRelocation(t)
	runner := manager.runner.(successorRunner)
	runner.fixtureRunner = fixtureRunner{selectedTarget: fmt.Sprintf("gui/%d/%s", os.Getuid(), neutralLaunchdLabel), selectedPID: os.Getpid()}
	manager.runner = runner
	if _, err := manager.PreviewSuccessorRecovery(context.Background(), SuccessorRecoveryPreviewRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision}); err == nil || !strings.Contains(err.Error(), "every supervisor") {
		t.Fatalf("running supervisor err=%v", err)
	}
}

func TestSuccessorPrepareLinksTerminalPredecessorAndRejectsOrdinaryPrepare(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedEligibleSuccessor(t)
	result, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil || result.State != "prepared" || result.PredecessorUpgradeID != predecessor.UpgradeID || !validUpgradeID(result.UpgradeID) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	persisted, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil || persisted.Phase != "superseded" || persisted.SuccessorID != result.UpgradeID || persisted.SupersededAt == nil {
		t.Fatalf("predecessor=%+v err=%v", persisted, err)
	}
	if _, err := os.Stat(filepath.Join(predecessorBundle, "snapshot.db")); err != nil {
		t.Fatal("predecessor snapshot evidence was not preserved")
	}
	active, err := manager.readActiveUpgrade()
	if err != nil || active.UpgradeID != result.UpgradeID {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	if _, err := manager.Prepare(context.Background(), PrepareRequest{Revision: revision, Supervisor: predecessor.Supervisor, BinaryPath: predecessor.BinaryPath, ConfigPath: predecessor.ConfigPath}); err == nil || !strings.Contains(err.Error(), "active managed upgrade") {
		t.Fatalf("ordinary prepare err=%v", err)
	}
}

func TestIneligibleSuccessorAttentionDoesNotMutateEvidence(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedEligibleSuccessor(t)
	predecessor.FailureReason = "heartbeat_identity_failed"
	if err := writeJournal(predecessorBundle, predecessor, manager.uid); err != nil {
		t.Fatal(err)
	}
	beforeJournal, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	beforeEntries, _ := os.ReadDir(manager.upgradeRoot())
	if _, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision}); err == nil || !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("successor err=%v", err)
	}
	afterJournal, _ := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	afterEntries, _ := os.ReadDir(manager.upgradeRoot())
	if string(beforeJournal) != string(afterJournal) || len(beforeEntries) != len(afterEntries) {
		t.Fatal("ineligible successor preparation changed durable evidence")
	}
}

func TestSuccessorPrepareDurableTransitionsReplayWithoutDuplicates(t *testing.T) {
	for _, point := range []string{"after_successor_prepare_intent", "after_successor_bundle", "after_predecessor_superseded", "after_successor_activation"} {
		t.Run(point, func(t *testing.T) {
			manager, predecessor, _, revision, buildCount := seedEligibleSuccessor(t)
			manager.fail = func(observed string) error {
				if observed == point {
					return errors.New("injected successor interruption")
				}
				return nil
			}
			if _, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision}); err == nil {
				t.Fatal("injected successor interruption was ignored")
			}
			manager.fail = nil
			resumed, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
			if err != nil || resumed.State != "prepared" {
				t.Fatalf("resumed=%+v err=%v", resumed, err)
			}
			persisted, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
			if err != nil || persisted.SuccessorID != resumed.UpgradeID || persisted.Phase != "superseded" {
				t.Fatalf("predecessor=%+v err=%v", persisted, err)
			}
			entries, _ := os.ReadDir(manager.upgradeRoot())
			bundleCount := 0
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "upgrade-") {
					bundleCount++
				}
			}
			if bundleCount != 2 || *buildCount > 1 {
				t.Fatalf("bundle_count=%d build_count=%d", bundleCount, *buildCount)
			}
		})
	}
}

func TestSuccessorPrepareRemovesOnlyVerifiedPartialOwnedStagingOnReplay(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedEligibleSuccessor(t)
	manager.fail = func(point string) error {
		if point == "after_successor_prepare_intent" {
			return errors.New("injected successor interruption")
		}
		return nil
	}
	if _, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision}); err == nil {
		t.Fatal("injected successor interruption was ignored")
	}
	manager.fail = nil
	intent, _, err := manager.loadJournal(predecessor.UpgradeID)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(manager.upgradeRoot(), "."+intent.SuccessorID+".prepare")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(staging, "candidate.bin"), "partial", 0o600)
	journalTemporary := filepath.Join(predecessorBundle, ".journal.json.tmp")
	activeTemporary := filepath.Join(manager.upgradeRoot(), ".active.json.tmp")
	writeFixtureFile(t, journalTemporary, "partial", 0o600)
	writeFixtureFile(t, activeTemporary, "partial", 0o600)
	resumed, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil || resumed.State != "prepared" || exists(staging) || exists(journalTemporary) || exists(activeTemporary) {
		t.Fatalf("resumed=%+v err=%v staging=%t journal_tmp=%t active_tmp=%t", resumed, err, exists(staging), exists(journalTemporary), exists(activeTemporary))
	}
}

func TestSuccessorReplacementRequiresFreshBackupAndCannotRestorePreBootstrapBinary(t *testing.T) {
	manager, predecessor, _, revision, _ := seedEligibleSuccessor(t)
	prepared, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = fixtureRunner{}
	if _, err := manager.Replace(context.Background(), prepared.UpgradeID, false); err == nil || !strings.Contains(err.Error(), "newly confirmed") {
		t.Fatalf("replacement without backup err=%v", err)
	}
	if _, err := manager.Replace(context.Background(), prepared.UpgradeID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rollback(context.Background(), prepared.UpgradeID); err != nil {
		t.Fatal(err)
	}
	digest, _ := digestFile(predecessor.BinaryPath)
	if digest != predecessor.Candidate.Digest || digest == predecessor.Previous.Digest {
		t.Fatalf("rollback digest=%s predecessor_candidate=%s pre_bootstrap=%s", digest, predecessor.Candidate.Digest, predecessor.Previous.Digest)
	}
}

func TestManagedSuccessorCanBecomeAnImmutablePredecessorInALaterRecovery(t *testing.T) {
	manager, predecessor, _, firstRevision, _ := seedEligibleSuccessor(t)
	runner := manager.runner.(successorRunner)
	first, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: firstRevision})
	if err != nil {
		t.Fatal(err)
	}
	runner.fixtureRunner = fixtureRunner{}
	manager.runner = runner
	if _, err := manager.Replace(context.Background(), first.UpgradeID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AuthorizeBootstrap(context.Background(), first.UpgradeID); err != nil {
		t.Fatal(err)
	}
	firstJournal, _, err := manager.loadJournal(first.UpgradeID)
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	runner.fixtureRunner = fixtureRunner{selectedTarget: fmt.Sprintf("gui/%d/%s", os.Getuid(), neutralLaunchdLabel), selectedPID: pid}
	manager.runner = runner
	started, err := processStartIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := heartbeatEvidence{SchemaVersion: 3, WorkerInstanceID: "first-successor-worker", ProcessID: pid, ProcessStartID: started, BuildIdentity: firstJournal.Candidate.Build.BuildIdentity, ConfigurationDigest: firstJournal.ConfigDigest, Status: "parked", ObservedAt: manager.now()}
	if err := writePrivateJSON(firstJournal.ConfigPath+".worker-status.json", heartbeat, manager.uid); err != nil {
		t.Fatal(err)
	}
	attention, err := manager.Observe(context.Background(), first.UpgradeID)
	if err != nil || attention.State != "attention" || attention.ControllerReadiness != "not_ready" {
		t.Fatalf("attention=%+v err=%v", attention, err)
	}
	secondRevision := strings.Repeat("e", 40)
	second, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: first.UpgradeID, Revision: secondRevision})
	if err != nil || second.PredecessorUpgradeID != first.UpgradeID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	terminalFirst, _, err := manager.loadBundleJournal(first.UpgradeID)
	if err != nil || terminalFirst.Phase != "superseded" || terminalFirst.PredecessorID != predecessor.UpgradeID || terminalFirst.SuccessorID != second.UpgradeID {
		t.Fatalf("terminal first=%+v err=%v", terminalFirst, err)
	}
}

func TestSuccessfulSuccessorCleanupRetainsTerminalPredecessor(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedEligibleSuccessor(t)
	prepared, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = fixtureRunner{}
	if _, err := manager.Replace(context.Background(), prepared.UpgradeID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AuthorizeBootstrap(context.Background(), prepared.UpgradeID); err != nil {
		t.Fatal(err)
	}
	successor, _, err := manager.loadJournal(prepared.UpgradeID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteURI(successor.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE controller_integrity_observations SET effective_readiness='ready' WHERE observation_id='observation-1'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	manager.runner = fixtureRunner{selectedTarget: fmt.Sprintf("gui/%d/%s", os.Getuid(), neutralLaunchdLabel), selectedPID: pid}
	started, err := processStartIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := heartbeatEvidence{SchemaVersion: 3, WorkerInstanceID: "successor-worker", ProcessID: pid, ProcessStartID: started, BuildIdentity: successor.Candidate.Build.BuildIdentity, ConfigurationDigest: successor.ConfigDigest, Status: "parked", ObservedAt: manager.now()}
	if err := writePrivateJSON(successor.ConfigPath+".worker-status.json", heartbeat, manager.uid); err != nil {
		t.Fatal(err)
	}
	observed, err := manager.Observe(context.Background(), successor.UpgradeID)
	if err != nil || observed.State != "healthy" {
		t.Fatalf("observed=%+v err=%v", observed, err)
	}
	cleaned, err := manager.Cleanup(context.Background(), successor.UpgradeID)
	if err != nil || cleaned.State != "cleaned" {
		t.Fatalf("cleaned=%+v err=%v", cleaned, err)
	}
	if exists(manager.activePath()) || exists(manager.bundlePath(successor.UpgradeID)) || !exists(predecessorBundle) {
		t.Fatalf("active=%t successor=%t predecessor=%t", exists(manager.activePath()), exists(manager.bundlePath(successor.UpgradeID)), exists(predecessorBundle))
	}
	persisted, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil || persisted.Phase != "superseded" || persisted.SuccessorID != successor.UpgradeID {
		t.Fatalf("predecessor=%+v err=%v", persisted, err)
	}
	status, err := manager.Status(context.Background(), predecessor.UpgradeID)
	if err != nil || status.State != "superseded" || status.SuccessorUpgradeID != successor.UpgradeID || status.NextAction != "none" {
		t.Fatalf("terminal status=%+v err=%v", status, err)
	}
}
