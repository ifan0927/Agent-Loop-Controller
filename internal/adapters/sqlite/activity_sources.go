package sqlite

import (
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func newRunTransitionActivityEvent(runID string, sequence int64, from, to, reason, evidence, head string, occurredAt time.Time, binding, operationID string, ingestion application.ActivityIngestionClass) application.ActivityEvent {
	sourceDigest := digestActivitySource(strings.Join([]string{runID, strconv.FormatInt(sequence, 10), from, to, reason, evidence, head, formatTime(occurredAt)}, "\x00"))
	return application.NewActivityEvent(application.ActivityEventInput{
		SourceKind: "run_transition", SourceIdentity: runID + ":" + strconv.FormatInt(sequence, 10), SourceEvidenceDigest: sourceDigest,
		Category: application.ActivityRun, EventKind: application.ActivityRunTransition, Actor: application.ActivityActorController,
		Scope: application.ScopeRun, TargetID: runID, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonStateChanged,
		PriorState: from, ResultingState: to, PriorVersion: max(sequence-1, 0), ResultingVersion: sequence, OccurredAt: occurredAt,
		RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeRun, ID: runID}}, OperationIDs: compactStrings(operationID), EvidenceDigests: []string{sourceDigest},
		Coverage: application.ActivityEventCoverage{IngestionClass: ingestion, LegacyReconstructable: true},
	})
}

func newRepositoryReadinessActivityEvent(snapshot application.RepositoryReadinessSnapshot, operationID string, ingestion application.ActivityIngestionClass) application.ActivityEvent {
	sourceDigest := digestActivitySource(strings.Join([]string{snapshot.SnapshotID, string(snapshot.Status), snapshot.ReasonCode, snapshot.SnapshotDigest, formatTime(snapshot.ObservedAt), formatTime(snapshot.PublishedAt)}, "\x00"))
	return application.NewActivityEvent(application.ActivityEventInput{
		SourceKind: "repository_lifecycle", SourceIdentity: snapshot.SnapshotID, SourceEvidenceDigest: sourceDigest,
		Category: application.ActivityRepository, EventKind: application.ActivityRepositoryGateChange, Actor: application.ActivityActorController,
		Scope: application.ScopeRepository, TargetID: snapshot.Repository, TargetBindingDigest: snapshot.RepositoryBindingDigest,
		ReasonCode: application.ActivityReasonReadinessChanged, ResultingState: string(snapshot.Status), ResultingVersion: snapshot.LifecycleVersion,
		OccurredAt: snapshot.PublishedAt, ObservedAt: &snapshot.ObservedAt,
		RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeRepository, ID: snapshot.Repository}}, OperationIDs: compactStrings(operationID),
		EvidenceDigests: compactDigests(snapshot.SnapshotDigest, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: ingestion, LegacyReconstructable: true},
	})
}

func newOperatorAttentionActivityEvent(eventKey, payloadDigest, reasonCode, evidenceDigest string, occurredAt, observedAt time.Time, scope application.AuthorityScopeKind, target, binding string, ingestion application.ActivityIngestionClass) application.ActivityEvent {
	sourceDigest := digestActivitySource(strings.Join([]string{eventKey, payloadDigest, evidenceDigest, reasonCode, formatTime(occurredAt), formatTime(observedAt)}, "\x00"))
	return application.NewActivityEvent(application.ActivityEventInput{
		SourceKind: "operator_attention", SourceIdentity: eventKey, SourceEvidenceDigest: sourceDigest,
		Category: application.ActivityAttention, EventKind: application.ActivityAttentionOpened, Actor: application.ActivityActorController,
		Scope: scope, TargetID: target, TargetBindingDigest: binding, ReasonCode: application.ActivityReasonOpened,
		OccurredAt: occurredAt, ObservedAt: &observedAt, RelatedResources: []application.ActivityRelatedResource{{Kind: scope, ID: target}},
		EvidenceDigests: compactDigests(evidenceDigest, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: ingestion, LegacyReconstructable: true},
	})
}

func newOnboardingActivityEvent(onboardingID string, step domain.OnboardingStep, order int64, outcome application.OperationOutcome, reason, evidence string, observedAt time.Time, binding, operationID string, ingestion application.ActivityIngestionClass) (application.ActivityEvent, bool) {
	state := domain.OnboardingRunning
	version := order - 1
	switch outcome {
	case application.OperationOutcomeSucceeded:
		version = order
		if step == domain.OnboardingStepSettled {
			state = domain.OnboardingReadyDisabled
		}
	case application.OperationOutcomeFailed, application.OperationOutcomePending:
		state = domain.OnboardingWaitingForOperator
	case application.OperationOutcomeConflict, application.OperationOutcomeAmbiguous:
		state = domain.OnboardingConflict
	default:
		return application.ActivityEvent{}, false
	}
	eventKind, eventReason := application.ActivityOnboardingMilestone, application.ActivityReasonMilestone
	if state == domain.OnboardingReadyDisabled {
		eventKind, eventReason = application.ActivityOnboardingCompleted, application.ActivityReasonCompleted
	} else if state == domain.OnboardingConflict {
		eventKind, eventReason = application.ActivityOnboardingConflict, application.ActivityReasonConflict
	}
	operationIDs := []string(nil)
	if state == domain.OnboardingReadyDisabled || state == domain.OnboardingConflict {
		operationIDs = compactStrings(operationID)
	}
	sourceDigest := digestActivitySource(strings.Join([]string{onboardingID, string(step), string(outcome), reason, evidence, formatTime(observedAt)}, "\x00"))
	return application.NewActivityEvent(application.ActivityEventInput{
		SourceKind: "onboarding", SourceIdentity: onboardingID + ":" + string(step), SourceEvidenceDigest: sourceDigest,
		Category: application.ActivityOnboarding, EventKind: eventKind, Actor: application.ActivityActorController,
		Scope: application.ScopeOnboarding, TargetID: onboardingID, TargetBindingDigest: binding, ReasonCode: eventReason,
		ResultingState: string(state), ResultingVersion: version, OccurredAt: observedAt,
		RelatedResources: []application.ActivityRelatedResource{{Kind: application.ScopeOnboarding, ID: onboardingID}}, OperationIDs: operationIDs,
		EvidenceDigests: compactDigests(evidence, sourceDigest), Coverage: application.ActivityEventCoverage{IngestionClass: ingestion, LegacyReconstructable: true},
	}), true
}
