package localregistry

import (
	"context"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestVerifierReadinessResolvesPolicyWithoutRunner(t *testing.T) {
	root := t.TempDir()
	repository := fixtureRepository(t, root, "owner", "repo", 101)
	registry := loadFixture(t, root, File{Version: CurrentVersion, Repositories: []Repository{repository}})
	profile, found, err := registry.RepositoryProfile(context.Background(), "owner/repo")
	if err != nil || !found {
		t.Fatalf("profile found=%t err=%v", found, err)
	}
	result, err := registry.ObserveRepositoryVerifiers(context.Background(), profile.Profile)
	if err != nil || result.Status != domain.RepositoryReady || result.ReasonCode != "verifier_policy_ready" || result.Identity != "builtin:v1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
