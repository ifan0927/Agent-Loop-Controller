package application

import (
	"strings"
	"testing"
)

func TestCleanupSourceRecoveryStageObservationRejectsIdentityAndEffectDrift(t *testing.T) {
	authority := CleanupSourceRecoveryAuthority{
		ReplacementSourceDigest: strings.Repeat("1", 64), ReplacementIdentityDigest: strings.Repeat("2", 64),
		RepositoryOriginDigest: strings.Repeat("3", 64), RegistrationDigest: strings.Repeat("4", 64),
		Branch: "codex/recovery", CandidateHead: strings.Repeat("a", 40),
	}
	base := CleanupSourceRecoveryObservation{
		ReplacementSourceDigest: authority.ReplacementSourceDigest, ReplacementIdentityDigest: authority.ReplacementIdentityDigest,
		RepositoryOriginDigest: authority.RepositoryOriginDigest, RegistrationDigest: authority.RegistrationDigest,
		Branch: authority.Branch, CandidateHead: authority.CandidateHead, WorktreeClean: true,
	}
	valid := []struct {
		name        string
		stage       CleanupSourceRecoveryStage
		observation CleanupSourceRecoveryObservation
	}{
		{"accepted", CleanupSourceRecoveryAccepted, withCleanupObservation(base, true, false, false, false)},
		{"repair pre effect", CleanupSourceRecoveryRepairIntent, withCleanupObservation(base, true, false, false, false)},
		{"repair response loss", CleanupSourceRecoveryRepairIntent, withCleanupObservation(base, true, false, true, false)},
		{"repair observed", CleanupSourceRecoveryRepairObserved, withCleanupObservation(base, true, false, true, false)},
		{"detach pre effect", CleanupSourceRecoveryDetachIntent, withCleanupObservation(base, true, false, true, false)},
		{"detach response loss", CleanupSourceRecoveryDetachIntent, withCleanupObservation(base, true, false, true, true)},
		{"detach observed", CleanupSourceRecoveryDetachObserved, withCleanupObservation(base, true, false, true, true)},
		{"cleanup pre effect", CleanupSourceRecoveryCleanupIntent, withCleanupObservation(base, true, false, true, true)},
		{"cleanup response loss", CleanupSourceRecoveryCleanupIntent, withCleanupObservation(base, false, false, false, false)},
		{"cleanup observed", CleanupSourceRecoveryCleanupObserved, withCleanupObservation(base, false, false, false, false)},
		{"succeeded replay", CleanupSourceRecoverySucceeded, withCleanupObservation(base, false, false, false, false)},
	}
	t.Run("accepted external repair", func(t *testing.T) {
		observation := withCleanupObservation(base, true, false, true, false)
		if err := validateCleanupSourceRecoveryStageObservation(CleanupSourceRecoveryIntent{Authority: authority, Stage: CleanupSourceRecoveryAccepted}, observation); err == nil {
			t.Fatal("repair without durable repair intent was adopted")
		}
	})
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCleanupSourceRecoveryStageObservation(CleanupSourceRecoveryIntent{Authority: authority, Stage: test.stage}, test.observation); err != nil {
				t.Fatal(err)
			}
		})
	}

	drifts := []struct {
		name   string
		mutate func(*CleanupSourceRecoveryObservation)
	}{
		{"replacement path digest", func(v *CleanupSourceRecoveryObservation) { v.ReplacementSourceDigest = strings.Repeat("9", 64) }},
		{"replacement identity", func(v *CleanupSourceRecoveryObservation) { v.ReplacementIdentityDigest = strings.Repeat("9", 64) }},
		{"origin", func(v *CleanupSourceRecoveryObservation) { v.RepositoryOriginDigest = strings.Repeat("9", 64) }},
		{"registration", func(v *CleanupSourceRecoveryObservation) { v.RegistrationDigest = strings.Repeat("9", 64) }},
		{"branch", func(v *CleanupSourceRecoveryObservation) { v.Branch = "codex/other" }},
		{"head", func(v *CleanupSourceRecoveryObservation) { v.CandidateHead = strings.Repeat("b", 40) }},
		{"dirty", func(v *CleanupSourceRecoveryObservation) { v.WorktreeClean = false }},
	}
	for _, test := range drifts {
		t.Run(test.name, func(t *testing.T) {
			observation := withCleanupObservation(base, true, false, true, false)
			test.mutate(&observation)
			if err := validateCleanupSourceRecoveryStageObservation(CleanupSourceRecoveryIntent{Authority: authority, Stage: CleanupSourceRecoveryRepairObserved}, observation); err == nil {
				t.Fatal("drift was accepted")
			}
		})
	}
}

func withCleanupObservation(value CleanupSourceRecoveryObservation, worktree, branch, repaired, detached bool) CleanupSourceRecoveryObservation {
	value.WorktreePresent = worktree
	value.BranchPresent = branch
	value.LinkRepaired = repaired
	value.HeadDetached = detached
	return value
}
