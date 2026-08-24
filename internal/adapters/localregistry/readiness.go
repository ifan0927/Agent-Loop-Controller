package localregistry

import (
	"context"
	"slices"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ObserveRepositoryVerifiers resolves only configured IDs. It deliberately
// has no verifier runner and therefore cannot execute a command.
func (r Registry) ObserveRepositoryVerifiers(_ context.Context, profile application.LocalRepository) (domain.RepositoryDimensionResult, error) {
	now := time.Now().UTC()
	status, reason := domain.RepositoryReady, "verifier_policy_ready"
	binding, err := r.Resolve(profile.CanonicalRepository)
	if err != nil {
		status, reason = domain.RepositoryUnknown, "verifier_registry_unavailable"
	} else if binding.VerifierRegistryRef != profile.VerifierRegistryRef || !slices.Equal(binding.VerifierIDs, profile.VerifierIDs) || len(profile.VerifierIDs) == 0 {
		status, reason = domain.RepositoryConflict, "verifier_policy_conflict"
	}
	identity := profile.VerifierRegistryRef
	return domain.RepositoryDimensionResult{Dimension: domain.ReadinessVerifierPolicy, Status: status, ReasonCode: reason, Identity: identity, EvidenceDigest: application.ConfigurationEvidenceDigest("repository-verifier-readiness-v1", profile.ProfileDigest, profile.RepositoryBindingDigest, string(status), reason, identity), ObservedAt: now}, nil
}
