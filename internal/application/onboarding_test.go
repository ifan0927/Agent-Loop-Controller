package application

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOnboardingSourcePathClaimsDetectAncestorWithoutRetainingPath(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "private", "controller-fixture")
	parentSource := filepath.Join(root, "source")
	childSource := filepath.Join(parentSource, "nested")
	parentDigest, parentClaims := onboardingSourcePathDigests(parentSource)
	childDigest, childClaims := onboardingSourcePathDigests(childSource)
	if parentDigest == childDigest || !slices.Contains(parentClaims, parentDigest) || !slices.Contains(childClaims, parentDigest) || !slices.Contains(childClaims, childDigest) {
		t.Fatalf("parent=%s child=%s parentClaims=%v childClaims=%v", parentDigest, childDigest, parentClaims, childClaims)
	}
	for _, claim := range append(parentClaims, childClaims...) {
		if len(claim) != 64 || claim == parentSource || claim == childSource {
			t.Fatalf("unsafe claim=%q", claim)
		}
	}
}

func TestOnboardingProjectionStripsRequesterAndDerivesLegalActions(t *testing.T) {
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	value := Onboarding{OnboardingID: "onboarding-projection", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/repository", Status: domain.OnboardingReadyDisabled, PrivateInputDigest: digestText("private"), SourcePathDigest: digestText("path"), RequestDigest: digestText("request"), ConfigurationBaseGenerationID: 1, ConfigurationBaseDigest: digestText("configuration"), ConfigurationAuthorityVersion: 1, LinearLabelID: "label-1", Requester: requester, CompletedSteps: append([]domain.OnboardingStep(nil), domain.OnboardingOrderedSteps...), CreatedAt: time.Now().UTC()}
	projected := projectOnboarding(value)
	if projected.Requester != (domain.GitHubUserIdentity{}) || !slices.Equal(projected.LegalNextActions, []domain.OnboardingNextAction{domain.OnboardingActionEnable}) || projected.LinearLabelID != "label-1" {
		t.Fatalf("projection=%+v", projected)
	}
	projected.CompletedSteps[0] = domain.OnboardingStepSettled
	if value.CompletedSteps[0] != domain.OnboardingStepRootsCreated {
		t.Fatal("projection shared mutable step storage")
	}
}
