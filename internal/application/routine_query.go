package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	RoutineQuerySchemaVersion = "v1"
	RoutineQueryDefaultLimit  = 25
	RoutineQueryMaximumLimit  = 100
	RoutineOverviewItemLimit  = 10
)

type RoutineProjectionMetadata struct {
	SchemaVersion string    `json:"schema_version"`
	ObservedAt    time.Time `json:"observed_at"`
	Digest        string    `json:"digest"`
}

type RoutineCollectionMetadata struct {
	Total      int    `json:"total"`
	Truncated  bool   `json:"truncated"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type AggregateReadiness string

const (
	AggregateReady             AggregateReadiness = "ready"
	AggregateDegraded          AggregateReadiness = "degraded"
	AggregateAttentionRequired AggregateReadiness = "attention_required"
	AggregateRestartRequired   AggregateReadiness = "restart_required"
	AggregateStale             AggregateReadiness = "stale"
	AggregateOffline           AggregateReadiness = "offline"
	AggregateUnknown           AggregateReadiness = "unknown"
	AggregateConflict          AggregateReadiness = "conflict"
)

var aggregateReadinessPrecedence = []AggregateReadiness{
	AggregateConflict,
	AggregateUnknown,
	AggregateAttentionRequired,
	AggregateRestartRequired,
	AggregateOffline,
	AggregateStale,
	AggregateDegraded,
	AggregateReady,
}

// ClassifyAggregateReadiness applies Controller-owned precedence. Capacity
// saturation and repository readiness counts are deliberately not inputs.
func ClassifyAggregateReadiness(states ...AggregateReadiness) AggregateReadiness {
	if len(states) == 0 {
		return AggregateUnknown
	}
	for _, candidate := range aggregateReadinessPrecedence {
		if slices.Contains(states, candidate) {
			return candidate
		}
	}
	return AggregateUnknown
}

type RoutineRunSummary struct {
	RunID            string       `json:"run_id"`
	LinearIdentifier string       `json:"linear_identifier"`
	Repository       string       `json:"repository"`
	State            domain.State `json:"state"`
	CandidateHead    string       `json:"candidate_head,omitempty"`
	Attention        bool         `json:"attention"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
}

type RoutineRunPage struct {
	Metadata   RoutineProjectionMetadata `json:"metadata"`
	Collection RoutineCollectionMetadata `json:"collection"`
	Repository string                    `json:"repository,omitempty"`
	Lifecycle  RunLifecycleFilter        `json:"lifecycle"`
	Runs       []RoutineRunSummary       `json:"runs"`
}

type RoutineTransition struct {
	From       domain.State `json:"from_state"`
	To         domain.State `json:"to_state"`
	ReasonCode string       `json:"reason_code"`
	BoundHead  string       `json:"bound_head,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
}

type RoutineWaitClassification string

const (
	RoutineWaitNone             RoutineWaitClassification = "none"
	RoutineWaitHumanDecision    RoutineWaitClassification = "human_decision"
	RoutineWaitHumanApproval    RoutineWaitClassification = "human_approval"
	RoutineWaitExternalChecks   RoutineWaitClassification = "external_checks"
	RoutineWaitMergeability     RoutineWaitClassification = "mergeability"
	RoutineWaitLinearCompletion RoutineWaitClassification = "linear_completion"
	RoutineWaitManual           RoutineWaitClassification = "manual_intervention"
	RoutineWaitTerminal         RoutineWaitClassification = "terminal"
	RoutineWaitUnknown          RoutineWaitClassification = "unknown"
)

func ClassifyRoutineWait(state domain.State) RoutineWaitClassification {
	switch state {
	case domain.StateAwaitingHumanDecision:
		return RoutineWaitHumanDecision
	case domain.StateAwaitingHumanApproval:
		return RoutineWaitHumanApproval
	case domain.StatePROpen, domain.StateReconcilingReviews, domain.StateReplyingReviewFeedback:
		return RoutineWaitExternalChecks
	case domain.StateAwaitingGitHubMergeability:
		return RoutineWaitMergeability
	case domain.StateAwaitingLinearCompletion:
		return RoutineWaitLinearCompletion
	case domain.StateManualIntervention:
		return RoutineWaitManual
	case domain.StateCompleted, domain.StateFailed, domain.StateRejected:
		return RoutineWaitTerminal
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting,
		domain.StateVerifying, domain.StateFreshReview, domain.StateApprovalReady,
		domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR,
		domain.StateRepairing, domain.StateMerging, domain.StateCleaning:
		return RoutineWaitNone
	default:
		return RoutineWaitUnknown
	}
}

type RoutineRunPhase string

const (
	RoutinePhaseAdmission        RoutineRunPhase = "admission"
	RoutinePhaseWorkspace        RoutineRunPhase = "workspace"
	RoutinePhaseImplementation   RoutineRunPhase = "implementation"
	RoutinePhaseVerification     RoutineRunPhase = "verification"
	RoutinePhaseReview           RoutineRunPhase = "review"
	RoutinePhasePublication      RoutineRunPhase = "publication"
	RoutinePhasePullRequest      RoutineRunPhase = "pull_request"
	RoutinePhaseApproval         RoutineRunPhase = "approval"
	RoutinePhaseMerge            RoutineRunPhase = "merge"
	RoutinePhaseLinearCompletion RoutineRunPhase = "linear_completion"
	RoutinePhaseCleanup          RoutineRunPhase = "cleanup"
	RoutinePhaseManual           RoutineRunPhase = "manual_intervention"
	RoutinePhaseEnded            RoutineRunPhase = "ended"
	RoutinePhaseUnknown          RoutineRunPhase = "unknown"
)

func ClassifyRoutineRunPhase(state domain.State) RoutineRunPhase {
	switch state {
	case domain.StateReceived, domain.StateAdmitting:
		return RoutinePhaseAdmission
	case domain.StateProvisioning:
		return RoutinePhaseWorkspace
	case domain.StateExecuting, domain.StateAwaitingHumanDecision, domain.StateRepairing:
		return RoutinePhaseImplementation
	case domain.StateVerifying:
		return RoutinePhaseVerification
	case domain.StateFreshReview:
		return RoutinePhaseReview
	case domain.StateApprovalReady, domain.StatePushingBranch, domain.StateBranchPushed:
		return RoutinePhasePublication
	case domain.StateOpeningPR, domain.StatePROpen, domain.StateReconcilingReviews, domain.StateReplyingReviewFeedback:
		return RoutinePhasePullRequest
	case domain.StateAwaitingHumanApproval:
		return RoutinePhaseApproval
	case domain.StateMerging, domain.StateAwaitingGitHubMergeability:
		return RoutinePhaseMerge
	case domain.StateAwaitingLinearCompletion:
		return RoutinePhaseLinearCompletion
	case domain.StateCleaning:
		return RoutinePhaseCleanup
	case domain.StateManualIntervention:
		return RoutinePhaseManual
	case domain.StateCompleted, domain.StateFailed, domain.StateRejected:
		return RoutinePhaseEnded
	default:
		return RoutinePhaseUnknown
	}
}

type RoutineWaitAssessment string

const (
	RoutineAssessmentProgressing  RoutineWaitAssessment = "progressing"
	RoutineAssessmentNormalWait   RoutineWaitAssessment = "normal_wait"
	RoutineAssessmentAbnormalWait RoutineWaitAssessment = "abnormal_wait"
	RoutineAssessmentUnknown      RoutineWaitAssessment = "unknown"
	RoutineAssessmentConflict     RoutineWaitAssessment = "conflict"
	RoutineAssessmentEnded        RoutineWaitAssessment = "ended"
)

type DeliveryGateName string

const (
	GateVerification        DeliveryGateName = "verification"
	GateIndependentReview   DeliveryGateName = "independent_review"
	GateBranchPublication   DeliveryGateName = "branch_publication"
	GatePullRequest         DeliveryGateName = "pull_request"
	GateRequiredChecks      DeliveryGateName = "required_checks"
	GateReviewConversations DeliveryGateName = "review_conversations"
	GateHumanApproval       DeliveryGateName = "human_approval"
	GateMerge               DeliveryGateName = "merge"
	GateLinearCompletion    DeliveryGateName = "linear_completion"
	GateSourceCheckout      DeliveryGateName = "source_checkout"
	GateCleanup             DeliveryGateName = "cleanup"
)

var routineDeliveryGateOrder = []DeliveryGateName{
	GateVerification,
	GateIndependentReview,
	GateBranchPublication,
	GatePullRequest,
	GateRequiredChecks,
	GateReviewConversations,
	GateHumanApproval,
	GateMerge,
	GateLinearCompletion,
	GateSourceCheckout,
	GateCleanup,
}

type DeliveryGateStatus string

const (
	GatePending       DeliveryGateStatus = "pending"
	GateRunning       DeliveryGateStatus = "running"
	GatePassed        DeliveryGateStatus = "passed"
	GateFailed        DeliveryGateStatus = "failed"
	GateBlocked       DeliveryGateStatus = "blocked"
	GateUnknown       DeliveryGateStatus = "unknown"
	GateConflict      DeliveryGateStatus = "conflict"
	GateNotApplicable DeliveryGateStatus = "not_applicable"
)

type RoutineDeliveryGate struct {
	Name              DeliveryGateName   `json:"name"`
	Status            DeliveryGateStatus `json:"status"`
	ReasonCode        string             `json:"reason_code"`
	BoundHead         string             `json:"bound_head,omitempty"`
	ObservedAt        *time.Time         `json:"observed_at,omitempty"`
	EvidenceCount     int                `json:"evidence_count"`
	EvidenceTruncated bool               `json:"evidence_truncated"`
}

type RoutinePullRequestSummary struct {
	Number     int64      `json:"number"`
	State      string     `json:"state"`
	HeadSHA    string     `json:"head_sha"`
	Merged     bool       `json:"merged"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

type RoutineAttentionState string

const (
	RoutineAttentionActive   RoutineAttentionState = "active"
	RoutineAttentionUnknown  RoutineAttentionState = "unknown"
	RoutineAttentionConflict RoutineAttentionState = "conflict"
)

type RoutineAttentionSummary struct {
	EventID    string                `json:"event_id"`
	Scope      AuthorityScopeKind    `json:"scope"`
	TargetID   string                `json:"target_id"`
	Severity   string                `json:"severity"`
	ReasonCode string                `json:"reason_code"`
	State      RoutineAttentionState `json:"state"`
	OccurredAt time.Time             `json:"occurred_at"`
	ObservedAt time.Time             `json:"observed_at"`
}

type RoutineDecisionOption struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type RoutineDecisionRequest struct {
	Question       string                  `json:"question"`
	Context        string                  `json:"context"`
	Options        []RoutineDecisionOption `json:"options"`
	Recommendation string                  `json:"recommendation"`
	BlockingReason string                  `json:"blocking_reason"`
	ContentTrust   string                  `json:"content_trust"`
}

type RoutineRunDetail struct {
	Metadata         RoutineProjectionMetadata  `json:"metadata"`
	Run              RoutineRunSummary          `json:"run"`
	LatestTransition *RoutineTransition         `json:"latest_transition,omitempty"`
	Phase            RoutineRunPhase            `json:"phase"`
	Wait             RoutineWaitClassification  `json:"wait"`
	WaitAssessment   RoutineWaitAssessment      `json:"wait_assessment"`
	PullRequest      *RoutinePullRequestSummary `json:"pull_request,omitempty"`
	Gates            []RoutineDeliveryGate      `json:"gates"`
	Attention        []RoutineAttentionSummary  `json:"active_attention"`
	Offers           []LegalActionOffer         `json:"legal_action_offers"`
	Decision         *RoutineDecisionRequest    `json:"decision_request,omitempty"`
}

type RoutineRunQueryService struct {
	queries QueryService
	store   QueryStore
	actions *LegalActionService
}

func NewRoutineRunQueryService(store QueryStore, authorizer *AuthorizationService, repositories RepositoryAuthoritySource) (*RoutineRunQueryService, error) {
	queries, err := NewScopedQueryService(store, authorizer, repositories)
	if err != nil {
		return nil, err
	}
	actionStore, ok := store.(LegalActionStore)
	if !ok {
		return nil, errors.New("routine run query requires current legal-action authority")
	}
	actions, err := NewLegalActionService(actionStore, authorizer)
	if err != nil {
		return nil, err
	}
	return &RoutineRunQueryService{queries: queries, store: store, actions: actions}, nil
}

func (s *RoutineRunQueryService) List(ctx context.Context, query RunSummaryQuery, observedAt time.Time) (RoutineRunPage, error) {
	if s == nil {
		return RoutineRunPage{}, serviceError(ErrorInternal, "routine run query is unavailable", nil)
	}
	page, err := s.queries.ListRunSummaries(ctx, query)
	if err != nil {
		return RoutineRunPage{}, err
	}
	result := RoutineRunPage{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, Collection: RoutineCollectionMetadata{Total: page.TotalCount, Truncated: page.HasMore, NextCursor: page.NextCursor}, Repository: page.Repository, Lifecycle: page.Lifecycle, Runs: make([]RoutineRunSummary, 0, len(page.Runs))}
	for _, run := range page.Runs {
		attention := false
		if current, ok := s.store.(CurrentOperatorAttentionQuery); ok {
			var event OperatorAttentionEvent
			event, attention, err = current.CurrentOperatorAttention(ctx, run.RunID)
			if err != nil {
				return RoutineRunPage{}, classifyServiceError(err)
			}
			if attention {
				inspection, inspectErr := s.store.Inspect(ctx, run.RunID)
				if inspectErr != nil {
					return RoutineRunPage{}, classifyServiceError(inspectErr)
				}
				attention = classifyRoutineAttention(inspection, event) != ""
			}
		}
		result.Runs = append(result.Runs, RoutineRunSummary{RunID: run.RunID, LinearIdentifier: run.IssueID, Repository: run.Repository, State: run.State, CandidateHead: run.CandidateHead, Attention: attention, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt})
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func (s *RoutineRunQueryService) Detail(ctx context.Context, query RunDetailQuery, observedAt time.Time) (RoutineRunDetail, error) {
	if s == nil || strings.TrimSpace(query.RunID) == "" {
		return RoutineRunDetail{}, serviceError(ErrorInvalidInput, "run is required", nil)
	}
	run, err := s.queries.ResolveRunTarget(ctx, QueryInput{Requester: query.Requester, RunID: query.RunID})
	if err != nil {
		return RoutineRunDetail{}, err
	}
	inspection, err := s.store.Inspect(ctx, run.ID)
	if err != nil {
		return RoutineRunDetail{}, classifyServiceError(err)
	}
	if inspection.Run.ID != run.ID || inspection.Run.RepositoryBindingDigest != run.RepositoryBindingDigest {
		return RoutineRunDetail{}, serviceError(ErrorInternal, "routine run evidence conflicts", nil)
	}
	offers, err := s.actions.ListLegalActionOffers(ctx, LegalActionOfferQuery{Requester: query.Requester, RunID: run.ID})
	if err != nil {
		return RoutineRunDetail{}, err
	}
	result := projectRoutineRunDetail(inspection, offers, observedAt.UTC())
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func projectRoutineRunDetail(inspection RunInspection, offers []LegalActionOffer, observedAt time.Time) RoutineRunDetail {
	run := inspection.Run
	result := RoutineRunDetail{
		Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt},
		Run:      RoutineRunSummary{RunID: run.ID, LinearIdentifier: run.IssueID, Repository: run.Repository, State: run.State, CandidateHead: run.CandidateHead, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt},
		Phase:    ClassifyRoutineRunPhase(run.State),
		Wait:     ClassifyRoutineWait(run.State),
		Gates:    ClassifyRoutineDeliveryGates(inspection),
		Offers:   append([]LegalActionOffer(nil), offers...),
	}
	if len(inspection.Timeline) != 0 {
		latest := inspection.Timeline[len(inspection.Timeline)-1]
		result.LatestTransition = &RoutineTransition{From: latest.From, To: latest.To, ReasonCode: routineTransitionReason(latest), BoundHead: latest.BoundHead, ObservedAt: latest.CreatedAt.UTC()}
	}
	if inspection.PullRequest != nil {
		pr := inspection.PullRequest
		result.PullRequest = &RoutinePullRequestSummary{Number: pr.Number, State: strings.ToLower(pr.State), HeadSHA: pr.HeadSHA, Merged: pr.Merged}
		if !pr.MergedAt.IsZero() {
			mergedAt := pr.MergedAt.UTC()
			result.PullRequest.ObservedAt = &mergedAt
		} else if inspection.GitHubEvidence != nil && inspection.GitHubEvidence.PullRequest.Number == pr.Number && inspection.GitHubEvidence.PullRequest.HeadSHA == pr.HeadSHA && !inspection.GitHubEvidence.ObservedAt.IsZero() {
			observed := inspection.GitHubEvidence.ObservedAt.UTC()
			result.PullRequest.ObservedAt = &observed
		}
	}
	for _, event := range inspection.OperatorAttention {
		state := classifyRoutineAttention(inspection, event)
		if state == "" {
			continue
		}
		result.Attention = append(result.Attention, RoutineAttentionSummary{EventID: event.EventKey, Scope: routineAttentionScope(event), TargetID: routineAttentionTarget(event), Severity: event.Severity, ReasonCode: event.ReasonCode, State: state, OccurredAt: event.OccurredAt.UTC(), ObservedAt: event.ObservedAt.UTC()})
	}
	result.Run.Attention = len(result.Attention) != 0
	result.WaitAssessment = classifyRoutineWaitAssessment(result.Wait, result.Attention, result.Gates)
	if run.State == domain.StateAwaitingHumanDecision && slices.ContainsFunc(offers, func(offer LegalActionOffer) bool { return offer.Action == OperationDecide }) {
		result.Decision = projectRoutineDecisionRequest(inspection)
	}
	return result
}

func classifyRoutineWaitAssessment(wait RoutineWaitClassification, attention []RoutineAttentionSummary, gates []RoutineDeliveryGate) RoutineWaitAssessment {
	if wait == RoutineWaitTerminal {
		return RoutineAssessmentEnded
	}
	if slices.ContainsFunc(attention, func(item RoutineAttentionSummary) bool { return item.State == RoutineAttentionConflict }) ||
		slices.ContainsFunc(gates, func(gate RoutineDeliveryGate) bool { return gate.Status == GateConflict }) {
		return RoutineAssessmentConflict
	}
	if wait == RoutineWaitUnknown || slices.ContainsFunc(attention, func(item RoutineAttentionSummary) bool { return item.State == RoutineAttentionUnknown }) {
		return RoutineAssessmentUnknown
	}
	if wait == RoutineWaitManual || slices.ContainsFunc(attention, func(item RoutineAttentionSummary) bool { return item.State == RoutineAttentionActive }) {
		return RoutineAssessmentAbnormalWait
	}
	if wait == RoutineWaitNone {
		return RoutineAssessmentProgressing
	}
	return RoutineAssessmentNormalWait
}

func routineTransitionReason(transition Transition) string {
	if transition.From == "" || transition.To == "" {
		return "unknown_transition"
	}
	return string(transition.From) + "_to_" + string(transition.To)
}

func routineAttentionScope(event OperatorAttentionEvent) AuthorityScopeKind {
	if event.RunID != "" {
		return ScopeRun
	}
	if event.RepositoryProfileID != "" && event.RepositoryProfileID != "automation" {
		return ScopeRepository
	}
	return ScopeController
}

func routineAttentionTarget(event OperatorAttentionEvent) string {
	if event.RunID != "" {
		return event.RunID
	}
	if event.RepositoryProfileID != "" {
		return event.RepositoryProfileID
	}
	return controllerScopeID
}

func classifyRoutineAttention(inspection RunInspection, event OperatorAttentionEvent) RoutineAttentionState {
	if err := ValidateOperatorAttentionEvent(event); err != nil {
		if ValidatePreviousOperatorAttentionEvent(event) != nil && ValidateLegacyOperatorAttentionEvent(event) != nil {
			return RoutineAttentionConflict
		}
	}
	if event.RunID != "" && event.RunID != inspection.Run.ID {
		return RoutineAttentionConflict
	}
	switch event.EventType {
	case OperatorAttentionHumanDecision:
		if inspection.Run.State == domain.StateAwaitingHumanDecision {
			return RoutineAttentionActive
		}
		return ""
	case OperatorAttentionManualIntervention, OperatorAttentionAdmissionAuthority:
		if inspection.Run.State == domain.StateManualIntervention {
			return RoutineAttentionActive
		}
		return ""
	case OperatorAttentionCISlow, OperatorAttentionCIWaitRecovery:
		if slices.Contains([]domain.State{domain.StatePROpen, domain.StateReconcilingReviews, domain.StateAwaitingHumanApproval, domain.StateAwaitingGitHubMergeability}, inspection.Run.State) {
			return RoutineAttentionActive
		}
		return ""
	case OperatorAttentionSourceCheckoutSkipped:
		for _, record := range inspection.Cleanup {
			if record.Kind == "source_checkout" && record.Status == "skipped_attention" {
				return RoutineAttentionActive
			}
		}
		return ""
	case OperatorAttentionCleanupResidue:
		for _, record := range inspection.Cleanup {
			if record.Status == "failed" || record.Status == "intent" || record.Status == "skipped_attention" {
				return RoutineAttentionActive
			}
		}
		return ""
	default:
		// Scheduler and retry supersession depends on evidence outside a single
		// run aggregate. Retain it conservatively and offer no authority here.
		return RoutineAttentionUnknown
	}
}

func projectRoutineDecisionRequest(inspection RunInspection) *RoutineDecisionRequest {
	for index := len(inspection.Attempts) - 1; index >= 0; index-- {
		attempt := inspection.Attempts[index]
		if (attempt.Kind != "implementation" && attempt.Kind != "resume") || attempt.Status != "succeeded" {
			continue
		}
		outcome, err := readOutcome[domain.AgentOutcome](attempt.OutcomePath, attempt.OutcomeHash)
		if err != nil || outcome.Status != domain.AgentNeedsHumanDecision || outcome.DecisionRequest == nil || !boundedRoutineDecision(*outcome.DecisionRequest) {
			return nil
		}
		request := outcome.DecisionRequest
		result := &RoutineDecisionRequest{Question: request.Question, Context: request.Context, Recommendation: request.Recommendation, BlockingReason: request.BlockingReason, ContentTrust: "untrusted"}
		for _, option := range request.Options {
			result.Options = append(result.Options, RoutineDecisionOption{ID: option.ID, Description: option.Description})
		}
		return result
	}
	return nil
}

func boundedRoutineDecision(request domain.DecisionRequest) bool {
	if request.Validate() != nil || len(request.Options) > 20 {
		return false
	}
	if len([]byte(request.Question)) > 4096 || len([]byte(request.Context)) > 8192 || len([]byte(request.BlockingReason)) > 4096 || len([]byte(request.Recommendation)) > 256 {
		return false
	}
	total := 0
	for _, option := range request.Options {
		if len([]byte(option.ID)) > 256 || len([]byte(option.Description)) > 4096 {
			return false
		}
		total += len([]byte(option.ID)) + len([]byte(option.Description))
	}
	return total <= 32<<10
}

func routineProjectionDigest(value any) string {
	raw, _ := json.Marshal(value)
	var document any
	if json.Unmarshal(raw, &document) == nil {
		clearProjectionDigest(document)
		raw, _ = json.Marshal(document)
	}
	digest := sha256.Sum256(append([]byte("routine-query-v1\x00"), raw...))
	return hex.EncodeToString(digest[:])
}

func clearProjectionDigest(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if metadata, ok := typed["metadata"].(map[string]any); ok {
			metadata["digest"] = ""
		}
		for _, child := range typed {
			clearProjectionDigest(child)
		}
	case []any:
		for _, child := range typed {
			clearProjectionDigest(child)
		}
	}
}

func ClassifyRoutineDeliveryGates(inspection RunInspection) []RoutineDeliveryGate {
	gates := make([]RoutineDeliveryGate, 0, len(routineDeliveryGateOrder))
	for _, name := range routineDeliveryGateOrder {
		gates = append(gates, classifyRoutineDeliveryGate(name, inspection))
	}
	return gates
}

func classifyRoutineDeliveryGate(name DeliveryGateName, inspection RunInspection) RoutineDeliveryGate {
	run := inspection.Run
	gate := RoutineDeliveryGate{Name: name, Status: GatePending, ReasonCode: "not_reached"}
	terminal := TerminalRunState(run.State)
	setObserved := func(at time.Time) {
		if !at.IsZero() {
			value := at.UTC()
			gate.ObservedAt = &value
		}
	}
	conflictHead := func(head string) bool { return run.CandidateHead != "" && head != "" && head != run.CandidateHead }
	switch name {
	case GateVerification:
		for _, record := range inspection.Verifications {
			if conflictHead(record.VerifiedHead) {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
				continue
			}
			if record.VerifiedHead != run.CandidateHead || run.CandidateHead == "" {
				continue
			}
			gate.EvidenceCount++
			gate.BoundHead = record.VerifiedHead
			setObserved(record.CreatedAt)
			if record.ProcessOutcome == VerificationOutcomeExited && record.ExitCode == 0 && record.FailureCategory == "" {
				gate.Status, gate.ReasonCode = GatePassed, "verified_exact_head"
			} else if record.ProcessOutcome == VerificationOutcomeNotStarted {
				gate.Status, gate.ReasonCode = GateRunning, "verification_started"
			} else {
				gate.Status, gate.ReasonCode = GateFailed, "verification_failed"
			}
		}
		if gate.Status == GatePending && run.State == domain.StateVerifying {
			gate.Status, gate.ReasonCode = GateRunning, "verification_running"
		}
	case GateIndependentReview:
		for _, record := range inspection.Reviews {
			if conflictHead(record.ReviewedHead) {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
				continue
			}
			if record.ReviewedHead != run.CandidateHead || run.CandidateHead == "" {
				continue
			}
			gate.EvidenceCount++
			gate.BoundHead = record.ReviewedHead
			setObserved(record.CreatedAt)
			switch record.Verdict {
			case string(domain.ReviewPass):
				gate.Status, gate.ReasonCode = GatePassed, "review_passed_exact_head"
			case string(domain.ReviewFindings):
				gate.Status, gate.ReasonCode = GateFailed, "review_findings"
			default:
				gate.Status, gate.ReasonCode = GateUnknown, "review_evidence_unknown"
			}
		}
		if gate.Status == GatePending && run.State == domain.StateFreshReview {
			gate.Status, gate.ReasonCode = GateRunning, "review_running"
		}
	case GateBranchPublication:
		for _, record := range inspection.SideEffects {
			if record.Kind != "push" || record.Status != "observed" {
				continue
			}
			gate.EvidenceCount++
			gate.BoundHead = record.IdempotencyKey
			setObserved(record.ObservedAt)
			var evidence struct {
				PushedSHA string `json:"pushed_sha"`
				ExitCode  int    `json:"exit_code"`
			}
			if json.Unmarshal([]byte(record.ResultJSON), &evidence) != nil || evidence.PushedSHA == "" {
				gate.Status, gate.ReasonCode = GateUnknown, "branch_publication_evidence_invalid"
			} else if record.IdempotencyKey != run.CandidateHead || evidence.PushedSHA != run.CandidateHead {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
			} else if evidence.ExitCode != 0 {
				gate.Status, gate.ReasonCode = GateFailed, "branch_publication_failed"
			} else {
				gate.Status, gate.ReasonCode = GatePassed, "branch_publication_observed"
			}
		}
		if gate.Status == GatePending && run.State == domain.StatePushingBranch {
			gate.Status, gate.ReasonCode = GateRunning, "branch_publication_running"
		}
	case GatePullRequest:
		if inspection.PullRequest != nil {
			gate.EvidenceCount = 1
			gate.BoundHead = inspection.PullRequest.HeadSHA
			if conflictHead(gate.BoundHead) {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
			} else {
				gate.Status, gate.ReasonCode = GatePassed, "pull_request_observed"
				if inspection.GitHubEvidence != nil && inspection.GitHubEvidence.PullRequest.Number == inspection.PullRequest.Number && inspection.GitHubEvidence.PullRequest.HeadSHA == inspection.PullRequest.HeadSHA {
					setObserved(inspection.GitHubEvidence.ObservedAt)
				}
			}
		}
		if gate.Status == GatePending && run.State == domain.StateOpeningPR {
			gate.Status, gate.ReasonCode = GateRunning, "pull_request_opening"
		}
	case GateRequiredChecks:
		if inspection.GitHubEvidence != nil {
			evidence := inspection.GitHubEvidence
			gate.EvidenceCount = len(evidence.Checks)
			gate.EvidenceTruncated = inspection.GitHubEvidenceTruncated
			gate.BoundHead = evidence.PullRequest.HeadSHA
			setObserved(evidence.ObservedAt)
			if conflictHead(gate.BoundHead) {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
			} else {
				switch evidence.RequiredChecksStatus() {
				case domain.ReconciliationPass:
					if gate.EvidenceTruncated {
						gate.Status, gate.ReasonCode = GateUnknown, "evidence_truncated"
					} else {
						gate.Status, gate.ReasonCode = GatePassed, "required_checks_passed"
					}
				case domain.ReconciliationPending:
					gate.Status, gate.ReasonCode = GateRunning, "required_checks_pending"
				case domain.ReconciliationActionable:
					gate.Status, gate.ReasonCode = GateFailed, "required_checks_failed"
				default:
					gate.Status, gate.ReasonCode = GateUnknown, "required_checks_unknown"
				}
			}
		}
	case GateReviewConversations:
		if inspection.GitHubEvidence != nil {
			evidence := inspection.GitHubEvidence
			gate.EvidenceCount = len(evidence.ReviewThreads)
			gate.EvidenceTruncated = inspection.GitHubEvidenceTruncated
			gate.BoundHead = evidence.PullRequest.HeadSHA
			setObserved(evidence.ObservedAt)
			if conflictHead(gate.BoundHead) {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
			} else {
				unresolved := slices.ContainsFunc(evidence.ReviewThreads, func(thread domain.GitHubReviewThread) bool { return !thread.Resolved && !thread.Outdated })
				if unresolved {
					gate.Status, gate.ReasonCode = GateBlocked, "review_conversations_unresolved"
				} else if gate.EvidenceTruncated {
					gate.Status, gate.ReasonCode = GateUnknown, "evidence_truncated"
				} else {
					gate.Status, gate.ReasonCode = GatePassed, "review_conversations_resolved"
				}
			}
		}
	case GateHumanApproval:
		if inspection.Approval != nil {
			gate.EvidenceCount = 1
			gate.BoundHead = inspection.Approval.ApprovedSHA
			setObserved(inspection.Approval.ObservedAt)
			if conflictHead(gate.BoundHead) || inspection.Approval.ReviewSHA != run.CandidateHead {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
			} else {
				gate.Status, gate.ReasonCode = GatePassed, "human_approval_exact_head"
			}
		} else if inspection.ApprovalObservation != nil {
			gate.EvidenceCount = 1
			gate.BoundHead = inspection.ApprovalObservation.CandidateHead
			setObserved(inspection.ApprovalObservation.ObservedAt)
			switch inspection.ApprovalObservation.Status {
			case domain.HumanApprovalPending:
				gate.Status, gate.ReasonCode = GateRunning, "human_approval_pending"
			case domain.HumanApprovalStaleHead:
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
			case domain.HumanApprovalChangesRequested, domain.HumanApprovalDismissed:
				gate.Status, gate.ReasonCode = GateFailed, "human_approval_rejected"
			default:
				gate.Status, gate.ReasonCode = GateUnknown, "human_approval_unknown"
			}
		}
	case GateMerge:
		if inspection.Merge != nil {
			gate.EvidenceCount = 1
			gate.BoundHead = inspection.Merge.PreMergeSHA
			setObserved(inspection.Merge.MergedAt)
			if inspection.Merge.ValidateAuthority() != nil {
				gate.Status, gate.ReasonCode = GateUnknown, "merge_evidence_invalid"
			} else if conflictHead(gate.BoundHead) {
				gate.Status, gate.ReasonCode = GateConflict, "stale_head_evidence"
			} else {
				gate.Status, gate.ReasonCode = GatePassed, "merge_observed"
			}
		} else if run.State == domain.StateMerging {
			gate.Status, gate.ReasonCode = GateRunning, "merge_running"
		} else if run.State == domain.StateAwaitingGitHubMergeability {
			gate.Status, gate.ReasonCode = GateBlocked, "mergeability_blocked"
		}
	case GateLinearCompletion:
		for _, record := range inspection.LinearCompletion {
			gate.EvidenceCount++
			setObserved(record.ObservedAt)
			if inspection.Merge != nil && record.MergeSHA != inspection.Merge.MergeSHA {
				gate.Status, gate.ReasonCode = GateConflict, "merge_evidence_conflict"
				continue
			}
			switch record.Status {
			case LinearCompletionCompleted:
				gate.Status, gate.ReasonCode = GatePassed, "linear_completion_observed"
			case LinearCompletionPending:
				gate.Status, gate.ReasonCode = GateRunning, "linear_completion_pending"
			case LinearCompletionCanceled, LinearCompletionInvalid, LinearCompletionError, LinearCompletionTimeout:
				gate.Status, gate.ReasonCode = GateFailed, "linear_completion_failed"
			default:
				gate.Status, gate.ReasonCode = GateUnknown, "linear_completion_unknown"
			}
		}
	case GateSourceCheckout:
		for _, record := range inspection.Cleanup {
			if record.Kind != "source_checkout" {
				continue
			}
			gate.EvidenceCount++
			setObserved(record.UpdatedAt)
			switch record.Status {
			case "synced":
				gate.Status, gate.ReasonCode = GatePassed, "source_checkout_synced"
			case "intent":
				gate.Status, gate.ReasonCode = GateRunning, "source_checkout_running"
			case "skipped_attention":
				gate.Status, gate.ReasonCode = GateBlocked, "source_checkout_attention"
			case "failed":
				gate.Status, gate.ReasonCode = GateFailed, "source_checkout_failed"
			default:
				gate.Status, gate.ReasonCode = GateUnknown, "source_checkout_unknown"
			}
		}
	case GateCleanup:
		cleanupCount, complete, failed, running := 0, true, false, false
		for _, record := range inspection.Cleanup {
			if record.Kind == "source_checkout" {
				continue
			}
			cleanupCount++
			setObserved(record.UpdatedAt)
			switch record.Status {
			case "deleted", "retained":
			case "failed", "skipped_attention":
				complete, failed = false, true
			case "intent":
				complete, running = false, true
			default:
				complete = false
			}
		}
		gate.EvidenceCount = cleanupCount
		switch {
		case cleanupCount > 0 && complete:
			gate.Status, gate.ReasonCode = GatePassed, "cleanup_completed"
		case failed:
			gate.Status, gate.ReasonCode = GateFailed, "cleanup_failed"
		case running:
			gate.Status, gate.ReasonCode = GateRunning, "cleanup_running"
		case cleanupCount > 0:
			gate.Status, gate.ReasonCode = GateUnknown, "cleanup_unknown"
		case run.State == domain.StateCleaning:
			gate.Status, gate.ReasonCode = GateRunning, "cleanup_running"
		}
	}
	if terminal && gate.Status == GatePending {
		gate.Status, gate.ReasonCode = GateNotApplicable, "terminal_before_gate"
	}
	if run.State == domain.StateManualIntervention && gate.Status == GatePending {
		gate.Status, gate.ReasonCode = GateBlocked, "manual_intervention"
	}
	return gate
}
