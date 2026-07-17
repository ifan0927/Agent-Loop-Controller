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

func feedbackRecord(runID string) application.TrustedReviewFeedbackRecord {
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	line := 9
	return application.TrustedReviewFeedbackRecord{RunID: runID, TrustedReviewFeedback: domain.TrustedReviewFeedback{PRNumber: 1, PRDatabaseID: 2, PRNodeID: "PR_2", ReviewDatabaseID: 3, ReviewNodeID: "REVIEW_3", ThreadNodeID: "THREAD_4", RootCommentDatabaseID: 5, RootCommentNodeID: "COMMENT_5", Author: domain.ActorIdentity{DatabaseID: 6, NodeID: "USER_6", Login: "ifan0927", Type: "User"}, OriginalReviewHeadSHA: strings.Repeat("a", 40), Path: "internal/example.go", Line: &line, Body: "bounded trusted feedback", SourceAt: now, ObservedAt: now}}
}

func createFeedbackRun(t *testing.T, store *Store, id string) {
	t.Helper()
	run := application.Run{ID: id, IssueID: id, IdempotencyKey: id, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/" + id}
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedReviewFeedbackIsImmutableBoundedAndCASOnly(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createFeedbackRun(t, store, "feedback-run")
	record := feedbackRecord("feedback-run")
	saved, created, err := store.SaveTrustedReviewFeedback(ctx, record)
	if err != nil || !created || saved.BodyDigest == "" {
		t.Fatalf("saved=%+v created=%t err=%v", saved, created, err)
	}
	inspectionAfterSave, err := store.Inspect(ctx, record.RunID)
	if err != nil || len(inspectionAfterSave.TrustedFeedback) != 1 || saved.BoundRepairHead != inspectionAfterSave.TrustedFeedback[0].BoundRepairHead || saved.ReplyIntentKey != inspectionAfterSave.TrustedFeedback[0].ReplyIntentKey || saved.ReplyDatabaseID != inspectionAfterSave.TrustedFeedback[0].ReplyDatabaseID || saved.ReplyNodeID != inspectionAfterSave.TrustedFeedback[0].ReplyNodeID || saved.Resolved != inspectionAfterSave.TrustedFeedback[0].Resolved || saved.Outdated != inspectionAfterSave.TrustedFeedback[0].Outdated {
		t.Fatalf("save return and persisted feedback differ: saved=%+v inspection=%+v err=%v", saved, inspectionAfterSave.TrustedFeedback, err)
	}
	duplicate := record
	duplicate.Resolved, duplicate.Outdated = true, true
	duplicate.ObservedAt = duplicate.ObservedAt.Add(time.Hour)
	if _, _, err := store.SaveTrustedReviewFeedback(ctx, duplicate); err == nil {
		t.Fatal("duplicate accepted future lifecycle evidence")
	}
	if saved, created, err := store.SaveTrustedReviewFeedback(ctx, record); err != nil || created || saved.Resolved || saved.Outdated || !saved.ObservedAt.Equal(record.ObservedAt) {
		t.Fatalf("identical duplicate mutated lifecycle evidence: saved=%+v created=%t err=%v", saved, created, err)
	}
	drift := record
	drift.Body = "changed body"
	if _, _, err := store.SaveTrustedReviewFeedback(ctx, drift); err == nil {
		t.Fatal("body drift was accepted")
	}
	inspection, err := store.Inspect(ctx, record.RunID)
	if err != nil || len(inspection.TrustedFeedback) != 1 || inspection.TrustedFeedback[0].Body != record.Body || len(inspection.FeedbackConflicts) != 1 {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	if _, ok, err := store.TransitionTrustedReviewFeedback(ctx, record.RunID, record.RootCommentNodeID, domain.TrustedReviewFeedbackObserved, domain.TrustedReviewFeedbackSelectedForRepair, "", "", 0, "", false, false); err != nil || !ok {
		t.Fatalf("select ok=%t err=%v", ok, err)
	}
	selectedReplay, created, err := store.SaveTrustedReviewFeedback(ctx, record)
	if err != nil || created || selectedReplay.Lifecycle != domain.TrustedReviewFeedbackSelectedForRepair || selectedReplay.Resolved || selectedReplay.Outdated {
		t.Fatalf("selected replay changed lifecycle evidence: replay=%+v created=%t err=%v", selectedReplay, created, err)
	}
	for _, transition := range []struct {
		expected  domain.TrustedReviewFeedbackLifecycle
		next      domain.TrustedReviewFeedbackLifecycle
		head      string
		intent    string
		replyID   int64
		replyNode string
		resolved  bool
	}{
		{domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackRepairVerified, strings.Repeat("b", 40), "", 0, "", false},
		{domain.TrustedReviewFeedbackRepairVerified, domain.TrustedReviewFeedbackReplyPending, "", "reply-intent", 0, "", false},
		{domain.TrustedReviewFeedbackReplyPending, domain.TrustedReviewFeedbackReplied, "", "", 9, "COMMENT_REPLY_9", false},
		{domain.TrustedReviewFeedbackReplied, domain.TrustedReviewFeedbackResolved, "", "", 0, "", true},
	} {
		if _, ok, err := store.TransitionTrustedReviewFeedback(ctx, record.RunID, record.RootCommentNodeID, transition.expected, transition.next, transition.head, transition.intent, transition.replyID, transition.replyNode, transition.resolved, false); err != nil || !ok {
			t.Fatalf("transition=%+v ok=%t err=%v", transition, ok, err)
		}
	}
	replay, created, err := store.SaveTrustedReviewFeedback(ctx, record)
	if err != nil || created || replay.Lifecycle != domain.TrustedReviewFeedbackResolved || !replay.Resolved || replay.Outdated {
		t.Fatalf("resolved replay changed lifecycle evidence: replay=%+v created=%t err=%v", replay, created, err)
	}
	if _, ok, err := store.TransitionTrustedReviewFeedback(ctx, record.RunID, record.RootCommentNodeID, domain.TrustedReviewFeedbackObserved, domain.TrustedReviewFeedbackSelectedForRepair, "", "", 0, "", false, false); err != nil || ok {
		t.Fatalf("stale CAS ok=%t err=%v", ok, err)
	}
	if _, _, err := store.TransitionTrustedReviewFeedback(ctx, record.RunID, record.RootCommentNodeID, domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackReplied, "", "", 0, "", false, false); err == nil {
		t.Fatal("illegal lifecycle accepted")
	}
}

func TestTrustedReviewFeedbackInitialSaveRejectsFutureEvidence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createFeedbackRun(t, store, "initial-evidence-run")
	base := feedbackRecord("initial-evidence-run")
	for _, mutate := range []func(*application.TrustedReviewFeedbackRecord){
		func(v *application.TrustedReviewFeedbackRecord) { v.BoundRepairHead = strings.Repeat("b", 40) },
		func(v *application.TrustedReviewFeedbackRecord) { v.ReplyIntentKey = "reply-intent" },
		func(v *application.TrustedReviewFeedbackRecord) { v.ReplyDatabaseID, v.ReplyNodeID = 9, "REPLY_9" },
		func(v *application.TrustedReviewFeedbackRecord) { v.Resolved = true },
		func(v *application.TrustedReviewFeedbackRecord) { v.Outdated = true },
	} {
		candidate := base
		mutate(&candidate)
		if _, _, err := store.SaveTrustedReviewFeedback(ctx, candidate); err == nil {
			t.Fatalf("initial feedback accepted future evidence: %+v", candidate)
		}
	}
	inspection, err := store.Inspect(ctx, base.RunID)
	if err != nil || len(inspection.TrustedFeedback) != 0 {
		t.Fatalf("rejected initial feedback persisted: inspection=%+v err=%v", inspection.TrustedFeedback, err)
	}
}

func TestTrustedFeedbackDriftPersistsConflictAndManualTransitionAtomically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createFeedbackRun(t, store, "drift-run")
	record := feedbackRecord("drift-run")
	if _, _, err := store.SaveTrustedReviewFeedback(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner := "drift-lease"
	if ok, err := store.AcquireLease(ctx, record.RunID, owner, time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("lease ok=%t err=%v", ok, err)
	}
	pr := domain.PullRequest{Number: 1, DatabaseID: 2, URL: "https://example.invalid/pr/1", NodeID: record.PRNodeID, HeadBranch: "feature", BaseBranch: "main", HeadSHA: record.OriginalReviewHeadSHA, BaseSHA: "base", BodyDigest: "body", OwnershipKey: record.RunID, State: "open"}
	if err := store.SavePullRequest(ctx, record.RunID, pr); err != nil {
		t.Fatal(err)
	}
	repo := domain.RepositoryIdentity{ID: 9, NodeID: "REPO", Owner: "owner", Name: "repo"}
	now := time.Now().UTC()
	metadata := application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: "permissions", ObservedAt: now}
	evidence := domain.GitHubReadEvidence{Repository: repo, PullRequest: pr, ObservedAt: now}
	observations := []application.GitHubRequestObservation{{RunID: record.RunID, Operation: "review_threads", Category: "graphql", HTTPStatus: 200, ResponseDigest: "read", InstallationID: 2, Repository: repo, ObservedAt: now}}
	observed := domain.TrustedReviewFeedbackDigest("edited body")
	badObservations := append([]application.GitHubRequestObservation(nil), observations...)
	badObservations = append(badObservations, application.GitHubRequestObservation{RunID: "other-run"})
	if err := store.RequireManualInterventionForTrustedFeedbackDrift(ctx, record.RunID, owner, domain.StateReceived, record.RunID, badObservations, pr, metadata, evidence, record.RootCommentNodeID, observed); err == nil {
		t.Fatal("fault-injected observation was accepted")
	}
	before, err := store.Inspect(ctx, record.RunID)
	if err != nil || before.Run.State != domain.StateReceived || before.GitHubEvidence != nil || len(before.GitHubRequests) != 0 || len(before.FeedbackConflicts) != 0 {
		t.Fatalf("partial drift persistence: %+v err=%v", before, err)
	}
	if err := store.RequireManualInterventionForTrustedFeedbackDrift(ctx, record.RunID, owner, domain.StateReceived, record.RunID, observations, pr, metadata, evidence, record.RootCommentNodeID, observed); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, record.RunID)
	if err != nil || inspection.Run.State != domain.StateManualIntervention || inspection.GitHubEvidence == nil || len(inspection.GitHubRequests) != 1 || len(inspection.FeedbackConflicts) != 1 || inspection.FeedbackConflicts[0].RootCommentNodeID != record.RootCommentNodeID || inspection.FeedbackConflicts[0].ObservedDigest != observed {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	if got := inspection.Timeline[len(inspection.Timeline)-1].Reason; got != application.TrustedReviewFeedbackDriftReason {
		t.Fatalf("drift reason=%q", got)
	}
	if err := store.RequireManualInterventionForTrustedFeedbackDrift(ctx, record.RunID, owner, domain.StateReceived, record.RunID, observations, pr, metadata, evidence, record.RootCommentNodeID, domain.TrustedReviewFeedbackDigest("second edit")); err == nil {
		t.Fatal("stale drift write was accepted")
	}
	inspection, err = store.Inspect(ctx, record.RunID)
	if err != nil || len(inspection.FeedbackConflicts) != 1 {
		t.Fatalf("non-atomic stale write: %+v err=%v", inspection, err)
	}
}

func TestTrustedReviewFeedbackAggregateTextBound(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createFeedbackRun(t, store, "text-bound-run")
	base := feedbackRecord("text-bound-run")
	base.Body = strings.Repeat("x", domain.MaxTrustedReviewFeedbackBodyBytes)
	for i := 0; i < 4; i++ {
		record := base
		record.RootCommentNodeID = "BODY_COMMENT_" + string(rune('a'+i))
		record.RootCommentDatabaseID = int64(200 + i)
		if _, created, err := store.SaveTrustedReviewFeedback(ctx, record); err != nil || !created {
			t.Fatalf("index=%d created=%t err=%v", i, created, err)
		}
	}
	extra := base
	extra.RootCommentNodeID, extra.RootCommentDatabaseID, extra.Body = "BODY_COMMENT_extra", 300, "x"
	if _, _, err := store.SaveTrustedReviewFeedback(ctx, extra); err == nil {
		t.Fatal("aggregate text bound was exceeded")
	}
}

func TestTrustedReviewFeedbackTransitionRequiresStageEvidence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createFeedbackRun(t, store, "evidence-run")
	record := feedbackRecord("evidence-run")
	record.RootCommentNodeID, record.RootCommentDatabaseID = "COMMENT_evidence", 70
	if _, _, err := store.SaveTrustedReviewFeedback(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.TransitionTrustedReviewFeedback(ctx, record.RunID, "\x00", domain.TrustedReviewFeedbackObserved, domain.TrustedReviewFeedbackSelectedForRepair, "", "", 0, "", false, false); err == nil {
		t.Fatal("transition accepted NUL root comment node identity")
	}
	transition := func(expected, next domain.TrustedReviewFeedbackLifecycle, head, intent string, replyID int64, replyNode string, resolved bool) error {
		_, _, err := store.TransitionTrustedReviewFeedback(ctx, record.RunID, record.RootCommentNodeID, expected, next, head, intent, replyID, replyNode, resolved, false)
		return err
	}
	if err := transition(domain.TrustedReviewFeedbackObserved, domain.TrustedReviewFeedbackSelectedForRepair, "", "", 0, "", true); err == nil {
		t.Fatal("selected transition accepted resolved=true")
	}
	if err := transition(domain.TrustedReviewFeedbackObserved, domain.TrustedReviewFeedbackSelectedForRepair, "", "", 0, "", false); err != nil {
		t.Fatal(err)
	}
	if err := transition(domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackRepairVerified, "", "", 0, "", false); err == nil {
		t.Fatal("repair_verified accepted without bound head")
	}
	head := strings.Repeat("b", 40)
	if err := transition(domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackRepairVerified, head, "", 0, "", false); err != nil {
		t.Fatal(err)
	}
	if err := transition(domain.TrustedReviewFeedbackRepairVerified, domain.TrustedReviewFeedbackReplyPending, "", "", 0, "", false); err == nil {
		t.Fatal("reply_pending accepted without intent")
	}
	if err := transition(domain.TrustedReviewFeedbackRepairVerified, domain.TrustedReviewFeedbackReplyPending, "", "reply-intent", 0, "", false); err != nil {
		t.Fatal(err)
	}
	if err := transition(domain.TrustedReviewFeedbackReplyPending, domain.TrustedReviewFeedbackReplied, "", "", 0, "", false); err == nil {
		t.Fatal("replied accepted without observed reply identity")
	}
	if err := transition(domain.TrustedReviewFeedbackReplyPending, domain.TrustedReviewFeedbackReplied, "", "", 9, "\x00", false); err == nil {
		t.Fatal("replied accepted NUL reply node identity")
	}
	if err := transition(domain.TrustedReviewFeedbackReplyPending, domain.TrustedReviewFeedbackReplied, "", "", 9, "REPLY_9", false); err != nil {
		t.Fatal(err)
	}
	if err := transition(domain.TrustedReviewFeedbackReplied, domain.TrustedReviewFeedbackResolved, "", "", 0, "", false); err == nil {
		t.Fatal("resolved accepted without resolved flag")
	}
	if err := transition(domain.TrustedReviewFeedbackReplied, domain.TrustedReviewFeedbackResolved, "", "", 10, "REPLY_10", true); err == nil {
		t.Fatal("resolved accepted with replacement reply identity")
	}
	if err := transition(domain.TrustedReviewFeedbackReplied, domain.TrustedReviewFeedbackResolved, "", "", 0, "", true); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeReviewReplyIsAtomicAcrossLifecycleEvidenceAndSideEffect(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createFeedbackRun(t, store, "atomic-reply")
	if ok, err := store.AcquireLease(ctx, "atomic-reply", "reply-owner", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("acquire lease ok=%t err=%v", ok, err)
	}
	if err := store.SaveReviewReplyObservations(ctx, "atomic-reply", "reply-owner", []application.GitHubRequestObservation{{RunID: "atomic-reply", Operation: "review_comment_replies", Category: "REST", ErrorClass: "transport_failure", InstallationID: 2, Repository: domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}, ObservedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	feedback := feedbackRecord("atomic-reply")
	feedback.RootCommentNodeID, feedback.RootCommentDatabaseID = "COMMENT_atomic", 71
	if _, _, err := store.SaveTrustedReviewFeedback(ctx, feedback); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	if _, ok, err := store.TransitionTrustedReviewFeedback(ctx, feedback.RunID, feedback.RootCommentNodeID, domain.TrustedReviewFeedbackObserved, domain.TrustedReviewFeedbackSelectedForRepair, "", "", 0, "", false, false); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := store.TransitionTrustedReviewFeedback(ctx, feedback.RunID, feedback.RootCommentNodeID, domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackRepairVerified, head, "", 0, "", false, false); err != nil || !ok {
		t.Fatal(err)
	}
	intent := strings.Repeat("c", 64)
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: feedback.RunID, Kind: "reply_to_review_comment", IdempotencyKey: intent, IntentJSON: `{"pull_request":1}`, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.TransitionTrustedReviewFeedback(ctx, feedback.RunID, feedback.RootCommentNodeID, domain.TrustedReviewFeedbackRepairVerified, domain.TrustedReviewFeedbackReplyPending, head, intent, 0, "", false, false); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET lease_expires_unix=0 WHERE run_id=?`, feedback.RunID); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.TransitionReviewReplyFeedback(ctx, feedback.RunID, "reply-owner", feedback.RootCommentNodeID, domain.TrustedReviewFeedbackReplyPending, domain.TrustedReviewFeedbackResolved, head, intent, 0, "", true, false); err == nil || changed {
		t.Fatalf("expired lease changed=%t err=%v", changed, err)
	}
	if ok, err := store.AcquireLease(ctx, feedback.RunID, "reply-owner", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("reacquire lease ok=%t err=%v", ok, err)
	}
	side.Status = "failed"
	if err := store.FinishReviewReplySideEffect(ctx, "reply-owner", side); err != nil {
		t.Fatal(err)
	}
	if retried, changed, err := store.RetryReviewReplySideEffect(ctx, "reply-owner", side, 3); err != nil || !changed || retried.Status != "intent" || retried.Attempt != 2 {
		t.Fatalf("retried=%+v changed=%t err=%v", retried, changed, err)
	}
	completion := application.ReviewReplyCompletion{Feedback: feedback, Head: head, Side: side, Reply: domain.ReviewReply{DatabaseID: 72, NodeID: "COMMENT_REPLY_72", ReplyToID: feedback.RootCommentDatabaseID, Actor: domain.ActorIdentity{AppID: 1}, CreatedAt: time.Now().UTC()}, Observations: []application.GitHubRequestObservation{{RunID: feedback.RunID, Operation: "reply_to_review_comment", Category: "REST", HTTPStatus: 201, ResponseDigest: strings.Repeat("d", 64), InstallationID: 2, Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}, ObservedAt: time.Now().UTC()}}, LeaseOwner: "reply-owner"}
	assertPending := func() {
		inspection, inspectErr := store.Inspect(ctx, feedback.RunID)
		if inspectErr != nil || inspection.TrustedFeedback[0].Lifecycle != domain.TrustedReviewFeedbackReplyPending || len(inspection.ReviewReplies) != 0 || inspection.SideEffects[0].Status != "intent" {
			t.Fatalf("inspection=%+v err=%v", inspection, inspectErr)
		}
	}
	wrongPR := completion
	wrongPR.Feedback.PRNumber++
	if ok, err := store.FinalizeReviewReply(ctx, wrongPR); err != nil || ok {
		t.Fatalf("wrong PR ok=%t err=%v", ok, err)
	}
	assertPending()
	wrongCallerRoot := completion
	wrongCallerRoot.Feedback.RootCommentDatabaseID++
	if ok, err := store.FinalizeReviewReply(ctx, wrongCallerRoot); err != nil || ok {
		t.Fatalf("wrong caller root ok=%t err=%v", ok, err)
	}
	assertPending()
	wrongRoot := completion
	wrongRoot.Reply.ReplyToID++
	if ok, err := store.FinalizeReviewReply(ctx, wrongRoot); err != nil || ok {
		t.Fatalf("wrong root ok=%t err=%v", ok, err)
	}
	assertPending()
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_reply_evidence BEFORE INSERT ON trusted_review_reply_evidence BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.FinalizeReviewReply(ctx, completion); err == nil || ok {
		t.Fatalf("ok=%t err=%v", ok, err)
	}
	inspection, err := store.Inspect(ctx, feedback.RunID)
	if err != nil || inspection.TrustedFeedback[0].Lifecycle != domain.TrustedReviewFeedbackReplyPending || len(inspection.ReviewReplies) != 0 || inspection.SideEffects[0].Status != "intent" {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_reply_evidence`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_reply_observation BEFORE INSERT ON github_request_observations BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.FinalizeReviewReply(ctx, completion); err == nil || ok {
		t.Fatalf("ok=%t err=%v", ok, err)
	}
	assertPending()
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_reply_observation`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_reply_side_effect BEFORE UPDATE OF status ON side_effects BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.FinalizeReviewReply(ctx, completion); err == nil || ok {
		t.Fatalf("ok=%t err=%v", ok, err)
	}
	inspection, err = store.Inspect(ctx, feedback.RunID)
	if err != nil || inspection.TrustedFeedback[0].Lifecycle != domain.TrustedReviewFeedbackReplyPending || len(inspection.ReviewReplies) != 0 || inspection.SideEffects[0].Status != "intent" {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_reply_side_effect`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.FinalizeReviewReply(ctx, completion); err != nil || !ok {
		t.Fatalf("ok=%t err=%v", ok, err)
	}
	inspection, err = store.Inspect(ctx, feedback.RunID)
	if err != nil || inspection.TrustedFeedback[0].Lifecycle != domain.TrustedReviewFeedbackReplied || len(inspection.ReviewReplies) != 1 || len(inspection.GitHubRequests) != 2 || inspection.SideEffects[0].Status != "observed" {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func TestTrustedReviewFeedbackMigratesExistingRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	createFeedbackRun(t, store, "active")
	createFeedbackRun(t, store, "completed")
	if _, err := store.db.Exec(`UPDATE runs SET current_state='completed' WHERE run_id='completed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version=13`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE trusted_review_feedback_conflicts`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE trusted_review_feedback`); err != nil {
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
	for _, id := range []string{"active", "completed"} {
		if _, err := store.GetRun(context.Background(), id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestTrustedReviewFeedbackHeadBoundsRejectBeforePersistence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createFeedbackRun(t, store, "bound-run")
	base := feedbackRecord("bound-run")
	for i := 0; i < domain.MaxTrustedReviewFeedbackPerHead; i++ {
		record := base
		record.RootCommentNodeID = "COMMENT_" + string(rune('a'+i))
		record.RootCommentDatabaseID = int64(100 + i)
		if _, created, err := store.SaveTrustedReviewFeedback(ctx, record); err != nil || !created {
			t.Fatalf("index=%d created=%t err=%v", i, created, err)
		}
	}
	extra := base
	extra.RootCommentNodeID, extra.RootCommentDatabaseID = "COMMENT_over", 1000
	if _, _, err := store.SaveTrustedReviewFeedback(ctx, extra); err == nil {
		t.Fatal("more than 50 feedback items were accepted")
	}
}
