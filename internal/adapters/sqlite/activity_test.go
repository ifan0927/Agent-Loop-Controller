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
	defer store.Close()
	result, err = store.BackfillActivityBatch(context.Background(), 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE category='run'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("replayed events=%d err=%v", events, err)
	}
}

func TestActivityV42MigrationReopensCompatibleConflictAndIntegrityConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 41)
	if err != nil {
		t.Fatal(err)
	}
	legacyAdmission, err := configureAdmissionTestStore(legacy, path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := strings.Repeat("d", 64)
	if _, err := legacyAdmission.db.Exec(`UPDATE activity_backfill_progress SET status='complete',updated_at=?`, nowText()); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyAdmission.db.Exec(`UPDATE activity_backfill_progress SET cursor_sequence=7,status='conflict',indexed_through=?,evidence_digest=?,reason_code='immutable_source_conflict',updated_at=? WHERE source_kind='run_transition'`, "2026-08-28T03:00:00Z", evidence, nowText()); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	var status, indexedThrough, retainedEvidence, reason string
	if err := store.db.QueryRow(`SELECT cursor_sequence,status,indexed_through,evidence_digest,reason_code FROM activity_backfill_progress WHERE source_kind='run_transition'`).Scan(&cursor, &status, &indexedThrough, &retainedEvidence, &reason); err != nil {
		t.Fatal(err)
	}
	if cursor != 7 || status != "pending" || indexedThrough != "2026-08-28T03:00:00Z" || retainedEvidence != evidence || reason != "immutable_source_conflict" {
		t.Fatalf("cursor=%d status=%s indexed=%s evidence=%s reason=%s", cursor, status, indexedThrough, retainedEvidence, reason)
	}
	result, err := store.BackfillActivityBatch(context.Background(), 25, time.Date(2026, 8, 28, 4, 0, 0, 0, time.UTC))
	if err != nil || result.SourceKind != "run_transition" || !result.Complete || result.Conflict {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	completeIntegrityActivityBackfill(t, store, time.Date(2026, 8, 28, 4, 1, 0, 0, time.UTC))
	publishIntegrityObservation(t, store, time.Date(2026, 8, 28, 4, 2, 0, 0, time.UTC))
	service, requester := integrityQueryService(t, store)
	summary, err := service.Summary(context.Background(), requester)
	if err != nil || summary.Readiness != application.IntegrityReady {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.db.QueryRow(`SELECT status,evidence_digest,reason_code FROM activity_backfill_progress WHERE source_kind='run_transition'`).Scan(&status, &retainedEvidence, &reason); err != nil || status != "complete" || retainedEvidence != "" || reason != "" {
		t.Fatalf("restart status=%s evidence=%s reason=%s err=%v", status, retainedEvidence, reason, err)
	}
}

func TestActivityV42MigrationDoesNotReopenUnrelatedConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 41)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`UPDATE activity_backfill_progress SET cursor_sequence=3,status='conflict',evidence_digest=?,reason_code='immutable_source_conflict',updated_at=? WHERE source_kind='configuration'`, strings.Repeat("e", 64), nowText()); err != nil {
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
	var status string
	var cursor int64
	if err := store.db.QueryRow(`SELECT cursor_sequence,status FROM activity_backfill_progress WHERE source_kind='configuration'`).Scan(&cursor, &status); err != nil || cursor != 3 || status != "conflict" {
		t.Fatalf("cursor=%d status=%s err=%v", cursor, status, err)
	}
}

func TestActivityV43MigrationPreservesRowsAndTracksOnlySemanticRuntimeChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`INSERT INTO activity_runtime_state(source_kind,source_identity,classification,source_evidence_digest,observed_at,status,reason_code) VALUES('worker_readiness','automatic-worker','running',?,'2026-08-28T05:00:00Z','ready','unchanged_classification')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	var legacyTriggers int
	if err := legacy.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'integrity_track_%'`).Scan(&legacyTriggers); err != nil {
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
	var rows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_runtime_state WHERE source_kind='worker_readiness' AND source_identity='automatic-worker'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	var triggers int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN ('integrity_track_activity_runtime_state_insert','integrity_track_activity_runtime_state_update','integrity_track_activity_runtime_state_delete')`).Scan(&triggers); err != nil || triggers != 3 {
		t.Fatalf("triggers=%d err=%v", triggers, err)
	}
	var migratedTriggers int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'integrity_track_%'`).Scan(&migratedTriggers); err != nil || migratedTriggers != legacyTriggers+3 {
		t.Fatalf("legacy_triggers=%d migrated_triggers=%d err=%v", legacyTriggers, migratedTriggers, err)
	}

	generation := func() int64 {
		var value int64
		if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := generation()
	if _, err := store.db.Exec(`UPDATE activity_runtime_state SET observed_at='2026-08-28T05:01:00Z' WHERE source_kind='worker_readiness' AND source_identity='automatic-worker'`); err != nil {
		t.Fatal(err)
	}
	if after := generation(); after != before {
		t.Fatalf("freshness-only update advanced generation before=%d after=%d", before, after)
	}

	updates := []string{
		`UPDATE activity_runtime_state SET source_kind='worker_runtime' WHERE source_kind='worker_readiness' AND source_identity='automatic-worker'`,
		`UPDATE activity_runtime_state SET source_identity='automatic-worker-v2' WHERE source_kind='worker_runtime' AND source_identity='automatic-worker'`,
		`UPDATE activity_runtime_state SET classification='parked' WHERE source_kind='worker_runtime' AND source_identity='automatic-worker-v2'`,
		`UPDATE activity_runtime_state SET source_evidence_digest='` + strings.Repeat("b", 64) + `' WHERE source_kind='worker_runtime' AND source_identity='automatic-worker-v2'`,
		`UPDATE activity_runtime_state SET status='degraded' WHERE source_kind='worker_runtime' AND source_identity='automatic-worker-v2'`,
		`UPDATE activity_runtime_state SET reason_code='runtime_degraded' WHERE source_kind='worker_runtime' AND source_identity='automatic-worker-v2'`,
	}
	for index, statement := range updates {
		before = generation()
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("semantic update %d: %v", index, err)
		}
		if after := generation(); after != before+1 {
			t.Fatalf("semantic update %d generation before=%d after=%d", index, before, after)
		}
	}
	before = generation()
	if _, err := store.db.Exec(`INSERT INTO activity_runtime_state VALUES('activity_indexing','v43-fixture','ready',?,'2026-08-28T05:02:00Z','ready','recovered')`, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if after := generation(); after != before+1 {
		t.Fatalf("insert generation before=%d after=%d", before, after)
	}
	before = generation()
	if _, err := store.db.Exec(`DELETE FROM activity_runtime_state WHERE source_kind='activity_indexing' AND source_identity='v43-fixture'`); err != nil {
		t.Fatal(err)
	}
	if after := generation(); after != before+1 {
		t.Fatalf("delete generation before=%d after=%d", before, after)
	}
	var revision int64
	if err := store.db.QueryRow(`SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='operation_activity' AND scope_kind='controller' AND scope_id='local-controller'`).Scan(&revision); err != nil || revision != generation() {
		t.Fatalf("revision=%d generation=%d err=%v", revision, generation(), err)
	}
}

func TestRuntimeFreshnessRefreshesDoNotAdvanceIntegrityGeneration(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	observation := application.RuntimeActivityObservation{SourceKind: "worker_readiness", SourceIdentity: "automatic-worker", Classification: "running", SourceEvidenceDigest: strings.Repeat("a", 64), TargetBindingDigest: strings.Repeat("b", 64), OccurredAt: now, ObservedAt: now}
	if _, created, err := store.ReconcileWorkerActivity(ctx, observation); err != nil || !created {
		t.Fatalf("initial created=%t err=%v", created, err)
	}
	observation.ObservedAt = now.Add(time.Minute)
	if _, created, err := store.ReconcileWorkerActivity(ctx, observation); err != nil || created {
		t.Fatalf("settling created=%t err=%v", created, err)
	}
	var settled int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	observation.ObservedAt = now.Add(2 * time.Minute)
	if _, created, err := store.ReconcileWorkerActivity(ctx, observation); err != nil || created {
		t.Fatalf("refresh created=%t err=%v", created, err)
	}
	if err := store.RecordActivityIndexingRecovery(ctx, "worker_readiness", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordActivityIndexingRecovery(ctx, "worker_readiness", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	// The first indexing-recovery call inserts one semantic runtime row. The
	// worker refresh and repeated recovery update only observed_at.
	if after != settled+1 {
		t.Fatalf("freshness refresh advanced generation settled=%d after=%d", settled, after)
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

func TestActivityCurrentAndBackfillInterleavingPreservesFirstSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 28, 2, 0, 0, 0, time.UTC)
	digestA, digestB, digestC := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	type eventPair struct {
		current  application.ActivityEvent
		backfill application.ActivityEvent
	}
	paired := func(build func(application.ActivityIngestionClass) application.ActivityEvent) eventPair {
		return eventPair{current: build(application.ActivityIngestionCurrent), backfill: build(application.ActivityIngestionBackfill)}
	}
	onboarding := func(attempt int64) func(application.ActivityIngestionClass) application.ActivityEvent {
		return func(ingestion application.ActivityIngestionClass) application.ActivityEvent {
			event, valid := newOnboardingActivityEvent("onboarding-1", domain.OnboardingStepSettled, 10, 1, application.OperationOutcomeSucceeded, "ready", digestA, now, digestB, "", ingestion)
			if attempt > 1 {
				event, valid = newOnboardingActivityEvent("onboarding-1", domain.OnboardingStepSettled, 10, attempt, application.OperationOutcomeSucceeded, "ready", digestA, now, digestB, "", ingestion)
			}
			if !valid {
				t.Fatal("onboarding fixture is invalid")
			}
			return event
		}
	}
	tests := map[string]eventPair{
		"run transition": paired(func(ingestion application.ActivityIngestionClass) application.ActivityEvent {
			return newRunTransitionActivityEvent("run-1", 2, "received", "admitting", "admit", "authority", digestA[:40], now, digestB, "", ingestion)
		}),
		"repository readiness": paired(func(ingestion application.ActivityIngestionClass) application.ActivityEvent {
			return newRepositoryReadinessActivityEvent(application.RepositoryReadinessSnapshot{SnapshotID: "snapshot-1", Repository: "owner/repo", RepositoryBindingDigest: digestB, LifecycleVersion: 3, Status: domain.RepositoryReady, ReasonCode: "ready", SnapshotDigest: digestC, ObservedAt: now.Add(-time.Minute), PublishedAt: now}, "", ingestion)
		}),
		"onboarding attempt 1": paired(onboarding(1)),
		"onboarding attempt 2": paired(onboarding(2)),
		"operator attention": paired(func(ingestion application.ActivityIngestionClass) application.ActivityEvent {
			return newOperatorAttentionActivityEvent("attention-1", digestA, "manual_intervention", digestC, now, now.Add(time.Second), application.ScopeController, "controller", digestB, ingestion)
		}),
		"settled operation": paired(func(ingestion application.ActivityIngestionClass) application.ActivityEvent {
			sourceDigest := digestActivitySource(strings.Join([]string{"operation-1", "observed", "succeeded", digestA, digestC}, "\x00"))
			return application.NewActivityEvent(application.ActivityEventInput{SourceKind: "operation_receipt", SourceIdentity: "operation-1", SourceEvidenceDigest: sourceDigest, Category: application.ActivityOperation, EventKind: application.ActivityOperationSettled, Actor: application.ActivityActorConfiguredOperator, Scope: application.ScopeController, TargetID: "controller-integrity", TargetBindingDigest: digestB, ReasonCode: application.ActivityReasonSucceeded, ResultingState: "ready", ResultingVersion: 1, OccurredAt: now, SettledAt: &now, RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeController, ID: "controller-integrity"}}, EvidenceDigests: []string{digestA, digestC}, Coverage: application.ActivityEventCoverage{IngestionClass: ingestion, LegacyReconstructable: true}})
		}),
	}
	for name, pair := range tests {
		for _, order := range []struct {
			name          string
			first, replay application.ActivityEvent
		}{{name: "live-first", first: pair.current, replay: pair.backfill}, {name: "backfill-first", first: pair.backfill, replay: pair.current}} {
			t.Run(name+"/"+order.name, func(t *testing.T) {
				store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				persisted, created, err := store.AppendActivityEvent(context.Background(), order.first)
				if err != nil || !created {
					t.Fatalf("initial append created=%t err=%v", created, err)
				}
				replayed, created, err := store.AppendActivityEvent(context.Background(), order.replay)
				if err != nil || created {
					t.Fatalf("replay created=%t err=%v", created, err)
				}
				if replayed.SnapshotDigest != persisted.SnapshotDigest || replayed.Coverage != persisted.Coverage || replayed.IngestionSequence != persisted.IngestionSequence {
					t.Fatalf("persisted=%+v replayed=%+v", persisted, replayed)
				}
			})
		}
	}
}

func TestOnboardingAttemptOneActivityConstructionRemainsByteCompatible(t *testing.T) {
	now := time.Date(2026, 8, 28, 2, 30, 0, 0, time.UTC)
	evidence, binding := strings.Repeat("a", 64), strings.Repeat("b", 64)
	actual, valid := newOnboardingActivityEvent("onboarding-compatibility", domain.OnboardingStepSettled, 10, 1, application.OperationOutcomeSucceeded, "ready", evidence, now, binding, "", application.ActivityIngestionCurrent)
	if !valid {
		t.Fatal("attempt-one activity is invalid")
	}
	sourceDigest := digestActivitySource(strings.Join([]string{"onboarding-compatibility", string(domain.OnboardingStepSettled), string(application.OperationOutcomeSucceeded), "ready", evidence, formatTime(now)}, "\x00"))
	expected := application.NewActivityEvent(application.ActivityEventInput{
		SourceKind: "onboarding", SourceIdentity: "onboarding-compatibility:settled", SourceEvidenceDigest: sourceDigest,
		Category: application.ActivityOnboarding, EventKind: application.ActivityOnboardingCompleted, Actor: application.ActivityActorController,
		Scope: application.ScopeOnboarding, TargetID: "onboarding-compatibility", TargetBindingDigest: binding, ReasonCode: application.ActivityReasonCompleted,
		ResultingState: string(domain.OnboardingReadyDisabled), ResultingVersion: 10, OccurredAt: now,
		RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeOnboarding, ID: "onboarding-compatibility"}},
		EvidenceDigests:  compactDigests(evidence, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: application.ActivityIngestionCurrent, LegacyReconstructable: true},
	})
	actualJSON, _ := json.Marshal(actual)
	expectedJSON, _ := json.Marshal(expected)
	if string(actualJSON) != string(expectedJSON) {
		t.Fatalf("attempt-one bytes changed:\nactual=%s\nexpected=%s", actualJSON, expectedJSON)
	}
}

func TestActivityCurrentBackfillSemanticDriftStillConflicts(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 28, 3, 0, 0, 0, time.UTC)
	current := newRunTransitionActivityEvent("run-drift", 2, "received", "admitting", "admit", "authority", strings.Repeat("a", 40), now, strings.Repeat("b", 64), "", application.ActivityIngestionCurrent)
	if _, _, err := store.AppendActivityEvent(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	backfill := newRunTransitionActivityEvent("run-drift", 2, "received", "failed", "admit", "authority", strings.Repeat("a", 40), now, strings.Repeat("b", 64), "", application.ActivityIngestionBackfill)
	if _, _, err := store.AppendActivityEvent(context.Background(), backfill); !errors.Is(err, application.ErrActivityConflict) {
		t.Fatalf("semantic drift error=%v", err)
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
