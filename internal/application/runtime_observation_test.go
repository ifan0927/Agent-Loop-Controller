package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type runtimeHeartbeatFixture struct {
	evidence RuntimeHeartbeatEvidence
	state    RuntimeHeartbeatReadState
	reads    int
}

func (f *runtimeHeartbeatFixture) ReadRuntimeHeartbeat(context.Context) (RuntimeHeartbeatEvidence, RuntimeHeartbeatReadState) {
	f.reads++
	return f.evidence, f.state
}

type runtimeProcessFixture struct {
	observation RuntimeProcessObservation
	reads       int
}

func (f *runtimeProcessFixture) ObserveRuntimeProcess(context.Context, int) RuntimeProcessObservation {
	f.reads++
	return f.observation
}

func runtimeObservationFixture(t *testing.T, evidence RuntimeHeartbeatEvidence, state RuntimeHeartbeatReadState, process RuntimeProcessObservation) (*RuntimeObservationService, *runtimeHeartbeatFixture, *runtimeProcessFixture, Requester) {
	t.Helper()
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	reader := &runtimeHeartbeatFixture{evidence: evidence, state: state}
	processes := &runtimeProcessFixture{observation: process}
	service, err := NewRuntimeObservationService(reader, processes, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	return service, reader, processes, requesterForUser(operator)
}

func currentRuntimeEvidence(now time.Time) RuntimeHeartbeatEvidence {
	return RuntimeHeartbeatEvidence{
		SchemaVersion:             WorkerHeartbeatSchemaVersion,
		WorkerInstanceID:          "worker-7",
		ProcessID:                 123,
		ProcessStartIdentity:      "darwin:100:2",
		BuildIdentity:             "0.1.0-dev",
		LoadedConfigurationDigest: strings.Repeat("a", 64),
		Activity:                  RuntimeActivityParked,
		PreviousActivity:          RuntimeActivityDriving,
		Cycles:                    4,
		ObservedAt:                now,
	}
}

func TestRuntimeCycleOutcomesIncludeOnlyLegalOnboardingDispatchStatuses(t *testing.T) {
	tests := []struct {
		status   domain.OnboardingStatus
		outcome  string
		activity RuntimeActivity
	}{
		{status: domain.OnboardingAccepted, outcome: RuntimeCycleOnboardingAccepted, activity: RuntimeActivityRunning},
		{status: domain.OnboardingRunning, outcome: RuntimeCycleOnboardingRunning, activity: RuntimeActivityRunning},
		{status: domain.OnboardingWaitingForOperator, outcome: RuntimeCycleOnboardingWaitingForOperator, activity: RuntimeActivityParked},
		{status: domain.OnboardingConflict, outcome: RuntimeCycleOnboardingConflict, activity: RuntimeActivityParked},
		{status: domain.OnboardingReadyDisabled, outcome: RuntimeCycleOnboardingReadyDisabled, activity: RuntimeActivityRunning},
	}
	for _, test := range tests {
		outcome, valid := OnboardingRuntimeCycleOutcome(test.status)
		activity, classified := RuntimeCycleOnboardingActivity(test.outcome)
		if !valid || outcome != test.outcome || !classified || activity != test.activity || !ValidRuntimeCycleOutcome(test.outcome) {
			t.Fatalf("status=%s outcome=%q valid=%t activity=%s classified=%t", test.status, outcome, valid, activity, classified)
		}
	}
	for _, status := range []domain.OnboardingStatus{domain.OnboardingOpened, domain.OnboardingPreflightReady, domain.OnboardingCancelled, "unknown"} {
		if outcome, valid := OnboardingRuntimeCycleOutcome(status); valid || outcome != "" {
			t.Fatalf("non-dispatch status=%s outcome=%q valid=%t", status, outcome, valid)
		}
	}
	for _, outcome := range []string{"onboarding_opened", "onboarding_preflight_ready", "onboarding_cancelled", "onboarding_unknown", "onboarding_"} {
		if activity, valid := RuntimeCycleOnboardingActivity(outcome); valid || activity != "" || ValidRuntimeCycleOutcome(outcome) {
			t.Fatalf("unknown outcome=%q activity=%s valid=%t", outcome, activity, valid)
		}
	}
}

func TestRuntimeObservationRequiresControllerScopeBeforeReadingEvidence(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	service, reader, processes, _ := runtimeObservationFixture(t, currentRuntimeEvidence(now), RuntimeHeartbeatCurrent, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: "darwin:100:2"})
	_, err := service.Observe(context.Background(), Requester{ID: "other", Kind: "github_login", DatabaseID: 8, NodeID: "U_8", ActorType: "User"}, now)
	if err == nil || reader.reads != 0 || processes.reads != 0 {
		t.Fatalf("err=%v heartbeat_reads=%d process_reads=%d", err, reader.reads, processes.reads)
	}
}

func TestRuntimeObservationFreshnessBoundaryAndParkedActivityAreIndependent(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 45, 0, time.UTC)
	evidence := currentRuntimeEvidence(now.Add(-WorkerHeartbeatStaleAfter))
	service, _, _, requester := runtimeObservationFixture(t, evidence, RuntimeHeartbeatCurrent, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: evidence.ProcessStartIdentity})
	observation, err := service.Observe(context.Background(), requester, now)
	if err != nil || observation.Liveness != RuntimeLivenessFresh || observation.Activity != RuntimeActivityParked || observation.HeartbeatAgeSeconds == nil || *observation.HeartbeatAgeSeconds != 45 {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	evidence.ObservedAt = evidence.ObservedAt.Add(-time.Nanosecond)
	service, _, _, requester = runtimeObservationFixture(t, evidence, RuntimeHeartbeatCurrent, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: evidence.ProcessStartIdentity})
	observation, err = service.Observe(context.Background(), requester, now)
	if err != nil || observation.Liveness != RuntimeLivenessStale || observation.Activity != RuntimeActivityParked || observation.Reason != RuntimeReasonHeartbeatStale {
		t.Fatalf("stale observation=%+v err=%v", observation, err)
	}
}

func TestRuntimeObservationFailsClosedForProcessAndClockContradictions(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		evidence RuntimeHeartbeatEvidence
		process  RuntimeProcessObservation
		liveness RuntimeLiveness
		reason   RuntimeObservationReason
	}{
		{name: "process absent", evidence: currentRuntimeEvidence(now), process: RuntimeProcessObservation{State: RuntimeProcessAbsent}, liveness: RuntimeLivenessOffline, reason: RuntimeReasonWorkerProcessAbsent},
		{name: "supervisor PID mismatch", evidence: func() RuntimeHeartbeatEvidence {
			evidence := currentRuntimeEvidence(now)
			evidence.SupervisorProcessConflict = true
			return evidence
		}(), process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: "darwin:100:2"}, liveness: RuntimeLivenessConflict, reason: RuntimeReasonProcessIdentityConflict},
		{name: "supervisor PID unavailable", evidence: func() RuntimeHeartbeatEvidence {
			evidence := currentRuntimeEvidence(now)
			evidence.SupervisorProcessUnavailable = true
			return evidence
		}(), process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: "darwin:100:2"}, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonProcessIdentityUnavailable},
		{name: "PID reused", evidence: currentRuntimeEvidence(now), process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: "darwin:200:4"}, liveness: RuntimeLivenessConflict, reason: RuntimeReasonProcessIdentityConflict},
		{name: "identity unavailable", evidence: currentRuntimeEvidence(now), process: RuntimeProcessObservation{State: RuntimeProcessUnavailable}, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonProcessIdentityUnavailable},
		{name: "future timestamp", evidence: currentRuntimeEvidence(now.Add(time.Nanosecond)), process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: "darwin:100:2"}, liveness: RuntimeLivenessConflict, reason: RuntimeReasonHeartbeatTimestampConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, requester := runtimeObservationFixture(t, test.evidence, RuntimeHeartbeatCurrent, test.process)
			observation, err := service.Observe(context.Background(), requester, now)
			if err != nil || observation.Liveness != test.liveness || observation.Reason != test.reason {
				t.Fatalf("observation=%+v err=%v", observation, err)
			}
		})
	}
}

func TestRuntimeObservationDistinguishesLegacyMissingUnsafeAndInvalidEvidence(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	legacy := currentRuntimeEvidence(now)
	legacy.SchemaVersion = WorkerHeartbeatLegacySchemaVersion
	legacy.BuildIdentity = ""
	legacy.LoadedConfigurationDigest = ""
	tests := []struct {
		name         string
		evidence     RuntimeHeartbeatEvidence
		state        RuntimeHeartbeatReadState
		liveness     RuntimeLiveness
		reason       RuntimeObservationReason
		processReads int
	}{
		{name: "legacy", evidence: legacy, state: RuntimeHeartbeatLegacy, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonLegacyActivitySnapshot, processReads: 1},
		{name: "absent", state: RuntimeHeartbeatAbsent, liveness: RuntimeLivenessOffline, reason: RuntimeReasonHeartbeatAbsent},
		{name: "unsafe", state: RuntimeHeartbeatUnavailable, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonHeartbeatUnavailable},
		{name: "invalid", state: RuntimeHeartbeatInvalid, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonHeartbeatInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _, processes, requester := runtimeObservationFixture(t, test.evidence, test.state, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: test.evidence.ProcessStartIdentity})
			observation, err := service.Observe(context.Background(), requester, now)
			if err != nil || observation.Liveness != test.liveness || observation.Reason != test.reason || processes.reads != test.processReads {
				t.Fatalf("observation=%+v process_reads=%d err=%v", observation, processes.reads, err)
			}
		})
	}
}

func TestRuntimeObservationValidatesLegacyProcessIdentityWithoutPromotingFreshness(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	legacy := currentRuntimeEvidence(now)
	legacy.SchemaVersion = WorkerHeartbeatLegacySchemaVersion
	legacy.BuildIdentity = ""
	legacy.LoadedConfigurationDigest = ""
	tests := []struct {
		name     string
		mutate   func(*RuntimeHeartbeatEvidence)
		process  RuntimeProcessObservation
		liveness RuntimeLiveness
		reason   RuntimeObservationReason
	}{
		{name: "exact identity remains legacy unknown", process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: legacy.ProcessStartIdentity}, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonLegacyActivitySnapshot},
		{name: "process absent", process: RuntimeProcessObservation{State: RuntimeProcessAbsent}, liveness: RuntimeLivenessOffline, reason: RuntimeReasonWorkerProcessAbsent},
		{name: "PID reused", process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: "darwin:200:4"}, liveness: RuntimeLivenessConflict, reason: RuntimeReasonProcessIdentityConflict},
		{name: "identity unavailable", process: RuntimeProcessObservation{State: RuntimeProcessUnavailable}, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonProcessIdentityUnavailable},
		{name: "supervisor mismatch", mutate: func(evidence *RuntimeHeartbeatEvidence) { evidence.SupervisorProcessConflict = true }, process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: legacy.ProcessStartIdentity}, liveness: RuntimeLivenessConflict, reason: RuntimeReasonProcessIdentityConflict},
		{name: "supervisor PID unavailable", mutate: func(evidence *RuntimeHeartbeatEvidence) { evidence.SupervisorProcessUnavailable = true }, process: RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: legacy.ProcessStartIdentity}, liveness: RuntimeLivenessUnknown, reason: RuntimeReasonProcessIdentityUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := legacy
			if test.mutate != nil {
				test.mutate(&evidence)
			}
			service, _, _, requester := runtimeObservationFixture(t, evidence, RuntimeHeartbeatLegacy, test.process)
			observation, err := service.Observe(context.Background(), requester, now)
			if err != nil || observation.Liveness != test.liveness || observation.Reason != test.reason {
				t.Fatalf("observation=%+v err=%v", observation, err)
			}
			if observation.Liveness == RuntimeLivenessFresh || observation.HeartbeatAgeSeconds != nil || observation.BuildIdentity != "" || observation.LoadedConfigurationDigest != "" {
				t.Fatalf("legacy evidence was promoted or disclosed current-only fields: %+v", observation)
			}
		})
	}
}

func TestRuntimeObservationProjectionDoesNotDisclosePrivateAuthority(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	evidence := currentRuntimeEvidence(now)
	service, _, _, requester := runtimeObservationFixture(t, evidence, RuntimeHeartbeatCurrent, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: evidence.ProcessStartIdentity})
	observation, err := service.Observe(context.Background(), requester, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"process_id", "process_start", "uid", "path", "launchctl", "stderr", "credential", "darwin:100:2", "123"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("projection disclosed %q: %s", forbidden, raw)
		}
	}
}

func TestRuntimeObservationProjectsCurrentCadenceAndKeepsSchemaV2Unknown(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	evidence := currentRuntimeEvidence(now)
	evidence.LastCycleOutcome = LinearTodoDispatchNoCandidate
	evidence.LastQueueDecisionReason = LinearTodoQueueDecisionNoCandidate
	evidence.LastCycleCompletedAt = now.Add(-time.Second)
	evidence.NextAdmissionEvaluationAt = now.Add(time.Minute)
	service, _, _, requester := runtimeObservationFixture(t, evidence, RuntimeHeartbeatCurrent, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: evidence.ProcessStartIdentity})
	observation, err := service.Observe(context.Background(), requester, now)
	if err != nil || observation.AdmissionCadence.State != RuntimeAdmissionCadenceKnown || observation.AdmissionCadence.LastCycleOutcome != LinearTodoDispatchNoCandidate || observation.AdmissionCadence.LastQueueDecisionReason != LinearTodoQueueDecisionNoCandidate || observation.AdmissionCadence.NextAdmissionEvaluationAt == nil {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}

	evidence.SchemaVersion = WorkerHeartbeatPreviousSchemaVersion
	evidence.LastCycleOutcome = ""
	evidence.LastQueueDecisionReason = ""
	evidence.LastCycleCompletedAt = time.Time{}
	evidence.NextAdmissionEvaluationAt = time.Time{}
	service, _, _, requester = runtimeObservationFixture(t, evidence, RuntimeHeartbeatCurrent, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: evidence.ProcessStartIdentity})
	observation, err = service.Observe(context.Background(), requester, now)
	if err != nil || observation.Liveness != RuntimeLivenessFresh || observation.Activity != evidence.Activity || observation.AdmissionCadence.State != RuntimeAdmissionCadenceUnknown {
		t.Fatalf("schema-v2 observation=%+v err=%v", observation, err)
	}
}
