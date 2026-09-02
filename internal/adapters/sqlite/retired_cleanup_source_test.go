package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestMigratesSchemaV44ToV45WithRetiredCleanupSourceEvidenceSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 44)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 1, 0, 0, 0, time.UTC)
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 1, NodeID: "U_1", ActorType: "User"}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecheckRepository, Scope: application.ScopeRepository, TargetID: "owner/repository", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: now})
	if _, _, err := legacy.BeginOperationReceipt(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.SchemaVersion(context.Background()); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var operationType string
	if err := store.db.QueryRow(`SELECT operation_type FROM operation_receipts WHERE operation_id=?`, receipt.OperationID).Scan(&operationType); err != nil || operationType != string(receipt.OperationType) {
		t.Fatalf("type=%s err=%v", operationType, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cleanup_source_recovery_intents'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("table count=%d err=%v", count, err)
	}
	var family string
	if err := store.db.QueryRow(`SELECT family FROM integrity_registry_sources WHERE registry_version='v1' AND table_name='cleanup_source_recovery_intents'`).Scan(&family); err != nil || family != string(application.IntegrityOwnedResourceCleanup) {
		t.Fatalf("integrity family=%s err=%v", family, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN ('cleanup_source_recovery_intent_immutable','integrity_track_cleanup_source_recovery_intents_insert','integrity_track_cleanup_source_recovery_intents_update','integrity_track_cleanup_source_recovery_intents_delete')`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("retained triggers=%d err=%v", count, err)
	}
}

func TestHistoricalCleanupSourceRecoveryEvidenceRemainsReadableAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	fixture, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	acceptedAt := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)
	profile := repositoryProfileFixture("owner/retired-cleanup-source", "b", "c")
	run := repositoryRunFixture(profile.Profile, "run-retired-cleanup-source", "IFAN-181")
	if _, created, err := fixture.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil || !created {
		fixture.Close()
		t.Fatalf("created=%t err=%v", created, err)
	}
	if err := fixture.Transition(ctx, run.ID, domain.StateReceived, domain.StateAdmitting, "historical_admission", "historical-evidence", ""); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	if err := fixture.Transition(ctx, run.ID, domain.StateAdmitting, domain.StateFailed, "historical_cleanup_residue", "historical-evidence", ""); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	if err := fixture.UpsertCleanup(ctx, application.CleanupRecord{RunID: run.ID, Kind: "worktree", Name: "historical-worktree", Status: "failed", ErrorClass: "operation_failed"}); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	if err := fixture.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: acceptedAt.Add(-5 * time.Minute)}); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	repositoryAuthority, err := fixture.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	disable := repositoryReceiptFixture(application.OperationDisableRepository, repositoryAuthority, profile, acceptedAt.Add(-4*time.Minute))
	if _, _, err := fixture.BeginOperationReceipt(ctx, disable); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	if _, _, err := fixture.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: repositoryAuthority, Intent: application.RepositoryDisabled, ChangedAt: acceptedAt.Add(-3 * time.Minute)}); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	repositoryAuthority, err = fixture.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	guards, err := fixture.EvaluateRepositoryRemovalGuards(ctx, repositoryAuthority, 1, acceptedAt)
	cleanupSettled := false
	for _, guard := range guards {
		if guard.Guard == "cleanup_settled" {
			cleanupSettled = guard.Allowed
		}
	}
	if err != nil || cleanupSettled {
		fixture.Close()
		t.Fatalf("cleanup guard=%+v err=%v", guards, err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{
		OperationType:           application.OperationRecoverCleanupSource,
		Scope:                   application.ScopeRun,
		TargetID:                run.ID,
		Requester:               profile.Authority.TrustedOperators[0],
		RequestDigest:           strings.Repeat("1", 64),
		ExpectedAuthorityDigest: strings.Repeat("2", 64),
		OperationAnchorDigest:   strings.Repeat("3", 64),
		TargetBindingDigest:     run.RepositoryBindingDigest,
		AcceptedAt:              acceptedAt,
	})
	receipt.Phase = application.OperationPhaseObserved
	receipt.Outcome = application.OperationOutcomeSucceeded
	receipt.ResultingAuthorityDigest = strings.Repeat("4", 64)
	receipt.ResultingState = "succeeded"
	receipt.EvidenceDigest = strings.Repeat("5", 64)
	receipt.ResultDigest = strings.Repeat("6", 64)
	receipt.AppliedAt = acceptedAt.Add(time.Minute)
	receipt.SettledAt = acceptedAt.Add(2 * time.Minute)
	if err := application.ValidateOperationReceipt(receipt); err != nil {
		fixture.Close()
		t.Fatal(err)
	}

	var generationBefore int64
	if err := fixture.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generationBefore); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`INSERT INTO operation_receipts(operation_id,authority_key,operation_anchor_digest,operation_type,scope_kind,target_id,requester_login,requester_database_id,requester_node_id,requester_actor_type,request_digest,expected_authority_digest,target_binding_digest,phase,outcome,resulting_authority_digest,resulting_state,resulting_version,evidence_digest,result_digest,accepted_at,applied_at,settled_at,source_action_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.OperationID, receipt.AuthorityKey, receipt.OperationAnchorDigest, string(receipt.OperationType), string(receipt.Scope), receipt.TargetID, receipt.Requester.Login, receipt.Requester.DatabaseID, receipt.Requester.NodeID, receipt.Requester.ActorType, receipt.RequestDigest, receipt.ExpectedAuthorityDigest, receipt.TargetBindingDigest, string(receipt.Phase), string(receipt.Outcome), receipt.ResultingAuthorityDigest, receipt.ResultingState, receipt.ResultingVersion, receipt.EvidenceDigest, receipt.ResultDigest, formatTime(receipt.AcceptedAt), formatTime(receipt.AppliedAt), formatTime(receipt.SettledAt), ""); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`INSERT INTO cleanup_source_recovery_intents(request_id,operation_id,run_id,repository,transition_sequence,abandon_action_digest,attention_event_key,attention_evidence_digest,ownership_digest,cleanup_digest,frozen_source_digest,replacement_source_digest,replacement_identity_digest,repository_origin_digest,registration_digest,repository_binding_digest,branch,candidate_head,preview_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,stage,created_at,updated_at,settled_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"historical-cleanup-source-request", receipt.OperationID, run.ID, run.Repository, 1, strings.Repeat("7", 64), "historical-attention", strings.Repeat("8", 64), strings.Repeat("9", 64), strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 64), run.RepositoryBindingDigest, "codex/historical", strings.Repeat("a", 40), strings.Repeat("0", 64), receipt.Requester.Login, receipt.Requester.DatabaseID, receipt.Requester.NodeID, receipt.Requester.ActorType, "succeeded", formatTime(acceptedAt), formatTime(receipt.SettledAt), formatTime(receipt.SettledAt)); err != nil {
		fixture.Close()
		t.Fatal(err)
	}
	var generationAfter int64
	if err := fixture.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generationAfter); err != nil || generationAfter <= generationBefore {
		fixture.Close()
		t.Fatalf("generation before=%d after=%d err=%v", generationBefore, generationAfter, err)
	}
	if _, err := fixture.db.Exec(`UPDATE cleanup_source_recovery_intents SET branch='codex/drifted' WHERE request_id='historical-cleanup-source-request'`); err == nil {
		fixture.Close()
		t.Fatal("historical recovery authority was mutable")
	}
	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var retainedStage string
	if err := store.db.QueryRow(`SELECT stage FROM cleanup_source_recovery_intents WHERE request_id='historical-cleanup-source-request'`).Scan(&retainedStage); err != nil || retainedStage != "succeeded" {
		t.Fatalf("stage=%s err=%v", retainedStage, err)
	}
	operator := profile.Authority.TrustedOperators[0]
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	activity, err := application.NewActivityQueryService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	history, err := application.NewOperationReceiptQueryService(store, authorizer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := history.Get(ctx, requester, receipt.OperationID)
	if err != nil || detail.OperationType != application.OperationRecoverCleanupSource || detail.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	if _, _, err := store.BeginOperationReceipt(ctx, application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecoverCleanupSource, Scope: application.ScopeRun, TargetID: run.ID, Requester: operator, RequestDigest: strings.Repeat("0", 64), ExpectedAuthorityDigest: strings.Repeat("1", 64), OperationAnchorDigest: strings.Repeat("2", 64), TargetBindingDigest: run.RepositoryBindingDigest, AcceptedAt: acceptedAt.Add(time.Hour)})); err == nil {
		t.Fatal("current SQLite store created a retired cleanup-source receipt")
	}
	if _, _, err := store.AdvanceOperationReceipt(ctx, application.OperationReceiptMutation{OperationID: receipt.OperationID, ExpectedPhase: application.OperationPhaseAccepted, Phase: application.OperationPhaseAccepted, Outcome: application.OperationOutcomeFailed, ResultDigest: receipt.ResultDigest, At: receipt.SettledAt.Add(time.Hour)}); err == nil {
		t.Fatal("current SQLite store advanced a retired cleanup-source receipt")
	}
	page, err := history.List(ctx, application.OperationHistoryQuery{Requester: requester, Filter: application.OperationHistoryFilter{OperationType: application.OperationRecoverCleanupSource}, Limit: 10}, acceptedAt.Add(time.Hour))
	if err != nil || len(page.Receipts) != 1 || page.Receipts[0].OperationID != receipt.OperationID {
		t.Fatalf("history=%+v err=%v", page, err)
	}

	completeIntegrityActivityBackfill(t, store, acceptedAt.Add(2*time.Hour))
	activities, err := activity.List(ctx, application.ActivityListQuery{Requester: requester, Filter: application.ActivityFilter{Category: application.ActivityOperation, Scope: application.ScopeRun, TargetID: run.ID}, Limit: 10}, acceptedAt.Add(3*time.Hour))
	if err != nil || len(activities.Events) != 1 || len(activities.Events[0].OperationIDs) != 1 || activities.Events[0].OperationIDs[0] != receipt.OperationID {
		t.Fatalf("activities=%+v err=%v", activities, err)
	}
	publishIntegrityObservation(t, store, acceptedAt.Add(4*time.Hour))
	service, integrityRequester := integrityQueryService(t, store)
	summary, err := service.Summary(ctx, integrityRequester)
	if err != nil || summary.Readiness != application.IntegrityReady {
		t.Fatalf("integrity=%+v err=%v", summary, err)
	}
}
