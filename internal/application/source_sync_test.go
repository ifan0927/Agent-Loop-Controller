package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type sourceSyncStore struct {
	deliveryMemoryStore
	upserts      []CleanupRecord
	failUpsertAt int
	attention    []OperatorAttentionEvent
}

func (s *sourceSyncStore) AppendOperatorAttention(_ context.Context, event OperatorAttentionEvent) (bool, error) {
	for _, existing := range s.attention {
		if existing.EventKey != event.EventKey {
			continue
		}
		if existing.PayloadDigest != event.PayloadDigest {
			return false, errors.New("operator attention conflict")
		}
		return false, nil
	}
	s.attention = append(s.attention, event)
	return true, nil
}

func (s *sourceSyncStore) UpsertCleanup(_ context.Context, value CleanupRecord) error {
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = time.Now().UTC()
	}
	s.upserts = append(s.upserts, value)
	if s.failUpsertAt > 0 && len(s.upserts) == s.failUpsertAt {
		return errors.New("cleanup persistence unavailable")
	}
	for i := range s.cleanup {
		if s.cleanup[i].RunID == value.RunID && s.cleanup[i].Kind == value.Kind && s.cleanup[i].Name == value.Name {
			s.cleanup[i] = value
			return nil
		}
	}
	s.cleanup = append(s.cleanup, value)
	return nil
}

func (s *sourceSyncStore) CleanupProgress(context.Context, string) ([]CleanupRecord, error) {
	return append([]CleanupRecord(nil), s.cleanup...), nil
}

type recordingSourceSync struct {
	result SourceSyncResult
	err    error
	calls  []SourceSyncRequest
	store  *sourceSyncStore
}

func (p *recordingSourceSync) Sync(_ context.Context, request SourceSyncRequest) (SourceSyncResult, error) {
	p.calls = append(p.calls, request)
	if p.store != nil && (len(p.store.upserts) == 0 || p.store.upserts[len(p.store.upserts)-1].Status != "intent") {
		return SourceSyncResult{}, errors.New("source sync was called before intent")
	}
	return p.result, p.err
}

func sourceSyncFixture(t *testing.T) (Run, MergeRecord) {
	t.Helper()
	repository, err := json.Marshal(LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo", SourcePath: "/frozen/source", OriginPath: "/frozen/origin", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run", Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", RepositoryConfigJSON: string(repository), BaseBranch: "main", CandidateHead: "candidate", State: domain.StateCleaning}
	return run, MergeRecord{RunID: run.ID, PreMergeSHA: run.CandidateHead, Method: "squash", MergeSHA: "exact-merge", MergedAt: time.Now().UTC()}
}

func TestSyncSourceCheckoutPersistsIntentForExactMergeAndDeduplicatesTerminalResult(t *testing.T) {
	run, merge := sourceSyncFixture(t)
	store := &sourceSyncStore{}
	port := &recordingSourceSync{store: store, result: SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncAlreadyAtTarget, MergeSHA: merge.MergeSHA}}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err != nil {
		t.Fatal(err)
	}
	if len(port.calls) != 1 || port.calls[0].MergeSHA != merge.MergeSHA || port.calls[0].MergeSHA == run.CandidateHead {
		t.Fatalf("calls=%+v", port.calls)
	}
	if len(store.upserts) != 2 || store.upserts[0].Status != "intent" || store.upserts[1].Status != "synced" || store.upserts[1].ErrorClass != "" {
		t.Fatalf("upserts=%+v", store.upserts)
	}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err != nil {
		t.Fatal(err)
	}
	if len(port.calls) != 1 {
		t.Fatalf("terminal source sync was repeated: %+v", port.calls)
	}
}

func TestSyncSourceCheckoutRestartsIntentAndMapsAttentionAndRetryableResults(t *testing.T) {
	run, merge := sourceSyncFixture(t)
	store := &sourceSyncStore{deliveryMemoryStore: deliveryMemoryStore{cleanup: []CleanupRecord{{RunID: run.ID, Kind: "source_checkout", Name: sourceCheckoutCleanupIdentity, Status: "intent"}}}}
	port := &recordingSourceSync{store: store, result: SourceSyncResult{Status: SourceSyncSkippedAttention, Outcome: SourceSyncNotApplied, Reason: SourceSyncReasonDirtySource, MergeSHA: merge.MergeSHA}}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err != nil {
		t.Fatal(err)
	}
	if len(port.calls) != 1 || store.cleanup[0].Status != "skipped_attention" || store.cleanup[0].ErrorClass != string(SourceSyncReasonDirtySource) {
		t.Fatalf("calls=%+v cleanup=%+v", port.calls, store.cleanup)
	}
	if len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSourceCheckoutSkipped || store.attention[0].ReasonCode != string(SourceSyncReasonDirtySource) || store.attention[0].DeliveryStatus != OperatorAttentionDeliveryPendingLocal {
		t.Fatalf("attention=%+v", store.attention)
	}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err != nil || len(port.calls) != 1 || len(store.attention) != 1 {
		t.Fatalf("restart err=%v calls=%+v attention=%+v", err, port.calls, store.attention)
	}

	store = &sourceSyncStore{}
	port = &recordingSourceSync{store: store, result: SourceSyncResult{Status: SourceSyncRetryableFailure, Outcome: SourceSyncNotApplied, Reason: SourceSyncReasonFetchFailed, MergeSHA: merge.MergeSHA}}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); !errors.Is(err, errSourceSyncRetryable) {
		t.Fatalf("err=%v", err)
	}
	if store.cleanup[0].Status != "failed" || store.cleanup[0].ErrorClass != string(SourceSyncReasonFetchFailed) {
		t.Fatalf("cleanup=%+v", store.cleanup)
	}
	port.result = SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncAlreadyContainsTarget, MergeSHA: merge.MergeSHA}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err != nil {
		t.Fatal(err)
	}
	if len(port.calls) != 2 || store.cleanup[0].Status != "synced" {
		t.Fatalf("calls=%+v cleanup=%+v", port.calls, store.cleanup)
	}
}

func TestSyncSourceCheckoutRejectsInvalidResultsAndStopsOnPersistenceFailure(t *testing.T) {
	run, merge := sourceSyncFixture(t)
	store := &sourceSyncStore{}
	port := &recordingSourceSync{store: store, result: SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncNotApplied, MergeSHA: merge.MergeSHA}}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err == nil || len(store.cleanup) != 1 || store.cleanup[0].Status != "intent" {
		t.Fatalf("err=%v cleanup=%+v", err, store.cleanup)
	}

	store = &sourceSyncStore{failUpsertAt: 2}
	port = &recordingSourceSync{store: store, result: SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncFastForwarded, MergeSHA: merge.MergeSHA}}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err == nil || len(store.cleanup) != 1 || store.cleanup[0].Status != "intent" {
		t.Fatalf("err=%v cleanup=%+v", err, store.cleanup)
	}
}

func TestSyncSourceCheckoutDoesNotInvokeGitWhenIntentPersistenceFails(t *testing.T) {
	run, merge := sourceSyncFixture(t)
	store := &sourceSyncStore{failUpsertAt: 1}
	port := &recordingSourceSync{store: store, result: SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncFastForwarded, MergeSHA: merge.MergeSHA}}
	if err := SyncSourceCheckout(context.Background(), store, port, run, merge); err == nil {
		t.Fatal("expected intent persistence failure")
	}
	if len(port.calls) != 0 || len(store.cleanup) != 0 {
		t.Fatalf("source adapter crossed an unpersisted intent boundary: calls=%+v cleanup=%+v", port.calls, store.cleanup)
	}
}
