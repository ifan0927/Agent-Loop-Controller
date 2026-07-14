package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestAdvanceFixtureLinearCompletionPersistsExactCompletedEvidence(t *testing.T) {
	store, run, merge := fixtureRunAwaitingLinearCompletion(t)
	defer store.Close()
	ctx := context.Background()

	if err := fixtureObserveLinearCompletion(ctx, store, run, merge); err != nil {
		t.Fatal(err)
	}
	if err := fixtureObserveLinearCompletion(ctx, store, run, merge); err != nil {
		t.Fatal(err)
	}
	before, err := store.Inspect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.LinearCompletion) != 1 {
		t.Fatalf("completion observations=%+v", before.LinearCompletion)
	}

	if err := advanceFixtureLinearCompletion(ctx, store, run); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.State != domain.StateCleaning {
		t.Fatalf("state=%s", inspection.Run.State)
	}
	if len(inspection.LinearCompletion) != 1 {
		t.Fatalf("completion observations=%+v", inspection.LinearCompletion)
	}
	got := inspection.LinearCompletion[0]
	if got.RunID != run.ID || got.MergeSHA != merge.MergeSHA || got.Identifier != run.IssueID || got.Status != application.LinearCompletionCompleted || got.StateType != "completed" || !got.ObservedAt.After(merge.MergedAt) {
		t.Fatalf("completion=%+v merge=%+v", got, merge)
	}
	if len(inspection.Timeline) < 3 {
		t.Fatalf("timeline=%+v", inspection.Timeline)
	}
	last := inspection.Timeline[len(inspection.Timeline)-1]
	previous := inspection.Timeline[len(inspection.Timeline)-2]
	if previous.From != domain.StateMerging || previous.To != domain.StateAwaitingLinearCompletion || last.From != domain.StateAwaitingLinearCompletion || last.To != domain.StateCleaning {
		t.Fatalf("post-merge timeline=%+v", inspection.Timeline[len(inspection.Timeline)-2:])
	}
}

func TestFixtureLinearCompletionGateRejectsMissingOrMismatchedEvidence(t *testing.T) {
	mergedAt := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	run := application.Run{ID: "run-fixture-completion", IssueID: "IFAN-LAB-1", BaseSHA: "base", CandidateHead: "candidate"}
	merge := application.MergeRecord{RunID: run.ID, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	completed := application.LinearCompletionObservation{RunID: run.ID, MergeSHA: merge.MergeSHA, Identifier: run.IssueID, StateType: "completed", Status: application.LinearCompletionCompleted, ObservedAt: mergedAt.Add(time.Nanosecond)}

	tests := []struct {
		name         string
		observations []application.LinearCompletionObservation
	}{
		{name: "missing"},
		{name: "wrong merge", observations: []application.LinearCompletionObservation{{RunID: run.ID, MergeSHA: "other", Identifier: run.IssueID, StateType: "completed", Status: application.LinearCompletionCompleted, ObservedAt: mergedAt.Add(time.Nanosecond)}}},
		{name: "pending", observations: []application.LinearCompletionObservation{{RunID: run.ID, MergeSHA: merge.MergeSHA, Identifier: run.IssueID, StateType: "started", Status: application.LinearCompletionPending, ObservedAt: mergedAt.Add(time.Nanosecond)}}},
		{name: "predates merge", observations: []application.LinearCompletionObservation{{RunID: run.ID, MergeSHA: merge.MergeSHA, Identifier: run.IssueID, StateType: "completed", Status: application.LinearCompletionCompleted, ObservedAt: mergedAt}}},
		{name: "completed", observations: []application.LinearCompletionObservation{completed}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateFixtureLinearCompletionGate(run, merge, test.observations)
			if test.name == "completed" && err != nil {
				t.Fatal(err)
			}
			if test.name != "completed" && err == nil {
				t.Fatal("expected Linear completion gate rejection")
			}
		})
	}
}

func fixtureRunAwaitingLinearCompletion(t *testing.T) (*sqlitestore.Store, application.Run, application.MergeRecord) {
	t.Helper()
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{
		ID:                   "run-fixture-completion",
		IssueID:              "IFAN-LAB-1",
		IdempotencyKey:       "fixture-key",
		SourceRevision:       "fixture-v1",
		RawIssueJSON:         "{}",
		RawIssueHash:         "raw",
		NormalizedTaskJSON:   "{}",
		TaskHash:             "task",
		Repository:           "fixture-owner/test-project",
		RepositoryConfigJSON: "{}",
		BaseBranch:           "main",
		WorkingBranch:        "ifan/ifan-lab-1-clamp",
		BaseSHA:              "base",
		ArtifactRoot:         filepath.Join(t.TempDir(), "artifacts"),
	}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SetWorkspace(ctx, input.ID, "base", filepath.Join(t.TempDir(), "worktree")); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, input.ID, "candidate"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	state := domain.StateReceived
	for _, next := range []domain.State{
		domain.StateAdmitting,
		domain.StateProvisioning,
		domain.StateExecuting,
		domain.StateVerifying,
		domain.StateFreshReview,
		domain.StateApprovalReady,
		domain.StatePushingBranch,
		domain.StateBranchPushed,
		domain.StateOpeningPR,
		domain.StatePROpen,
		domain.StateReconcilingReviews,
		domain.StateAwaitingHumanApproval,
		domain.StateMerging,
	} {
		if err := store.Transition(ctx, input.ID, state, next, "fixture test progression", "", "candidate"); err != nil {
			store.Close()
			t.Fatal(err)
		}
		state = next
	}
	mergedAt := time.Now().UTC().Add(-time.Second)
	merge := application.MergeRecord{RunID: input.ID, PRNumber: 1, PreMergeSHA: "candidate", BaseSHA: "base", Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	if err := store.SaveMerge(ctx, merge); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Transition(ctx, input.ID, state, domain.StateAwaitingLinearCompletion, "fixture merge observed", merge.MergeSHA, "candidate"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, input.ID)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, run, merge
}
