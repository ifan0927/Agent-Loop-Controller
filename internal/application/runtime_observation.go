package application

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	WorkerHeartbeatLegacySchemaVersion = 1
	WorkerHeartbeatSchemaVersion       = 2
	WorkerHeartbeatCadence             = 15 * time.Second
	WorkerHeartbeatStaleAfter          = 45 * time.Second
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

func (s *RuntimeObservationService) Observe(ctx context.Context, requester Requester, now time.Time) (RuntimeObservation, error) {
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
	return RuntimeObservation{Liveness: liveness, Activity: activity, Reason: reason}
}

func runtimeObservedAt(value time.Time) *time.Time {
	observed := value.UTC()
	return &observed
}

func validRuntimeHeartbeatEvidence(evidence RuntimeHeartbeatEvidence, legacy bool) bool {
	expectedSchema := WorkerHeartbeatSchemaVersion
	if legacy {
		expectedSchema = WorkerHeartbeatLegacySchemaVersion
	}
	if evidence.SchemaVersion != expectedSchema || !safeRuntimeIdentity(evidence.WorkerInstanceID, 128) || evidence.ProcessID < 1 || !safeRuntimeIdentity(evidence.ProcessStartIdentity, 128) || evidence.Cycles < 0 || evidence.ObservedAt.IsZero() || !validRuntimeActivity(evidence.Activity) {
		return false
	}
	if evidence.PreviousActivity != "" && !validRuntimeActivity(evidence.PreviousActivity) {
		return false
	}
	if legacy {
		return evidence.BuildIdentity == "" && evidence.LoadedConfigurationDigest == ""
	}
	return safeRuntimeIdentity(evidence.BuildIdentity, 128) && validRuntimeDigest(evidence.LoadedConfigurationDigest)
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
