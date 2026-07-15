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
	OperatorAttentionDeliveryPendingLocal = "pending_local"
	maxOperatorAttentionProjection        = 100
)

const (
	OperatorAttentionSourceCheckoutSkipped = "source_checkout_skipped_attention"
	OperatorAttentionCandidatePriorityTie  = "candidate_priority_tie"
	OperatorAttentionCandidateScan         = "candidate_scan_incomplete"
	OperatorAttentionSchedulerLease        = "scheduler_lease_attention"
	OperatorAttentionAdmissionAuthority    = "admission_authority_conflict"
	OperatorAttentionRetry                 = "automatic_retry_attention"
	operatorAttentionUnknown               = "unknown"
)

// OperatorAttentionEvent is the complete, sanitized local-outbox payload.
// It contains no external prose, paths, raw errors, URLs, commands, or credentials.
type OperatorAttentionEvent struct {
	EventKey              string    `json:"event_key"`
	EventType             string    `json:"event_type"`
	RunID                 string    `json:"run_id,omitempty"`
	LinearIdentifier      string    `json:"linear_identifier,omitempty"`
	RepositoryProfileID   string    `json:"repository_profile_id"`
	RepositoryProfileName string    `json:"repository_profile_name"`
	ControllerState       string    `json:"controller_state"`
	Severity              string    `json:"severity"`
	ReasonCode            string    `json:"reason_code"`
	EvidenceDigest        string    `json:"evidence_digest"`
	OccurredAt            time.Time `json:"occurred_at"`
	ObservedAt            time.Time `json:"observed_at"`
	DeliveryStatus        string    `json:"delivery_status"`
	PayloadDigest         string    `json:"-"`
}

// OperatorAttentionProfile is the only repository identity carried by an
// outbox event. It deliberately has no path, remote URL, or credential field.
type OperatorAttentionProfile struct {
	ID   string
	Name string
}

// OperatorAttentionStore is append-only and read-only after persistence. It
// intentionally exposes no delivery, acknowledgement, retry, deletion, or
// lifecycle-transition operation.
type OperatorAttentionStore interface {
	AppendOperatorAttention(context.Context, OperatorAttentionEvent) (bool, error)
	ListOperatorAttention(context.Context, int) ([]OperatorAttentionEvent, error)
}

// OperatorAttentionAppender is optional for older in-memory delivery fixtures.
// Production stores that implement it persist source-checkout attention before
// cleanup can advance a completed run.
type OperatorAttentionAppender interface {
	AppendOperatorAttention(context.Context, OperatorAttentionEvent) (bool, error)
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
// produce the same payload and SQLite can accept the replay idempotently.
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
var operatorAttentionProfileField = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)

func newOperatorAttentionEvent(input operatorAttentionEventInput) (OperatorAttentionEvent, error) {
	if !operatorAttentionScope.MatchString(input.ScopeID) || (input.RunID != "" && input.RunID != input.ScopeID) ||
		(input.LinearIdentifier != "" && !operatorAttentionIdentifier.MatchString(input.LinearIdentifier)) ||
		!operatorAttentionProfileField.MatchString(input.Profile.ID) || !operatorAttentionProfileField.MatchString(input.Profile.Name) ||
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
		EventKey:  "automation:" + input.ScopeID + ":" + eventType + ":" + suffix,
		EventType: eventType, RunID: input.RunID, LinearIdentifier: input.LinearIdentifier,
		RepositoryProfileID: input.Profile.ID, RepositoryProfileName: input.Profile.Name,
		ControllerState: state, Severity: severity, ReasonCode: reason, EvidenceDigest: input.EvidenceDigest,
		OccurredAt: input.OccurredAt.UTC(), ObservedAt: input.ObservedAt.UTC(), DeliveryStatus: OperatorAttentionDeliveryPendingLocal,
	}
	event.PayloadDigest = OperatorAttentionPayloadDigest(event)
	return event, nil
}

func operatorAttentionProfileForRun(run Run) (OperatorAttentionProfile, error) {
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil || repository.ProfileID != run.ProfileID || repository.CanonicalRepository != run.Repository {
		return OperatorAttentionProfile{}, errors.New("persisted operator attention profile is invalid")
	}
	return OperatorAttentionProfile{ID: run.ProfileID, Name: run.Repository}, nil
}

func sanitizedOperatorAttentionEventType(value string) string {
	switch value {
	case OperatorAttentionSourceCheckoutSkipped, OperatorAttentionCandidatePriorityTie, OperatorAttentionCandidateScan, OperatorAttentionSchedulerLease, OperatorAttentionAdmissionAuthority, OperatorAttentionRetry:
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
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateAwaitingHumanDecision, domain.StateVerifying, domain.StateFreshReview, domain.StateApprovalReady, domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StateRepairing, domain.StatePROpen, domain.StateReconcilingReviews, domain.StateAwaitingHumanApproval, domain.StateMerging, domain.StateAwaitingGitHubMergeability, domain.StateCleaning, domain.StateFailed, domain.StateCompleted, domain.StateRejected, domain.StateManualIntervention:
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
		EventType, RunID, LinearIdentifier, RepositoryProfileID, RepositoryProfileName, ControllerState, Severity, ReasonCode, EvidenceDigest, OccurredAt, ObservedAt, DeliveryStatus string
	}{event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ObservedAt.UTC().Format(time.RFC3339Nano), event.DeliveryStatus}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ValidateOperatorAttentionEvent verifies a persisted or caller-supplied
// record against the versioned local-outbox contract.
func ValidateOperatorAttentionEvent(event OperatorAttentionEvent) error {
	if event.DeliveryStatus != OperatorAttentionDeliveryPendingLocal || event.PayloadDigest == "" {
		return errors.New("operator attention outbox record is corrupt")
	}
	input := operatorAttentionEventInput{ScopeID: operatorAttentionScopeFromKey(event.EventKey), RunID: event.RunID, EventType: event.EventType, LinearIdentifier: event.LinearIdentifier, Profile: OperatorAttentionProfile{ID: event.RepositoryProfileID, Name: event.RepositoryProfileName}, State: event.ControllerState, Severity: event.Severity, ReasonCode: event.ReasonCode, EvidenceDigest: event.EvidenceDigest, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt}
	parts := strings.Split(event.EventKey, ":")
	if len(parts) != 4 || parts[0] != "automation" || parts[1] == "" || parts[2] != event.EventType {
		return errors.New("operator attention outbox record is corrupt")
	}
	if sequence, err := strconv.ParseInt(parts[3], 10, 64); err == nil && sequence > 0 {
		input.TransitionSequence = sequence
	}
	want, err := newOperatorAttentionEvent(input)
	if err != nil || want.EventKey != event.EventKey || want.PayloadDigest != event.PayloadDigest {
		return errors.New("operator attention outbox record is corrupt")
	}
	return nil
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
	return fmt.Errorf("operator attention outbox key conflicts: %s", event.EventKey)
}
