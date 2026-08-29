package localupgrade

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func installLegacyConvergenceExhaustion(t *testing.T, predecessor journal, currentGeneration int64) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`DELETE FROM controller_integrity_current`,
		`DELETE FROM controller_integrity_observations`,
		`ALTER TABLE controller_integrity_observations ADD COLUMN scan_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE controller_integrity_observations ADD COLUMN reason_code TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE controller_integrity_observations ADD COLUMN affected_scope_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE controller_integrity_observations ADD COLUMN count_complete INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE controller_integrity_observations ADD COLUMN coverage_complete INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE controller_integrity_observations ADD COLUMN observed_at TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE integrity_registry_families(registry_version TEXT,family TEXT,family_order INTEGER,reason_version TEXT)`,
		`CREATE TABLE controller_integrity_scans(scan_id TEXT PRIMARY KEY,registry_version TEXT,target_generation INTEGER,stable_boundary TEXT,family_cursor INTEGER,status TEXT,convergence_attempt INTEGER,reason_code TEXT)`,
		`CREATE TABLE controller_integrity_checked_families(scan_id TEXT,family TEXT,state TEXT,reason_code TEXT,checked_revision INTEGER,affected_scope_count INTEGER,count_complete INTEGER,coverage_complete INTEGER,findings_digest TEXT)`,
		`CREATE TABLE controller_integrity_scan_findings(scan_id TEXT,finding_id TEXT)`,
		`CREATE TABLE controller_integrity_observation_families(observation_id TEXT,family TEXT,state TEXT,reason_code TEXT,checked_revision INTEGER,affected_scope_count INTEGER,count_complete INTEGER,coverage_complete INTEGER)`,
		`CREATE TABLE controller_integrity_observation_findings(observation_id TEXT,finding_id TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	publishedGeneration := int64(1)
	scanID := localUpgradeIntegrityDigest("scan", "v1", fmt.Sprint(publishedGeneration))
	observationID := localUpgradeIntegrityDigest("observation", scanID)
	observedAt := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	parts := []string{"observation", "v1", "v1", observationID, "1", "1", observedAt, "unknown", "family_unknown", "0", "false", "false"}
	for index, family := range legacyIntegrityFamilies {
		if _, err := db.Exec(`INSERT INTO integrity_registry_families VALUES('v1',?,?, 'v1')`, family, index); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO controller_integrity_observation_families VALUES(?,?, 'unknown','convergence_bound_exhausted',?,0,0,0)`, observationID, family, index+1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO controller_integrity_checked_families VALUES(?,?, 'unknown','convergence_bound_exhausted',?,0,0,0,?)`, scanID, family, index+1, localUpgradeIntegrityDigest("findings")); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, family, "unknown", "convergence_bound_exhausted", fmt.Sprint(index+1), "0", "false", "false")
	}
	digest := localUpgradeIntegrityDigest(parts...)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE controller_integrity_generation SET generation=? WHERE singleton=1`, []any{currentGeneration}},
		{`INSERT INTO controller_integrity_scans VALUES(?,'v1',1,?,7,'published',8,'family_unknown')`, []any{scanID, localUpgradeIntegrityDigest("boundary", "1")}},
		{`INSERT INTO controller_integrity_observations(observation_id,schema_version,registry_version,observation_digest,target_generation,published_generation,effective_readiness,scan_id,reason_code,affected_scope_count,count_complete,coverage_complete,observed_at) VALUES(?,'v1','v1',?,1,1,'unknown',?,'family_unknown',0,0,0,?)`, []any{observationID, digest, scanID, observedAt}},
		{`INSERT INTO controller_integrity_current VALUES(1,?,?,1)`, []any{observationID, digest}},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyIntegrityConvergenceExhaustionBecomesEligibleSuccessorAttention(t *testing.T) {
	manager, predecessor, predecessorBundle, revision, _ := seedEligibleSuccessor(t)
	installLegacyConvergenceExhaustion(t, predecessor, 2)
	buildCount := 0
	manager.buildCandidate = func(_ context.Context, revision string, databaseSchema int) (preparedCandidate, error) {
		buildCount++
		build := fixtureBuild(revision)
		build.SupportedControllerSchemaVersion = legacyIntegrityConvergenceSchemaVersion + 1
		build.BuildIdentity = "sha256:" + strings.Repeat("d", 64)
		raw := binaryScript(build)
		path := filepath.Join(manager.controllerRoot(), fmt.Sprintf("convergence-successor-%d", buildCount))
		writeFixtureFile(t, path, raw, 0o755)
		digest, _ := digestFile(path)
		evidence := binaryEvidence{Digest: digest, Size: int64(len(raw)), Mode: 0o755, GoVersion: "go1.26.1", ModulePath: "github.com/ifan0927/Agent-Loop-Controller/cmd/agentctl", GoVCSRevision: revision, GoVCSTime: build.VCSTime, Build: build, Structured: true}
		if !candidateCompatible(evidence, revision, databaseSchema) {
			t.Fatal("convergence successor fixture is incompatible")
		}
		return preparedCandidate{Evidence: evidence, Path: path, Cleanup: func() { _ = os.Remove(path) }}, nil
	}
	now := manager.now()
	predecessor.Phase = "bootstrap_intent"
	predecessor.FailureReason = ""
	predecessor.BootstrapIntentAt = &now
	predecessor.UpdatedAt = now
	if err := writeJournal(predecessorBundle, predecessor, manager.uid); err != nil {
		t.Fatal(err)
	}

	observed, err := manager.Observe(context.Background(), predecessor.UpgradeID)
	if err != nil || observed.State != "attention" || observed.Reason != "integrity_convergence_exhausted" || observed.UpgradeHealth != "healthy" || observed.ControllerReadiness != "not_ready" {
		t.Fatalf("observed=%+v err=%v", observed, err)
	}
	persisted, _, err := manager.loadBundleJournal(predecessor.UpgradeID)
	if err != nil || persisted.Phase != "attention" || persisted.FailureReason != "integrity_convergence_exhausted" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	successor, err := manager.PrepareSuccessor(context.Background(), SuccessorPrepareRequest{PredecessorUpgradeID: predecessor.UpgradeID, Revision: revision})
	if err != nil || successor.State != "prepared" || successor.PredecessorUpgradeID != predecessor.UpgradeID || buildCount != 1 {
		t.Fatalf("successor=%+v builds=%d err=%v", successor, buildCount, err)
	}
}

func TestMalformedLegacyConvergenceEvidenceRemainsIneligible(t *testing.T) {
	scenarios := []struct {
		name           string
		mutate         string
		expectedSchema int
	}{
		{name: "invalid current pointer", mutate: `UPDATE controller_integrity_current SET observation_digest='` + strings.Repeat("f", 64) + `' WHERE singleton=1`},
		{name: "short convergence", mutate: `UPDATE controller_integrity_scans SET convergence_attempt=7`},
		{name: "partial family set", mutate: `DELETE FROM controller_integrity_observation_families WHERE family='owned_resource_cleanup'`},
		{name: "scan family mismatch", mutate: `UPDATE controller_integrity_checked_families SET checked_revision=99 WHERE family='operation_activity'`},
		{name: "unrelated family reason", mutate: `UPDATE controller_integrity_observation_families SET reason_code='bounded_check_failed' WHERE family='storage_schema'`},
		{name: "complete aggregate", mutate: `UPDATE controller_integrity_observations SET count_complete=1`},
		{name: "published generation ahead", mutate: `UPDATE controller_integrity_generation SET generation=0 WHERE singleton=1`},
		{name: "already migrated schema", mutate: `UPDATE schema_migrations SET version=43 WHERE version=42`, expectedSchema: 43},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			manager, predecessor, _, _, _ := seedEligibleSuccessor(t)
			installLegacyConvergenceExhaustion(t, predecessor, 2)
			db, err := sql.Open("sqlite", sqliteURI(predecessor.DatabasePath, false))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(scenario.mutate); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			expectedSchema := scenario.expectedSchema
			if expectedSchema == 0 {
				expectedSchema = predecessor.Database.SchemaVersion
			}
			readiness, reason := configurationAndIntegrityReadiness(context.Background(), predecessor.DatabasePath, predecessor.ConfigDigest, expectedSchema)
			if readiness == "not_ready" && reason == "integrity_convergence_exhausted" {
				t.Fatalf("malformed evidence became eligible readiness=%s reason=%s", readiness, reason)
			}
			if eligibleSuccessorReason(reason) {
				t.Fatalf("malformed evidence produced eligible reason %s", reason)
			}
			_ = manager
		})
	}
}
