package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// OpenPullRequest performs the only GitHub write currently supported by the
// production adapter. It first searches for an exactly matching controller
// marker and body digest so an interrupted create can be adopted safely.
func (c *Client) OpenPullRequest(ctx context.Context, request application.PullRequestOpenRequest) (domain.PullRequest, error) {
	if err := request.Validate(); err != nil {
		return domain.PullRequest{}, err
	}
	if !c.cfg.PullRequestsWrite {
		return domain.PullRequest{}, errors.New("pull request write capability is not enabled")
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.budgetMu.Lock()
	c.requestCount = 0
	c.budgetMu.Unlock()
	if err := c.ensureToken(ctx, false); err != nil {
		return domain.PullRequest{}, err
	}
	if err := c.verifyRepository(ctx); err != nil {
		return domain.PullRequest{}, err
	}

	matched, err := c.findOwnedPullRequest(ctx, request)
	if err != nil {
		return domain.PullRequest{}, err
	}
	if matched != nil {
		return *matched, nil
	}

	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}{Title: request.Title, Head: request.HeadBranch, Base: request.BaseBranch, Body: request.Body})
	if err != nil {
		return domain.PullRequest{}, errors.New("encode pull request request")
	}
	var created rawPR
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(c.cfg.RepositoryOwner), url.PathEscape(c.cfg.RepositoryName))
	if err := c.rest(ctx, "create_pull_request", "POST", path, bytes.NewReader(payload), &created, true); err != nil {
		return domain.PullRequest{}, err
	}
	return c.validateOwnedPullRequest(created, request)
}

func (c *Client) verifyRepository(ctx context.Context) error {
	var repo struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Name   string `json:"name"`
		Owner  struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(c.cfg.RepositoryOwner), url.PathEscape(c.cfg.RepositoryName))
	if err := c.rest(ctx, "repository", "GET", path, nil, &repo, true); err != nil {
		return err
	}
	if repo.ID != c.cfg.RepositoryID || repo.NodeID == "" || repo.Name != c.cfg.RepositoryName || repo.Owner.Login != c.cfg.RepositoryOwner {
		return errors.New("configured repository identity mismatch")
	}
	c.repo = domain.RepositoryIdentity{ID: repo.ID, NodeID: repo.NodeID, Owner: repo.Owner.Login, Name: repo.Name}
	return nil
}

func (c *Client) findOwnedPullRequest(ctx context.Context, request application.PullRequestOpenRequest) (*domain.PullRequest, error) {
	query := url.Values{}
	query.Set("state", "open")
	query.Set("head", c.cfg.RepositoryOwner+":"+request.HeadBranch)
	query.Set("base", request.BaseBranch)
	query.Set("per_page", "100")
	path := fmt.Sprintf("/repos/%s/%s/pulls?%s", url.PathEscape(c.cfg.RepositoryOwner), url.PathEscape(c.cfg.RepositoryName), query.Encode())
	var candidates []rawPR
	if err := c.rest(ctx, "find_pull_request", "GET", path, nil, &candidates, true); err != nil {
		return nil, err
	}
	if len(candidates) == 100 {
		return nil, errors.New("pull request lookup is ambiguous")
	}
	var matched *domain.PullRequest
	for _, candidate := range candidates {
		if ownershipMarker(candidate.Body) != request.OwnershipKey {
			continue
		}
		validated, err := c.validateOwnedPullRequest(candidate, request)
		if err != nil {
			return nil, err
		}
		if matched != nil {
			return nil, errors.New("multiple controller-owned pull requests match one run")
		}
		copy := validated
		matched = &copy
	}
	return matched, nil
}

func (c *Client) validateOwnedPullRequest(raw rawPR, request application.PullRequestOpenRequest) (domain.PullRequest, error) {
	if raw.Head.Repo.ID != c.cfg.RepositoryID || raw.Base.Repo.ID != c.cfg.RepositoryID {
		return domain.PullRequest{}, errors.New("pull request repository identity mismatch")
	}
	pr := raw.normalized()
	if pr.DatabaseID < 1 || pr.Number < 1 || strings.TrimSpace(pr.NodeID) == "" || strings.TrimSpace(pr.URL) == "" || !strings.EqualFold(pr.State, "open") || pr.Merged {
		return domain.PullRequest{}, errors.New("pull request response is incomplete or not open")
	}
	if err := pr.ValidateOwnership(request.HeadBranch, request.BaseBranch, request.CandidateSHA, request.OwnershipKey); err != nil {
		return domain.PullRequest{}, err
	}
	if pr.BaseSHA != request.BaseSHA || pr.BodyDigest != request.BodyDigest {
		return domain.PullRequest{}, errors.New("pull request response does not match immutable intent")
	}
	return pr, nil
}
