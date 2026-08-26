package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func activityTestOperator() domain.GitHubUserIdentity {
	return domain.GitHubUserIdentity{Login: "operator", DatabaseID: 91, NodeID: "USER_91", ActorType: "User"}
}

func activityTestRequester() application.Requester {
	operator := activityTestOperator()
	return application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
}

func activityTestRun(id string) application.Run {
	operator := activityTestOperator()
	canonical := "owner/" + id
	repository := application.LocalRepository{ProfileID: "repository-profile:" + canonical, CanonicalRepository: canonical, AllowedOperatorLogins: []string{operator.Login}, TrustedOperatorActors: []application.TrustedActorIdentity{{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType}}}
	raw, _ := json.Marshal(repository)
	return application.Run{ID: id, IssueID: "IFAN-135-" + id, IdempotencyKey: "idempotency-" + id, SourceRevision: "revision-" + id, RawIssueJSON: "{}", RawIssueHash: "raw-" + id, NormalizedTaskJSON: "{}", TaskHash: "task-" + id, Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), ProfileID: repository.ProfileID, RepositoryBindingDigest: application.ConfigurationEvidenceDigest("activity-test-binding-v1", canonical), BaseBranch: "main", WorkingBranch: "ifan/" + id, ArtifactRoot: "artifact-" + id, ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
}

func activityTestServices(t *testing.T, store *Store) (*application.ActivityQueryService, *application.OperationReceiptQueryService) {
	t.Helper()
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: activityTestOperator()})
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
	return activity, history
}

func TestActivityMigrationIsSchemaOnlyAndBackfillIsBoundedRestartSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 38)
	if err != nil {
		t.Fatal(err)
	}
	legacyAdmission, err := configureAdmissionTestStore(legacy, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := legacyAdmission.CreateRun(context.Background(), application.CreateRunInput{Run: activityTestRun("run-legacy")}); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var events int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	result, err := store.BackfillActivityBatch(context.Background(), 1, time.Now().UTC())
	if err != nil || result.SourceKind != "run_transition" || result.Indexed != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE category='run'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.BackfillActivityBatch(context.Background(), 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE category='run'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("replayed events=%d err=%v", events, err)
	}
}

func TestActivityReplayConflictAndCorruptDetailFailClosed(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 26, 2, 0, 0, 0, time.UTC)
	input := application.ActivityEventInput{SourceKind: "runtime-test", SourceIdentity: "source-1", SourceEvidenceDigest: strings.Repeat("a", 64), Category: application.ActivityWorker, EventKind: application.ActivityWorkerReadinessChange, Actor: application.ActivityActorController, Scope: application.ScopeController, TargetID: "controller-worker", TargetBindingDigest: strings.Repeat("b", 64), ReasonCode: application.ActivityReasonReadinessChanged, ResultingState: "ready", OccurredAt: now, EvidenceDigests: []string{strings.Repeat("a", 64)}, Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionRuntime}}
	event := application.NewActivityEvent(input)
	if _, created, err := store.AppendActivityEvent(context.Background(), event); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if _, created, err := store.AppendActivityEvent(context.Background(), event); err != nil || created {
		t.Fatalf("replay created=%t err=%v", created, err)
	}
	driftInput := input
	driftInput.ResultingState = "degraded"
	if _, _, err := store.AppendActivityEvent(context.Background(), application.NewActivityEvent(driftInput)); !errors.Is(err, application.ErrActivityConflict) {
		t.Fatalf("conflict=%v", err)
	}
	if _, err := store.db.Exec(`UPDATE activity_events SET snapshot_digest=? WHERE event_id=?`, strings.Repeat("f", 64), event.EventID); err != nil {
		t.Fatal(err)
	}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: activityTestOperator()})
	configured, _ := authorizer.ResolveConfiguredRequester(activityTestRequester())
	scopes, _ := authorizer.ControllerScopes(configured)
	if _, _, err := store.GetActivity(context.Background(), event.EventID, scopes); err == nil || errors.Is(err, application.ErrActivityNotFound) {
		t.Fatalf("corruption error=%v", err)
	}
}

func TestRunTransitionAndActivityAppendRollbackTogetherOnConflict(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := activityTestRun("run-atomic")
	if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	conflict := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "run_transition", SourceIdentity: run.ID + ":2", SourceEvidenceDigest: strings.Repeat("4", 64), Category: application.ActivityRun, EventKind: application.ActivityRunTransition, Actor: application.ActivityActorController, Scope: application.ScopeRun, TargetID: run.ID, TargetBindingDigest: run.RepositoryBindingDigest, ReasonCode: application.ActivityReasonStateChanged, PriorState: string(domain.StateReceived), ResultingState: string(domain.StateFailed), PriorVersion: 1, ResultingVersion: 2, OccurredAt: time.Now().UTC(), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeRun, ID: run.ID}}, EvidenceDigests: []string{strings.Repeat("4", 64)}, Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true}})
	if _, _, err := store.AppendActivityEvent(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, run.ID, domain.StateReceived, domain.StateAdmitting, "admit", "authority", ""); !errors.Is(err, application.ErrActivityConflict) {
		t.Fatalf("transition error=%v", err)
	}
	persisted, err := store.GetRun(ctx, run.ID)
	if err != nil || persisted.State != domain.StateReceived {
		t.Fatalf("run=%+v err=%v", persisted, err)
	}
	var transitions int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM transitions WHERE run_id=?`, run.ID).Scan(&transitions); err != nil || transitions != 1 {
		t.Fatalf("transitions=%d err=%v", transitions, err)
	}
}

func TestActivityPaginationFixesIngestionWatermark(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, id := range []string{"run-a", "run-b"} {
		if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: activityTestRun(id)}); err != nil || !created {
			t.Fatalf("id=%s created=%t err=%v", id, created, err)
		}
	}
	activity, _ := activityTestServices(t, store.Store)
	filter := application.ActivityFilter{Category: application.ActivityRun}
	baseline, err := activity.List(ctx, application.ActivityListQuery{Requester: activityTestRequester(), Filter: filter, Limit: 10}, time.Now().UTC())
	if err != nil || len(baseline.Events) != 2 {
		t.Fatalf("baseline=%+v err=%v", baseline, err)
	}
	first, err := activity.List(ctx, application.ActivityListQuery{Requester: activityTestRequester(), Filter: filter, Limit: 1}, time.Now().UTC())
	if err != nil || first.Collection.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	newEvent := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "run_transition", SourceIdentity: "run-new:1", SourceEvidenceDigest: strings.Repeat("7", 64), Category: application.ActivityRun, EventKind: application.ActivityRunTransition, Actor: application.ActivityActorController, Scope: application.ScopeRun, TargetID: "run-new", TargetBindingDigest: strings.Repeat("8", 64), ReasonCode: application.ActivityReasonStateChanged, ResultingState: string(domain.StateReceived), ResultingVersion: 1, OccurredAt: time.Now().UTC().Add(time.Hour), RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeRun, ID: "run-new"}}, EvidenceDigests: []string{strings.Repeat("7", 64)}, Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true}})
	if _, _, err := store.AppendActivityEvent(ctx, newEvent); err != nil {
		t.Fatal(err)
	}
	next, err := activity.List(ctx, application.ActivityListQuery{Requester: activityTestRequester(), Filter: filter, Limit: 1, Cursor: first.Collection.NextCursor}, time.Now().UTC())
	if err != nil || len(next.Events) != 1 || next.Events[0].EventID != baseline.Events[1].EventID {
		t.Fatalf("baseline=%+v next=%+v err=%v", baseline.Events, next, err)
	}
	if next.Collection.Total != 2 {
		t.Fatalf("watermarked total=%d", next.Collection.Total)
	}
}

func TestOperationHistoryIsBoundedFilteredAndStableAcrossReceiptAdvance(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	operator := activityTestOperator()
	var receipts []application.OperationReceipt
	for index := 0; index < 3; index++ {
		accepted := time.Date(2026, 8, 26, 3, index, 0, 0, time.UTC)
		receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRetry, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: operator, RequestDigest: strings.Repeat(string(rune('a'+index)), 64), ExpectedAuthorityDigest: strings.Repeat("d", 64), OperationAnchorDigest: strings.Repeat(string(rune('f'-index)), 64), TargetBindingDigest: strings.Repeat("e", 64), AcceptedAt: accepted})
		if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	_, history := activityTestServices(t, store.Store)
	first, err := history.List(ctx, application.OperationHistoryQuery{Requester: activityTestRequester(), Filter: application.OperationHistoryFilter{OperationType: application.OperationRetry}, Limit: 1}, time.Now().UTC())
	if err != nil || len(first.Receipts) != 1 || first.Collection.Total != 3 || first.Collection.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	latest := receipts[2]
	if _, _, err := store.AdvanceOperationReceipt(ctx, application.OperationReceiptMutation{OperationID: latest.OperationID, ExpectedPhase: application.OperationPhaseAccepted, Phase: application.OperationPhaseAccepted, Outcome: application.OperationOutcomeFailed, ResultDigest: strings.Repeat("9", 64), At: latest.AcceptedAt.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	next, err := history.List(ctx, application.OperationHistoryQuery{Requester: activityTestRequester(), Filter: application.OperationHistoryFilter{OperationType: application.OperationRetry}, Limit: 1, Cursor: first.Collection.NextCursor}, time.Now().UTC())
	if err != nil || len(next.Receipts) != 1 || next.Receipts[0].OperationID != receipts[1].OperationID {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	denied := activityTestRequester()
	denied.DatabaseID++
	if _, err := history.List(ctx, application.OperationHistoryQuery{Requester: denied}, time.Now().UTC()); err == nil {
		t.Fatal("unauthorized history was readable")
	}
}

func TestRuntimeActivitySuppressesUnchangedClassification(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	observation := application.RuntimeActivityObservation{SourceKind: "worker_readiness", SourceIdentity: "worker-1", Classification: "ready", SourceEvidenceDigest: strings.Repeat("a", 64), TargetBindingDigest: strings.Repeat("b", 64), OccurredAt: now, ObservedAt: now}
	if _, created, err := store.ReconcileWorkerActivity(context.Background(), observation); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	observation.ObservedAt = now.Add(time.Minute)
	if _, created, err := store.ReconcileWorkerActivity(context.Background(), observation); err != nil || created {
		t.Fatalf("unchanged created=%t err=%v", created, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE category='worker'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestActivityCoverageRecoversAfterTransientIndexingFailure(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.Exec(`UPDATE activity_backfill_progress SET status='complete'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.RecordActivityIndexingFailure(ctx, "legacy_backfill", "bounded_backfill_failed", now); err != nil {
		t.Fatal(err)
	}
	activity, _ := activityTestServices(t, store.Store)
	degraded, err := activity.List(ctx, application.ActivityListQuery{Requester: activityTestRequester()}, now)
	if err != nil || degraded.Coverage.State != application.ActivityCoverageDegraded {
		t.Fatalf("coverage=%+v err=%v", degraded.Coverage, err)
	}
	if err := store.RecordActivityIndexingRecovery(ctx, "legacy_backfill", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	recovered, err := activity.List(ctx, application.ActivityListQuery{Requester: activityTestRequester()}, now.Add(time.Minute))
	if err != nil || recovered.Coverage.State != application.ActivityCoverageComplete {
		t.Fatalf("coverage=%+v err=%v", recovered.Coverage, err)
	}
	if !slices.Contains(recovered.Coverage.LegacyLimitations, "legacy_repository_intent_history_unavailable") {
		t.Fatalf("legacy limitations=%v", recovered.Coverage.LegacyLimitations)
	}
}
