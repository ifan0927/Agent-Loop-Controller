package main

import (
	"slices"
	"strings"
	"testing"
)

func TestOnboardingCLIHasOnlyFixedVerbsAndDeterministicVerifierInput(t *testing.T) {
	if got := splitVerifierIDs(" second,fixture-go-test, second ,,first "); !slices.Equal(got, []string{"first", "fixture-go-test", "second", "second"}) {
		t.Fatalf("verifiers=%v", got)
	}
	for _, args := range [][]string{{}, {"force"}, {"step", "configuration_applied"}, {"existing"}, {"existing", "force"}, {"empty"}, {"empty", "force"}} {
		if err := onboardingCommand(args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	if err := onboardingCommand([]string{"empty", "open", "--source-path", "/operator/supplied"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("empty onboarding accepted source authority: %v", err)
	}
	if err := onboardingCommand([]string{"cancel"}); err == nil || !strings.Contains(err.Error(), "onboarding ID") {
		t.Fatalf("cancel without target error=%v", err)
	}
	if err := onboardingCommand([]string{"start", "onboarding-id", "--preflight-digest", strings.Repeat("a", 64), "--preview-digest", strings.Repeat("b", 64)}); err == nil || !strings.Contains(err.Error(), "requester") {
		t.Fatalf("start without requester error=%v", err)
	}
}
