package application

import (
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRoutineRepositoryAcceptanceConclusionsAreApplicationOwned(t *testing.T) {
	tests := []struct {
		name       string
		projection RepositoryProjection
		conclusion RoutineRepositoryAcceptanceConclusion
		direction  RoutineRepositoryOperatorDirection
	}{
		{name: "accepting new work", projection: routineRepositoryProjection(RepositoryEnabled, domain.RepositoryReady, true, "available"), conclusion: RoutineRepositoryAcceptingNewWork, direction: RoutineRepositoryDirectionNone},
		{name: "ready disabled", projection: routineRepositoryProjection(RepositoryDisabled, domain.RepositoryReady, false, "repository_disabled"), conclusion: RoutineRepositoryReadyDisabled, direction: RoutineRepositoryDirectionEnable},
		{name: "not ready", projection: routineRepositoryProjection(RepositoryEnabled, domain.RepositoryNotReady, false, "verifier_policy_not_ready"), conclusion: RoutineRepositoryNotReady, direction: RoutineRepositoryDirectionResolveReadiness},
		{name: "conflict", projection: routineRepositoryProjection(RepositoryEnabled, domain.RepositoryConflict, false, "profile_authority_conflict"), conclusion: RoutineRepositoryConflict, direction: RoutineRepositoryDirectionResolveConflict},
		{name: "unknown", projection: routineRepositoryProjection(RepositoryEnabled, domain.RepositoryUnknown, false, "configuration_authority_stale"), conclusion: RoutineRepositoryUnknown, direction: RoutineRepositoryDirectionRefreshAuthority},
		{name: "otherwise unavailable", projection: routineRepositoryProjection(RepositoryEnabled, domain.RepositoryReady, false, "repository_busy"), conclusion: RoutineRepositoryUnavailable, direction: RoutineRepositoryDirectionInspectUnavailability},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := routineRepositoryAcceptance(test.projection)
			if result.Conclusion != test.conclusion || result.NextDirection != test.direction || result.ReasonCode == "" {
				t.Fatalf("acceptance=%+v", result)
			}
			if (test.conclusion == RoutineRepositoryConflict || test.conclusion == RoutineRepositoryUnknown) && test.projection.Availability.Available {
				t.Fatalf("unsafe fixture rendered %s as available", test.conclusion)
			}
		})
	}
}

func TestRoutineRepositoryEnableOfferRequiresExactReadyDisabledAuthority(t *testing.T) {
	recheck := &RepositoryRecheckState{Refreshing: true}
	removal := &RepositoryRemovalProjection{State: "removal_pending_convergence"}
	tests := []struct {
		name       string
		projection RepositoryProjection
		offered    bool
	}{
		{name: "ready disabled", projection: routineRepositoryProjection(RepositoryDisabled, domain.RepositoryReady, false, "repository_disabled"), offered: true},
		{name: "already enabled", projection: routineRepositoryProjection(RepositoryEnabled, domain.RepositoryReady, true, "available")},
		{name: "not ready", projection: routineRepositoryProjection(RepositoryDisabled, domain.RepositoryNotReady, false, "not_ready")},
		{name: "conflict", projection: routineRepositoryProjection(RepositoryDisabled, domain.RepositoryConflict, false, "conflict")},
		{name: "unknown", projection: routineRepositoryProjection(RepositoryDisabled, domain.RepositoryUnknown, false, "unknown")},
		{name: "recheck active", projection: func() RepositoryProjection {
			value := routineRepositoryProjection(RepositoryDisabled, domain.RepositoryReady, false, "readiness_recheck_in_progress")
			value.Recheck = recheck
			return value
		}()},
		{name: "removal pending", projection: func() RepositoryProjection {
			value := routineRepositoryProjection(RepositoryDisabled, domain.RepositoryReady, false, removal.State)
			value.Removal = removal
			return value
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actions := routineRepositoryLegalNextActions(test.projection)
			if got := len(actions) == 1 && actions[0] == RoutineRepositoryActionEnable; got != test.offered {
				t.Fatalf("actions=%v offered=%t", actions, test.offered)
			}
		})
	}
}

func routineRepositoryProjection(intent RepositoryLifecycleIntent, readiness domain.RepositoryReadinessStatus, available bool, reason string) RepositoryProjection {
	return RepositoryProjection{
		Lifecycle:    RepositoryLifecycle{Intent: intent},
		Readiness:    RepositoryReadinessSnapshot{Status: readiness, ReasonCode: reason},
		Availability: RepositoryAvailability{Available: available, ReasonCode: reason},
	}
}
