package localupgrade

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type historicalTreeEntry struct {
	Mode    fs.FileMode
	Size    int64
	ModTime time.Time
	Data    string
}

func snapshotHistoricalTree(t *testing.T, root string) map[string]historicalTreeEntry {
	t.Helper()
	result := make(map[string]historicalTreeEntry)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := historicalTreeEntry{Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.Data = string(raw)
		}
		result[relative] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func fixtureHistoricalRecovery(value journal, version int, published bool) *databaseRecoveryEvidence {
	oldDatabase := value.Database
	replacementDatabase := value.Database
	if published {
		oldDatabase.Inode++
	} else {
		replacementDatabase.Inode++
	}
	verification := replacementDatabaseVerification{
		ContentDigest:             strings.Repeat("4", 64),
		AuthorityDigest:           strings.Repeat("5", 64),
		SchemaVersion:             value.Candidate.Build.SupportedControllerSchemaVersion,
		IntegrityOK:               true,
		ForeignKeysOK:             true,
		BindingMatches:            true,
		DesiredConfigurationMatch: true,
	}
	if version == 1 {
		verification.LegacyReadinessReason = value.FailureReason
	} else {
		verification.Readiness = recoveryReadinessVerification{
			Relationship:           recoveryReadinessExactMatch,
			PredecessorReason:      value.FailureReason,
			ReplacementReason:      value.FailureReason,
			GenerationRelationship: integrityGenerationCurrent,
			CurrentGeneration:      1,
			PublishedGeneration:    1,
		}
	}
	recovery := &databaseRecoveryEvidence{
		Version:                     version,
		PreviewDigest:               strings.Repeat("6", 64),
		OldDatabase:                 oldDatabase,
		ReplacementDatabase:         replacementDatabase,
		Verification:                verification,
		SuccessorRevision:           value.SuccessorRevision,
		DatabaseRelocationConfirmed: true,
		FullBackupConfirmed:         true,
		IntentAt:                    value.UpdatedAt,
	}
	if published {
		publishedAt := value.UpdatedAt
		recovery.LocatorPublishedAt = &publishedAt
	}
	return recovery
}

func makeHistoricalRecoveryState(t *testing.T, phase string, version int) (*Manager, journal, string, string) {
	t.Helper()
	manager, value, bundle, revision, _ := seedEligibleSuccessor(t)
	value.SuccessorID = "upgrade-" + strings.Repeat("7", 32)
	value.SuccessorRevision = revision
	value.SchemaVersion = journalSchemaVersion
	value.Phase = phase
	value.UpdatedAt = manager.now()
	published := phase != "successor_recovery_intent"
	value.DatabaseRecovery = fixtureHistoricalRecovery(value, version, published)
	if published {
		value.Database = value.DatabaseRecovery.ReplacementDatabase
	}
	if phase == "superseded" {
		supersededAt := value.UpdatedAt
		value.SupersededAt = &supersededAt
	}
	writeHistoricalJournalFixture(t, manager, bundle, value)
	return manager, value, bundle, revision
}

func seedHistoricalTransferredSuccessor(t *testing.T) (*Manager, journal, string, Result, string) {
	t.Helper()
	manager, predecessor, predecessorBundle, revision, _ := seedEligibleSuccessor(t)
	prepared, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	predecessor, _, err = manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil {
		t.Fatal(err)
	}
	predecessor.DatabaseRecovery = fixtureHistoricalRecovery(predecessor, databaseRecoveryEvidenceVersion, true)
	writeHistoricalJournalFixture(t, manager, predecessorBundle, predecessor)
	return manager, predecessor, predecessorBundle, prepared, revision
}

func TestJournalSchemasOneThroughThreeRemainStrictlyReadable(t *testing.T) {
	for schema := 1; schema <= journalSchemaVersion; schema++ {
		t.Run(fmt.Sprintf("schema_%d", schema), func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			value, bundle := seedBundle(t, manager, true)
			value.SchemaVersion = schema
			if err := writeJournal(bundle, value, manager.uid); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := manager.loadBundleJournal(value.UpgradeID)
			if err != nil || loaded.SchemaVersion != schema {
				t.Fatalf("loaded_schema=%d err=%v", loaded.SchemaVersion, err)
			}
		})
	}
}

func TestHistoricalDatabaseRecoveryEvidenceVersionsRemainStrictlyReadable(t *testing.T) {
	for version := 1; version <= databaseRecoveryEvidenceVersion; version++ {
		t.Run(fmt.Sprintf("version_%d", version), func(t *testing.T) {
			manager, value, bundle, _ := makeHistoricalRecoveryState(t, "successor_prepare_intent", version)
			loaded, _, err := manager.loadBundleJournal(value.UpgradeID)
			if err != nil || loaded.DatabaseRecovery == nil || loaded.DatabaseRecovery.Version != version {
				t.Fatalf("loaded=%+v err=%v", loaded.DatabaseRecovery, err)
			}
			value.DatabaseRecovery.Version = databaseRecoveryEvidenceVersion + 1
			writeHistoricalJournalFixtureRaw(t, manager, bundle, value)
			if _, _, err := manager.loadBundleJournal(value.UpgradeID); err == nil {
				t.Fatal("unsupported historical recovery evidence version was accepted")
			}
		})
	}
}

func writeHistoricalJournalFixtureRaw(t *testing.T, manager *Manager, bundle string, value journal) {
	t.Helper()
	if err := writePrivateJSON(filepath.Join(bundle, "journal.json"), value, manager.uid); err != nil {
		t.Fatal(err)
	}
}

func TestWriteJournalRejectsRetiredRecoveryBeforeFilesystemMutation(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	value, bundle := seedBundle(t, manager, true)
	before := snapshotHistoricalTree(t, manager.controllerRoot())
	value.Phase = "successor_recovery_intent"
	if err := writeJournal(bundle, value, manager.uid); err == nil {
		t.Fatal("retired recovery phase reached the journal writer")
	}
	if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected recovery phase changed bytes or metadata")
	}
	value.Phase = "prepared"
	value.DatabaseRecovery = &databaseRecoveryEvidence{}
	if err := writeJournal(bundle, value, manager.uid); err == nil {
		t.Fatal("retired recovery evidence reached the journal writer")
	}
	if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected recovery evidence changed bytes or metadata")
	}
}

func TestUnresolvedHistoricalRecoveryStatesFailStopWithoutMutation(t *testing.T) {
	for _, phase := range []string{"successor_recovery_intent", "successor_prepare_intent", "superseded"} {
		for version := 1; version <= databaseRecoveryEvidenceVersion; version++ {
			t.Run(fmt.Sprintf("%s_v%d", phase, version), func(t *testing.T) {
				manager, value, _, revision := makeHistoricalRecoveryState(t, phase, version)
				before := snapshotHistoricalTree(t, manager.controllerRoot())
				status, err := manager.Status(context.Background(), value.UpgradeID)
				if err != nil || status.State != "attention" || status.Reason != historicalRecoveryReason || status.NextAction != historicalRecoveryNextAction || status.UpgradeHealth != "failed" || status.ControllerReadiness != "conflict" {
					t.Fatalf("status=%+v err=%v", status, err)
				}
				commands := []func() error{
					func() error {
						_, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: value.UpgradeID, Revision: revision})
						return err
					},
					func() error { _, err := manager.Replace(context.Background(), value.UpgradeID, true); return err },
					func() error { _, err := manager.Rollback(context.Background(), value.UpgradeID); return err },
					func() error { _, err := manager.AuthorizeBootstrap(context.Background(), value.UpgradeID); return err },
					func() error { _, err := manager.Observe(context.Background(), value.UpgradeID); return err },
					func() error { _, err := manager.Cleanup(context.Background(), value.UpgradeID); return err },
				}
				for index, command := range commands {
					if err := command(); err == nil {
						t.Fatalf("command %d accepted unresolved historical recovery", index)
					}
					if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
						t.Fatalf("command %d changed historical recovery evidence", index)
					}
				}
			})
		}
	}
}

func TestPrepareRejectsRetainedHistoricalRecoveryWithoutActivePointer(t *testing.T) {
	manager, value, _, revision := makeHistoricalRecoveryState(t, "successor_recovery_intent", databaseRecoveryEvidenceVersion)
	if err := os.Remove(manager.activePath()); err != nil {
		t.Fatal(err)
	}
	before := snapshotHistoricalTree(t, manager.controllerRoot())
	if _, err := manager.Prepare(context.Background(), PrepareRequest{Revision: revision, Supervisor: value.Supervisor, BinaryPath: value.BinaryPath, ConfigPath: value.ConfigPath}); err == nil {
		t.Fatal("ordinary prepare bypassed retained unresolved recovery")
	}
	if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected prepare changed retained historical recovery")
	}
}

func TestHistoricalRecoveryActivePointerPositionsRemainFailStop(t *testing.T) {
	for _, position := range []string{"predecessor", "missing_successor", "unrelated", "absent"} {
		t.Run(position, func(t *testing.T) {
			manager, value, _, _ := makeHistoricalRecoveryState(t, "superseded", databaseRecoveryEvidenceVersion)
			switch position {
			case "predecessor":
			case "missing_successor":
				if err := mwriteActiveFixture(manager, value.SuccessorID); err != nil {
					t.Fatal(err)
				}
			case "unrelated":
				if err := mwriteActiveFixture(manager, "upgrade-"+strings.Repeat("9", 32)); err != nil {
					t.Fatal(err)
				}
			case "absent":
				if err := os.Remove(manager.activePath()); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotHistoricalTree(t, manager.controllerRoot())
			status, err := manager.Status(context.Background(), value.UpgradeID)
			if err != nil || status.State != "attention" || status.Reason != historicalRecoveryReason || status.NextAction != historicalRecoveryNextAction {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
				t.Fatal("historical active-pointer classification changed evidence")
			}
		})
	}
}

func mwriteActiveFixture(manager *Manager, id string) error {
	return writePrivateJSON(manager.activePath(), activeUpgrade{UpgradeID: id}, manager.uid)
}

func seedHistoricalRecoveryClone(t *testing.T, manager *Manager, source journal, id, successorID string) {
	t.Helper()
	clone := source
	clone.UpgradeID = id
	clone.SuccessorID = successorID
	bundle := manager.bundlePath(id)
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHistoricalJournalFixture(t, manager, bundle, clone)
}

func TestHistoricalRecoveryRecordsAndGlobalBlockerAreDeterministic(t *testing.T) {
	manager, source, _, _ := makeHistoricalRecoveryState(t, "successor_recovery_intent", databaseRecoveryEvidenceVersion)
	lowID := "upgrade-" + strings.Repeat("0", 32)
	highID := "upgrade-" + strings.Repeat("f", 32)
	seedHistoricalRecoveryClone(t, manager, source, highID, "upgrade-"+strings.Repeat("e", 32))
	seedHistoricalRecoveryClone(t, manager, source, lowID, "upgrade-"+strings.Repeat("1", 32))

	var blocker string
	for attempt := 0; attempt < 25; attempt++ {
		records, _, err := manager.historicalRecoveryRecords()
		if err != nil || len(records) != 3 || records[0].predecessor.UpgradeID != lowID || records[len(records)-1].predecessor.UpgradeID != highID {
			t.Fatalf("records=%+v err=%v", records, err)
		}
		status, ok := manager.historicalRecoveryStatus("upgrade-" + strings.Repeat("d", 32))
		if !ok || status.UpgradeID != lowID || status.State != "attention" || status.Reason != historicalRecoveryReason {
			t.Fatalf("status=%+v ok=%t", status, ok)
		}
		err = manager.admitHistoricalRecoveryMutation("", historicalRecoveryNewUpgrade)
		if err == nil {
			t.Fatal("multiple unresolved recovery records did not block a new upgrade")
		}
		if attempt == 0 {
			blocker = err.Error()
		} else if err.Error() != blocker {
			t.Fatalf("blocker changed from %q to %q", blocker, err)
		}
	}
}

func TestHistoricalRecoveryRootScanRejectsUpgradeIDNonDirectories(t *testing.T) {
	for _, kind := range []string{"regular_file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			manager := newFixtureManager(t, fixtureRunner{})
			path := manager.bundlePath("upgrade-" + strings.Repeat("c", 32))
			switch kind {
			case "regular_file":
				writeFixtureFile(t, path, "not a private upgrade directory\n", 0o600)
			case "symlink":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotHistoricalTree(t, manager.controllerRoot())
			if _, _, err := manager.historicalRecoveryRecords(); err == nil {
				t.Fatalf("%s named as an upgrade ID was ignored", kind)
			}
			if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected %s changed bytes or metadata", kind)
			}
		})
	}
}

func TestHistoricalRecoveryRootScanIgnoresKnownControlAndStagingNames(t *testing.T) {
	manager := newFixtureManager(t, fixtureRunner{})
	for _, name := range []string{".prepare-fixture", ".upgrade-" + strings.Repeat("a", 32) + ".prepare"} {
		if err := os.Mkdir(filepath.Join(manager.upgradeRoot(), name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if records, _, err := manager.historicalRecoveryRecords(); err != nil || len(records) != 0 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func advanceHistoricalSuccessorToHealthy(t *testing.T, manager *Manager, successorID string) journal {
	t.Helper()
	manager.runner = fixtureRunner{}
	if _, err := manager.Replace(context.Background(), successorID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AuthorizeBootstrap(context.Background(), successorID); err != nil {
		t.Fatal(err)
	}
	successor, _, err := manager.loadJournal(successorID)
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
	heartbeat := heartbeatEvidence{SchemaVersion: 3, WorkerInstanceID: "historical-successor-worker", ProcessID: pid, ProcessStartID: started, BuildIdentity: successor.Candidate.Build.BuildIdentity, ConfigurationDigest: successor.ConfigDigest, Status: "parked", ObservedAt: manager.now()}
	if err := writePrivateJSON(successor.ConfigPath+".worker-status.json", heartbeat, manager.uid); err != nil {
		t.Fatal(err)
	}
	observed, err := manager.Observe(context.Background(), successor.UpgradeID)
	if err != nil || observed.State != "healthy" {
		t.Fatalf("observed=%+v err=%v", observed, err)
	}
	successor, _, err = manager.loadJournal(successorID)
	if err != nil {
		t.Fatal(err)
	}
	return successor
}

func TestCompleteHistoricalTransferUsesOrdinarySuccessorLifecycleAndRetainsPredecessor(t *testing.T) {
	manager, predecessor, predecessorBundle, prepared, revision := seedHistoricalTransferredSuccessor(t)
	immutablePredecessor, err := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(context.Background(), predecessor.UpgradeID)
	if err != nil || status.State != "superseded" || status.NextAction != "status_successor" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	advanceHistoricalSuccessorToHealthy(t, manager, prepared.UpgradeID)
	cleaned, err := manager.Cleanup(context.Background(), prepared.UpgradeID)
	if err != nil || cleaned.State != "cleaned" {
		t.Fatalf("cleaned=%+v err=%v", cleaned, err)
	}
	after, err := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	if err != nil || string(after) != string(immutablePredecessor) {
		t.Fatalf("historical predecessor changed err=%v", err)
	}
	status, err = manager.Status(context.Background(), predecessor.UpgradeID)
	if err != nil || status.State != "superseded" || status.NextAction != "none" {
		t.Fatalf("completed status=%+v err=%v", status, err)
	}
	before := snapshotHistoricalTree(t, manager.controllerRoot())
	_, err = manager.Prepare(context.Background(), PrepareRequest{Revision: revision, Supervisor: predecessor.Supervisor, BinaryPath: predecessor.BinaryPath, ConfigPath: predecessor.ConfigPath})
	if err == nil || !strings.Contains(err.Error(), "historical database relocation recovery") {
		t.Fatalf("later managed upgrade was not blocked by retained recovery history: %v", err)
	}
	if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected later prepare changed completed historical evidence")
	}
}

func TestUnrelatedCurrentInstallationCannotProveHistoricalRecoveryComplete(t *testing.T) {
	manager, predecessor, _, prepared, _ := seedHistoricalTransferredSuccessor(t)
	advanceHistoricalSuccessorToHealthy(t, manager, prepared.UpgradeID)
	if _, err := manager.Cleanup(context.Background(), prepared.UpgradeID); err != nil {
		t.Fatal(err)
	}
	later := currentInstallation{
		SchemaVersion:  1,
		UpgradeID:      "upgrade-" + strings.Repeat("9", 32),
		Supervisor:     predecessor.Supervisor,
		BinaryDigest:   strings.Repeat("9", 64),
		BuildIdentity:  "sha256:" + strings.Repeat("9", 64),
		VCSRevision:    strings.Repeat("9", 40),
		DatabaseSchema: predecessor.Database.SchemaVersion,
		VerifiedAt:     predecessor.SupersededAt.Add(time.Second),
	}
	if err := writePrivateJSON(filepath.Join(manager.controllerRoot(), "current-installation.json"), later, manager.uid); err != nil {
		t.Fatal(err)
	}
	before := snapshotHistoricalTree(t, manager.controllerRoot())
	if status, err := manager.Status(context.Background(), predecessor.UpgradeID); err != nil || status.State != "attention" || status.Reason != historicalRecoveryReason || status.NextAction != historicalRecoveryNextAction {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	_, prepareErr := manager.Prepare(context.Background(), PrepareRequest{Revision: strings.Repeat("8", 40), Supervisor: predecessor.Supervisor, BinaryPath: predecessor.BinaryPath, ConfigPath: predecessor.ConfigPath})
	if prepareErr == nil || !strings.Contains(prepareErr.Error(), "historical database relocation") {
		t.Fatalf("unrelated current installation proved historical recovery complete: %v", prepareErr)
	}
	if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
		t.Fatal("false terminal proof checks changed bytes or metadata")
	}
}

func TestCleanupCommittedHistoricalSuccessorBlocksPrepareThenResumesCleanup(t *testing.T) {
	manager, predecessor, _, prepared, revision := seedHistoricalTransferredSuccessor(t)
	advanceHistoricalSuccessorToHealthy(t, manager, prepared.UpgradeID)
	manager.fail = func(point string) error {
		if point == "after_cleanup_active_sync" {
			return errors.New("injected post-commit interruption")
		}
		return nil
	}
	if _, err := manager.Cleanup(context.Background(), prepared.UpgradeID); err == nil || exists(manager.activePath()) || !exists(manager.bundlePath(prepared.UpgradeID)) {
		t.Fatalf("cleanup interruption err=%v active=%t bundle=%t", err, exists(manager.activePath()), exists(manager.bundlePath(prepared.UpgradeID)))
	}
	manager.fail = nil
	before := snapshotHistoricalTree(t, manager.controllerRoot())
	_, err := manager.Prepare(context.Background(), PrepareRequest{Revision: revision, Supervisor: predecessor.Supervisor, BinaryPath: predecessor.BinaryPath, ConfigPath: predecessor.ConfigPath})
	if err == nil || !strings.Contains(err.Error(), "historical database relocation recovery") {
		t.Fatalf("prepare bypassed cleanup-committed historical recovery: %v", err)
	}
	if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected prepare changed cleanup-committed evidence")
	}
	resumed, err := manager.Cleanup(context.Background(), prepared.UpgradeID)
	if err != nil || resumed.State != "cleaned" || exists(manager.bundlePath(prepared.UpgradeID)) {
		t.Fatalf("resumed=%+v err=%v bundle=%t", resumed, err, exists(manager.bundlePath(prepared.UpgradeID)))
	}
}

func TestCompleteHistoricalTransferAllowsOrdinaryPreBootstrapRollback(t *testing.T) {
	manager, _, predecessorBundle, prepared, _ := seedHistoricalTransferredSuccessor(t)
	immutablePredecessor, err := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = fixtureRunner{}
	if _, err := manager.Replace(context.Background(), prepared.UpgradeID, true); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := manager.Rollback(context.Background(), prepared.UpgradeID)
	if err != nil || rolledBack.State != "rolled_back" {
		t.Fatalf("rolled_back=%+v err=%v", rolledBack, err)
	}
	after, err := os.ReadFile(filepath.Join(predecessorBundle, "journal.json"))
	if err != nil || string(after) != string(immutablePredecessor) {
		t.Fatalf("historical predecessor changed err=%v", err)
	}
}

func TestHistoricalTransferRejectsIncompleteSuccessorBundleWithoutMutation(t *testing.T) {
	for _, mutation := range []string{"corrupt_manifest", "missing_back_reference", "multiple_claim"} {
		t.Run(mutation, func(t *testing.T) {
			manager, predecessor, _, prepared, _ := seedHistoricalTransferredSuccessor(t)
			switch mutation {
			case "corrupt_manifest":
				if err := os.WriteFile(filepath.Join(manager.bundlePath(prepared.UpgradeID), "candidate-manifest.json"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing_back_reference":
				successor, bundle, err := manager.loadBundleJournal(prepared.UpgradeID)
				if err != nil {
					t.Fatal(err)
				}
				successor.PredecessorID = ""
				if err := writeJournal(bundle, successor, manager.uid); err != nil {
					t.Fatal(err)
				}
			case "multiple_claim":
				claim := predecessor
				claim.UpgradeID = "upgrade-" + strings.Repeat("9", 32)
				claim.Revision = strings.Repeat("9", 40)
				claim.Candidate.Build.VCSRevision = claim.Revision
				claim.Candidate.Build.BuildIdentity = "sha256:" + strings.Repeat("9", 64)
				claim.Candidate.Digest = strings.Repeat("9", 64)
				claim.DatabaseRecovery = fixtureHistoricalRecovery(claim, databaseRecoveryEvidenceVersion, true)
				bundle := manager.bundlePath(claim.UpgradeID)
				if err := os.Mkdir(bundle, 0o700); err != nil {
					t.Fatal(err)
				}
				writeHistoricalJournalFixture(t, manager, bundle, claim)
			}
			before := snapshotHistoricalTree(t, manager.controllerRoot())
			status, err := manager.Status(context.Background(), predecessor.UpgradeID)
			if err != nil || status.State != "attention" || status.Reason != historicalRecoveryReason {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			if _, err := manager.Replace(context.Background(), prepared.UpgradeID, true); err == nil {
				t.Fatal("incomplete historical transfer authorized successor mutation")
			}
			if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected historical transfer changed bytes or metadata")
			}
		})
	}
}

func TestRecoveryOriginSuccessorRequiresExactBidirectionalLineage(t *testing.T) {
	manager, predecessor, predecessorBundle, prepared, revision := seedHistoricalTransferredSuccessor(t)
	predecessor.SuccessorID = "upgrade-" + strings.Repeat("8", 32)
	predecessor.DatabaseRecovery.SuccessorRevision = predecessor.SuccessorRevision
	writeHistoricalJournalFixture(t, manager, predecessorBundle, predecessor)
	before := snapshotHistoricalTree(t, manager.controllerRoot())
	commands := []func() error{
		func() error { _, err := manager.Status(context.Background(), prepared.UpgradeID); return err },
		func() error { _, err := manager.Replace(context.Background(), prepared.UpgradeID, true); return err },
		func() error {
			_, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: prepared.UpgradeID, Revision: revision})
			return err
		},
	}
	for index, command := range commands {
		err := command()
		if index == 0 {
			if err != nil {
				t.Fatalf("status returned an unsanitized error: %v", err)
			}
		} else if err == nil {
			t.Fatalf("command %d accepted conflicting historical lineage", index)
		}
		if after := snapshotHistoricalTree(t, manager.controllerRoot()); !reflect.DeepEqual(before, after) {
			t.Fatalf("command %d changed conflicting historical lineage", index)
		}
	}
}
