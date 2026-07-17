package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const MaxLinearCompletionObservations = 10

const (
	LinearCompletionPending   = "pending"
	LinearCompletionCompleted = "completed"
	LinearCompletionCanceled  = "canceled"
	LinearCompletionInvalid   = "invalid"
	LinearCompletionError     = "error"
	LinearCompletionTimeout   = "timeout"
)

type ProductionLinearCompletionCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionLinearCompletionResult struct {
	Action       ProductionAction `json:"action"`
	Run          RunResult        `json:"run"`
	Status       string           `json:"linear_completion_status"`
	Observations int              `json:"observation_count"`
}

type linearCompletionStore interface {
	SaveLinearCompletionObservation(context.Context, LinearCompletionObservation) error
	SaveLinearRequestObservation(context.Context, string, LinearRequestObservation) error
}

// ReconcileLinearCompletion performs exactly one bounded, read-only observation.
// It deliberately does not use admission revalidation: a successful automation
// changes Linear's source revision as part of the completion evidence.
func (c *ProductionCoordinator) ReconcileLinearCompletion(ctx context.Context, command ProductionLinearCompletionCommand) (_result ProductionLinearCompletionResult, _err error) {
	defer c.publishManualInterventionOnReturn(ctx, command.RunID, &_err)
	if command.RunID == "" || command.Repository == "" || command.ExpectedState == "" || command.IdempotencyKey == "" {
		return ProductionLinearCompletionResult{}, serviceError(ErrorInvalidInput, "run, expected state, repository, and idempotency key are required", nil)
	}
	run, err := c.store.GetRun(ctx, command.RunID)
	if err != nil {
		return ProductionLinearCompletionResult{}, classifyServiceError(err)
	}
	if run.Repository != command.Repository || run.State != command.ExpectedState || run.IdempotencyKey != command.IdempotencyKey {
		return ProductionLinearCompletionResult{}, serviceError(ErrorConflict, "run authority or state changed before Linear completion reconciliation", nil)
	}
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return ProductionLinearCompletionResult{}, err
	}
	if action, reason := productionNextAction(run.State); action != ProductionReconcileLinear {
		return ProductionLinearCompletionResult{Action: action, Run: projectRunResult(run)}, serviceError(ErrorConflict, reason, nil)
	}
	stores, ok := c.store.(linearCompletionStore)
	if !ok {
		return ProductionLinearCompletionResult{}, serviceError(ErrorInternal, "configured store cannot persist Linear completion evidence", nil)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionLinearCompletionResult{}, classifyServiceError(err)
	}
	if inspection.Merge == nil || inspection.Merge.RunID != run.ID || strings.TrimSpace(inspection.Merge.MergeSHA) == "" || inspection.Merge.MergedAt.IsZero() {
		return ProductionLinearCompletionResult{}, serviceError(ErrorConflict, "exact persisted GitHub merge evidence is required", nil)
	}
	expectedIssueID, err := persistedLinearIssueID(run)
	if err != nil {
		return c.finishLinearCompletion(ctx, run, stores, LinearCompletionObservation{RunID: run.ID, MergeSHA: inspection.Merge.MergeSHA, Identifier: run.IssueID, Status: LinearCompletionInvalid, ErrorClass: "persisted_identity", ObservedAt: time.Now().UTC()}, len(inspection.LinearCompletion)+1, "persisted Linear issue identity is invalid")
	}
	count := len(inspection.LinearCompletion)
	if count >= MaxLinearCompletionObservations {
		return c.finishLinearCompletion(ctx, run, stores, LinearCompletionObservation{RunID: run.ID, MergeSHA: inspection.Merge.MergeSHA, Identifier: run.IssueID, Status: LinearCompletionTimeout, ObservedAt: time.Now().UTC()}, count+1, "Linear completion observation limit reached")
	}
	source, requests, readErr := c.admission.reader.ReadIssue(ctx, run.IssueID)
	for _, request := range requests {
		if err := stores.SaveLinearRequestObservation(ctx, run.ID, request); err != nil {
			return ProductionLinearCompletionResult{}, classifyServiceError(err)
		}
	}
	observation := LinearCompletionObservation{RunID: run.ID, MergeSHA: inspection.Merge.MergeSHA, Identifier: run.IssueID, ObservedAt: time.Now().UTC()}
	if readErr != nil {
		observation.Status, observation.ErrorClass = LinearCompletionError, linearCompletionErrorClass(requests)
		return c.finishLinearCompletion(ctx, run, stores, observation, count+1, "Linear completion read failed; operator intervention is required")
	}
	observation.LinearIssueID, observation.SourceRevision = source.IssueID, source.SourceRevision
	observation.StateID, observation.StateName, observation.StateType = source.State.ID, source.State.Name, source.State.Type
	status, reason := classifyLinearCompletion(run, *inspection.Merge, expectedIssueID, source)
	observation.Status = status
	return c.finishLinearCompletion(ctx, run, stores, observation, count+1, reason)
}

func linearCompletionErrorClass(requests []LinearRequestObservation) string {
	for index := len(requests) - 1; index >= 0; index-- {
		if requests[index].ErrorClass != "" {
			return requests[index].ErrorClass
		}
	}
	return "read_issue"
}

func persistedLinearIssueID(run Run) (string, error) {
	source, err := sealedPersistedLinearSource(run)
	if err != nil {
		return "", err
	}
	return source.IssueID, nil
}

func classifyLinearCompletion(run Run, merge MergeRecord, expectedIssueID string, source LinearTaskSource) (string, string) {
	if source.Provider != "linear" || source.Identifier != run.IssueID || source.IssueID != expectedIssueID || source.Team.Key != "IFAN" || strings.TrimSpace(source.SourceRevision) == "" || source.UpdatedAt.IsZero() || source.ObservedAt.IsZero() || source.SourceRevision != source.UpdatedAt.UTC().Format(time.RFC3339Nano) {
		return LinearCompletionInvalid, "Linear completion identity, team, or source revision policy is invalid"
	}
	if baseline, err := time.Parse(time.RFC3339Nano, run.SourceRevision); err == nil && source.UpdatedAt.Before(baseline) {
		return LinearCompletionInvalid, "Linear completion source revision regressed"
	}
	switch strings.ToLower(strings.TrimSpace(source.State.Type)) {
	case "completed":
		if !source.UpdatedAt.After(merge.MergedAt) {
			return LinearCompletionInvalid, "Linear completed state predates the GitHub merge"
		}
		return LinearCompletionCompleted, "Linear completion observed after GitHub merge"
	case "canceled", "cancelled":
		return LinearCompletionCanceled, "Linear issue was canceled after GitHub merge"
	case "backlog", "unstarted", "started":
		return LinearCompletionPending, "Linear automation has not completed the issue yet"
	default:
		return LinearCompletionInvalid, "Linear completion state is ambiguous"
	}
}

func (c *ProductionCoordinator) finishLinearCompletion(ctx context.Context, run Run, store linearCompletionStore, observation LinearCompletionObservation, count int, reason string) (ProductionLinearCompletionResult, error) {
	to := domain.StateAwaitingLinearCompletion
	action := ProductionReconcileLinear
	switch observation.Status {
	case LinearCompletionCompleted:
		to, action = domain.StateCleaning, ProductionStop
	case LinearCompletionPending:
		if count >= MaxLinearCompletionObservations {
			observation.Status, reason = LinearCompletionTimeout, "Linear completion observation limit reached"
			to, action = domain.StateManualIntervention, ProductionStop
		}
	case LinearCompletionCanceled, LinearCompletionInvalid, LinearCompletionError, LinearCompletionTimeout:
		to, action = domain.StateManualIntervention, ProductionStop
	default:
		return ProductionLinearCompletionResult{}, serviceError(ErrorInternal, "unknown Linear completion observation status", errors.New(observation.Status))
	}
	if err := store.SaveLinearCompletionObservation(ctx, observation); err != nil {
		return ProductionLinearCompletionResult{}, classifyServiceError(err)
	}
	if to != run.State {
		if err := c.store.Transition(ctx, run.ID, run.State, to, reason, "linear_completion:"+observation.Status, run.CandidateHead); err != nil {
			return ProductionLinearCompletionResult{}, classifyServiceError(err)
		}
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionLinearCompletionResult{}, classifyServiceError(err)
	}
	return ProductionLinearCompletionResult{Action: action, Run: projectRunResult(next), Status: observation.Status, Observations: count}, nil
}
