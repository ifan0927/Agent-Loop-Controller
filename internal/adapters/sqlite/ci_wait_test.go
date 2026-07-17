package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestCIWaitPersistsFirstObservationWarnsOnceAndSeparatesHeads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := application.Run{ID: "ci-wait", IssueID: "IFAN-CI", IdempotencyKey: "ci-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", ProfileDigest: "profile", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/ci-wait"}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, "head-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateReconcilingReviews, run.ID); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{Number: 7, URL: "https://example.invalid/pr/7", NodeID: "PR_7", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	first, err := store.ObserveCIWait(ctx, run.ID, pr.Number, "head-1", "profile", 20*time.Minute, start, start)
	if err != nil || !first.WarningAt.IsZero() || first.FirstSeenAt != start {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	warned, err := store.ObserveCIWait(ctx, run.ID, pr.Number, "head-1", "profile", 20*time.Minute, start.Add(5*time.Minute), start.Add(21*time.Minute))
	if err != nil || warned.FirstSeenAt != start || warned.WarningAt != start.Add(20*time.Minute) {
		t.Fatalf("warned=%+v err=%v", warned, err)
	}
	replay, err := store.ObserveCIWait(ctx, run.ID, pr.Number, "head-1", "profile", 20*time.Minute, start.Add(10*time.Minute), start.Add(time.Hour))
	if err != nil || replay.WarningAt != warned.WarningAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := store.ObserveCIWait(ctx, run.ID, pr.Number, "head-1", "profile", 20*time.Minute, start.Add(3*time.Hour), start.Add(2*time.Hour)); err == nil {
		t.Fatal("future GitHub observation was accepted as a CI wait anchor")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateManualIntervention, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseInactiveCIWaits(ctx, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseInactiveCIWaits(ctx, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || len(inspection.CIWaits) != 1 || inspection.CIWaits[0].FirstSeenAt != start || inspection.CIWaits[0].ClosedAt.IsZero() {
		t.Fatalf("waits=%+v err=%v", inspection.CIWaits, err)
	}
}
