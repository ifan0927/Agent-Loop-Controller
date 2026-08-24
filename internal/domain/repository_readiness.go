package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// RepositoryReadinessStatus is intentionally closed. Missing or stale
// evidence is unknown; not_applicable is reserved for a dimension that cannot
// apply to the configured repository authority.
type RepositoryReadinessStatus string

const (
	RepositoryReady         RepositoryReadinessStatus = "ready"
	RepositoryNotReady      RepositoryReadinessStatus = "not_ready"
	RepositoryUnknown       RepositoryReadinessStatus = "unknown"
	RepositoryConflict      RepositoryReadinessStatus = "conflict"
	RepositoryNotApplicable RepositoryReadinessStatus = "not_applicable"
)

type RepositoryReadinessDimension string

const (
	ReadinessProfileConfiguration     RepositoryReadinessDimension = "profile_configuration"
	ReadinessConfigurationConvergence RepositoryReadinessDimension = "configuration_convergence"
	ReadinessLocalCheckout            RepositoryReadinessDimension = "local_checkout"
	ReadinessBaseBranch               RepositoryReadinessDimension = "base_branch"
	ReadinessGitHubRepository         RepositoryReadinessDimension = "github_repository"
	ReadinessGitHubApp                RepositoryReadinessDimension = "github_app"
	ReadinessLinearLabel              RepositoryReadinessDimension = "linear_label"
	ReadinessVerifierPolicy           RepositoryReadinessDimension = "verifier_policy"
)

var RepositoryReadinessDimensions = []RepositoryReadinessDimension{
	ReadinessProfileConfiguration,
	ReadinessConfigurationConvergence,
	ReadinessLocalCheckout,
	ReadinessBaseBranch,
	ReadinessGitHubRepository,
	ReadinessGitHubApp,
	ReadinessLinearLabel,
	ReadinessVerifierPolicy,
}

type RepositoryDimensionResult struct {
	Dimension      RepositoryReadinessDimension `json:"dimension"`
	Status         RepositoryReadinessStatus    `json:"status"`
	ReasonCode     string                       `json:"reason_code"`
	Identity       string                       `json:"identity,omitempty"`
	EvidenceDigest string                       `json:"evidence_digest"`
	ObservedAt     time.Time                    `json:"observed_at"`
}

func (r RepositoryDimensionResult) Validate() error {
	if !slices.Contains(RepositoryReadinessDimensions, r.Dimension) || !validRepositoryReadinessStatus(r.Status) || !validRepositoryReason(r.ReasonCode) || len(r.Identity) > 256 || strings.ContainsRune(r.Identity, '\x00') || !validRepositoryDigest(r.EvidenceDigest) || r.ObservedAt.IsZero() {
		return errors.New("repository readiness dimension result is invalid")
	}
	return nil
}

// AggregateRepositoryReadiness applies the approved precedence after
// excluding explicitly non-applicable dimensions.
func AggregateRepositoryReadiness(results []RepositoryDimensionResult) (RepositoryReadinessStatus, error) {
	if err := ValidateCompleteRepositoryReadiness(results); err != nil {
		return "", err
	}
	overall := RepositoryReady
	for _, result := range results {
		switch result.Status {
		case RepositoryConflict:
			return RepositoryConflict, nil
		case RepositoryUnknown:
			if overall != RepositoryConflict {
				overall = RepositoryUnknown
			}
		case RepositoryNotReady:
			if overall == RepositoryReady {
				overall = RepositoryNotReady
			}
		case RepositoryReady, RepositoryNotApplicable:
		}
	}
	return overall, nil
}

func ValidateCompleteRepositoryReadiness(results []RepositoryDimensionResult) error {
	if len(results) != len(RepositoryReadinessDimensions) {
		return errors.New("repository readiness snapshot is incomplete")
	}
	seen := make(map[RepositoryReadinessDimension]struct{}, len(results))
	for _, result := range results {
		if result.Validate() != nil {
			return errors.New("repository readiness snapshot contains invalid evidence")
		}
		if _, exists := seen[result.Dimension]; exists {
			return errors.New("repository readiness snapshot contains a duplicate dimension")
		}
		seen[result.Dimension] = struct{}{}
	}
	for _, dimension := range RepositoryReadinessDimensions {
		if _, exists := seen[dimension]; !exists {
			return errors.New("repository readiness snapshot is incomplete")
		}
	}
	return nil
}

func RepositoryReadinessDigest(results []RepositoryDimensionResult) (string, error) {
	if err := ValidateCompleteRepositoryReadiness(results); err != nil {
		return "", err
	}
	ordered := make([]RepositoryDimensionResult, 0, len(results))
	for _, dimension := range RepositoryReadinessDimensions {
		index := slices.IndexFunc(results, func(result RepositoryDimensionResult) bool { return result.Dimension == dimension })
		ordered = append(ordered, results[index])
	}
	payload, err := json.Marshal(ordered)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("repository-readiness-v1\x00"), payload...))
	return hex.EncodeToString(sum[:]), nil
}

func validRepositoryReadinessStatus(status RepositoryReadinessStatus) bool {
	switch status {
	case RepositoryReady, RepositoryNotReady, RepositoryUnknown, RepositoryConflict, RepositoryNotApplicable:
		return true
	default:
		return false
	}
}

func validRepositoryReason(reason string) bool {
	if reason == "" || len(reason) > 96 || strings.ContainsRune(reason, '\x00') {
		return false
	}
	for _, r := range reason {
		if r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validRepositoryDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
