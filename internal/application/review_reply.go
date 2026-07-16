package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ReplyToReviewCommentRequest is deliberately not a generic comment request.
// The root database ID is durable authority admitted from the original review.
type ReplyToReviewCommentRequest struct {
	PullRequestNumber int64
	RootCommentID     int64
	Body              string
	MarkerDigest      string
}

// ReviewCommentReplyPort exposes exactly the read/reply operations needed for
// restart-safe reconciliation. It cannot create PR comments or resolve threads.
type ReviewCommentReplyPort interface {
	FindReviewCommentReplies(context.Context, int64, int64) ([]domain.ReviewReply, error)
	ReplyToReviewComment(context.Context, ReplyToReviewCommentRequest) (domain.ReviewReply, error)
}

// ReviewReplyObservationPort is optional telemetry supplied by the narrowly
// scoped reply adapter. Observations are metadata only and are committed with
// the reply evidence, never with request or response bodies.
type ReviewReplyObservationPort interface {
	DrainReviewReplyObservations() []GitHubRequestObservation
}

// ReviewReplyInconclusiveError means the adapter cannot prove that a marker is
// absent (for example a bounded listing was exhausted). Retrying could create
// a duplicate, so the run must wait for human reconciliation.
type ReviewReplyInconclusiveError struct{}

func (*ReviewReplyInconclusiveError) Error() string {
	return "review reply reconciliation is inconclusive"
}

// ReviewReplyRejectedError marks an authoritative 403/404 response. It is
// intentionally message-free so adapters cannot surface remote response text.
type ReviewReplyRejectedError struct{}

func (*ReviewReplyRejectedError) Error() string { return "review reply target was rejected" }

type ProductionReplyCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionReplyResult struct {
	Action     ProductionAction `json:"action"`
	Run        RunResult        `json:"run"`
	Idempotent bool             `json:"idempotent"`
}

type reviewReplyStore interface {
	BeginReviewReplySideEffect(context.Context, string, SideEffectRecord) (SideEffectRecord, bool, error)
	FinishReviewReplySideEffect(context.Context, string, SideEffectRecord) error
	RetryReviewReplySideEffect(context.Context, string, SideEffectRecord, int) (SideEffectRecord, bool, error)
	TransitionReviewReplyFeedback(context.Context, string, string, string, domain.TrustedReviewFeedbackLifecycle, domain.TrustedReviewFeedbackLifecycle, string, string, int64, string, bool, bool) (TrustedReviewFeedbackRecord, bool, error)
	ResolveReviewReplyFeedback(context.Context, string, string, TrustedReviewFeedbackRecord, string, bool, []GitHubRequestObservation) (bool, error)
	TransitionReviewReplyRun(context.Context, string, string, domain.State, domain.State, string, string, string) error
	SaveReviewReplyEvidence(context.Context, ReviewReplyEvidence) error
	FinalizeReviewReply(context.Context, ReviewReplyCompletion) (bool, error)
}

const maxReviewReplyPostAttempts = 3

type reviewReplyObservationStore interface {
	SaveReviewReplyObservations(context.Context, string, string, []GitHubRequestObservation) error
}

// ReviewReplyCompletion is committed atomically after GitHub accepted a reply.
// It joins feedback lifecycle, immutable evidence, and side-effect observation.
type ReviewReplyCompletion struct {
	Feedback     TrustedReviewFeedbackRecord
	Head         string
	Reply        domain.ReviewReply
	Side         SideEffectRecord
	Observations []GitHubRequestObservation
	LeaseOwner   string
}

// ReplyReviewFeedback performs at most one root-comment action. It always
// re-reads remote replies before posting and persists intent before that post.
func (c *ProductionCoordinator) ReplyReviewFeedback(ctx context.Context, command ProductionReplyCommand, validator ApprovalValidator, reader GitHubReadPort, replies ReviewCommentReplyPort) (_result ProductionReplyResult, _err error) {
	defer c.publishManualInterventionOnReturn(ctx, command.RunID, &_err)
	if validator == nil || reader == nil || replies == nil {
		return ProductionReplyResult{}, serviceError(ErrorInvalidInput, "approval validator, GitHub reader, and review reply port are required", nil)
	}
	run, err := c.admission.Revalidate(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionReplyResult{}, err
	}
	if run.State != domain.StateReplyingReviewFeedback {
		action, reason := productionNextAction(run.State)
		return ProductionReplyResult{Action: action, Run: projectRunResult(run)}, serviceError(ErrorConflict, reason, nil)
	}
	owner, err := randomIdentifier("reply-")
	if err != nil {
		return ProductionReplyResult{}, classifyServiceError(err)
	}
	acquired, err := c.store.AcquireLease(ctx, run.ID, owner, time.Now().UTC().Add(reconcileLeaseTTL))
	if err != nil {
		return ProductionReplyResult{}, classifyServiceError(err)
	}
	if !acquired {
		return ProductionReplyResult{}, serviceError(ErrorConflict, "review reply is already leased", nil)
	}
	leaseCtx, cancelLease := context.WithCancelCause(ctx)
	stopLease := make(chan struct{})
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(reconcileLeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopLease:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				ok, renewErr := c.store.RenewLease(context.Background(), run.ID, owner, time.Now().UTC().Add(reconcileLeaseTTL))
				if renewErr != nil {
					cancelLease(fmt.Errorf("renew review reply lease: %w", renewErr))
					return
				}
				if !ok {
					cancelLease(errors.New("review reply lease ownership was lost"))
					return
				}
			}
		}
	}()
	defer func() {
		close(stopLease)
		cancelLease(nil)
		<-leaseDone
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.store.ReleaseLease(releaseCtx, run.ID, owner)
	}()
	ctx = leaseCtx
	if err := validator.ValidateApprovalReady(ctx, run.ID); err != nil {
		return ProductionReplyResult{}, serviceError(ErrorConflict, "exact-HEAD repair evidence is no longer valid", err)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionReplyResult{}, classifyServiceError(err)
	}
	if inspection.PullRequest == nil || inspection.RepositoryBinding == nil {
		return c.replyManual(ctx, run, owner, "review reply authority is incomplete")
	}
	if repliedReviewEvidenceMissing(inspection.TrustedFeedback, inspection.ReviewReplies) {
		return c.replyManual(ctx, run, owner, "replied review feedback lacks immutable evidence")
	}
	feedback := pendingReviewReply(inspectReplyFeedback(inspection.TrustedFeedback), run.CandidateHead)
	if feedback == nil {
		if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
			return ProductionReplyResult{}, err
		}
		store, ok := c.store.(reviewReplyStore)
		if !ok {
			return ProductionReplyResult{}, serviceError(ErrorInternal, "review reply persistence is unavailable", nil)
		}
		if err := store.TransitionReviewReplyRun(ctx, run.ID, owner, domain.StateReplyingReviewFeedback, domain.StateAwaitingHumanApproval, "all verified review replies are reconciled", "", run.CandidateHead); err != nil {
			return ProductionReplyResult{}, classifyServiceError(err)
		}
		updated, err := c.store.GetRun(ctx, run.ID)
		if err != nil {
			return ProductionReplyResult{}, classifyServiceError(err)
		}
		return ProductionReplyResult{Action: ProductionReconcileGitHub, Run: projectRunResult(updated), Idempotent: true}, nil
	}
	if feedback.PRNumber != inspection.PullRequest.Number || feedback.PRDatabaseID != inspection.PullRequest.DatabaseID || feedback.PRNodeID != inspection.PullRequest.NodeID || feedback.BoundRepairHead != run.CandidateHead {
		return c.replyManual(ctx, run, owner, "review reply target authority drifted")
	}
	if err := validateReviewReplyAuthority(run, inspection, reader.Authority()); err != nil {
		return c.replyManual(ctx, run, owner, "review reply credential authority drifted")
	}
	evidence, handoff, observations, metadata, err := reader.Read(ctx, feedback.PRNumber, run.CandidateHead)
	if err != nil {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return ProductionReplyResult{}, serviceError(ErrorUnavailable, "review reply authority read is unavailable", err)
	}
	if err := validateReviewReplyAuthority(run, inspection, metadata); err != nil {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply metadata authority drifted")
	}
	if evidence.PullRequest.Number != feedback.PRNumber || evidence.PullRequest.DatabaseID != feedback.PRDatabaseID || evidence.PullRequest.NodeID != feedback.PRNodeID || evidence.PullRequest.HeadSHA != run.CandidateHead || evidence.RequiredChecksStatus() != domain.ReconciliationPass {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply pull request authority drifted")
	}
	if resolved, outdated, valid := replyTargetStatus(evidence.ReviewThreads, handoff, *feedback); !valid {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply root authority drifted")
	} else if resolved {
		store, ok := c.store.(reviewReplyStore)
		if !ok {
			return ProductionReplyResult{}, serviceError(ErrorInternal, "review reply persistence is unavailable", nil)
		}
		if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
			return ProductionReplyResult{}, err
		}
		if changed, err := store.ResolveReviewReplyFeedback(ctx, run.ID, owner, *feedback, run.CandidateHead, outdated, observations); err != nil || !changed {
			return c.replyManual(ctx, run, owner, "resolved review feedback persistence conflict")
		}
		return ProductionReplyResult{Action: ProductionReplyReviewFeedback, Run: projectRunResult(run), Idempotent: true}, nil
	}
	marker, digest, err := domain.ReviewReplyMarker(run.ID, feedback.PRNumber, feedback.ThreadNodeID, feedback.RootCommentDatabaseID, feedback.RootCommentNodeID, feedback.BodyDigest, run.CandidateHead)
	if err != nil {
		return c.replyManual(ctx, run, owner, "review reply marker authority is invalid")
	}
	body, err := domain.ReviewReplyBody(run.CandidateHead, marker)
	if err != nil {
		return ProductionReplyResult{}, serviceError(ErrorInternal, "controller review reply construction failed", err)
	}
	store, ok := c.store.(reviewReplyStore)
	if !ok {
		return ProductionReplyResult{}, serviceError(ErrorInternal, "review reply persistence is unavailable", nil)
	}
	intentJSON, _ := json.Marshal(struct {
		PR     int64  `json:"pull_request"`
		Root   int64  `json:"root_comment"`
		Head   string `json:"head"`
		Marker string `json:"marker_digest"`
	}{feedback.PRNumber, feedback.RootCommentDatabaseID, run.CandidateHead, digest})
	if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
		return ProductionReplyResult{}, err
	}
	side, _, err := store.BeginReviewReplySideEffect(ctx, owner, SideEffectRecord{RunID: run.ID, Kind: "reply_to_review_comment", IdempotencyKey: digest, IntentJSON: string(intentJSON), Attempt: 1})
	if err != nil {
		return ProductionReplyResult{}, classifyServiceError(err)
	}
	if feedback.Lifecycle == domain.TrustedReviewFeedbackRepairVerified {
		if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
			return ProductionReplyResult{}, err
		}
		if _, changed, err := store.TransitionReviewReplyFeedback(ctx, run.ID, owner, feedback.RootCommentNodeID, domain.TrustedReviewFeedbackRepairVerified, domain.TrustedReviewFeedbackReplyPending, run.CandidateHead, digest, 0, "", false, false); err != nil || !changed {
			return c.replyManual(ctx, run, owner, "review reply intent lifecycle conflict")
		}
	}
	if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
		return ProductionReplyResult{}, err
	}
	observed, err := replies.FindReviewCommentReplies(ctx, feedback.PRNumber, feedback.RootCommentDatabaseID)
	observations = append(observations, drainReviewReplyObservations(replies)...)
	var rejected *ReviewReplyRejectedError
	var inconclusive *ReviewReplyInconclusiveError
	if errors.As(err, &rejected) {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply reconciliation was rejected")
	}
	if errors.As(err, &inconclusive) {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply reconciliation is inconclusive")
	}
	if err != nil {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyFailure(ctx, owner, store, side, "reply_reconciliation_unavailable", err)
	}
	match, conflict := matchingReply(observed, digest, feedback.RootCommentDatabaseID, inspection.RepositoryBinding.GitHubAppID)
	if conflict {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply reconciliation is ambiguous")
	}
	if match != nil {
		return c.completeReviewReply(ctx, run, owner, store, side, *feedback, *match, observations, true)
	}
	if side.Status == "failed" {
		if side.Attempt >= maxReviewReplyPostAttempts {
			if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
				return ProductionReplyResult{}, auditErr
			}
			return c.replyManual(ctx, run, owner, "review reply retry budget exhausted")
		}
		if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
			return ProductionReplyResult{}, err
		}
		var retried bool
		side, retried, err = store.RetryReviewReplySideEffect(ctx, owner, side, maxReviewReplyPostAttempts)
		if err != nil || !retried {
			return c.replyManual(ctx, run, owner, "review reply retry intent conflict")
		}
	}
	if side.Status != "intent" {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply side effect state is not retryable")
	}
	if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
		return ProductionReplyResult{}, err
	}
	created, err := replies.ReplyToReviewComment(ctx, ReplyToReviewCommentRequest{PullRequestNumber: feedback.PRNumber, RootCommentID: feedback.RootCommentDatabaseID, Body: body, MarkerDigest: digest})
	observations = append(observations, drainReviewReplyObservations(replies)...)
	if errors.As(err, &rejected) {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply post was rejected")
	}
	if err != nil {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyFailure(ctx, owner, store, side, "reply_post_ambiguous", err)
	}
	if created.ReplyToID != feedback.RootCommentDatabaseID || created.DatabaseID < 1 || created.NodeID == "" || created.MarkerDigest != digest || created.Actor.AppID != inspection.RepositoryBinding.GitHubAppID {
		if auditErr := c.persistReplyObservations(ctx, run.ID, owner, observations); auditErr != nil {
			return ProductionReplyResult{}, auditErr
		}
		return c.replyManual(ctx, run, owner, "review reply response authority mismatch")
	}
	return c.completeReviewReply(ctx, run, owner, store, side, *feedback, created, observations, false)
}

func (c *ProductionCoordinator) ensureReplyLease(ctx context.Context, runID, owner string) error {
	if cause := context.Cause(ctx); cause != nil {
		return serviceError(ErrorConflict, "review reply lease ownership was lost", cause)
	}
	ok, err := c.store.RenewLease(ctx, runID, owner, time.Now().UTC().Add(reconcileLeaseTTL))
	if err != nil {
		return classifyServiceError(err)
	}
	if !ok {
		return serviceError(ErrorConflict, "review reply lease ownership was lost", nil)
	}
	return nil
}

func (c *ProductionCoordinator) persistReplyObservations(ctx context.Context, runID, owner string, observations []GitHubRequestObservation) error {
	if len(observations) == 0 {
		return nil
	}
	store, ok := c.store.(reviewReplyObservationStore)
	if !ok {
		return serviceError(ErrorInternal, "review reply observation persistence is unavailable", nil)
	}
	if err := c.ensureReplyLease(ctx, runID, owner); err != nil {
		return err
	}
	if err := store.SaveReviewReplyObservations(ctx, runID, owner, observations); err != nil {
		return classifyServiceError(err)
	}
	return nil
}

func drainReviewReplyObservations(port ReviewCommentReplyPort) []GitHubRequestObservation {
	if observed, ok := port.(ReviewReplyObservationPort); ok {
		return observed.DrainReviewReplyObservations()
	}
	return nil
}

// validateReviewReplyAuthority binds the persisted repository profile and the
// configured GitHub App metadata to the same immutable repository binding
// before any reply-list read or reply POST can be attempted.
func validateReviewReplyAuthority(run Run, inspection RunInspection, metadata GitHubInstallationMetadata) error {
	if inspection.RepositoryBinding == nil {
		return errors.New("repository binding is missing")
	}
	var profile LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &profile); err != nil || profile.CanonicalRepository != run.Repository || profile.GitHubAppID != inspection.RepositoryBinding.GitHubAppID || profile.GitHubInstallationID != inspection.RepositoryBinding.GitHubInstallationID || profile.ExpectedRepositoryID != inspection.RepositoryBinding.ExpectedRepositoryID {
		return errors.New("persisted GitHub App profile mismatches binding")
	}
	return validateReaderAuthority(inspection, metadata)
}

func inspectReplyFeedback(items []TrustedReviewFeedbackRecord) []TrustedReviewFeedbackRecord {
	result := append([]TrustedReviewFeedbackRecord(nil), items...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].RootCommentDatabaseID != result[j].RootCommentDatabaseID {
			return result[i].RootCommentDatabaseID < result[j].RootCommentDatabaseID
		}
		return result[i].RootCommentNodeID < result[j].RootCommentNodeID
	})
	return result
}

func pendingReviewReply(items []TrustedReviewFeedbackRecord, head string) *TrustedReviewFeedbackRecord {
	for i := range items {
		if (items[i].Lifecycle == domain.TrustedReviewFeedbackRepairVerified || items[i].Lifecycle == domain.TrustedReviewFeedbackReplyPending) && !items[i].Resolved && !items[i].Outdated && items[i].BoundRepairHead == head {
			return &items[i]
		}
	}
	return nil
}

func repliedReviewEvidenceMissing(feedback []TrustedReviewFeedbackRecord, evidence []ReviewReplyEvidence) bool {
	byRoot := make(map[string]ReviewReplyEvidence, len(evidence))
	for _, item := range evidence {
		byRoot[item.RootCommentNodeID] = item
	}
	for _, item := range feedback {
		if item.Lifecycle != domain.TrustedReviewFeedbackReplied {
			continue
		}
		observed, found := byRoot[item.RootCommentNodeID]
		if !found || observed.PullRequestNumber != item.PRNumber || observed.RootCommentID != item.RootCommentDatabaseID || observed.RepairedHead != item.BoundRepairHead || observed.MarkerDigest != item.ReplyIntentKey || observed.ReplyDatabaseID != item.ReplyDatabaseID || observed.ReplyNodeID != item.ReplyNodeID {
			return true
		}
	}
	return false
}

func matchingReply(items []domain.ReviewReply, marker string, root, appID int64) (*domain.ReviewReply, bool) {
	var found *domain.ReviewReply
	for i := range items {
		item := &items[i]
		if item.MarkerDigest != marker {
			continue
		}
		if item.ReplyToID != root || item.Actor.AppID != appID {
			return nil, true
		}
		if found != nil {
			return nil, true
		}
		found = item
	}
	return found, false
}

func replyTargetStatus(threads []domain.GitHubReviewThread, bodies domain.InlineReviewBodyHandoff, feedback TrustedReviewFeedbackRecord) (bool, bool, bool) {
	if err := bodies.Validate(); err != nil {
		return false, false, false
	}
	bodyByID := make(map[string]domain.InlineReviewBody, len(bodies.Comments))
	for _, body := range bodies.Comments {
		bodyByID[body.CommentNodeID] = body
	}
	for _, thread := range threads {
		if thread.NodeID != feedback.ThreadNodeID {
			continue
		}
		if thread.OriginalCommitSHA != feedback.OriginalReviewHeadSHA || thread.Path != feedback.Path || !sameReplyLocationLine(thread.Line, feedback.Line, thread.Outdated) {
			return false, false, false
		}
		for _, comment := range thread.Comments {
			if comment.NodeID != feedback.RootCommentNodeID || comment.DatabaseID != feedback.RootCommentDatabaseID {
				continue
			}
			body, found := bodyByID[comment.NodeID]
			if !found || body.ThreadNodeID != feedback.ThreadNodeID || body.BodyDigest != feedback.BodyDigest || comment.BodyDigest != feedback.BodyDigest || comment.ReplyToDatabaseID != 0 || comment.ReplyToNodeID != "" || !sameReplyActor(comment.Author, feedback.Author) || comment.Review.DatabaseID != feedback.ReviewDatabaseID || comment.Review.NodeID != feedback.ReviewNodeID || comment.Review.State != "CHANGES_REQUESTED" || comment.Review.CommitSHA != feedback.OriginalReviewHeadSHA || !sameReplyActor(&comment.Review.Actor, feedback.Author) {
				return false, false, false
			}
			// A repair necessarily changes the PR head. GitHub may consequently
			// mark this original review thread outdated; that is not identity drift.
			return thread.Resolved, thread.Outdated, true
		}
	}
	return false, false, false
}

func sameReplyActor(observed *domain.ActorIdentity, expected domain.ActorIdentity) bool {
	return observed != nil && observed.Type == "User" && expected.Type == "User" && observed.DatabaseID == expected.DatabaseID && observed.NodeID == expected.NodeID && strings.EqualFold(observed.Login, expected.Login)
}

func sameReplyLine(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameReplyLocationLine(observed, expected *int, outdated bool) bool {
	return sameReplyLine(observed, expected) || (outdated && observed == nil && expected != nil)
}

func (c *ProductionCoordinator) completeReviewReply(ctx context.Context, run Run, owner string, store reviewReplyStore, side SideEffectRecord, feedback TrustedReviewFeedbackRecord, reply domain.ReviewReply, observations []GitHubRequestObservation, idempotent bool) (ProductionReplyResult, error) {
	if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
		return ProductionReplyResult{}, err
	}
	completed, err := store.FinalizeReviewReply(ctx, ReviewReplyCompletion{Feedback: feedback, Head: run.CandidateHead, Reply: reply, Side: side, Observations: observations, LeaseOwner: owner})
	if err != nil || !completed {
		return c.replyManual(ctx, run, owner, "review reply persistence conflict")
	}
	updated, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionReplyResult{}, classifyServiceError(err)
	}
	return ProductionReplyResult{Action: ProductionReplyReviewFeedback, Run: projectRunResult(updated), Idempotent: idempotent}, nil
}

func (c *ProductionCoordinator) replyFailure(ctx context.Context, owner string, store reviewReplyStore, side SideEffectRecord, _ string, cause error) (ProductionReplyResult, error) {
	side.Status = "failed"
	_ = store.FinishReviewReplySideEffect(ctx, owner, side)
	return ProductionReplyResult{}, serviceError(ErrorUnavailable, "review reply outcome requires reconciliation", cause)
}

func (c *ProductionCoordinator) replyManual(ctx context.Context, run Run, owner, reason string) (ProductionReplyResult, error) {
	if err := c.ensureReplyLease(ctx, run.ID, owner); err != nil {
		return ProductionReplyResult{}, err
	}
	store, ok := c.store.(reviewReplyStore)
	if !ok {
		return ProductionReplyResult{}, serviceError(ErrorInternal, "review reply persistence is unavailable", nil)
	}
	if err := store.TransitionReviewReplyRun(ctx, run.ID, owner, run.State, domain.StateManualIntervention, reason, "", run.CandidateHead); err != nil {
		return ProductionReplyResult{}, classifyServiceError(err)
	}
	updated, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionReplyResult{}, classifyServiceError(err)
	}
	return ProductionReplyResult{Action: ProductionStop, Run: projectRunResult(updated)}, serviceError(ErrorConflict, "review reply requires manual intervention", errors.New(reason))
}
