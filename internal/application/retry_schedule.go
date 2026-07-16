package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// RetryScheduleStatus is durable control-flow evidence. A scheduled record
// gates the same run and phase; an attention record is a permanent automatic
// stop until an operator changes the run through an explicit path.
type RetryScheduleStatus string

const (
	RetryScheduleScheduled RetryScheduleStatus = "scheduled"
	RetryScheduleAttention RetryScheduleStatus = "attention"
)

// RetryFailureClass is controller-owned classification, never a raw process
// or transport error. Only retryable classes can create a future eligibility
// time.
type RetryFailureClass string

const (
	RetryFailureProcessStart RetryFailureClass = "process_start"
	RetryFailureUnavailable  RetryFailureClass = "unavailable"
	RetryFailureAuthority    RetryFailureClass = "authority_conflict"
	RetryFailureIntegrity    RetryFailureClass = "integrity_failure"
	RetryFailureManual       RetryFailureClass = "manual_state"
	RetryFailureTerminal     RetryFailureClass = "terminal_failure"
	RetryFailurePersistence  RetryFailureClass = "persistence_conflict"
)

const (
	RetryReasonProcessStart    = "process_start"
	RetryReasonUnavailable     = "unavailable"
	RetryReasonAuthority       = "authority_conflict"
	RetryReasonIntegrity       = "integrity_failure"
	RetryReasonManual          = "manual_state"
	RetryReasonTerminal        = "terminal_failure"
	RetryReasonPersistence     = "persistence_conflict"
	RetryReasonBudgetExhausted = "retry_budget_exhausted"
	RetryReasonOperatorRetry   = "operator_retry"
)

// AutomaticRetryPolicy is deliberately small and bounded. AttemptCount is the
// number of failures recorded for the run/phase, not a permission to start a
// second run or to reuse another run's artifacts.
type AutomaticRetryPolicy struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaximumDelay time.Duration
}

const (
	DefaultAutomaticRetryMaxAttempts  = 3
	DefaultAutomaticRetryInitialDelay = time.Second
	DefaultAutomaticRetryMaximumDelay = 30 * time.Second
)

func DefaultAutomaticRetryPolicy() AutomaticRetryPolicy {
	return AutomaticRetryPolicy{MaxAttempts: DefaultAutomaticRetryMaxAttempts, InitialDelay: DefaultAutomaticRetryInitialDelay, MaximumDelay: DefaultAutomaticRetryMaximumDelay}
}

func (p AutomaticRetryPolicy) normalized() AutomaticRetryPolicy {
	if p.MaxAttempts == 0 && p.InitialDelay == 0 && p.MaximumDelay == 0 {
		return DefaultAutomaticRetryPolicy()
	}
	return p
}

func (p AutomaticRetryPolicy) validate() error {
	p = p.normalized()
	if p.MaxAttempts < 1 || p.MaxAttempts > 10 || p.InitialDelay <= 0 || p.MaximumDelay < p.InitialDelay || p.MaximumDelay > 24*time.Hour {
		return errors.New("automatic retry policy is invalid")
	}
	return nil
}

func AutomaticRetryDelay(policy AutomaticRetryPolicy, attempt int) time.Duration {
	p := policy.normalized()
	delay := p.InitialDelay
	for index := 1; index < attempt && delay < p.MaximumDelay; index++ {
		if delay > p.MaximumDelay/2 {
			return p.MaximumDelay
		}
		delay *= 2
	}
	if delay > p.MaximumDelay {
		return p.MaximumDelay
	}
	return delay
}

// RetrySchedule is sanitized, restart-safe scheduling evidence. The schedule
// stores the controller state that produced it so a later read cannot silently
// turn a state-specific retry into authority for another phase.
type RetrySchedule struct {
	RunID              string              `json:"run_id"`
	Phase              string              `json:"phase"`
	ControllerState    string              `json:"controller_state"`
	AttemptCount       int                 `json:"attempt_count"`
	MaxAttempts        int                 `json:"max_attempts"`
	InitialDelay       time.Duration       `json:"initial_delay_ns"`
	MaximumDelay       time.Duration       `json:"maximum_delay_ns"`
	FailureClass       RetryFailureClass   `json:"failure_class"`
	FailureEvidenceRef string              `json:"failure_evidence_ref,omitempty"`
	ReasonCode         string              `json:"reason_code"`
	Status             RetryScheduleStatus `json:"status"`
	NextEligibleAt     time.Time           `json:"next_eligible_at,omitempty"`
	AttentionAt        time.Time           `json:"attention_at,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
}

// RetryFailureRequest is the only mutable input to the durable retry CAS.
// ExpectedAttempt prevents a stale worker from extending a newer schedule.
type RetryFailureRequest struct {
	RunID              string
	Phase              string
	ControllerState    domain.State
	ExpectedAttempt    int
	FailureClass       RetryFailureClass
	FailureEvidenceRef string
	ReasonCode         string
	Now                time.Time
	Policy             AutomaticRetryPolicy
}

type RetryScheduleStore interface {
	GetRetrySchedule(context.Context, string, string) (RetrySchedule, bool, error)
	ListRetrySchedules(context.Context) ([]RetrySchedule, error)
	ApplyRetryFailure(context.Context, RetryFailureRequest) (RetrySchedule, bool, error)
	ClearRetrySchedule(context.Context, string, string, int) (bool, error)
}

var retryScheduleKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (p AutomaticRetryPolicy) normalizedAndValidated() (AutomaticRetryPolicy, error) {
	p = p.normalized()
	if err := p.validate(); err != nil {
		return AutomaticRetryPolicy{}, err
	}
	return p, nil
}

func ValidateAutomaticRetryPolicy(policy AutomaticRetryPolicy) error {
	_, err := policy.normalizedAndValidated()
	return err
}

func validateRetryKey(value string) bool {
	return retryScheduleKey.MatchString(value)
}

func (r RetryFailureRequest) validate() error {
	if !validateRetryKey(r.RunID) || !validateRetryKey(r.Phase) || r.ExpectedAttempt < 0 || r.Now.IsZero() || !validRetryControllerState(r.ControllerState) || !validRetryFailureClass(r.FailureClass) {
		return errors.New("automatic retry failure request is invalid")
	}
	if !validRetryReasonCode(r.ReasonCode) || r.ReasonCode == RetryReasonBudgetExhausted || retryReasonForClass(r.FailureClass) != r.ReasonCode {
		return errors.New("automatic retry reason code is invalid")
	}
	if r.FailureClass == RetryFailureProcessStart {
		if !validRetryProcessEvidenceRef(r.FailureEvidenceRef) {
			return errors.New("process-start retry failure evidence reference is invalid")
		}
	} else if r.FailureEvidenceRef != "" {
		return errors.New("retry failure evidence reference does not match the failure class")
	}
	_, err := r.Policy.normalizedAndValidated()
	return err
}

func ValidateRetryFailureRequest(r RetryFailureRequest) error {
	return r.validate()
}

func (s RetrySchedule) validate() error {
	if !validateRetryKey(s.RunID) || !validateRetryKey(s.Phase) || !validRetryControllerState(domain.State(s.ControllerState)) || s.AttemptCount < 1 || s.MaxAttempts < 1 || s.MaxAttempts > 10 || s.InitialDelay <= 0 || s.MaximumDelay < s.InitialDelay || s.MaximumDelay > 24*time.Hour || !validRetryFailureClass(s.FailureClass) || !validRetryReasonCode(s.ReasonCode) || (s.Status != RetryScheduleScheduled && s.Status != RetryScheduleAttention) || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return errors.New("automatic retry schedule is invalid")
	}
	if s.ReasonCode == RetryReasonBudgetExhausted {
		if s.Status != RetryScheduleAttention || s.AttemptCount <= s.MaxAttempts || !retryFailureIsRetryable(s.FailureClass) {
			return errors.New("automatic retry budget evidence is inconsistent")
		}
	} else if s.ReasonCode == RetryReasonOperatorRetry {
		if s.Status != RetryScheduleScheduled || s.AttemptCount <= s.MaxAttempts || !retryFailureIsRetryable(s.FailureClass) {
			return errors.New("operator retry schedule evidence is inconsistent")
		}
	} else if retryReasonForClass(s.FailureClass) != s.ReasonCode {
		return errors.New("automatic retry schedule classification is inconsistent")
	}
	if s.FailureClass == RetryFailureProcessStart {
		if !validRetryProcessEvidenceRef(s.FailureEvidenceRef) {
			return errors.New("process-start retry evidence reference is invalid")
		}
	} else if s.FailureEvidenceRef != "" {
		return errors.New("retry evidence reference does not match the failure class")
	}
	if s.Status == RetryScheduleScheduled && ((s.AttemptCount > s.MaxAttempts && s.ReasonCode != RetryReasonOperatorRetry) || !s.NextEligibleAt.After(s.UpdatedAt) || !s.AttentionAt.IsZero()) {
		return errors.New("scheduled retry evidence is incomplete")
	}
	if s.Status == RetryScheduleAttention && (s.AttentionAt.IsZero() || !s.NextEligibleAt.IsZero() || (s.AttemptCount > s.MaxAttempts && (s.ReasonCode != RetryReasonBudgetExhausted || !retryFailureIsRetryable(s.FailureClass)))) {
		return errors.New("retry attention evidence is incomplete")
	}
	return nil
}

func validRetryProcessEvidenceRef(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || parts[0] != "attempt" && parts[0] != "verification" || parts[1] == "" {
		return false
	}
	for _, char := range parts[1] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func ValidateRetrySchedule(s RetrySchedule) error {
	return s.validate()
}

func validRetryFailureClass(value RetryFailureClass) bool {
	switch value {
	case RetryFailureProcessStart, RetryFailureUnavailable, RetryFailureAuthority, RetryFailureIntegrity, RetryFailureManual, RetryFailureTerminal, RetryFailurePersistence:
		return true
	default:
		return false
	}
}

func validRetryReasonCode(value string) bool {
	switch value {
	case RetryReasonProcessStart, RetryReasonUnavailable, RetryReasonAuthority, RetryReasonIntegrity, RetryReasonManual, RetryReasonTerminal, RetryReasonPersistence, RetryReasonBudgetExhausted, RetryReasonOperatorRetry:
		return true
	default:
		return false
	}
}

func validRetryControllerState(state domain.State) bool {
	switch state {
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting,
		domain.StateAwaitingHumanDecision, domain.StateVerifying, domain.StateFreshReview, domain.StateApprovalReady,
		domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StateRepairing,
		domain.StatePROpen, domain.StateReconcilingReviews, domain.StateReplyingReviewFeedback,
		domain.StateAwaitingHumanApproval, domain.StateMerging, domain.StateAwaitingGitHubMergeability,
		domain.StateAwaitingLinearCompletion, domain.StateCleaning, domain.StateCompleted, domain.StateFailed,
		domain.StateRejected, domain.StateManualIntervention:
		return true
	default:
		return false
	}
}

func ValidateRetryControllerState(state domain.State) error {
	if !validRetryControllerState(state) {
		return errors.New("automatic retry controller state is invalid")
	}
	return nil
}

func automaticRetryStateStop(state domain.State) (RetryFailureClass, string, bool) {
	switch state {
	case domain.StateFailed, domain.StateRejected:
		return RetryFailureTerminal, RetryReasonTerminal, true
	case domain.StateAwaitingHumanDecision, domain.StateAwaitingHumanApproval, domain.StateManualIntervention:
		return RetryFailureManual, RetryReasonManual, true
	default:
		return "", "", false
	}
}

func retryFailureIsRetryable(class RetryFailureClass) bool {
	return class == RetryFailureProcessStart || class == RetryFailureUnavailable
}

func RetryFailureIsRetryable(class RetryFailureClass) bool {
	return retryFailureIsRetryable(class)
}

func retryReasonForClass(class RetryFailureClass) string {
	switch class {
	case RetryFailureProcessStart:
		return RetryReasonProcessStart
	case RetryFailureUnavailable:
		return RetryReasonUnavailable
	case RetryFailureAuthority:
		return RetryReasonAuthority
	case RetryFailureIntegrity:
		return RetryReasonIntegrity
	case RetryFailureManual:
		return RetryReasonManual
	case RetryFailurePersistence:
		return RetryReasonPersistence
	default:
		return RetryReasonTerminal
	}
}

// AutomaticRetryPhaseForRun binds scheduling to the persisted state. It is
// intentionally not a caller-provided free-form action name.
func AutomaticRetryPhaseForRun(run Run) string {
	return "state_" + string(run.State)
}

// ClassifyAutomaticRetryFailure maps only controller-owned evidence. Adapter
// errors without a typed class fail closed as terminal rather than retrying
// arbitrary or potentially authority-changing failures.
func ClassifyAutomaticRetryFailure(err error) (RetryFailureClass, string) {
	if err == nil {
		return RetryFailureTerminal, RetryReasonTerminal
	}
	var service *ServiceError
	if errors.As(err, &service) {
		switch service.Category {
		case ErrorUnavailable:
			return RetryFailureUnavailable, RetryReasonUnavailable
		case ErrorConflict:
			return RetryFailureAuthority, RetryReasonAuthority
		case ErrorInvalidInput, ErrorNotFound:
			return RetryFailureAuthority, RetryReasonAuthority
		}
	}
	var evidence interface{ AutomaticRetryFailureClass() string }
	if errors.As(err, &evidence) {
		class := RetryFailureClass(evidence.AutomaticRetryFailureClass())
		if validRetryFailureClass(class) {
			return class, retryReasonForClass(class)
		}
	}
	return RetryFailureTerminal, RetryReasonTerminal
}

func formatRetryScheduleConflict(runID, phase string) error {
	return fmt.Errorf("automatic retry schedule authority changed for %s/%s", runID, phase)
}

func retryAttentionScope(schedule RetrySchedule) string {
	return schedule.RunID
}

func retryAttentionDigest(schedule RetrySchedule) string {
	sum := sha256.Sum256([]byte(schedule.RunID + "\x00" + schedule.Phase + "\x00" + fmt.Sprint(schedule.AttemptCount) + "\x00" + string(schedule.FailureClass) + "\x00" + schedule.ReasonCode))
	return hex.EncodeToString(sum[:])
}
