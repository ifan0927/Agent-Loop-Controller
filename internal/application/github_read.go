package application

import (
	"context"
	"fmt"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type GitHubReadPort interface {
	Authority() GitHubInstallationMetadata
	Read(context.Context, int64, string) (domain.GitHubReadEvidence, []GitHubRequestObservation, GitHubInstallationMetadata, error)
}

type GitHubRequestObservation struct {
	RunID              string                    `json:"run_id,omitempty"`
	Operation          string                    `json:"operation"`
	Category           string                    `json:"endpoint_category"`
	HTTPStatus         int                       `json:"http_status"`
	RequestID          string                    `json:"request_id,omitempty"`
	RateLimitLimit     int                       `json:"rate_limit_limit,omitempty"`
	RateLimitRemaining int                       `json:"rate_limit_remaining,omitempty"`
	RateLimitReset     time.Time                 `json:"rate_limit_reset,omitempty"`
	ResponseDigest     string                    `json:"response_digest"`
	ErrorClass         string                    `json:"error_class,omitempty"`
	InstallationID     int64                     `json:"installation_id"`
	Repository         domain.RepositoryIdentity `json:"repository"`
	ObservedAt         time.Time                 `json:"observation_timestamp"`
}

type GitHubInstallationMetadata struct {
	AppID             int64                     `json:"app_id"`
	InstallationID    int64                     `json:"installation_id"`
	Repository        domain.RepositoryIdentity `json:"repository"`
	TokenExpiresAt    time.Time                 `json:"token_expires_at"`
	PermissionsDigest string                    `json:"permissions_digest"`
	ObservedAt        time.Time                 `json:"observation_timestamp"`
}

type GitHubEvidenceStore interface {
	SaveGitHubInstallation(context.Context, string, GitHubInstallationMetadata) error
	SaveGitHubRequest(context.Context, GitHubRequestObservation) error
	SaveGitHubEvidence(context.Context, string, domain.GitHubReadEvidence) error
}

func ReconcileGitHubRead(expectedRepository domain.RepositoryIdentity, expectedPR domain.PullRequest, branch, base, head, baseSHA, ownershipKey, bodyDigest string, got domain.GitHubReadEvidence) error {
	if got.Repository.ID != expectedRepository.ID || got.Repository.Owner != expectedRepository.Owner || got.Repository.Name != expectedRepository.Name || (expectedRepository.NodeID != "" && got.Repository.NodeID != expectedRepository.NodeID) {
		return fmt.Errorf("GitHub repository identity mismatch")
	}
	if got.PullRequest.Number != expectedPR.Number || got.PullRequest.NodeID != expectedPR.NodeID || got.PullRequest.URL != expectedPR.URL || (expectedPR.DatabaseID > 0 && got.PullRequest.DatabaseID != expectedPR.DatabaseID) {
		return fmt.Errorf("GitHub pull request identity mismatch")
	}
	if expectedPR.HeadSHA == "" || expectedPR.HeadSHA != head || got.PullRequest.HeadSHA != expectedPR.HeadSHA {
		return fmt.Errorf("persisted pull request head SHA mismatch")
	}
	if err := got.PullRequest.ValidateOwnership(branch, base, head, ownershipKey); err != nil {
		return err
	}
	if got.PullRequest.BodyDigest != bodyDigest {
		return fmt.Errorf("pull request body digest mismatch")
	}
	if got.PullRequest.BaseSHA != baseSHA {
		return fmt.Errorf("pull request base SHA drift")
	}
	return nil
}
