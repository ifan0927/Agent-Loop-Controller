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
	events, err := store.ListOperatorAttention(ctx, 1)
	if err != nil || len(events) != 1 || events[0].EventKey != second.EventKey {
		t.Fatalf("bounded projection=%+v err=%v", events, err)
	}
	if _, err := store.ListOperatorAttention(ctx, 0); err == nil {
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
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.OperatorAttention) != 1 {
		t.Fatalf("inspection=%+v err=%v", inspection.OperatorAttention, err)
	}
	status, err := application.NewQueryService(store).Inspect(context.Background(), application.QueryInput{Requester: application.Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil || len(status.OperatorAttentionOutbox) != 1 {
		t.Fatalf("status=%+v err=%v", status.OperatorAttentionOutbox, err)
	}
	projected, _ := json.Marshal(status.OperatorAttentionOutbox[0])
	wantProjection := application.OperatorAttentionOutboxResult{EventKey: event.EventKey, EventType: event.EventType, RunID: event.RunID, LinearIdentifier: event.LinearIdentifier, RepositoryProfileID: event.RepositoryProfileID, RepositoryProfileName: event.RepositoryProfileName, ControllerState: event.ControllerState, Severity: event.Severity, ReasonCode: event.ReasonCode, EvidenceDigest: event.EvidenceDigest, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt, DeliveryStatus: event.DeliveryStatus}
	if !reflect.DeepEqual(status.OperatorAttentionOutbox[0], wantProjection) || bytes.Contains(projected, []byte("payload_digest")) {
		t.Fatalf("projection parity failed: status=%+v inspect=%+v", status.OperatorAttentionOutbox[0], inspection.OperatorAttention[0])
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
