package localupgrade

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func installUnstableIntegrityPublicationFixture(t *testing.T, path, configDigest string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations VALUES(43)`,
		`CREATE TABLE configuration_generations(generation_id INTEGER PRIMARY KEY,digest TEXT,lifecycle TEXT)`,
		`CREATE TABLE configuration_authority(authority_id INTEGER PRIMARY KEY,desired_generation_id INTEGER,effective_generation_id INTEGER)`,
		`INSERT INTO configuration_generations VALUES(1,'` + configDigest + `','effective')`,
		`INSERT INTO configuration_authority VALUES(1,1,1)`,
		`CREATE TABLE controller_integrity_generation(singleton INTEGER PRIMARY KEY,generation INTEGER)`,
		`INSERT INTO controller_integrity_generation VALUES(1,202)`,
		`CREATE TABLE integrity_registry_families(registry_version TEXT,family TEXT,family_order INTEGER,reason_version TEXT)`,
		`CREATE TABLE controller_integrity_scans(scan_id TEXT PRIMARY KEY,registry_version TEXT,target_generation INTEGER,stable_boundary TEXT,family_cursor INTEGER,status TEXT,convergence_attempt INTEGER,reason_code TEXT)`,
		`CREATE TABLE controller_integrity_checked_families(scan_id TEXT,family TEXT,state TEXT,reason_code TEXT,checked_revision INTEGER,affected_scope_count INTEGER,count_complete INTEGER,coverage_complete INTEGER,findings_digest TEXT)`,
		`CREATE TABLE controller_integrity_scan_findings(scan_id TEXT,finding_id TEXT)`,
		`CREATE TABLE controller_integrity_observations(observation_id TEXT PRIMARY KEY,schema_version TEXT,registry_version TEXT,observation_digest TEXT,target_generation INTEGER,published_generation INTEGER,scan_id TEXT,effective_readiness TEXT,reason_code TEXT,affected_scope_count INTEGER,count_complete INTEGER,coverage_complete INTEGER,observed_at TEXT)`,
		`CREATE TABLE controller_integrity_observation_families(observation_id TEXT,family TEXT,state TEXT,reason_code TEXT,checked_revision INTEGER,affected_scope_count INTEGER,count_complete INTEGER,coverage_complete INTEGER)`,
		`CREATE TABLE controller_integrity_observation_findings(observation_id TEXT,finding_id TEXT)`,
		`CREATE TABLE controller_integrity_current(singleton INTEGER PRIMARY KEY,observation_id TEXT,observation_digest TEXT,published_generation INTEGER)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for index, family := range legacyIntegrityFamilies {
		if _, err := db.Exec(`INSERT INTO integrity_registry_families VALUES('v1',?,?,'v1')`, family, index); err != nil {
			t.Fatal(err)
		}
	}
	insertReady := func(generation int64, observedAt time.Time) (string, string) {
		scanID := localUpgradeIntegrityDigest("scan", "v1", fmt.Sprint(generation))
		observationID := localUpgradeIntegrityDigest("observation", scanID)
		formatted := observedAt.UTC().Format(time.RFC3339Nano)
		parts := []string{"observation", "v1", "v1", observationID, fmt.Sprint(generation), fmt.Sprint(generation), formatted, "ready", "complete", "0", "true", "true"}
		for index, family := range legacyIntegrityFamilies {
			revision := generation + int64(index)
			parts = append(parts, family, "ready", "complete", fmt.Sprint(revision), "0", "true", "true")
			if _, err := db.Exec(`INSERT INTO controller_integrity_observation_families VALUES(?,?,'ready','complete',?,0,1,1)`, observationID, family, revision); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO controller_integrity_checked_families VALUES(?,?,'ready','complete',?,0,1,1,?)`, scanID, family, revision, localUpgradeIntegrityDigest("findings")); err != nil {
				t.Fatal(err)
			}
		}
		digest := localUpgradeIntegrityDigest(parts...)
		if _, err := db.Exec(`INSERT INTO controller_integrity_scans VALUES(?,'v1',?,?,7,'published',8,'complete')`, scanID, generation, localUpgradeIntegrityDigest("boundary", fmt.Sprint(generation))); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO controller_integrity_observations VALUES(?,'v1','v1',?,?,?,?, 'ready','complete',0,1,1,?)`, observationID, digest, generation, generation, scanID, formatted); err != nil {
			t.Fatal(err)
		}
		return observationID, digest
	}
	insertReady(100, time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC))
	latestID, latestDigest := insertReady(200, time.Date(2026, 8, 29, 8, 4, 0, 0, time.UTC))
	for _, scan := range []struct {
		generation int64
		status     string
		reason     string
	}{
		{generation: 150, status: "superseded", reason: "source_generation_advanced"},
		{generation: 201, status: "active", reason: ""},
	} {
		scanID := localUpgradeIntegrityDigest("scan", "v1", fmt.Sprint(scan.generation))
		if _, err := db.Exec(`INSERT INTO controller_integrity_scans VALUES(?,'v1',?,?,1,?,5,?)`, scanID, scan.generation, localUpgradeIntegrityDigest("boundary", fmt.Sprint(scan.generation)), scan.status, scan.reason); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO controller_integrity_current VALUES(1,?,?,200)`, latestID, latestDigest); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledV43UnstableIntegrityPublicationBecomesSuccessorEligible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	digest := strings.Repeat("a", 64)
	installUnstableIntegrityPublicationFixture(t, path, digest)
	readiness, reason := configurationAndIntegrityReadiness(context.Background(), path, digest, installedIntegrityPublicationSchemaVersion)
	if readiness != "not_ready" || reason != "integrity_publication_not_stable" || !eligibleSuccessorReason(reason) {
		t.Fatalf("readiness=%s reason=%s eligible=%t", readiness, reason, eligibleSuccessorReason(reason))
	}
}

func TestMalformedOrGenericV43IntegrityPendingRemainsIneligible(t *testing.T) {
	scenarios := []struct {
		name   string
		mutate string
	}{
		{name: "only one publication", mutate: `DELETE FROM controller_integrity_observations WHERE published_generation=100`},
		{name: "current pointer digest mismatch", mutate: `UPDATE controller_integrity_current SET observation_digest='` + strings.Repeat("f", 64) + `'`},
		{name: "latest attempt below bound", mutate: `UPDATE controller_integrity_scans SET convergence_attempt=7 WHERE target_generation=200`},
		{name: "partial family set", mutate: `DELETE FROM controller_integrity_observation_families WHERE observation_id=(SELECT observation_id FROM controller_integrity_observations WHERE published_generation=200) AND family='owned_resource_cleanup'`},
		{name: "not ready family", mutate: `UPDATE controller_integrity_observation_families SET state='not_ready' WHERE observation_id=(SELECT observation_id FROM controller_integrity_observations WHERE published_generation=200) AND family='run_delivery'`},
		{name: "checked family mismatch", mutate: `UPDATE controller_integrity_checked_families SET checked_revision=999 WHERE scan_id=(SELECT scan_id FROM controller_integrity_observations WHERE published_generation=200) AND family='operation_activity'`},
		{name: "observation digest malformed", mutate: `UPDATE controller_integrity_observations SET observation_digest='` + strings.Repeat("e", 64) + `' WHERE published_generation=200`},
		{name: "missing prior supersession", mutate: `DELETE FROM controller_integrity_scans WHERE target_generation=150`},
		{name: "prior supersession identity mismatch", mutate: `UPDATE controller_integrity_scans SET scan_id='` + strings.Repeat("d", 64) + `' WHERE target_generation=150`},
		{name: "missing post publication scan", mutate: `DELETE FROM controller_integrity_scans WHERE target_generation=201`},
		{name: "post publication scan identity mismatch", mutate: `UPDATE controller_integrity_scans SET stable_boundary='` + strings.Repeat("c", 64) + `' WHERE target_generation=201`},
		{name: "post scan has no newer mutation", mutate: `UPDATE controller_integrity_generation SET generation=201`},
		{name: "superseded post scan target equals current", mutate: `UPDATE controller_integrity_scans SET status='superseded',reason_code='source_generation_advanced' WHERE target_generation=201; UPDATE controller_integrity_generation SET generation=201`},
		{name: "publication chronology conflict", mutate: `UPDATE controller_integrity_observations SET observed_at='2026-08-29T07:59:00Z' WHERE published_generation=200`},
		{name: "generic stale ready", mutate: `UPDATE controller_integrity_scans SET convergence_attempt=7 WHERE target_generation IN (100,200)`},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.db")
			digest := strings.Repeat("a", 64)
			installUnstableIntegrityPublicationFixture(t, path, digest)
			db, err := sql.Open("sqlite", sqliteURI(path, false))
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
			readiness, reason := configurationAndIntegrityReadiness(context.Background(), path, digest, installedIntegrityPublicationSchemaVersion)
			if readiness == "not_ready" || eligibleSuccessorReason(reason) {
				t.Fatalf("malformed evidence became eligible: readiness=%s reason=%s", readiness, reason)
			}
		})
	}
}
