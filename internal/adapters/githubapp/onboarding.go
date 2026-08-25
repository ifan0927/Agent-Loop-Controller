package githubapp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type BaseRefStatus string

const (
	BaseRefReady       BaseRefStatus = "ready"
	BaseRefUnavailable BaseRefStatus = "unavailable"
	BaseRefConflict    BaseRefStatus = "conflict"
)

type BaseRefObservation struct {
	Status         BaseRefStatus
	ReasonCode     string
	EvidenceDigest string
	ObservedAt     time.Time
}

// ObserveInitialBase performs only repository and exact Git-ref reads. It is
// intentionally separate from Git transport and never creates a branch.
func (c *Client) ObserveInitialBase(ctx context.Context, repository, baseBranch, expectedSHA string) BaseRefObservation {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.budgetMu.Lock()
	c.requestCount = 0
	c.budgetMu.Unlock()
	now := c.clock.Now().UTC()
	result := func(status BaseRefStatus, reason string) BaseRefObservation {
		return BaseRefObservation{Status: status, ReasonCode: reason, EvidenceDigest: application.ConfigurationEvidenceDigest("github-initial-base-v1", repository, baseBranch, expectedSHA, string(status), reason), ObservedAt: now}
	}
	if repository != strings.ToLower(c.cfg.RepositoryOwner+"/"+c.cfg.RepositoryName) || strings.TrimSpace(baseBranch) == "" || expectedSHA == "" {
		return result(BaseRefConflict, "github_base_authority_conflict")
	}
	if err := c.ensureToken(ctx, false); err != nil {
		return result(BaseRefUnavailable, "github_base_observation_unavailable")
	}
	var repo struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Name   string `json:"name"`
		Owner  struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(c.cfg.RepositoryOwner), url.PathEscape(c.cfg.RepositoryName))
	if err := c.rest(ctx, "onboarding_repository_revalidation", "GET", path, nil, &repo, true); err != nil {
		return result(BaseRefUnavailable, "github_base_observation_unavailable")
	}
	if repo.ID != c.cfg.RepositoryID || repo.NodeID == "" || !strings.EqualFold(repo.Owner.Login, c.cfg.RepositoryOwner) || repo.Name != c.cfg.RepositoryName {
		return result(BaseRefConflict, "github_repository_identity_conflict")
	}
	segments := strings.Split(baseBranch, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.rest(ctx, "onboarding_initial_base", "GET", path+"/git/ref/heads/"+strings.Join(segments, "/"), nil, &ref, true); err != nil {
		return result(BaseRefUnavailable, "github_base_observation_unavailable")
	}
	if ref.Ref != "refs/heads/"+baseBranch || ref.Object.Type != "commit" || ref.Object.SHA != expectedSHA {
		return result(BaseRefConflict, "github_base_identity_conflict")
	}
	return result(BaseRefReady, "github_base_ready")
}
