package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func integrityRecheckService(t *testing.T, store *Store) (*application.IntegrityRecheckService, application.Requester) {
	t.Helper()
	operator := domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := application.NewIntegrityMaintenanceService(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewIntegrityRecheckService(store, authorizer, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	return service, application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
}

func TestIntegrityRecheckPublishesPostReceiptObservationAndReplaysExactly(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	completeIntegrityActivityBackfill(t, store.Store, now)
	var before int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	service, requester := integrityRecheckService(t, store.Store)
	result, err := service.Recheck(ctx, application.IntegrityRecheckCommand{Requester: requester, RequestID: "operator-request-1", Owner: "integrity-cli:test"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != application.IntegrityRecheckSucceeded || result.Receipt.OperationType != application.OperationRecheckIntegrity || result.Receipt.Phase != application.OperationPhaseObserved || result.Receipt.Outcome != application.OperationOutcomeSucceeded || result.Observation == nil || result.Observation.TargetGeneration != result.TargetGeneration || result.TargetGeneration <= before {
		t.Fatalf("result=%+v", result)
	}
	var receipts, links, activities, pointers, guards int
	if err := store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM operation_receipts WHERE operation_id=?),(SELECT COUNT(*) FROM activity_operation_links WHERE operation_id=?),(SELECT COUNT(*) FROM activity_events WHERE source_kind='operation_receipt' AND source_identity=?),(SELECT COUNT(*) FROM controller_integrity_active_recheck),(SELECT COUNT(*) FROM controller_integrity_finalization_guard)`, result.Receipt.OperationID, result.Receipt.OperationID, result.Receipt.OperationID).Scan(&receipts, &links, &activities, &pointers, &guards); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || links != 1 || activities != 1 || pointers != 0 || guards != 0 {
		t.Fatalf("receipts=%d links=%d activities=%d pointers=%d guards=%d", receipts, links, activities, pointers, guards)
	}
	var generation int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generation); err != nil || generation != result.TargetGeneration {
		t.Fatalf("generation=%d target=%d err=%v", generation, result.TargetGeneration, err)
	}
	replay, err := service.Recheck(ctx, application.IntegrityRecheckCommand{Requester: requester, RequestID: "operator-request-1", Owner: "integrity-cli:replay"})
	if err != nil || replay.State != application.IntegrityRecheckSucceeded || replay.Receipt.OperationID != result.Receipt.OperationID || replay.ScanID != result.ScanID || replay.Observation == nil || replay.Observation.Digest != result.Observation.Digest {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	var replayGeneration int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&replayGeneration); err != nil || replayGeneration != generation {
		t.Fatalf("replay generation=%d before=%d err=%v", replayGeneration, generation, err)
	}
}

func TestIntegrityRecheckOneActiveIdentityAndCompetingLease(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	completeIntegrityActivityBackfill(t, store.Store, now)
	operator := domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"}
	accepted, err := store.AcceptIntegrityRecheck(context.Background(), application.IntegrityRecheckAcceptance{Requester: operator, RequestID: "active-request", AcceptedAt: now.Add(time.Minute)})
	if err != nil || accepted.State != application.IntegrityRecheckPending {
		t.Fatalf("accepted=%+v err=%v", accepted, err)
	}
	if _, err := store.AcceptIntegrityRecheck(context.Background(), application.IntegrityRecheckAcceptance{Requester: operator, RequestID: "different-request", AcceptedAt: now.Add(2 * time.Minute)}); !errors.Is(err, application.ErrIntegrityRecheckActive) {
		t.Fatalf("active error=%v", err)
	}
	different := operator
	different.DatabaseID++
	if _, err := store.AcceptIntegrityRecheck(context.Background(), application.IntegrityRecheckAcceptance{Requester: different, RequestID: "active-request", AcceptedAt: now.Add(2 * time.Minute)}); !errors.Is(err, application.ErrIntegrityRecheckConflict) {
		t.Fatalf("identity drift error=%v", err)
	}
	leaseExpiry := now.Add(time.Hour)
	if _, err := store.db.Exec(`UPDATE controller_integrity_scans SET lease_owner='competing-worker',lease_version=1,lease_expires_at=? WHERE scan_id=?`, formatTime(leaseExpiry), accepted.ScanID); err != nil {
		t.Fatal(err)
	}
	service, requester := integrityRecheckService(t, store.Store)
	pending, err := service.Recheck(context.Background(), application.IntegrityRecheckCommand{Requester: requester, RequestID: "active-request", Owner: "integrity-cli:blocked"})
	if err != nil || pending.State != application.IntegrityRecheckPending {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	var owner string
	if err := store.db.QueryRow(`SELECT lease_owner FROM controller_integrity_scans WHERE scan_id=?`, accepted.ScanID).Scan(&owner); err != nil || owner != "competing-worker" {
		t.Fatalf("owner=%q err=%v", owner, err)
	}
}

func TestIntegrityRecheckGenerationAdvanceSettlesConflictWithoutRebind(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	completeIntegrityActivityBackfill(t, store.Store, now)
	operator := domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"}
	accepted, err := store.AcceptIntegrityRecheck(context.Background(), application.IntegrityRecheckAcceptance{Requester: operator, RequestID: "superseded-request", AcceptedAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE heavy_capacity_authority SET effective_identity='integrity-recheck-race' WHERE namespace='local_heavy_work'`); err != nil {
		t.Fatal(err)
	}
	maintenance, err := store.RunIntegrityMaintenance(context.Background(), "automatic-worker", now.Add(2*time.Minute))
	if err != nil || !maintenance.Superseded || maintenance.ScanID != accepted.ScanID {
		t.Fatalf("maintenance=%+v err=%v", maintenance, err)
	}
	result, err := store.GetIntegrityRecheck(context.Background(), application.IntegrityRecheckRequestKey(operator, "superseded-request"), application.IntegrityRecheckRequestClaim("superseded-request"))
	if err != nil || result.State != application.IntegrityRecheckConflict || result.Receipt.Outcome != application.OperationOutcomeConflict || result.Observation != nil || result.ScanID != accepted.ScanID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var pointers, links int
	if err := store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM controller_integrity_active_recheck),(SELECT COUNT(*) FROM activity_operation_links WHERE operation_id=?)`, result.Receipt.OperationID).Scan(&pointers, &links); err != nil || pointers != 0 || links != 1 {
		t.Fatalf("pointers=%d links=%d err=%v", pointers, links, err)
	}
}

func TestIntegrityRecheckFinalizationFailureRollsBackAndRetryCompletes(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	completeIntegrityActivityBackfill(t, store.Store, now)
	operator := domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"}
	accepted, err := store.AcceptIntegrityRecheck(context.Background(), application.IntegrityRecheckAcceptance{Requester: operator, RequestID: "rollback-request", AcceptedAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 6; index++ {
		if _, err := store.RunIntegrityMaintenance(context.Background(), "worker", now.Add(time.Duration(index+2)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_integrity_observation_insert BEFORE INSERT ON controller_integrity_observations BEGIN SELECT RAISE(ABORT,'injected finalization failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunIntegrityMaintenance(context.Background(), "worker", now.Add(9*time.Minute)); err == nil {
		t.Fatal("injected finalization failure was not returned")
	}
	var phase, outcome, scanStatus string
	var links, observations, guards, pointers, cursor int
	if err := store.db.QueryRow(`SELECT r.phase,r.outcome,s.status,s.family_cursor,(SELECT COUNT(*) FROM activity_operation_links WHERE operation_id=r.operation_id),(SELECT COUNT(*) FROM controller_integrity_observations WHERE scan_id=s.scan_id),(SELECT COUNT(*) FROM controller_integrity_finalization_guard),(SELECT COUNT(*) FROM controller_integrity_active_recheck) FROM operation_receipts r JOIN controller_integrity_rechecks b ON b.operation_id=r.operation_id JOIN controller_integrity_scans s ON s.scan_id=b.scan_id WHERE b.request_key=?`, application.IntegrityRecheckRequestKey(operator, "rollback-request")).Scan(&phase, &outcome, &scanStatus, &cursor, &links, &observations, &guards, &pointers); err != nil {
		t.Fatal(err)
	}
	if phase != "applied" || outcome != "pending" || scanStatus != "active" || cursor != 6 || links != 0 || observations != 0 || guards != 0 || pointers != 1 {
		t.Fatalf("phase=%s outcome=%s scan=%s cursor=%d links=%d observations=%d guards=%d pointers=%d", phase, outcome, scanStatus, cursor, links, observations, guards, pointers)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_integrity_observation_insert`); err != nil {
		t.Fatal(err)
	}
	maintenance, err := store.RunIntegrityMaintenance(context.Background(), "worker", now.Add(10*time.Minute))
	if err != nil || !maintenance.Published {
		t.Fatalf("maintenance=%+v err=%v", maintenance, err)
	}
	result, err := store.GetIntegrityRecheck(context.Background(), application.IntegrityRecheckRequestKey(operator, "rollback-request"), application.IntegrityRecheckRequestClaim("rollback-request"))
	if err != nil || result.State != application.IntegrityRecheckSucceeded || result.Observation == nil || result.ScanID != accepted.ScanID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestIntegrityFinalizationGuardRejectsMismatchAndCannotSuppressUnrelatedMutation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	operator := domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"}
	accepted, err := store.AcceptIntegrityRecheck(context.Background(), application.IntegrityRecheckAcceptance{Requester: operator, RequestID: "guard-request", AcceptedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO controller_integrity_finalization_guard(singleton,operation_id,scan_id,target_generation,activity_event_id,activity_source_identity,activity_link_operation_id,entered_at) VALUES(1,?,?,?,?,?,?,?)`, accepted.Receipt.OperationID, accepted.ScanID, accepted.TargetGeneration+1, "mismatched-event", accepted.Receipt.OperationID, accepted.Receipt.OperationID, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE operation_receipts SET phase='observed',outcome='succeeded',settled_at=? WHERE operation_id=?`, formatTime(now.Add(time.Second)), accepted.Receipt.OperationID); err == nil {
		t.Fatal("mismatched guard suppressed receipt mutation")
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	guardResult, _, err := checkIntegrityFamilyTx(context.Background(), tx, application.IntegrityStorageSchema)
	_ = tx.Rollback()
	if err != nil || guardResult.State != application.IntegrityConflict || guardResult.ReasonCode != "finalization_guard_conflict" {
		t.Fatalf("guard result=%+v err=%v", guardResult, err)
	}
	if _, err := store.db.Exec(`DELETE FROM controller_integrity_finalization_guard`); err != nil {
		t.Fatal(err)
	}
	activityID := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "operation_receipt", SourceIdentity: accepted.Receipt.OperationID, EventKind: application.ActivityOperationSettled}).EventID
	if _, err := store.db.Exec(`INSERT INTO controller_integrity_finalization_guard(singleton,operation_id,scan_id,target_generation,activity_event_id,activity_source_identity,activity_link_operation_id,entered_at) VALUES(1,?,?,?,?,?,?,?)`, accepted.Receipt.OperationID, accepted.ScanID, accepted.TargetGeneration, activityID, accepted.Receipt.OperationID, accepted.Receipt.OperationID, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	unrelated := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: operator, RequestDigest: application.NoOperationInputDigest(), ExpectedAuthorityDigest: application.ConfigurationEvidenceDigest("unrelated-authority"), OperationAnchorDigest: application.ConfigurationEvidenceDigest("unrelated-anchor"), TargetBindingDigest: application.ConfigurationEvidenceDigest("unrelated-binding"), AcceptedAt: now.Add(time.Minute)})
	if _, created, err := store.BeginOperationReceipt(context.Background(), unrelated); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	var after int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&after); err != nil || after != before+1 {
		t.Fatalf("before=%d after=%d err=%v", before, after, err)
	}
}

func TestIntegrityV41MigrationPreservesV40EvidenceAndOlderBinaryFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 40)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	completeIntegrityActivityBackfill(t, legacy, now)
	publishIntegrityObservation(t, legacy, now.Add(time.Minute))
	var beforeObservation, beforeFinding int
	if err := legacy.db.QueryRow(`SELECT (SELECT COUNT(*) FROM controller_integrity_observations),(SELECT COUNT(*) FROM controller_integrity_observation_findings)`).Scan(&beforeObservation, &beforeFinding); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var observations, findings, rechecks, guards int
	if err := store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM controller_integrity_observations),(SELECT COUNT(*) FROM controller_integrity_observation_findings),(SELECT COUNT(*) FROM controller_integrity_rechecks),(SELECT COUNT(*) FROM controller_integrity_finalization_guard)`).Scan(&observations, &findings, &rechecks, &guards); err != nil {
		t.Fatal(err)
	}
	if observations != beforeObservation || findings != beforeFinding || rechecks != 0 || guards != 0 {
		t.Fatalf("observations=%d/%d findings=%d/%d rechecks=%d guards=%d", observations, beforeObservation, findings, beforeFinding, rechecks, guards)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if older, err := openWithSupportedSchema(path, 40); err == nil {
		older.Close()
		t.Fatal("schema-v40 binary accepted schema v41")
	}
}
