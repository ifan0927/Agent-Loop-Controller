package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOperatorAttentionOutboxIsAppendOnlyIdempotentAndBounded(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first := candidateAttention(t, "scan-2", strings.Repeat("b", 64), time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC))
	second := candidateAttention(t, "scan-1", strings.Repeat("a", 64), time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC))
	if created, err := store.AppendOperatorAttention(ctx, first); err != nil || !created {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	if created, err := store.AppendOperatorAttention(ctx, first); err != nil || created {
		t.Fatalf("duplicate created=%v err=%v", created, err)
	}
	conflict := first
	conflict.ObservedAt = conflict.ObservedAt.Add(time.Second)
	conflict.PayloadDigest = application.OperatorAttentionPayloadDigest(conflict)
	if _, err := store.AppendOperatorAttention(ctx, conflict); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict err=%v", err)
	}
	if created, err := store.AppendOperatorAttention(ctx, second); err != nil || !created {
		t.Fatalf("second created=%v err=%v", created, err)
	}
	events, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 1})
	if err != nil || len(events) != 1 || events[0].EventKey != second.EventKey {
		t.Fatalf("bounded projection=%+v err=%v", events, err)
	}
	if _, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{}); err == nil {
		t.Fatal("expected invalid bound")
	}
}

func TestOperatorAttentionOutboxConcurrentInsertAndSanitizedProjectionParity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := outboxRun(t, "run-outbox")
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	event, err := application.SourceCheckoutSkippedAttentionEvent(run, 0, string(application.SourceSyncReasonDirtySource), strings.Repeat("c", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	created := make(chan bool, 16)
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, appendErr := store.AppendOperatorAttention(context.Background(), event)
			created <- ok
			errs <- appendErr
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	count := 0
	for ok := range created {
		if ok {
			count++
		}
	}
	for appendErr := range errs {
		if appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	if count != 1 {
		t.Fatalf("created=%d", count)
	}
	status, err := application.NewQueryService(store).Inspect(context.Background(), application.QueryInput{Requester: application.Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil || len(status.OperatorAttentionEvents) != 1 {
		t.Fatalf("status=%+v err=%v", status.OperatorAttentionEvents, err)
	}
	projected, _ := json.Marshal(status.OperatorAttentionEvents[0])
	wantProjection := application.OperatorAttentionEventResult{SchemaVersion: event.SchemaVersion, EventKey: event.EventKey, EventType: event.EventType, RunID: event.RunID, LinearIdentifier: event.LinearIdentifier, RepositoryProfileID: event.RepositoryProfileID, RepositoryProfileName: event.RepositoryProfileName, ControllerState: event.ControllerState, Severity: event.Severity, ReasonCode: event.ReasonCode, AllowedActions: event.AllowedActions, PayloadDigest: event.PayloadDigest, EvidenceDigest: event.EvidenceDigest, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt}
	if !reflect.DeepEqual(status.OperatorAttentionEvents[0], wantProjection) || !bytes.Contains(projected, []byte("payload_digest")) || bytes.Contains(projected, []byte("delivery_status")) {
		t.Fatalf("projection parity failed: status=%+v event=%+v", status.OperatorAttentionEvents[0], event)
	}
	secret := "Authorization: Bearer not-for-output"
	unknown, err := application.CandidateScanIncompleteAttentionEvent("scan-secret", application.OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}, secret, strings.Repeat("d", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOperatorAttention(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("database leak=%v", err)
	}
}

func TestOperatorAttentionMigrationPreservesLegacyEvidenceAndNormalizesEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_attention_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_actions`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE automatic_retry_schedules DROP COLUMN failure_evidence_ref`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE attempts DROP COLUMN process_control_key`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrationV17 {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (23,24,25,26,27,28)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)
	evidence := strings.Repeat("e", 64)
	key := "automation:run-legacy:automatic_retry_attention:" + evidence
	legacyEvent := application.OperatorAttentionEvent{SchemaVersion: application.OperatorAttentionLegacySchemaVersion, EventKey: key, EventType: application.OperatorAttentionRetry, RunID: "run-legacy", RepositoryProfileID: "repository-profile:owner/repo", RepositoryProfileName: "owner/repo", ControllerState: string(domain.StateExecuting), Severity: "error", ReasonCode: application.RetryReasonBudgetExhausted, AllowedActions: []application.OperatorAttentionActionID{}, EvidenceDigest: evidence, OccurredAt: now, ObservedAt: now}
	legacyDigest := legacyOperatorAttentionPayloadDigest(legacyEvent, "pending_local")
	_, err = store.db.ExecContext(ctx, `INSERT INTO operator_attention_outbox(event_key,payload_digest,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,evidence_digest,occurred_at,observed_at,delivery_status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, key, legacyDigest, application.OperatorAttentionRetry, "run-legacy", "", "repository-profile:owner/repo", "owner/repo", string(domain.StateExecuting), "error", application.RetryReasonBudgetExhausted, evidence, formatTime(now), formatTime(now), "pending_local", formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	event := events[0]
	if event.SchemaVersion != application.OperatorAttentionLegacySchemaVersion || event.PayloadDigest != legacyDigest || len(event.AllowedActions) != 0 {
		t.Fatalf("normalized event=%+v", event)
	}
	var retainedDigest, retainedStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT legacy_payload_digest,legacy_delivery_status FROM operator_attention_outbox WHERE event_key=?`, key).Scan(&retainedDigest, &retainedStatus); err != nil {
		t.Fatal(err)
	}
	if retainedDigest != legacyDigest || retainedStatus != "pending_local" {
		t.Fatalf("legacy digest=%q status=%q", retainedDigest, retainedStatus)
	}
	replay := event
	replay.SchemaVersion = application.OperatorAttentionSchemaVersion
	// Legacy rows did not persist a failure class. A current replay must stay
	// conservative and cannot infer retry authority from the old presentation.
	replay.AllowedActions = []application.OperatorAttentionActionID{application.OperatorAttentionActionAbandon}
	replay.PayloadDigest = application.OperatorAttentionPayloadDigest(replay)
	if created, err := store.AppendOperatorAttention(ctx, replay); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	conflict := replay
	conflict.ObservedAt = conflict.ObservedAt.Add(time.Second)
	conflict.PayloadDigest = application.OperatorAttentionPayloadDigest(conflict)
	if _, err := store.AppendOperatorAttention(ctx, conflict); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("legacy conflict err=%v", err)
	}
}

func TestOperatorAttentionMigrationAcceptsFrozenLegacyProfileContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_attention_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_actions`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE automatic_retry_schedules DROP COLUMN failure_evidence_ref`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE attempts DROP COLUMN process_control_key`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrationV17 {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (23,24,25,26,27,28)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC)
	evidence := strings.Repeat("a", 64)
	event := application.OperatorAttentionEvent{SchemaVersion: application.OperatorAttentionLegacySchemaVersion, EventKey: "automation:scan-legacy:candidate_scan_incomplete:" + evidence, EventType: application.OperatorAttentionCandidateScan, RepositoryProfileID: "https://host/profile", RepositoryProfileName: "file:/private/repo", ControllerState: "scan", Severity: "warning", ReasonCode: "truncated", AllowedActions: []application.OperatorAttentionActionID{}, EvidenceDigest: evidence, OccurredAt: now, ObservedAt: now}
	event.PayloadDigest = legacyOperatorAttentionPayloadDigest(event, "pending_local")
	_, err = store.db.ExecContext(ctx, `INSERT INTO operator_attention_outbox(event_key,payload_digest,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,evidence_digest,occurred_at,observed_at,delivery_status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.EventKey, event.PayloadDigest, event.EventType, "", "", event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, formatTime(now), formatTime(now), "pending_local", formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
	if err != nil || len(events) != 1 || events[0].PayloadDigest != event.PayloadDigest || events[0].RepositoryProfileID != event.RepositoryProfileID || events[0].RepositoryProfileName != event.RepositoryProfileName {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestManualInterventionReplayBindsTransitionInsteadOfMutableRunTimestamp(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := outboxRun(t, "run-manual-transition")
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]domain.State{{domain.StateReceived, domain.StateAdmitting}, {domain.StateAdmitting, domain.StateProvisioning}, {domain.StateProvisioning, domain.StateExecuting}, {domain.StateExecuting, domain.StateManualIntervention}} {
		if err := store.Transition(ctx, run.ID, edge[0], edge[1], "fixture transition", "fixture_evidence", ""); err != nil {
			t.Fatal(err)
		}
	}
	run, err = store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || len(inspection.Timeline) == 0 {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	transition := inspection.Timeline[len(inspection.Timeline)-1]
	first, err := application.ManualInterventionAttentionEvent(run, transition)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.AppendOperatorAttention(ctx, first); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if acquired, err := store.AcquireLease(ctx, run.ID, "fixture-owner", time.Now().UTC().Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	if err := store.ReleaseLease(ctx, run.ID, "fixture-owner"); err != nil {
		t.Fatal(err)
	}
	run, err = store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := application.ManualInterventionAttentionEvent(run, transition)
	if err != nil || second.EventKey != first.EventKey || second.PayloadDigest != first.PayloadDigest {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	if created, err := store.AppendOperatorAttention(ctx, second); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
}

func candidateAttention(t *testing.T, scanID, digest string, now time.Time) application.OperatorAttentionEvent {
	t.Helper()
	event, err := application.CandidateScanIncompleteAttentionEvent(scanID, application.OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}, "truncated", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func outboxRun(t *testing.T, id string) application.Run {
	t.Helper()
	config, err := json.Marshal(application.LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{"operator"}})
	if err != nil {
		t.Fatal(err)
	}
	return application.Run{ID: id, IssueID: "IFAN-34", IdempotencyKey: "key-" + id, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: string(config), ProfileID: "repository-profile:owner/repo", BaseBranch: "main", WorkingBranch: "ifan/34", ArtifactRoot: "/tmp/" + id, ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
}
