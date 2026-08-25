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

const (
	OnboardingExistingCheckout OnboardingKind = "existing_checkout"
	OnboardingEmptyRepository  OnboardingKind = "empty_repository"
)

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
	OnboardingStepManagedSourceCreated   OnboardingStep = "managed_source_created"
	OnboardingStepInitialRevisionCreated OnboardingStep = "initial_revision_created"
	OnboardingStepInitialBasePublished   OnboardingStep = "initial_base_published"
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

var EmptyRepositoryOnboardingSteps = []OnboardingStep{
	OnboardingStepRootsCreated,
	OnboardingStepManagedSourceCreated,
	OnboardingStepInitialRevisionCreated,
	OnboardingStepInitialBasePublished,
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

type EmptyRepositoryOnboardingInput struct {
	CanonicalRepository string
	GitHubAppProfileRef string
	BaseBranch          string
	VerifierIDs         []string
	LinearLabelSlug     string
	CISlowThreshold     time.Duration
}

// RepositoryOnboardingInput is the private, closed kind-discriminated input.
// Empty-repository source authority is Controller-derived and never accepted
// from an operator-facing command.
type RepositoryOnboardingInput struct {
	Kind                OnboardingKind `json:"kind"`
	SourcePath          string         `json:"source_path"`
	CanonicalRepository string         `json:"canonical_repository"`
	GitHubAppProfileRef string         `json:"github_app_profile_ref"`
	BaseBranch          string         `json:"base_branch"`
	VerifierIDs         []string       `json:"verifier_ids"`
	LinearLabelSlug     string         `json:"linear_label_slug"`
	CISlowThreshold     time.Duration  `json:"ci_slow_threshold"`
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
	return validateOnboardingPolicy(i.CanonicalRepository, i.GitHubAppProfileRef, i.BaseBranch, i.VerifierIDs, i.LinearLabelSlug, i.CISlowThreshold)
}

func (i EmptyRepositoryOnboardingInput) Validate() error {
	return validateOnboardingPolicy(i.CanonicalRepository, i.GitHubAppProfileRef, i.BaseBranch, i.VerifierIDs, i.LinearLabelSlug, i.CISlowThreshold)
}

func validateOnboardingPolicy(repository, profile, branch string, verifiers []string, label string, threshold time.Duration) error {
	if !onboardingRepositoryPattern.MatchString(repository) || repository != strings.ToLower(repository) {
		return errors.New("canonical repository is invalid")
	}
	if !onboardingProfilePattern.MatchString(profile) || !onboardingReferencePattern.MatchString(branch) {
		return errors.New("repository policy reference is invalid")
	}
	if len(verifiers) == 0 || len(verifiers) > 32 || !slices.IsSorted(verifiers) {
		return errors.New("verifier policy is invalid")
	}
	for index, verifierID := range verifiers {
		if !onboardingReferencePattern.MatchString(verifierID) || index > 0 && verifierID == verifiers[index-1] {
			return errors.New("verifier policy is invalid")
		}
	}
	if !onboardingLabelPattern.MatchString(label) {
		return errors.New("Linear repository label slug is invalid")
	}
	if threshold != 0 && (threshold < time.Minute || threshold > 24*time.Hour) {
		return errors.New("CI slow threshold is out of bounds")
	}
	return nil
}

func ExistingRepositoryOnboardingInput(input ExistingCheckoutOnboardingInput) RepositoryOnboardingInput {
	return RepositoryOnboardingInput{Kind: OnboardingExistingCheckout, SourcePath: input.SourcePath, CanonicalRepository: input.CanonicalRepository, GitHubAppProfileRef: input.GitHubAppProfileRef, BaseBranch: input.BaseBranch, VerifierIDs: append([]string(nil), input.VerifierIDs...), LinearLabelSlug: input.LinearLabelSlug, CISlowThreshold: input.CISlowThreshold}
}

func ManagedEmptyRepositoryOnboardingInput(input EmptyRepositoryOnboardingInput, derivedSourcePath string) RepositoryOnboardingInput {
	return RepositoryOnboardingInput{Kind: OnboardingEmptyRepository, SourcePath: derivedSourcePath, CanonicalRepository: input.CanonicalRepository, GitHubAppProfileRef: input.GitHubAppProfileRef, BaseBranch: input.BaseBranch, VerifierIDs: append([]string(nil), input.VerifierIDs...), LinearLabelSlug: input.LinearLabelSlug, CISlowThreshold: input.CISlowThreshold}
}

func (i RepositoryOnboardingInput) Validate() error {
	if i.Kind != OnboardingExistingCheckout && i.Kind != OnboardingEmptyRepository {
		return errors.New("onboarding kind is invalid")
	}
	if !filepath.IsAbs(i.SourcePath) || filepath.Clean(i.SourcePath) != i.SourcePath || strings.ContainsRune(i.SourcePath, '\x00') || len(i.SourcePath) > 4096 {
		return errors.New("source path must be absolute and canonical")
	}
	return validateOnboardingPolicy(i.CanonicalRepository, i.GitHubAppProfileRef, i.BaseBranch, i.VerifierIDs, i.LinearLabelSlug, i.CISlowThreshold)
}

func OnboardingStepPlan(kind OnboardingKind) ([]OnboardingStep, bool) {
	switch kind {
	case OnboardingExistingCheckout:
		return append([]OnboardingStep(nil), OnboardingOrderedSteps...), true
	case OnboardingEmptyRepository:
		return append([]OnboardingStep(nil), EmptyRepositoryOnboardingSteps...), true
	default:
		return nil, false
	}
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

func OnboardingStepCanFollowKind(kind OnboardingKind, completed []OnboardingStep, next OnboardingStep) bool {
	plan, ok := OnboardingStepPlan(kind)
	if !ok || len(completed) >= len(plan) || next != plan[len(completed)] {
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
