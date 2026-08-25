package domain

import (
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type OnboardingKind string

const OnboardingExistingCheckout OnboardingKind = "existing_checkout"

type OnboardingStatus string

const (
	OnboardingOpened             OnboardingStatus = "opened"
	OnboardingPreflightReady     OnboardingStatus = "preflight_ready"
	OnboardingCancelled          OnboardingStatus = "cancelled"
	OnboardingAccepted           OnboardingStatus = "accepted"
	OnboardingRunning            OnboardingStatus = "running"
	OnboardingWaitingForOperator OnboardingStatus = "waiting_for_operator"
	OnboardingConflict           OnboardingStatus = "conflict"
	OnboardingReadyDisabled      OnboardingStatus = "ready_disabled"
)

type OnboardingStep string

const (
	OnboardingStepRootsCreated           OnboardingStep = "roots_created"
	OnboardingStepLinearLabelObserved    OnboardingStep = "linear_label_observed"
	OnboardingStepConfigurationApplied   OnboardingStep = "configuration_applied"
	OnboardingStepConfigurationConverged OnboardingStep = "configuration_converged"
	OnboardingStepLifecycleCreated       OnboardingStep = "lifecycle_created"
	OnboardingStepReadinessPublished     OnboardingStep = "readiness_published"
	OnboardingStepSettled                OnboardingStep = "settled"
)

var OnboardingOrderedSteps = []OnboardingStep{
	OnboardingStepRootsCreated,
	OnboardingStepLinearLabelObserved,
	OnboardingStepConfigurationApplied,
	OnboardingStepConfigurationConverged,
	OnboardingStepLifecycleCreated,
	OnboardingStepReadinessPublished,
	OnboardingStepSettled,
}

type OnboardingNextAction string

const (
	OnboardingActionPreflight     OnboardingNextAction = "preflight"
	OnboardingActionPreview       OnboardingNextAction = "preview"
	OnboardingActionStart         OnboardingNextAction = "start"
	OnboardingActionCancel        OnboardingNextAction = "cancel"
	OnboardingActionRestartWorker OnboardingNextAction = "restart_worker"
	OnboardingActionResume        OnboardingNextAction = "resume"
	OnboardingActionRunbook       OnboardingNextAction = "runbook"
	OnboardingActionEnable        OnboardingNextAction = "enable_repository"
)

type ExistingCheckoutOnboardingInput struct {
	SourcePath          string
	CanonicalRepository string
	GitHubAppProfileRef string
	BaseBranch          string
	VerifierIDs         []string
	LinearLabelSlug     string
	CISlowThreshold     time.Duration
}

var (
	onboardingRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9._-]{1,100}$`)
	onboardingReferencePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	onboardingLabelPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	onboardingProfilePattern    = regexp.MustCompile(`^github-app-profile:[a-z0-9][a-z0-9._-]{0,63}$`)
)

func (i ExistingCheckoutOnboardingInput) Validate() error {
	if !filepath.IsAbs(i.SourcePath) || filepath.Clean(i.SourcePath) != i.SourcePath || strings.ContainsRune(i.SourcePath, '\x00') || len(i.SourcePath) > 4096 {
		return errors.New("source path must be absolute and canonical")
	}
	if !onboardingRepositoryPattern.MatchString(i.CanonicalRepository) || i.CanonicalRepository != strings.ToLower(i.CanonicalRepository) {
		return errors.New("canonical repository is invalid")
	}
	if !onboardingProfilePattern.MatchString(i.GitHubAppProfileRef) || !onboardingReferencePattern.MatchString(i.BaseBranch) {
		return errors.New("repository policy reference is invalid")
	}
	if len(i.VerifierIDs) == 0 || len(i.VerifierIDs) > 32 || !slices.IsSorted(i.VerifierIDs) {
		return errors.New("verifier policy is invalid")
	}
	for index, verifierID := range i.VerifierIDs {
		if !onboardingReferencePattern.MatchString(verifierID) || index > 0 && verifierID == i.VerifierIDs[index-1] {
			return errors.New("verifier policy is invalid")
		}
	}
	if !onboardingLabelPattern.MatchString(i.LinearLabelSlug) {
		return errors.New("Linear repository label slug is invalid")
	}
	if i.CISlowThreshold != 0 && (i.CISlowThreshold < time.Minute || i.CISlowThreshold > 24*time.Hour) {
		return errors.New("CI slow threshold is out of bounds")
	}
	return nil
}

func OnboardingCanCancel(status OnboardingStatus) bool {
	return status == OnboardingOpened || status == OnboardingPreflightReady
}

func OnboardingStepCanFollow(completed []OnboardingStep, next OnboardingStep) bool {
	if len(completed) >= len(OnboardingOrderedSteps) || next != OnboardingOrderedSteps[len(completed)] {
		return false
	}
	return !slices.Contains(completed, next)
}

func OnboardingLegalActions(status OnboardingStatus, hasPreflight bool, reasonCode ...string) []OnboardingNextAction {
	switch status {
	case OnboardingOpened:
		return []OnboardingNextAction{OnboardingActionPreflight, OnboardingActionCancel}
	case OnboardingPreflightReady:
		if hasPreflight {
			return []OnboardingNextAction{OnboardingActionPreview, OnboardingActionStart, OnboardingActionCancel}
		}
		return []OnboardingNextAction{OnboardingActionPreflight, OnboardingActionCancel}
	case OnboardingWaitingForOperator:
		if len(reasonCode) != 0 && reasonCode[0] == "worker_restart_required" {
			return []OnboardingNextAction{OnboardingActionRestartWorker}
		}
		return []OnboardingNextAction{OnboardingActionResume}
	case OnboardingConflict:
		return []OnboardingNextAction{OnboardingActionRunbook}
	case OnboardingReadyDisabled:
		return []OnboardingNextAction{OnboardingActionEnable}
	default:
		return []OnboardingNextAction{}
	}
}
