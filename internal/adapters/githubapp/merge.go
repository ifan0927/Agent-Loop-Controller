package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// SquashMerge performs the controller's sole merge write. The application
// persists and verifies the intent before this adapter is called; this adapter
// independently re-reads the exact PR and binds the REST request to its head.
func (c *Client) SquashMerge(ctx context.Context, request application.SquashMergeRequest) (domain.PullRequest, error) {
	if err := request.Validate(); err != nil {
		return domain.PullRequest{}, err
	}
	if !c.cfg.SquashMergeWrite {
		return domain.PullRequest{}, errors.New("squash merge capability is not enabled")
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
		return domain.PullRequest{}, mergeRequestFailure(err)
	}

	var before rawPR
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(c.cfg.RepositoryOwner), url.PathEscape(c.cfg.RepositoryName), request.PullRequest)
	if err := c.rest(ctx, "merge_pull_request_preflight", "GET", path, nil, &before, true); err != nil {
		return domain.PullRequest{}, mergeRequestFailure(err)
	}
	if err := c.validateMergeTarget(before, request, false); err != nil {
		return domain.PullRequest{}, mergeRejected(err)
	}
	payload, err := json.Marshal(struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}{SHA: request.ExpectedHeadSHA, MergeMethod: "squash"})
	if err != nil {
		return domain.PullRequest{}, errors.New("encode squash merge request")
	}
	var response struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	if err := c.rest(ctx, "squash_merge_pull_request", "PUT", path+"/merge", bytes.NewReader(payload), &response, true); err != nil {
		return domain.PullRequest{}, mergeRequestFailure(err)
	}
	if !response.Merged || strings.TrimSpace(response.SHA) == "" {
		return domain.PullRequest{}, mergeRejected(errors.New("GitHub did not confirm squash merge"))
	}
	var after rawPR
	if err := c.rest(ctx, "merge_pull_request_observe", "GET", path, nil, &after, true); err != nil {
		return domain.PullRequest{}, err
	}
	if err := c.validateMergeTarget(after, request, true); err != nil {
		return domain.PullRequest{}, mergeRejected(err)
	}
	merged := after.normalized()
	if merged.MergeSHA != response.SHA {
		return domain.PullRequest{}, mergeRejected(errors.New("merge response SHA does not match observed pull request"))
	}
	return merged, nil
}

func mergeRequestFailure(err error) error {
	var status *statusError
	if !errors.As(err, &status) {
		return err
	}
	if status.status != http.StatusForbidden && status.status != http.StatusNotFound && status.status != http.StatusMethodNotAllowed && status.status != http.StatusConflict && status.status != http.StatusUnprocessableEntity {
		return err
	}
	return &application.MergeRejectedError{Cause: err}
}

func mergeRejected(err error) error { return &application.MergeRejectedError{Cause: err} }

func (c *Client) validateMergeTarget(raw rawPR, request application.SquashMergeRequest, merged bool) error {
	if raw.Head.Repo.ID != c.cfg.RepositoryID || raw.Base.Repo.ID != c.cfg.RepositoryID {
		return errors.New("pull request repository identity mismatch")
	}
	pr := raw.normalized()
	if pr.DatabaseID < 1 || pr.Number != request.PullRequest || strings.TrimSpace(pr.NodeID) == "" || strings.TrimSpace(pr.URL) == "" || pr.HeadSHA != request.ExpectedHeadSHA || pr.HeadBranch != request.HeadBranch || pr.BaseBranch != request.BaseBranch || pr.OwnershipKey != request.OwnershipKey {
		return errors.New("pull request response does not match immutable merge intent")
	}
	if !merged {
		if !strings.EqualFold(pr.State, "open") || pr.Merged || pr.BaseSHA != request.ExpectedBaseSHA {
			return errors.New("pull request is not open at the expected merge base")
		}
		return nil
	}
	if !strings.EqualFold(pr.State, "closed") || !pr.Merged || strings.TrimSpace(pr.MergeSHA) == "" || pr.MergedAt.IsZero() {
		return errors.New("pull request merge observation is incomplete")
	}
	return nil
}
