package domain

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestExistingCheckoutOnboardingInputAndOrderedLifecycle(t *testing.T) {
	input := ExistingCheckoutOnboardingInput{
		SourcePath:          filepath.Join(t.TempDir(), "checkout"),
		CanonicalRepository: "owner/repository",
		GitHubAppProfileRef: "github-app-profile:primary",
		BaseBranch:          "main",
		VerifierIDs:         []string{"fixture-go-test"},
		LinearLabelSlug:     "repository",
		CISlowThreshold:     15 * time.Minute,
	}
	if err := input.Validate(); err != nil {
		t.Fatal(err)
	}
	for index, step := range OnboardingOrderedSteps {
		if !OnboardingStepCanFollow(OnboardingOrderedSteps[:index], step) {
			t.Fatalf("step %s rejected at index %d", step, index)
		}
	}
	if OnboardingStepCanFollow(nil, OnboardingStepConfigurationApplied) {
		t.Fatal("out-of-order configuration mutation was accepted")
	}
	if !OnboardingCanCancel(OnboardingPreflightReady) || OnboardingCanCancel(OnboardingRunning) {
		t.Fatal("cancel boundary is not start-fenced")
	}
	if actions := OnboardingLegalActions(OnboardingReadyDisabled, true); !slices.Equal(actions, []OnboardingNextAction{OnboardingActionEnable}) {
		t.Fatalf("ready-disabled actions=%v", actions)
	}
	if actions := OnboardingLegalActions(OnboardingWaitingForOperator, true, "worker_restart_required"); !slices.Equal(actions, []OnboardingNextAction{OnboardingActionRestartWorker}) {
		t.Fatalf("restart actions=%v", actions)
	}
}

func TestEmptyRepositoryOnboardingInputHasClosedTenStepPlan(t *testing.T) {
	input := EmptyRepositoryOnboardingInput{CanonicalRepository: "owner/repository", GitHubAppProfileRef: "github-app-profile:primary", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, LinearLabelSlug: "repository"}
	if err := input.Validate(); err != nil {
		t.Fatal(err)
	}
	managed := ManagedEmptyRepositoryOnboardingInput(input, filepath.Join(t.TempDir(), "repositories", "owner--repository", "source"))
	if err := managed.Validate(); err != nil {
		t.Fatal(err)
	}
	plan, ok := OnboardingStepPlan(OnboardingEmptyRepository)
	if !ok || len(plan) != 10 || plan[1] != OnboardingStepManagedSourceCreated || plan[2] != OnboardingStepInitialRevisionCreated || plan[3] != OnboardingStepInitialBasePublished || plan[4] != OnboardingStepLinearLabelObserved {
		t.Fatalf("plan=%v ok=%t", plan, ok)
	}
	for index, step := range plan {
		if !OnboardingStepCanFollowKind(OnboardingEmptyRepository, plan[:index], step) {
			t.Fatalf("step %s cannot follow %v", step, plan[:index])
		}
	}
	if OnboardingStepCanFollowKind(OnboardingExistingCheckout, nil, OnboardingStepManagedSourceCreated) {
		t.Fatal("empty-repository step entered existing-checkout plan")
	}
}

func TestExistingCheckoutOnboardingInputRejectsUnsafeOrAmbiguousPolicy(t *testing.T) {
	valid := ExistingCheckoutOnboardingInput{SourcePath: filepath.Join(t.TempDir(), "checkout"), CanonicalRepository: "owner/repository", GitHubAppProfileRef: "github-app-profile:primary", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, LinearLabelSlug: "repository"}
	tests := []ExistingCheckoutOnboardingInput{
		func() ExistingCheckoutOnboardingInput { value := valid; value.SourcePath = "relative"; return value }(),
		func() ExistingCheckoutOnboardingInput {
			value := valid
			value.CanonicalRepository = "Owner/repository"
			return value
		}(),
		func() ExistingCheckoutOnboardingInput {
			value := valid
			value.VerifierIDs = []string{"z", "a"}
			return value
		}(),
		func() ExistingCheckoutOnboardingInput {
			value := valid
			value.VerifierIDs = []string{"a", "a"}
			return value
		}(),
		func() ExistingCheckoutOnboardingInput {
			value := valid
			value.CISlowThreshold = 30 * time.Second
			return value
		}(),
	}
	for index, input := range tests {
		if input.Validate() == nil {
			t.Fatalf("invalid input %d was accepted", index)
		}
	}
}
