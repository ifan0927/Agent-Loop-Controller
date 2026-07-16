package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	OperatorAttentionLegacySchemaVersion = 0
	OperatorAttentionSchemaVersion       = 1
	maxOperatorAttentionProjection       = 100
)

type OperatorAttentionActionID string

const (
	OperatorAttentionActionRetry   OperatorAttentionActionID = "retry"
	OperatorAttentionActionAbandon OperatorAttentionActionID = "abandon"
	OperatorAttentionActionDecide  OperatorAttentionActionID = "decide"
)

const (
	OperatorAttentionSourceCheckoutSkipped = "source_checkout_skipped_attention"
	OperatorAttentionCandidatePriorityTie  = "candidate_priority_tie"
	OperatorAttentionCandidateScan         = "candidate_scan_incomplete"
	OperatorAttentionSchedulerLease        = "scheduler_lease_attention"
	OperatorAttentionAdmissionAuthority    = "admission_authority_conflict"
	OperatorAttentionRetry                 = "automatic_retry_attention"
	OperatorAttentionCleanupResidue        = "cleanup_residue_attention"
	OperatorAttentionManualIntervention    = "manual_intervention_attention"
	OperatorAttentionHumanDecision         = "human_decision_attention"
	operatorAttentionUnknown               = "unknown"
)

// OperatorAttentionEvent is the complete, sanitized transport-neutral payload.
// It contains no external prose, paths, raw errors, URLs, commands, or credentials.
type OperatorAttentionEvent struct {
	SchemaVersion         int                         `json:"schema_version"`
	EventKey              string                      `json:"event_key"`
	EventType             string                      `json:"event_type"`
	RunID                 string                      `json:"run_id,omitempty"`
	LinearIdentifier      string                      `json:"linear_identifier,omitempty"`
	RepositoryProfileID   string                      `json:"repository_profile_id"`
	RepositoryProfileName string                      `json:"repository_profile_name"`
	ControllerState       string                      `json:"controller_state"`
	Severity              string                      `json:"severity"`
	ReasonCode            string                      `json:"reason_code"`
	AllowedActions        []OperatorAttentionActionID `json:"allowed_actions"`
	EvidenceDigest        string                      `json:"evidence_digest"`
	OccurredAt            time.Time                   `json:"occurred_at"`
	ObservedAt            time.Time                   `json:"observed_at"`
	PayloadDigest         string                      `json:"payload_digest"`
}

// OperatorAttentionProfile is the only repository identity carried by an
// attention event. It deliberately has no path, remote URL, or credential field.
type OperatorAttentionProfile struct {
	ID   string
	Name string
}

// OperatorAttentionPublisher is the sole application write port. Publishing
// an event never grants workflow authority or advances controller state.
type OperatorAttentionPublisher interface {
	AppendOperatorAttention(context.Context, OperatorAttentionEvent) (bool, error)
}

// OperatorAttentionQuery is a separate bounded read port for CLI inspection
// and future presentation adapters.
type OperatorAttentionQuery interface {
	ListOperatorAttention(context.Context, OperatorAttentionQueryInput) ([]OperatorAttentionEvent, error)
}

// CurrentOperatorAttentionQuery is an authority read, not a presentation
// projection. Implementations must return only the newest durably published
// run-scoped event so historical display records cannot authorize an action.
type CurrentOperatorAttentionQuery interface {
	CurrentOperatorAttention(context.Context, string) (OperatorAttentionEvent, bool, error)
}

type OperatorAttentionQueryInput struct {
	RunID string
	Limit int
}

// SourceCheckoutSkippedAttentionEvent maps the sole existing source-checkout
// attention signal. The caller supplies a stable evidence digest derived from
// controller-owned merge evidence rather than any checkout path or raw error.
func SourceCheckoutSkippedAttentionEvent(run Run, transitionSequence int64, reason string, evidenceDigest string, observedAt time.Time) (OperatorAttentionEvent, error) {
	profile, err := operatorAttentionProfileForRun(run)
	if err != nil {
		return OperatorAttentionEvent{}, err
	}
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: run.ID, RunID: run.ID, EventType: OperatorAttentionSourceCheckoutSkipped,
		Profile: profile, State: run.State, Severity: "warning", ReasonCode: reason,
		EvidenceDigest: evidenceDigest, TransitionSequence: transitionSequence,
		OccurredAt: observedAt, ObservedAt: observedAt,
	})
}

// CleanupResidueAttentionEvent reports retained post-terminal operator work.
// It advertises no workflow action: the run slot is already released and the
// underlying ownership and cleanup rows remain the recovery authority.
func CleanupResidueAttentionEvent(run Run, transitionSequence int64, evidenceDigest string, observedAt time.Time) (OperatorAttentionEvent, error) {
	profile, err := operatorAttentionProfileForRun(run)
	if err != nil {
		return OperatorAttentionEvent{}, err
	}
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: run.ID, RunID: run.ID, EventType: OperatorAttentionCleanupResidue,
		Profile: profile, State: run.State, Severity: "warning", ReasonCode: "cleanup_residue",
		EvidenceDigest: evidenceDigest, TransitionSequence: transitionSequence,
		OccurredAt: observedAt, ObservedAt: observedAt,
	})
}

// CandidatePriorityTieAttentionEvent maps a deterministic, top-priority tie
// without selecting a candidate or mutating Linear.
func CandidatePriorityTieAttentionEvent(scanID, linearIdentifier string, profile OperatorAttentionProfile, evidenceDigest string, observedAt time.Time) (OperatorAttentionEvent, error) {
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: scanID, EventType: OperatorAttentionCandidatePriorityTie, LinearIdentifier: linearIdentifier,
		Profile: profile, State: "scan", Severity: "warning", ReasonCode: "top_priority_tie",
		EvidenceDigest: evidenceDigest, OccurredAt: observedAt, ObservedAt: observedAt,
	})
}

// CandidateScanIncompleteAttentionEvent maps bounded scan truncation or
// authority incompleteness. It has no admission authority.
func CandidateScanIncompleteAttentionEvent(scanID string, profile OperatorAttentionProfile, reason, evidenceDigest string, observedAt time.Time) (OperatorAttentionEvent, error) {
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: scanID, EventType: OperatorAttentionCandidateScan, Profile: profile,
		State: "scan", Severity: "warning", ReasonCode: reason, EvidenceDigest: evidenceDigest,
		OccurredAt: observedAt, ObservedAt: observedAt,
	})
}

// SchedulerLeaseAttentionEvent maps scheduler ownership loss or conflict. It
// does not alter the lease or authorize a scheduler retry.
func SchedulerLeaseAttentionEvent(scanID string, profile OperatorAttentionProfile, reason, evidenceDigest string, observedAt time.Time) (OperatorAttentionEvent, error) {
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: scanID, EventType: OperatorAttentionSchedulerLease, Profile: profile,
		State: "scheduler", Severity: "warning", ReasonCode: reason, EvidenceDigest: evidenceDigest,
		OccurredAt: observedAt, ObservedAt: observedAt,
	})
}

// AdmissionAuthorityConflictAttentionEvent maps an automatic admission or
// mutation authority conflict without changing a run's durable state.
func AdmissionAuthorityConflictAttentionEvent(run Run, reason, evidenceDigest string, observedAt time.Time) (OperatorAttentionEvent, error) {
	profile, err := operatorAttentionProfileForRun(run)
	if err != nil {
		return OperatorAttentionEvent{}, err
	}
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: run.ID, RunID: run.ID, EventType: OperatorAttentionAdmissionAuthority,
		Profile: profile, State: run.State, Severity: "warning", ReasonCode: reason,
		EvidenceDigest: evidenceDigest, OccurredAt: observedAt, ObservedAt: observedAt,
	})
}

// AutomaticRetryAttentionEvent projects one durable retry stop. Its timestamps
// come from the immutable attention schedule so repeated worker restarts
// produce the same payload and the publisher can accept the replay idempotently.
func AutomaticRetryAttentionEvent(run Run, schedule RetrySchedule) (OperatorAttentionEvent, error) {
	if err := schedule.validate(); err != nil || schedule.RunID != run.ID || schedule.Status != RetryScheduleAttention {
		if err != nil {
			return OperatorAttentionEvent{}, errors.New("automatic retry attention evidence is invalid")
		}
		return OperatorAttentionEvent{}, errors.New("automatic retry attention evidence is invalid")
	}
	profile, err := operatorAttentionProfileForRun(run)
	if err != nil {
		return OperatorAttentionEvent{}, err
	}
	evidence := retryAttentionDigest(schedule)
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: retryAttentionScope(schedule), RunID: run.ID, EventType: OperatorAttentionRetry,
		Profile: profile, State: schedule.ControllerState, Severity: "error", ReasonCode: schedule.ReasonCode,
		EvidenceDigest: evidence, OccurredAt: schedule.AttentionAt, ObservedAt: schedule.AttentionAt,
	})
}

// ManualInterventionAttentionEvent maps one persisted manual stop. Its key and
// timestamp bind to the exact transition rather than mutable run metadata.
func ManualInterventionAttentionEvent(run Run, transition Transition) (OperatorAttentionEvent, error) {
	if run.State != domain.StateManualIntervention || transition.Sequence < 1 || transition.To != domain.StateManualIntervention || transition.CreatedAt.IsZero() {
		return OperatorAttentionEvent{}, errors.New("manual intervention attention evidence is invalid")
	}
	profile, err := operatorAttentionProfileForRun(run)
	if err != nil {
		return OperatorAttentionEvent{}, err
	}
	evidence := manualInterventionAttentionDigest(run, transition)
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: run.ID, RunID: run.ID, EventType: OperatorAttentionManualIntervention,
		Profile: profile, State: run.State, Severity: "error", ReasonCode: "manual_intervention",
		EvidenceDigest: evidence, TransitionSequence: transition.Sequence,
		OccurredAt: transition.CreatedAt, ObservedAt: transition.CreatedAt,
	})
}

// HumanDecisionAttentionEvent binds the presentation action to the exact
// transition that persisted the offered decision. Re-observation after a
// worker restart therefore replays one immutable event key.
func HumanDecisionAttentionEvent(run Run, transition Transition) (OperatorAttentionEvent, error) {
	if run.State != domain.StateAwaitingHumanDecision || transition.Sequence < 1 || transition.To != domain.StateAwaitingHumanDecision || transition.CreatedAt.IsZero() {
		return OperatorAttentionEvent{}, errors.New("human decision attention evidence is invalid")
	}
	profile, err := operatorAttentionProfileForRun(run)
	if err != nil {
		return OperatorAttentionEvent{}, err
	}
	evidence := humanDecisionAttentionDigest(run, transition)
	return newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: run.ID, RunID: run.ID, EventType: OperatorAttentionHumanDecision,
		Profile: profile, State: run.State, Severity: "warning", ReasonCode: "human_decision_required",
		EvidenceDigest: evidence, TransitionSequence: transition.Sequence,
		OccurredAt: transition.CreatedAt, ObservedAt: transition.CreatedAt,
	})
}

func manualInterventionAttentionDigest(run Run, transition Transition) string {
	payload := struct {
		RunID, From, To, Reason, EvidenceReference, BoundHead, CreatedAt string
		Sequence                                                         int64
	}{run.ID, string(transition.From), string(transition.To), transition.Reason, transition.EvidenceReference, transition.BoundHead, transition.CreatedAt.UTC().Format(time.RFC3339Nano), transition.Sequence}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func humanDecisionAttentionDigest(run Run, transition Transition) string {
	payload := struct {
		RunID, From, To, Reason, EvidenceReference, BoundHead, CreatedAt string
		Sequence                                                         int64
	}{run.ID, string(transition.From), string(transition.To), transition.Reason, transition.EvidenceReference, transition.BoundHead, transition.CreatedAt.UTC().Format(time.RFC3339Nano), transition.Sequence}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func latestManualInterventionTransition(inspection RunInspection) (Transition, error) {
	for index := len(inspection.Timeline) - 1; index >= 0; index-- {
		if inspection.Timeline[index].To == domain.StateManualIntervention {
			return inspection.Timeline[index], nil
		}
	}
	return Transition{}, errors.New("manual intervention transition evidence is missing")
}

func latestHumanDecisionTransition(inspection RunInspection) (Transition, error) {
	for index := len(inspection.Timeline) - 1; index >= 0; index-- {
		if inspection.Timeline[index].To == domain.StateAwaitingHumanDecision {
			return inspection.Timeline[index], nil
		}
	}
	return Transition{}, errors.New("human decision transition evidence is missing")
}

func publishManualInterventionAttention(ctx context.Context, run Run, inspection RunInspection, publisher OperatorAttentionPublisher) error {
	if publisher == nil || inspection.Run.ID != "" && inspection.Run.ID != run.ID {
		return errors.New("manual intervention attention dependencies are invalid")
	}
	transition, err := latestManualInterventionTransition(inspection)
	if err != nil {
		return err
	}
	event, err := ManualInterventionAttentionEvent(run, transition)
	if err != nil {
		return err
	}
	_, err = publisher.AppendOperatorAttention(ctx, event)
	return err
}

func publishHumanDecisionAttention(ctx context.Context, run Run, inspection RunInspection, publisher OperatorAttentionPublisher) error {
	if publisher == nil || inspection.Run.ID != "" && inspection.Run.ID != run.ID {
		return errors.New("human decision attention dependencies are invalid")
	}
	transition, err := latestHumanDecisionTransition(inspection)
	if err != nil {
		return err
	}
	event, err := HumanDecisionAttentionEvent(run, transition)
	if err != nil {
		return err
	}
	_, err = publisher.AppendOperatorAttention(ctx, event)
	return err
}

type operatorAttentionEventInput struct {
	ScopeID            string
	RunID              string
	EventType          string
	LinearIdentifier   string
	Profile            OperatorAttentionProfile
	State              any
	Severity           string
	ReasonCode         string
	EvidenceDigest     string
	TransitionSequence int64
	OccurredAt         time.Time
	ObservedAt         time.Time
}

var operatorAttentionScope = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var operatorAttentionIdentifier = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[1-9][0-9]{0,9}$`)
var operatorAttentionProfileID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,159}$`)
var operatorAttentionRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var legacyOperatorAttentionProfileField = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)

func newOperatorAttentionEvent(input operatorAttentionEventInput) (OperatorAttentionEvent, error) {
	if !operatorAttentionScope.MatchString(input.ScopeID) || (input.RunID != "" && input.RunID != input.ScopeID) ||
		(input.LinearIdentifier != "" && !operatorAttentionIdentifier.MatchString(input.LinearIdentifier)) ||
		!validOperatorAttentionProfile(input.Profile) ||
		!validOperatorAttentionDigest(input.EvidenceDigest) || input.OccurredAt.IsZero() || input.ObservedAt.IsZero() || input.ObservedAt.Before(input.OccurredAt) || input.TransitionSequence < 0 {
		return OperatorAttentionEvent{}, errors.New("operator attention event is invalid")
	}
	eventType := sanitizedOperatorAttentionEventType(input.EventType)
	reason := sanitizedOperatorAttentionReason(eventType, input.ReasonCode)
	state := sanitizedOperatorAttentionState(input.State)
	severity := sanitizedOperatorAttentionSeverity(input.Severity)
	suffix := input.EvidenceDigest
	if input.TransitionSequence > 0 {
		suffix = strconv.FormatInt(input.TransitionSequence, 10)
	}
	event := OperatorAttentionEvent{
		SchemaVersion: OperatorAttentionSchemaVersion,
		EventKey:      "automation:" + input.ScopeID + ":" + eventType + ":" + suffix,
		EventType:     eventType, RunID: input.RunID, LinearIdentifier: input.LinearIdentifier,
		RepositoryProfileID: input.Profile.ID, RepositoryProfileName: input.Profile.Name,
		ControllerState: state, Severity: severity, ReasonCode: reason,
		AllowedActions: allowedOperatorAttentionActions(eventType, state, reason), EvidenceDigest: input.EvidenceDigest,
		OccurredAt: input.OccurredAt.UTC(), ObservedAt: input.ObservedAt.UTC(),
	}
	event.PayloadDigest = OperatorAttentionPayloadDigest(event)
	return event, nil
}

func validOperatorAttentionProfile(profile OperatorAttentionProfile) bool {
	if !operatorAttentionRepository.MatchString(profile.Name) && !operatorAttentionProfileID.MatchString(profile.Name) {
		return false
	}
	for _, value := range []string{profile.ID, profile.Name} {
		lower := strings.ToLower(value)
		for _, forbidden := range []string{"authorization", "bearer", "credential", "secret", "token", "password", "passwd", "api-key", "apikey", "private-key", "client-secret"} {
			if strings.Contains(lower, forbidden) {
				return false
			}
		}
		if strings.ContainsAny(value, "\\@?#%") {
			return false
		}
	}
	if operatorAttentionProfileID.MatchString(profile.ID) {
		return true
	}
	return profile.ID == "repository-profile:"+profile.Name
}

func allowedOperatorAttentionActions(eventType, state, reason string) []OperatorAttentionActionID {
	if eventType == OperatorAttentionRetry && reason != operatorAttentionUnknown && knownOperatorAttentionState(domain.State(state)) {
		return []OperatorAttentionActionID{OperatorAttentionActionRetry, OperatorAttentionActionAbandon}
	}
	if eventType == OperatorAttentionManualIntervention && state == string(domain.StateManualIntervention) && reason == "manual_intervention" {
		return []OperatorAttentionActionID{OperatorAttentionActionAbandon}
	}
	if eventType == OperatorAttentionHumanDecision && state == string(domain.StateAwaitingHumanDecision) && reason == "human_decision_required" {
		return []OperatorAttentionActionID{OperatorAttentionActionDecide}
	}
	return []OperatorAttentionActionID{}
}

func operatorAttentionProfileForRun(run Run) (OperatorAttentionProfile, error) {
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil || repository.ProfileID != run.ProfileID || repository.CanonicalRepository != run.Repository {
		return OperatorAttentionProfile{}, errors.New("persisted operator attention profile is invalid")
	}
	if run.ProfileID == "" {
		return OperatorAttentionProfile{ID: "legacy-profile", Name: run.Repository}, nil
	}
	return OperatorAttentionProfile{ID: run.ProfileID, Name: run.Repository}, nil
}

func sanitizedOperatorAttentionEventType(value string) string {
	switch value {
	case OperatorAttentionSourceCheckoutSkipped, OperatorAttentionCandidatePriorityTie, OperatorAttentionCandidateScan, OperatorAttentionSchedulerLease, OperatorAttentionAdmissionAuthority, OperatorAttentionRetry, OperatorAttentionCleanupResidue, OperatorAttentionManualIntervention, OperatorAttentionHumanDecision:
		return value
	default:
		return operatorAttentionUnknown
	}
}

func sanitizedOperatorAttentionReason(eventType, value string) string {
	allowed := map[string]map[string]bool{
		OperatorAttentionSourceCheckoutSkipped: {string(SourceSyncReasonDirtySource): true, string(SourceSyncReasonWrongBranch): true, string(SourceSyncReasonDetachedHead): true, string(SourceSyncReasonSourceDiverged): true, string(SourceSyncReasonStateDrift): true},
		OperatorAttentionCandidatePriorityTie:  {"top_priority_tie": true},
		OperatorAttentionCandidateScan:         {"truncated": true, "incomplete_authority": true},
		OperatorAttentionSchedulerLease:        {"lease_conflict": true, "lease_lost": true},
		OperatorAttentionAdmissionAuthority:    {"admission_authority_conflict": true, "mutation_authority_conflict": true},
		OperatorAttentionRetry:                 {RetryReasonProcessStart: true, RetryReasonUnavailable: true, RetryReasonAuthority: true, RetryReasonIntegrity: true, RetryReasonManual: true, RetryReasonTerminal: true, RetryReasonPersistence: true, RetryReasonBudgetExhausted: true},
		OperatorAttentionCleanupResidue:        {"cleanup_residue": true},
		OperatorAttentionManualIntervention:    {"manual_intervention": true},
		OperatorAttentionHumanDecision:         {"human_decision_required": true},
	}
	if allowed[eventType][value] {
		return value
	}
	return operatorAttentionUnknown
}

func sanitizedOperatorAttentionState(value any) string {
	state, ok := value.(domain.State)
	if ok {
		if knownOperatorAttentionState(state) {
			return string(state)
		}
		return operatorAttentionUnknown
	}
	if text, ok := value.(string); ok {
		if text == "scan" || text == "scheduler" {
			return text
		}
		if knownOperatorAttentionState(domain.State(text)) {
			return text
		}
	}
	return operatorAttentionUnknown
}

func knownOperatorAttentionState(state domain.State) bool {
	switch state {
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateAwaitingHumanDecision, domain.StateVerifying, domain.StateFreshReview, domain.StateApprovalReady, domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StateRepairing, domain.StatePROpen, domain.StateReconcilingReviews, domain.StateReplyingReviewFeedback, domain.StateAwaitingHumanApproval, domain.StateMerging, domain.StateAwaitingGitHubMergeability, domain.StateAwaitingLinearCompletion, domain.StateCleaning, domain.StateFailed, domain.StateCompleted, domain.StateRejected, domain.StateManualIntervention:
		return true
	default:
		return false
	}
}

func sanitizedOperatorAttentionSeverity(value string) string {
	switch value {
	case "info", "warning", "error":
		return value
	default:
		return "warning"
	}
}

func validOperatorAttentionDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// OperatorAttentionPayloadDigest deterministically hashes only the allowlisted
// payload fields. Storage uses it to distinguish idempotent replays from a
// conflicting attempt to reuse an event key.
func OperatorAttentionPayloadDigest(event OperatorAttentionEvent) string {
	payload := struct {
		SchemaVersion                                                                                                                         int
		EventType, RunID, LinearIdentifier, RepositoryProfileID, RepositoryProfileName, ControllerState, Severity, ReasonCode, EvidenceDigest string
		AllowedActions                                                                                                                        []OperatorAttentionActionID
		OccurredAt, ObservedAt                                                                                                                string
	}{event.SchemaVersion, event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, event.AllowedActions, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ObservedAt.UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// OperatorAttentionContentDigest identifies the transport-neutral fields that
// are common to legacy and current envelopes. Storage adapters use it only to
// recognize a current producer replay of an immutable legacy event.
func OperatorAttentionContentDigest(event OperatorAttentionEvent) string {
	payload := struct {
		EventType, RunID, LinearIdentifier, RepositoryProfileID, RepositoryProfileName, ControllerState, Severity, ReasonCode, EvidenceDigest string
		OccurredAt, ObservedAt                                                                                                                string
	}{event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ObservedAt.UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ValidateOperatorAttentionEvent verifies a persisted or caller-supplied
// record against the versioned transport-neutral contract.
func ValidateOperatorAttentionEvent(event OperatorAttentionEvent) error {
	if event.SchemaVersion != OperatorAttentionSchemaVersion || event.PayloadDigest == "" || event.AllowedActions == nil {
		return errors.New("operator attention event record is corrupt")
	}
	input := operatorAttentionEventInput{ScopeID: operatorAttentionScopeFromKey(event.EventKey), RunID: event.RunID, EventType: event.EventType, LinearIdentifier: event.LinearIdentifier, Profile: OperatorAttentionProfile{ID: event.RepositoryProfileID, Name: event.RepositoryProfileName}, State: event.ControllerState, Severity: event.Severity, ReasonCode: event.ReasonCode, EvidenceDigest: event.EvidenceDigest, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt}
	parts := strings.Split(event.EventKey, ":")
	if len(parts) != 4 || parts[0] != "automation" || parts[1] == "" || parts[2] != event.EventType {
		return errors.New("operator attention event record is corrupt")
	}
	if sequence, err := strconv.ParseInt(parts[3], 10, 64); err == nil && sequence > 0 {
		input.TransitionSequence = sequence
	}
	want, err := newOperatorAttentionEvent(input)
	if err != nil || want.EventKey != event.EventKey || want.PayloadDigest != event.PayloadDigest || !equalOperatorAttentionActions(want.AllowedActions, event.AllowedActions) {
		return errors.New("operator attention event record is corrupt")
	}
	return nil
}

// ValidateLegacyOperatorAttentionEvent validates the sanitized projection of
// an immutable schema-0 row without reinterpreting its original payload digest
// or adding current presentation actions.
func ValidateLegacyOperatorAttentionEvent(event OperatorAttentionEvent) error {
	if event.SchemaVersion != OperatorAttentionLegacySchemaVersion || !validOperatorAttentionDigest(event.PayloadDigest) || event.AllowedActions == nil || len(event.AllowedActions) != 0 {
		return errors.New("legacy operator attention event record is corrupt")
	}
	parts := strings.Split(event.EventKey, ":")
	if len(parts) != 4 || parts[0] != "automation" || !operatorAttentionScope.MatchString(parts[1]) || parts[2] != event.EventType ||
		(event.RunID != "" && event.RunID != parts[1]) || (event.LinearIdentifier != "" && !operatorAttentionIdentifier.MatchString(event.LinearIdentifier)) ||
		!legacyOperatorAttentionProfileField.MatchString(event.RepositoryProfileID) || !legacyOperatorAttentionProfileField.MatchString(event.RepositoryProfileName) ||
		!validOperatorAttentionDigest(event.EvidenceDigest) || event.OccurredAt.IsZero() || event.ObservedAt.IsZero() || event.ObservedAt.Before(event.OccurredAt) ||
		event.EventType != sanitizedOperatorAttentionEventType(event.EventType) || event.ControllerState != sanitizedOperatorAttentionState(event.ControllerState) ||
		event.ReasonCode != sanitizedOperatorAttentionReason(event.EventType, event.ReasonCode) || event.Severity != sanitizedOperatorAttentionSeverity(event.Severity) {
		return errors.New("legacy operator attention event record is corrupt")
	}
	if parts[3] != event.EvidenceDigest {
		sequence, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || sequence < 1 {
			return errors.New("legacy operator attention event record is corrupt")
		}
	}
	return nil
}

func projectedOperatorAttentionProfile(event OperatorAttentionEvent) OperatorAttentionProfile {
	profile := OperatorAttentionProfile{ID: event.RepositoryProfileID, Name: event.RepositoryProfileName}
	if validOperatorAttentionProfile(profile) {
		return profile
	}
	return OperatorAttentionProfile{ID: "legacy-profile", Name: "legacy-repository"}
}

func equalOperatorAttentionActions(left, right []OperatorAttentionActionID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func operatorAttentionScopeFromKey(key string) string {
	parts := strings.Split(key, ":")
	if len(parts) != 4 {
		return ""
	}
	return parts[1]
}

// FormatOperatorAttentionConflict is intentionally safe for transport output.
func FormatOperatorAttentionConflict(event OperatorAttentionEvent) error {
	return fmt.Errorf("operator attention event key conflicts: %s", event.EventKey)
}
