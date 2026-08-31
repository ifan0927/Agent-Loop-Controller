package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	WorkerHeartbeatLegacySchemaVersion   = 1
	WorkerHeartbeatPreviousSchemaVersion = 2
	WorkerHeartbeatSchemaVersion         = 3
	WorkerHeartbeatCadence               = 15 * time.Second
	WorkerHeartbeatStaleAfter            = 45 * time.Second
)

type RuntimeLiveness string

const (
	RuntimeLivenessFresh    RuntimeLiveness = "fresh"
	RuntimeLivenessStale    RuntimeLiveness = "stale"
	RuntimeLivenessOffline  RuntimeLiveness = "offline"
	RuntimeLivenessUnknown  RuntimeLiveness = "unknown"
	RuntimeLivenessConflict RuntimeLiveness = "conflict"
)

type RuntimeActivity string

const (
	RuntimeActivityRunning  RuntimeActivity = "running"
	RuntimeActivityDriving  RuntimeActivity = "driving"
	RuntimeActivityParked   RuntimeActivity = "parked"
	RuntimeActivityStopping RuntimeActivity = "stopping"
	RuntimeActivityUnknown  RuntimeActivity = "unknown"
)

const (
	RuntimeCycleOnboardingAccepted           = "onboarding_accepted"
	RuntimeCycleOnboardingRunning            = "onboarding_running"
	RuntimeCycleOnboardingWaitingForOperator = "onboarding_waiting_for_operator"
	RuntimeCycleOnboardingConflict           = "onboarding_conflict"
	RuntimeCycleOnboardingReadyDisabled      = "onboarding_ready_disabled"
)

type onboardingRuntimeCycleDefinition struct {
	status   domain.OnboardingStatus
	outcome  string
	activity RuntimeActivity
}

var onboardingRuntimeCycleDefinitions = [...]onboardingRuntimeCycleDefinition{
	{status: domain.OnboardingAccepted, outcome: RuntimeCycleOnboardingAccepted, activity: RuntimeActivityRunning},
	{status: domain.OnboardingRunning, outcome: RuntimeCycleOnboardingRunning, activity: RuntimeActivityRunning},
	{status: domain.OnboardingWaitingForOperator, outcome: RuntimeCycleOnboardingWaitingForOperator, activity: RuntimeActivityParked},
	{status: domain.OnboardingConflict, outcome: RuntimeCycleOnboardingConflict, activity: RuntimeActivityParked},
	{status: domain.OnboardingReadyDisabled, outcome: RuntimeCycleOnboardingReadyDisabled, activity: RuntimeActivityRunning},
}

// OnboardingRuntimeCycleOutcome returns the exact heartbeat outcome for a
// status that can legally cross the automatic onboarding dispatch boundary.
func OnboardingRuntimeCycleOutcome(status domain.OnboardingStatus) (string, bool) {
	for _, definition := range onboardingRuntimeCycleDefinitions {
		if definition.status == status {
			return definition.outcome, true
		}
	}
	return "", false
}

// RuntimeCycleOnboardingActivity classifies only the closed onboarding cycle
// vocabulary. Unknown prefixes and non-dispatch onboarding states fail closed.
func RuntimeCycleOnboardingActivity(outcome string) (RuntimeActivity, bool) {
	for _, definition := range onboardingRuntimeCycleDefinitions {
		if definition.outcome == outcome {
			return definition.activity, true
		}
	}
	return "", false
}

type RuntimeObservationReason string

const (
	RuntimeReasonHeartbeatFresh             RuntimeObservationReason = "heartbeat_fresh"
	RuntimeReasonHeartbeatStale             RuntimeObservationReason = "heartbeat_stale"
	RuntimeReasonHeartbeatAbsent            RuntimeObservationReason = "heartbeat_absent"
	RuntimeReasonLegacyActivitySnapshot     RuntimeObservationReason = "legacy_activity_snapshot"
	RuntimeReasonHeartbeatUnavailable       RuntimeObservationReason = "heartbeat_unavailable"
	RuntimeReasonHeartbeatInvalid           RuntimeObservationReason = "heartbeat_invalid"
	RuntimeReasonHeartbeatTimestampConflict RuntimeObservationReason = "heartbeat_timestamp_conflict"
	RuntimeReasonWorkerProcessAbsent        RuntimeObservationReason = "worker_process_absent"
	RuntimeReasonProcessIdentityUnavailable RuntimeObservationReason = "process_identity_unavailable"
	RuntimeReasonProcessIdentityConflict    RuntimeObservationReason = "process_identity_conflict"
)

type RuntimeHeartbeatReadState string

const (
	RuntimeHeartbeatCurrent     RuntimeHeartbeatReadState = "current"
	RuntimeHeartbeatLegacy      RuntimeHeartbeatReadState = "legacy"
	RuntimeHeartbeatAbsent      RuntimeHeartbeatReadState = "absent"
	RuntimeHeartbeatUnavailable RuntimeHeartbeatReadState = "unavailable"
	RuntimeHeartbeatInvalid     RuntimeHeartbeatReadState = "invalid"
)

// RuntimeHeartbeatEvidence is private Controller evidence. ProcessID and
// ProcessStartIdentity are intentionally absent from RuntimeObservation.
type RuntimeHeartbeatEvidence struct {
	SchemaVersion                int
	WorkerInstanceID             string
	ProcessID                    int
	ProcessStartIdentity         string
	BuildIdentity                string
	LoadedConfigurationDigest    string
	Activity                     RuntimeActivity
	PreviousActivity             RuntimeActivity
	Cycles                       int
	LastCycleOutcome             string
	LastQueueDecisionReason      string
	LastCycleCompletedAt         time.Time
	NextAdmissionEvaluationAt    time.Time
	ObservedAt                   time.Time
	SupervisorProcessUnavailable bool
	SupervisorProcessConflict    bool
}

type RuntimeHeartbeatReader interface {
	ReadRuntimeHeartbeat(context.Context) (RuntimeHeartbeatEvidence, RuntimeHeartbeatReadState)
}

type RuntimeProcessState string

const (
	RuntimeProcessPresent     RuntimeProcessState = "present"
	RuntimeProcessAbsent      RuntimeProcessState = "absent"
	RuntimeProcessUnavailable RuntimeProcessState = "unavailable"
)

type RuntimeProcessObservation struct {
	State         RuntimeProcessState
	StartIdentity string
}

type RuntimeProcessObserver interface {
	ObserveRuntimeProcess(context.Context, int) RuntimeProcessObservation
}

type RuntimeObservation struct {
	Liveness                  RuntimeLiveness          `json:"liveness"`
	Activity                  RuntimeActivity          `json:"activity"`
	PreviousActivity          RuntimeActivity          `json:"previous_activity,omitempty"`
	WorkerInstanceID          string                   `json:"worker_instance_id,omitempty"`
	BuildIdentity             string                   `json:"build_identity,omitempty"`
	LoadedConfigurationDigest string                   `json:"loaded_configuration_digest,omitempty"`
	LastObservedAt            *time.Time               `json:"last_observed_at,omitempty"`
	HeartbeatAgeSeconds       *int64                   `json:"heartbeat_age_seconds,omitempty"`
	Reason                    RuntimeObservationReason `json:"reason"`
	AdmissionCadence          RuntimeAdmissionCadence  `json:"admission_cadence"`
}

type RuntimeAdmissionCadenceState string

const (
	RuntimeAdmissionCadenceKnown   RuntimeAdmissionCadenceState = "known"
	RuntimeAdmissionCadenceUnknown RuntimeAdmissionCadenceState = "unknown"
)

// RuntimeAdmissionCadence is a sanitized projection of worker-owned cadence.
// Schema-v2 heartbeat files remain useful for liveness but cannot establish
// these newer fields.
type RuntimeAdmissionCadence struct {
	State                     RuntimeAdmissionCadenceState `json:"state"`
	LastCycleOutcome          string                       `json:"last_cycle_outcome,omitempty"`
	LastQueueDecisionReason   string                       `json:"last_queue_decision_reason,omitempty"`
	LastCycleCompletedAt      *time.Time                   `json:"last_cycle_completed_at,omitempty"`
	NextAdmissionEvaluationAt *time.Time                   `json:"next_admission_evaluation_at,omitempty"`
}

type RuntimeObservationService struct {
	reader     RuntimeHeartbeatReader
	processes  RuntimeProcessObserver
	authorizer *AuthorizationService
}

func NewRuntimeObservationService(reader RuntimeHeartbeatReader, processes RuntimeProcessObserver, authorizer *AuthorizationService) (*RuntimeObservationService, error) {
	if reader == nil || processes == nil || authorizer == nil {
		return nil, errors.New("runtime observation dependencies are required")
	}
	return &RuntimeObservationService{reader: reader, processes: processes, authorizer: authorizer}, nil
}

func NewConfigurationRuntimeObservationService(reader RuntimeHeartbeatReader, processes RuntimeProcessObserver) (*RuntimeObservationService, error) {
	if reader == nil || processes == nil {
		return nil, errors.New("configuration runtime observation dependencies are required")
	}
	return &RuntimeObservationService{reader: reader, processes: processes}, nil
}

func (s *RuntimeObservationService) Observe(ctx context.Context, requester Requester, now time.Time) (RuntimeObservation, error) {
	if s == nil || s.authorizer == nil {
		return RuntimeObservation{}, hiddenTargetError()
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RuntimeObservation{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil || !scopes.HasController() {
		return RuntimeObservation{}, hiddenTargetError()
	}
	if err := ctx.Err(); err != nil {
		return RuntimeObservation{}, classifyServiceError(err)
	}
	evidence, state := s.reader.ReadRuntimeHeartbeat(ctx)
	return s.classify(ctx, evidence, state, now.UTC()), nil
}

// ObserveConfigurationRuntime is Controller-internal evidence collection for
// configuration convergence and admission fencing. Presentation queries must
// continue through Observe so requester authorization remains mandatory.
func (s *RuntimeObservationService) ObserveConfigurationRuntime(ctx context.Context, now time.Time) (RuntimeObservation, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeObservation{}, classifyServiceError(err)
	}
	evidence, state := s.reader.ReadRuntimeHeartbeat(ctx)
	return s.classify(ctx, evidence, state, now.UTC()), nil
}

func (s *RuntimeObservationService) classify(ctx context.Context, evidence RuntimeHeartbeatEvidence, state RuntimeHeartbeatReadState, now time.Time) RuntimeObservation {
	legacy := false
	switch state {
	case RuntimeHeartbeatAbsent:
		return runtimeObservation(RuntimeLivenessOffline, RuntimeActivityUnknown, RuntimeReasonHeartbeatAbsent)
	case RuntimeHeartbeatUnavailable:
		return runtimeObservation(RuntimeLivenessUnknown, RuntimeActivityUnknown, RuntimeReasonHeartbeatUnavailable)
	case RuntimeHeartbeatInvalid:
		return runtimeObservation(RuntimeLivenessUnknown, RuntimeActivityUnknown, RuntimeReasonHeartbeatInvalid)
	case RuntimeHeartbeatLegacy:
		if !validRuntimeHeartbeatEvidence(evidence, true) {
			return runtimeObservation(RuntimeLivenessUnknown, RuntimeActivityUnknown, RuntimeReasonHeartbeatInvalid)
		}
		legacy = true
	case RuntimeHeartbeatCurrent:
		if !validRuntimeHeartbeatEvidence(evidence, false) {
			return runtimeObservation(RuntimeLivenessUnknown, RuntimeActivityUnknown, RuntimeReasonHeartbeatInvalid)
		}
	default:
		return runtimeObservation(RuntimeLivenessUnknown, RuntimeActivityUnknown, RuntimeReasonHeartbeatInvalid)
	}

	observation := runtimeObservation(RuntimeLivenessUnknown, evidence.Activity, RuntimeReasonHeartbeatInvalid)
	observation.PreviousActivity = evidence.PreviousActivity
	observation.WorkerInstanceID = evidence.WorkerInstanceID
	if !legacy {
		observation.BuildIdentity = evidence.BuildIdentity
		observation.LoadedConfigurationDigest = evidence.LoadedConfigurationDigest
	}
	observation.LastObservedAt = runtimeObservedAt(evidence.ObservedAt)
	if evidence.SchemaVersion == WorkerHeartbeatSchemaVersion {
		observation.AdmissionCadence = RuntimeAdmissionCadence{
			State:                     RuntimeAdmissionCadenceKnown,
			LastCycleOutcome:          evidence.LastCycleOutcome,
			LastQueueDecisionReason:   evidence.LastQueueDecisionReason,
			LastCycleCompletedAt:      runtimeOptionalObservedAt(evidence.LastCycleCompletedAt),
			NextAdmissionEvaluationAt: runtimeOptionalObservedAt(evidence.NextAdmissionEvaluationAt),
		}
	}
	if evidence.ObservedAt.After(now) {
		observation.Liveness = RuntimeLivenessConflict
		observation.Reason = RuntimeReasonHeartbeatTimestampConflict
		return observation
	}
	age := now.Sub(evidence.ObservedAt)
	if !legacy {
		seconds := int64(age / time.Second)
		observation.HeartbeatAgeSeconds = &seconds
	}
	if evidence.SupervisorProcessUnavailable {
		observation.Liveness = RuntimeLivenessUnknown
		observation.Reason = RuntimeReasonProcessIdentityUnavailable
		return observation
	}
	if evidence.SupervisorProcessConflict {
		observation.Liveness = RuntimeLivenessConflict
		observation.Reason = RuntimeReasonProcessIdentityConflict
		return observation
	}
	process := s.processes.ObserveRuntimeProcess(ctx, evidence.ProcessID)
	switch process.State {
	case RuntimeProcessAbsent:
		observation.Liveness = RuntimeLivenessOffline
		observation.Reason = RuntimeReasonWorkerProcessAbsent
		return observation
	case RuntimeProcessUnavailable:
		observation.Liveness = RuntimeLivenessUnknown
		observation.Reason = RuntimeReasonProcessIdentityUnavailable
		return observation
	case RuntimeProcessPresent:
		if process.StartIdentity == "" {
			observation.Liveness = RuntimeLivenessUnknown
			observation.Reason = RuntimeReasonProcessIdentityUnavailable
			return observation
		}
		if process.StartIdentity != evidence.ProcessStartIdentity {
			observation.Liveness = RuntimeLivenessConflict
			observation.Reason = RuntimeReasonProcessIdentityConflict
			return observation
		}
	default:
		observation.Liveness = RuntimeLivenessUnknown
		observation.Reason = RuntimeReasonProcessIdentityUnavailable
		return observation
	}
	if legacy {
		observation.Liveness = RuntimeLivenessUnknown
		observation.Reason = RuntimeReasonLegacyActivitySnapshot
		return observation
	}
	if age > WorkerHeartbeatStaleAfter {
		observation.Liveness = RuntimeLivenessStale
		observation.Reason = RuntimeReasonHeartbeatStale
		return observation
	}
	observation.Liveness = RuntimeLivenessFresh
	observation.Reason = RuntimeReasonHeartbeatFresh
	return observation
}

func runtimeObservation(liveness RuntimeLiveness, activity RuntimeActivity, reason RuntimeObservationReason) RuntimeObservation {
	return RuntimeObservation{Liveness: liveness, Activity: activity, Reason: reason, AdmissionCadence: RuntimeAdmissionCadence{State: RuntimeAdmissionCadenceUnknown}}
}

func runtimeObservedAt(value time.Time) *time.Time {
	observed := value.UTC()
	return &observed
}

func runtimeOptionalObservedAt(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return runtimeObservedAt(value)
}

func validRuntimeHeartbeatEvidence(evidence RuntimeHeartbeatEvidence, legacy bool) bool {
	validSchema := evidence.SchemaVersion == WorkerHeartbeatSchemaVersion || evidence.SchemaVersion == WorkerHeartbeatPreviousSchemaVersion
	if legacy {
		validSchema = evidence.SchemaVersion == WorkerHeartbeatLegacySchemaVersion
	}
	if !validSchema || !safeRuntimeIdentity(evidence.WorkerInstanceID, 128) || evidence.ProcessID < 1 || !safeRuntimeIdentity(evidence.ProcessStartIdentity, 128) || evidence.Cycles < 0 || evidence.ObservedAt.IsZero() || !validRuntimeActivity(evidence.Activity) {
		return false
	}
	if evidence.PreviousActivity != "" && !validRuntimeActivity(evidence.PreviousActivity) {
		return false
	}
	if legacy {
		return evidence.BuildIdentity == "" && evidence.LoadedConfigurationDigest == ""
	}
	if !safeRuntimeIdentity(evidence.BuildIdentity, 128) || !validRuntimeDigest(evidence.LoadedConfigurationDigest) {
		return false
	}
	if evidence.SchemaVersion == WorkerHeartbeatPreviousSchemaVersion {
		return evidence.LastCycleOutcome == "" && evidence.LastQueueDecisionReason == "" && evidence.LastCycleCompletedAt.IsZero() && evidence.NextAdmissionEvaluationAt.IsZero()
	}
	if evidence.LastCycleOutcome == "" {
		return evidence.LastQueueDecisionReason == "" && evidence.LastCycleCompletedAt.IsZero()
	}
	return evidence.Cycles > 0 && ValidRuntimeCycleOutcome(evidence.LastCycleOutcome) && (evidence.LastQueueDecisionReason == "" || ValidRuntimeQueueDecisionReason(evidence.LastQueueDecisionReason)) && !evidence.LastCycleCompletedAt.IsZero()
}

func ValidRuntimeCycleOutcome(value string) bool {
	switch value {
	case LinearTodoDispatchNoCandidate, LinearTodoDispatchDriven, LinearTodoDispatchAttention, LinearTodoDispatchWaiting, LinearTodoDispatchRetryWait, LinearTodoDispatchRetryScheduled:
		return true
	}
	_, valid := RuntimeCycleOnboardingActivity(value)
	return valid
}

func ValidRuntimeQueueDecisionReason(value string) bool {
	switch value {
	case LinearTodoQueueDecisionNoCandidate, LinearTodoQueueDecisionActiveRun, LinearTodoQueueDecisionIncompleteScan,
		LinearTodoQueueDecisionSelectedPriority, LinearTodoQueueDecisionSchedulerAttention, LinearTodoQueueDecisionRetryAttention,
		LinearTodoQueueDecisionCapacityFull, LinearTodoQueueDecisionAdmissionBusy, LinearTodoQueueDecisionNoEligibleCandidate,
		LinearTodoQueueDecisionConfigurationFenced, QueueCandidateRepositoryIneligible:
		return true
	default:
		return false
	}
}

func validRuntimeActivity(activity RuntimeActivity) bool {
	switch activity {
	case RuntimeActivityRunning, RuntimeActivityDriving, RuntimeActivityParked, RuntimeActivityStopping:
		return true
	default:
		return false
	}
}

func safeRuntimeIdentity(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.:+", character) {
			continue
		}
		return false
	}
	return true
}

func validRuntimeDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
