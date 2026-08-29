package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func integrityQueryService(t *testing.T, store *Store) (*application.IntegrityQueryService, application.Requester) {
	t.Helper()
	operator := domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewIntegrityQueryService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	return service, application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
}

func completeIntegrityActivityBackfill(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	for index := 0; index < 32; index++ {
		var remaining int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_backfill_progress WHERE status IN ('pending','running')`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			return
		}
		if _, err := store.BackfillActivityBatch(context.Background(), 25, now.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("activity backfill did not converge")
}

func publishIntegrityObservation(t *testing.T, store *Store, now time.Time) application.IntegrityMaintenanceResult {
	t.Helper()
	for index := 0; index < 16; index++ {
		result, err := store.RunIntegrityMaintenance(context.Background(), "integrity-test-worker", now.Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if result.Published {
			return result
		}
	}
	t.Fatal("integrity observation did not publish")
	return application.IntegrityMaintenanceResult{}
}

func TestIntegrityMigrationGenerationAndReadyPublication(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	service, requester := integrityQueryService(t, store.Store)
	summary, err := service.Summary(ctx, requester)
	if err != nil || summary.Readiness != application.IntegrityUnknown || summary.ReasonCode != "initial_scan_required" {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	completeIntegrityActivityBackfill(t, store.Store, now)
	var before int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	first, err := store.RunIntegrityMaintenance(ctx, "integrity-test-worker", now.Add(time.Minute))
	if err != nil || first.Family != application.IntegrityStorageSchema {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	var after int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&after); err != nil || after != before {
		t.Fatalf("audit-owned scan changed source generation before=%d after=%d err=%v", before, after, err)
	}
	publishIntegrityObservation(t, store.Store, now.Add(2*time.Minute))
	summary, err = service.Summary(ctx, requester)
	if err != nil || !summary.Current || summary.Readiness != application.IntegrityReady || len(summary.Observation.Results) != 7 || summary.Observation.AffectedScopeCount != 0 {
		findings, findingErr := service.Findings(ctx, application.IntegrityFindingQuery{Requester: requester, Family: application.IntegrityStorageSchema})
		var privateID string
		_ = store.db.QueryRow(`SELECT private_scope_id FROM controller_integrity_scan_findings WHERE family='storage_schema' LIMIT 1`).Scan(&privateID)
		t.Fatalf("summary=%+v err=%v storage_findings=%+v finding_err=%v private=%s", summary, err, findings, findingErr, privateID)
	}
	if _, err := store.db.Exec(`UPDATE heavy_capacity_authority SET effective_identity='integrity-generation-change' WHERE namespace='local_heavy_work'`); err != nil {
		t.Fatal(err)
	}
	summary, err = service.Summary(ctx, requester)
	if err != nil || summary.Current || summary.Readiness != application.IntegrityUnknown || summary.ReasonCode != "source_generation_advanced" {
		t.Fatalf("stale summary=%+v err=%v", summary, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER controller_integrity_observation_immutable_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE controller_integrity_observations SET observation_digest=? WHERE observation_id=?`, application.ConfigurationEvidenceDigest("corrupt-observation"), summary.Observation.ObservationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE controller_integrity_current SET observation_digest=? WHERE singleton=1`, application.ConfigurationEvidenceDigest("corrupt-observation")); err != nil {
		t.Fatal(err)
	}
	summary, err = service.Summary(ctx, requester)
	if err != nil || summary.Readiness != application.IntegrityConflict || summary.ReasonCode != "observation_conflict" {
		t.Fatalf("corrupt summary=%+v err=%v", summary, err)
	}
}

func TestIntegrityV40UpgradeIsSchemaOnlyAndOlderBinaryFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 39)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var scans, observations, families, sources int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM controller_integrity_scans`).Scan(&scans); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM controller_integrity_observations`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM integrity_registry_families`).Scan(&families); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM integrity_registry_sources`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if scans != 0 || observations != 0 || families != 7 || sources == 0 {
		t.Fatalf("scans=%d observations=%d families=%d sources=%d", scans, observations, families, sources)
	}
	rows, err := store.db.Query(`SELECT table_name FROM integrity_registry_sources ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		var triggers int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN (?,?,?)`, "integrity_track_"+table+"_insert", "integrity_track_"+table+"_update", "integrity_track_"+table+"_delete").Scan(&triggers); err != nil || triggers != 3 {
			t.Fatalf("table=%s triggers=%d err=%v", table, triggers, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if older, err := openWithSupportedSchema(path, 39); err == nil {
		older.Close()
		t.Fatal("schema-v39 binary accepted integrity schema")
	}
}

func TestIntegrityFindingIsSanitizedStableAndPaginated(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	completeIntegrityActivityBackfill(t, store.Store, now)
	for _, id := range []string{"integrity-invalid-a", "integrity-invalid-b"} {
		run := activityTestRun(id)
		if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil || !created {
			t.Fatalf("create %s created=%t err=%v", id, created, err)
		}
		if _, err := store.db.Exec(`UPDATE runs SET current_state='executing' WHERE run_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	publishIntegrityObservation(t, store.Store, now.Add(time.Minute))
	service, requester := integrityQueryService(t, store.Store)
	summary, err := service.Summary(ctx, requester)
	if err != nil || summary.Readiness != application.IntegrityNotReady {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	first, err := service.Findings(ctx, application.IntegrityFindingQuery{Requester: requester, Family: application.IntegrityRunDelivery, Limit: 1})
	if err != nil || len(first.Findings) != 1 || first.NextCursor == "" || first.Findings[0].Scope != application.ScopeRun || first.Findings[0].ReasonCode != "run_transition_continuity_violation" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := store.db.Exec(`UPDATE heavy_capacity_authority SET effective_identity='new-observation' WHERE namespace='local_heavy_work'`); err != nil {
		t.Fatal(err)
	}
	publishIntegrityObservation(t, store.Store, now.Add(3*time.Minute))
	second, err := service.Findings(ctx, application.IntegrityFindingQuery{Requester: requester, Family: application.IntegrityRunDelivery, Limit: 1, Cursor: first.NextCursor})
	if err != nil || len(second.Findings) != 1 || second.Findings[0].FindingID == first.Findings[0].FindingID || second.ObservationDigest != first.ObservationDigest {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	unauthorized := requester
	unauthorized.DatabaseID++
	if _, err := service.Findings(ctx, application.IntegrityFindingQuery{Requester: unauthorized, TargetID: "integrity-invalid-a"}); err == nil {
		t.Fatal("unauthorized finding query was accepted")
	}
}

func TestIntegrityScanRestartAndMutationFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC)
	completeIntegrityActivityBackfill(t, store.Store, now)
	first, err := store.RunIntegrityMaintenance(context.Background(), "worker-before-restart", now.Add(time.Minute))
	if err != nil || first.Family != application.IntegrityStorageSchema {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.db.Exec(`UPDATE heavy_capacity_authority SET effective_identity='racing-mutation' WHERE namespace='local_heavy_work'`); err != nil {
		t.Fatal(err)
	}
	result, err := reopened.RunIntegrityMaintenance(context.Background(), "worker-after-restart", now.Add(2*time.Minute))
	if err != nil || !result.Superseded || result.TargetGeneration <= first.TargetGeneration {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	publishIntegrityObservation(t, reopened.Store, now.Add(3*time.Minute))
	var duplicates int
	if err := reopened.db.QueryRow(`SELECT COUNT(*)-COUNT(DISTINCT finding_id) FROM controller_integrity_scan_findings`).Scan(&duplicates); err != nil || duplicates != 0 {
		t.Fatalf("duplicates=%d err=%v", duplicates, err)
	}
}

func TestIntegrityConvergenceBoundPublishesCurrentFullFamilyResult(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	completeIntegrityActivityBackfill(t, store.Store, now)
	run := activityTestRun("convergence-finding")
	if _, created, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if _, err := store.db.Exec(`UPDATE runs SET current_state='executing' WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunIntegrityMaintenance(context.Background(), "racing-worker", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var final application.IntegrityMaintenanceResult
	for index := 1; index <= 8; index++ {
		if _, err := store.db.Exec(`UPDATE heavy_capacity_authority SET effective_identity=? WHERE namespace='local_heavy_work'`, "race-"+string(rune('a'+index))); err != nil {
			t.Fatal(err)
		}
		final, err = store.RunIntegrityMaintenance(context.Background(), "racing-worker", now.Add(time.Duration(index+1)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	if !final.Published || !final.Superseded {
		t.Fatalf("final=%+v", final)
	}
	service, requester := integrityQueryService(t, store.Store)
	summary, err := service.Summary(context.Background(), requester)
	if err != nil || !summary.Current || summary.Readiness != application.IntegrityNotReady || !summary.Observation.CountComplete || !summary.Observation.CoverageComplete {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	for _, result := range summary.Observation.Results {
		if result.Family == application.IntegrityRunDelivery {
			if result.State != application.IntegrityNotReady || result.ReasonCode != "deterministic_findings" {
				t.Fatalf("run delivery result=%+v", result)
			}
			continue
		}
		if result.State != application.IntegrityReady || result.ReasonCode != "complete" {
			t.Fatalf("result=%+v", result)
		}
	}
}

func TestIntegrityConvergenceFallbackRejectsRacingSourceMutation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 27, 5, 0, 0, 0, time.UTC)
	completeIntegrityActivityBackfill(t, store.Store, now)
	if _, err := store.RunIntegrityMaintenance(context.Background(), "racing-worker", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for index := 1; index < 8; index++ {
		if _, err := store.db.Exec(`UPDATE heavy_capacity_authority SET effective_identity=? WHERE namespace='local_heavy_work'`, fmt.Sprintf("race-%d", index)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RunIntegrityMaintenance(context.Background(), "racing-worker", now.Add(time.Duration(index+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`CREATE TRIGGER integrity_test_fallback_race AFTER INSERT ON controller_integrity_checked_families BEGIN UPDATE controller_integrity_generation SET generation=generation+1 WHERE singleton=1; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE heavy_capacity_authority SET effective_identity='race-final' WHERE namespace='local_heavy_work'`); err != nil {
		t.Fatal(err)
	}
	result, err := store.RunIntegrityMaintenance(context.Background(), "racing-worker", now.Add(9*time.Minute))
	if err != nil || result.Published || !result.Superseded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var published int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM controller_integrity_scans WHERE target_generation=? AND status='published'`, result.TargetGeneration).Scan(&published); err != nil || published != 0 {
		t.Fatalf("published=%d err=%v", published, err)
	}
}
