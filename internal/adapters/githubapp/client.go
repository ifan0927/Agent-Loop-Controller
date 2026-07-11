package githubapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 4 << 20

type Observer func(application.GitHubRequestObservation)
type Client struct {
	cfg               Config
	http              *http.Client
	clock             Clock
	observe           Observer
	mu                sync.Mutex
	opMu              sync.Mutex
	token             string
	expires           time.Time
	repo              domain.RepositoryIdentity
	permissionsDigest string
}

func New(cfg Config, clock Clock, observer Observer) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = RealClock{}
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.HTTPTimeout}, clock: clock, observe: observer}, nil
}
func (c *Client) Read(ctx context.Context, pr int64, expectedHead string) (domain.GitHubReadEvidence, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if pr < 1 || expectedHead == "" {
		return domain.GitHubReadEvidence{}, errors.New("PR number and expected head are required")
	}
	if err := c.ensureToken(ctx, false); err != nil {
		return domain.GitHubReadEvidence{}, err
	}
	var repo struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Name   string `json:"name"`
		Owner  struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := c.rest(ctx, "repository", "GET", fmt.Sprintf("/repos/%s/%s", url.PathEscape(c.cfg.RepositoryOwner), url.PathEscape(c.cfg.RepositoryName)), nil, &repo, true); err != nil {
		return domain.GitHubReadEvidence{}, err
	}
	identity := domain.RepositoryIdentity{ID: repo.ID, NodeID: repo.NodeID, Owner: repo.Owner.Login, Name: repo.Name}
	if repo.ID != c.cfg.RepositoryID || repo.Owner.Login != c.cfg.RepositoryOwner || repo.Name != c.cfg.RepositoryName {
		return domain.GitHubReadEvidence{}, errors.New("configured repository identity mismatch")
	}
	c.repo = identity
	var raw rawPR
	if err := c.rest(ctx, "pull_request", "GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, pr), nil, &raw, true); err != nil {
		return domain.GitHubReadEvidence{}, err
	}
	if raw.Base.Repo.ID != c.cfg.RepositoryID || raw.Head.Repo.ID != c.cfg.RepositoryID {
		return domain.GitHubReadEvidence{}, errors.New("pull request head/base repository identity mismatch")
	}
	e := domain.GitHubReadEvidence{Repository: identity, PullRequest: raw.normalized(), ObservedAt: c.clock.Now().UTC()}
	if e.PullRequest.DatabaseID < 1 || e.PullRequest.NodeID == "" {
		return e, errors.New("pull request identity is incomplete")
	}
	if e.PullRequest.HeadSHA != expectedHead {
		return e, errors.New("pull request head SHA mismatch")
	}
	checks, coderabbitCheck, unknown, err := c.readChecks(ctx, expectedHead, e.PullRequest.BaseBranch)
	if err != nil {
		return e, err
	}
	e.Checks = checks
	e.UnknownEvents = unknown
	decision, reviews, findings, cr, unknown2, err := c.readReviews(ctx, pr, expectedHead, coderabbitCheck)
	if err != nil {
		return e, err
	}
	decision2, reviews2, findings2, cr2, unknown3, err := c.readReviews(ctx, pr, expectedHead, coderabbitCheck)
	if err != nil {
		return e, err
	}
	if reviewTopologyDigest(decision, reviews, findings, cr, unknown2) != reviewTopologyDigest(decision2, reviews2, findings2, cr2, unknown3) {
		return e, errors.New("review topology drifted while collecting GitHub evidence")
	}
	decision, reviews, findings, cr, unknown2 = decision2, reviews2, findings2, cr2, unknown3
	checks2, coderabbitCheck2, unknownChecks2, err := c.readChecks(ctx, expectedHead, e.PullRequest.BaseBranch)
	if err != nil {
		return e, err
	}
	if checkTopologyDigest(checks, coderabbitCheck, unknown) != checkTopologyDigest(checks2, coderabbitCheck2, unknownChecks2) {
		return e, errors.New("check topology drifted while collecting GitHub evidence")
	}
	checks, unknown = checks2, unknownChecks2
	e.Checks = checks
	e.UnknownEvents = unknown
	decision3, reviews3, findings3, cr3, unknown4, err := c.readReviews(ctx, pr, expectedHead, coderabbitCheck2)
	if err != nil {
		return e, err
	}
	if reviewTopologyDigest(decision2, reviews2, findings2, cr2, unknown3) != reviewTopologyDigest(decision3, reviews3, findings3, cr3, unknown4) {
		return e, errors.New("review topology drifted after final check collection")
	}
	decision, reviews, findings, cr, unknown2 = decision3, reviews3, findings3, cr3, unknown4
	e.ReviewDecision = decision
	e.Reviews = reviews
	e.Findings = findings
	e.CodeRabbit = cr
	e.UnknownEvents = append(unknown, unknown2...)
	var final rawPR
	if err := c.rest(ctx, "pull_request_final", "GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, pr), nil, &final, true); err != nil {
		return e, err
	}
	finalPR := final.normalized()
	if final.Head.Repo.ID != c.cfg.RepositoryID || final.Base.Repo.ID != c.cfg.RepositoryID || finalPR.Number != e.PullRequest.Number || finalPR.DatabaseID != e.PullRequest.DatabaseID || finalPR.NodeID != e.PullRequest.NodeID || finalPR.HeadSHA != e.PullRequest.HeadSHA || finalPR.BaseSHA != e.PullRequest.BaseSHA || finalPR.HeadBranch != e.PullRequest.HeadBranch || finalPR.BaseBranch != e.PullRequest.BaseBranch || finalPR.BodyDigest != e.PullRequest.BodyDigest {
		return e, errors.New("pull request drifted while collecting GitHub evidence")
	}
	return e, nil
}

func reviewTopologyDigest(decision string, reviews []domain.GitHubReview, findings []domain.NormalizedFinding, cr domain.CodeRabbitState, unknown []string) string {
	copies := append([]domain.NormalizedFinding(nil), findings...)
	for i := range copies {
		copies[i].ObservedAt = time.Time{}
	}
	raw, _ := json.Marshal(struct {
		Decision   string
		Reviews    []domain.GitHubReview
		Findings   []domain.NormalizedFinding
		CodeRabbit domain.CodeRabbitState
		Unknown    []string
	}{decision, reviews, copies, cr, unknown})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func checkTopologyDigest(checks []domain.GitHubCheck, cr domain.CodeRabbitState, unknown []string) string {
	copies := append([]domain.GitHubCheck(nil), checks...)
	for i := range copies {
		copies[i].ObservedAt = time.Time{}
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].ID < copies[j].ID })
	events := append([]string(nil), unknown...)
	sort.Strings(events)
	raw, _ := json.Marshal(struct {
		Checks     []domain.GitHubCheck
		CodeRabbit domain.CodeRabbitState
		Unknown    []string
	}{copies, cr, events})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *Client) ensureToken(ctx context.Context, force bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.clock.Now().Add(c.cfg.TokenRefreshSkew).Before(c.expires) {
		return nil
	}
	key, err := ReadPrivateKeyFile(c.cfg.PrivateKeyFile)
	if err != nil {
		return err
	}
	jwt, err := JWTSigner{AppID: c.cfg.AppID, KeyPEM: key, Clock: c.clock}.Sign()
	if err != nil {
		return err
	}
	var out struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repositories"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", c.cfg.InstallationID)
	requestBody, err := json.Marshal(struct {
		RepositoryIDs []int64 `json:"repository_ids"`
	}{RepositoryIDs: []int64{c.cfg.RepositoryID}})
	if err != nil {
		return errors.New("encode installation token scope")
	}
	if err := c.do(ctx, "mint_installation_token", "REST", "POST", c.cfg.APIBaseURL+path, bytes.NewReader(requestBody), "Bearer "+jwt, &out, false); err != nil {
		return err
	}
	if out.Token == "" || !out.ExpiresAt.After(c.clock.Now()) {
		return errors.New("invalid installation token metadata")
	}
	for name, level := range out.Permissions {
		if level != "read" {
			return fmt.Errorf("installation permission %s is not read-only", name)
		}
	}
	for _, name := range []string{"metadata", "contents", "pull_requests", "checks", "statuses", "administration"} {
		if out.Permissions[name] != "read" {
			return fmt.Errorf("installation lacks required read-only permission %s", name)
		}
	}
	if len(out.Repositories) != 1 {
		return errors.New("installation token scope includes unexpected repositories")
	}
	found := false
	for _, r := range out.Repositories {
		if r.ID == c.cfg.RepositoryID && r.Name == c.cfg.RepositoryName && r.Owner.Login == c.cfg.RepositoryOwner {
			found = true
		}
	}
	if !found {
		return errors.New("installation does not grant configured repository")
	}
	permissionJSON, _ := json.Marshal(out.Permissions)
	permissionSum := sha256.Sum256(permissionJSON)
	c.permissionsDigest = hex.EncodeToString(permissionSum[:])
	c.token = out.Token
	c.expires = out.ExpiresAt
	return nil
}

func (c *Client) InstallationMetadata() application.GitHubInstallationMetadata {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return application.GitHubInstallationMetadata{AppID: c.cfg.AppID, InstallationID: c.cfg.InstallationID, Repository: c.repo, TokenExpiresAt: c.expires, PermissionsDigest: c.permissionsDigest, ObservedAt: c.clock.Now().UTC()}
}

func (c *Client) rest(ctx context.Context, op, method, path string, body io.Reader, out any, retry bool) error {
	err := c.do(ctx, op, "REST", method, c.cfg.APIBaseURL+path, body, "Bearer "+c.token, out, true)
	var se *statusError
	if retry && errors.As(err, &se) && se.status == 401 {
		if e := c.ensureToken(ctx, true); e != nil {
			return e
		}
		return c.do(ctx, op, "REST", method, c.cfg.APIBaseURL+path, body, "Bearer "+c.token, out, true)
	}
	return err
}
func (c *Client) graphql(ctx context.Context, op, query string, vars any, out any) error {
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": vars, "operationName": op})
	err := c.do(ctx, op, "GraphQL", "POST", c.cfg.GraphQLURL, bytes.NewReader(payload), "Bearer "+c.token, out, true)
	var se *statusError
	if errors.As(err, &se) && se.status == 401 {
		if refresh := c.ensureToken(ctx, true); refresh != nil {
			return refresh
		}
		return c.do(ctx, op, "GraphQL", "POST", c.cfg.GraphQLURL, bytes.NewReader(payload), "Bearer "+c.token, out, true)
	}
	return err
}

type statusError struct {
	status int
	class  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GitHub request failed: %s (%d)", e.class, e.status)
}
func (c *Client) do(ctx context.Context, op, category, method, target string, body io.Reader, auth string, out any, installation bool) error {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", c.cfg.APIVersion)
	req.Header.Set("User-Agent", "Agent-Loop-Controller")
	req.Header.Set("Authorization", auth)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if c.observe != nil {
			c.observe(application.GitHubRequestObservation{Operation: op, Category: category, ErrorClass: "transport_failure", InstallationID: c.cfg.InstallationID, Repository: c.repo, ObservedAt: c.clock.Now().UTC()})
		}
		return fmt.Errorf("GitHub transport failure: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if c.observe != nil {
			c.observe(application.GitHubRequestObservation{Operation: op, Category: category, HTTPStatus: resp.StatusCode, ErrorClass: "body_read_failure", InstallationID: c.cfg.InstallationID, Repository: c.repo, ObservedAt: c.clock.Now().UTC()})
		}
		return err
	}
	if len(data) > maxBody {
		if c.observe != nil {
			c.observe(application.GitHubRequestObservation{Operation: op, Category: category, HTTPStatus: resp.StatusCode, ErrorClass: "body_too_large", InstallationID: c.cfg.InstallationID, Repository: c.repo, ObservedAt: c.clock.Now().UTC()})
		}
		return errors.New("GitHub response exceeds body limit")
	}
	sum := sha256.Sum256(data)
	obs := application.GitHubRequestObservation{Operation: op, Category: category, HTTPStatus: resp.StatusCode, RequestID: resp.Header.Get("X-GitHub-Request-Id"), ResponseDigest: hex.EncodeToString(sum[:]), InstallationID: c.cfg.InstallationID, Repository: c.repo, ObservedAt: c.clock.Now().UTC()}
	obs.RateLimitLimit, _ = strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	obs.RateLimitRemaining, _ = strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if n, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); n > 0 {
		obs.RateLimitReset = time.Unix(n, 0).UTC()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		class := "http_error"
		if resp.StatusCode == 401 {
			class = "unauthorized"
		} else if resp.StatusCode == 403 && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			class = "rate_limited"
		} else if resp.StatusCode == 403 {
			class = "permission_denied"
		} else if resp.StatusCode == 404 {
			class = "not_found"
		}
		obs.ErrorClass = class
		if c.observe != nil {
			c.observe(obs)
		}
		return &statusError{resp.StatusCode, class}
	}
	if err := json.Unmarshal(data, out); err != nil {
		obs.ErrorClass = "malformed_json"
		if c.observe != nil {
			c.observe(obs)
		}
		return errors.New("malformed GitHub JSON response")
	}
	if category == "GraphQL" {
		var envelope struct {
			Errors []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(data, &envelope) == nil && len(envelope.Errors) > 0 {
			obs.ErrorClass = "graphql_errors"
		}
	}
	if c.observe != nil {
		c.observe(obs)
	}
	return nil
}

type rawPR struct {
	ID       int64     `json:"id"`
	Number   int64     `json:"number"`
	HTMLURL  string    `json:"html_url"`
	NodeID   string    `json:"node_id"`
	State    string    `json:"state"`
	Merged   bool      `json:"merged"`
	MergeSHA string    `json:"merge_commit_sha"`
	MergedAt time.Time `json:"merged_at"`
	Body     string    `json:"body"`
	Head     struct {
		Ref, SHA string
		Repo     struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref, SHA string
		Repo     struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"base"`
}

func (r rawPR) normalized() domain.PullRequest {
	d := sha256.Sum256([]byte(r.Body))
	return domain.PullRequest{Number: r.Number, DatabaseID: r.ID, URL: r.HTMLURL, NodeID: r.NodeID, HeadBranch: r.Head.Ref, BaseBranch: r.Base.Ref, HeadSHA: r.Head.SHA, BaseSHA: r.Base.SHA, BodyDigest: hex.EncodeToString(d[:]), OwnershipKey: ownershipMarker(r.Body), State: r.State, Merged: r.Merged, MergeSHA: r.MergeSHA, MergedAt: r.MergedAt}
}

func ownershipMarker(body string) string {
	const prefix = "<!-- controller-run:"
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, " -->")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func (c *Client) readChecks(ctx context.Context, sha, base string) ([]domain.GitHubCheck, domain.CodeRabbitState, []string, error) {
	var all []domain.GitHubCheck
	var unknown []string
	var protection struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
			AppID   int64  `json:"app_id"`
		} `json:"checks"`
	}
	if err := c.rest(ctx, "required_checks", "GET", fmt.Sprintf("/repos/%s/%s/branches/%s/protection/required_status_checks", c.cfg.RepositoryOwner, c.cfg.RepositoryName, url.PathEscape(base)), nil, &protection, true); err != nil {
		return nil, domain.CodeRabbitUnknown, nil, err
	}
	required := map[string]int64{}
	for _, name := range protection.Contexts {
		required[name] = 0
	}
	for _, check := range protection.Checks {
		required[check.Context] = check.AppID
	}
	coderabbitCheck := domain.CodeRabbitAbsent
	type checkRun struct {
		ID          int64     `json:"id"`
		Name        string    `json:"name"`
		Status      string    `json:"status"`
		Conclusion  string    `json:"conclusion"`
		StartedAt   time.Time `json:"started_at"`
		CompletedAt time.Time `json:"completed_at"`
		App         struct {
			ID int64 `json:"id"`
		} `json:"app"`
	}
	latestRuns := map[string]checkRun{}
	page := 1
	for page <= 20 {
		var raw struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, sha, page)
		if err := c.rest(ctx, "check_runs", "GET", path, nil, &raw, true); err != nil {
			return nil, domain.CodeRabbitUnknown, nil, err
		}
		for _, r := range raw.CheckRuns {
			key := fmt.Sprintf("%s\x00%d", r.Name, r.App.ID)
			previous, ok := latestRuns[key]
			if !ok || r.ID > previous.ID || r.ID == previous.ID && r.StartedAt.After(previous.StartedAt) || r.ID == previous.ID && r.StartedAt.Equal(previous.StartedAt) && r.CompletedAt.After(previous.CompletedAt) {
				latestRuns[key] = r
			}
		}
		if len(raw.CheckRuns) < 100 {
			break
		}
		page++
	}
	if page > 20 {
		return nil, domain.CodeRabbitUnknown, nil, errors.New("check-run pagination exceeded bounded limit")
	}
	for _, r := range latestRuns {
		state := mapCheck(r.Status, r.Conclusion)
		if state == domain.CheckUnknown {
			unknown = append(unknown, "unknown_check_state:"+r.Status+":"+r.Conclusion)
		}
		requiredApp, requiredName := required[r.Name]
		if c.cfg.CodeRabbitAppID > 0 && r.App.ID == c.cfg.CodeRabbitAppID {
			candidate := domain.CodeRabbitUnknown
			switch state {
			case domain.CheckSuccess, domain.CheckNeutral, domain.CheckSkipped:
				candidate = domain.CodeRabbitPass
			case domain.CheckQueued, domain.CheckInProgress, domain.CheckPending, domain.CheckRequested, domain.CheckWaiting:
				candidate = domain.CodeRabbitPending
			case domain.CheckFailure, domain.CheckActionRequired:
				candidate = domain.CodeRabbitActionable
			case domain.CheckCancelled, domain.CheckTimedOut, domain.CheckStale:
				candidate = domain.CodeRabbitInfrastructure
			}
			coderabbitCheck = mergeCodeRabbitState(coderabbitCheck, candidate)
		}
		sourceAt := r.CompletedAt
		if sourceAt.IsZero() {
			sourceAt = r.StartedAt
		}
		all = append(all, domain.GitHubCheck{ID: strconv.FormatInt(r.ID, 10), Name: r.Name, Required: requiredName && (requiredApp == 0 || requiredApp == r.App.ID), Source: "check_run", AppID: r.App.ID, State: state, ObservedSHA: sha, SourceAt: sourceAt, ObservedAt: c.clock.Now().UTC()})
	}
	type statusPage struct {
		TotalCount int `json:"total_count"`
		Statuses   []struct {
			ID        int64     `json:"id"`
			Context   string    `json:"context"`
			State     string    `json:"state"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"statuses"`
	}
	var statuses statusPage
	for page := 1; page <= 20; page++ {
		var current statusPage
		if err := c.rest(ctx, "commit_statuses", "GET", fmt.Sprintf("/repos/%s/%s/commits/%s/status?per_page=100&page=%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, sha, page), nil, &current, true); err != nil {
			return nil, domain.CodeRabbitUnknown, nil, err
		}
		if page == 1 {
			statuses.TotalCount = current.TotalCount
		}
		statuses.Statuses = append(statuses.Statuses, current.Statuses...)
		if len(statuses.Statuses) >= statuses.TotalCount || len(current.Statuses) < 100 {
			break
		}
		if page == 20 {
			return nil, domain.CodeRabbitUnknown, nil, errors.New("commit-status pagination exceeded bounded limit")
		}
	}
	if len(statuses.Statuses) < statuses.TotalCount {
		return nil, domain.CodeRabbitUnknown, nil, errors.New("commit-status pagination was incomplete")
	}
	latestStatuses := map[string]struct {
		ID             int64
		Context, State string
		UpdatedAt      time.Time
	}{}
	for _, status := range statuses.Statuses {
		previous, ok := latestStatuses[status.Context]
		if !ok || status.UpdatedAt.After(previous.UpdatedAt) || status.UpdatedAt.Equal(previous.UpdatedAt) && status.ID > previous.ID {
			latestStatuses[status.Context] = struct {
				ID             int64
				Context, State string
				UpdatedAt      time.Time
			}{status.ID, status.Context, status.State, status.UpdatedAt}
		}
	}
	for _, status := range latestStatuses {
		state := mapCheck("completed", status.State)
		if status.State == "pending" {
			state = domain.CheckPending
		}
		if state == domain.CheckUnknown {
			unknown = append(unknown, "unknown_commit_status:"+status.State)
		}
		requiredApp, requiredName := required[status.Context]
		all = append(all, domain.GitHubCheck{ID: "status:" + strconv.FormatInt(status.ID, 10), Name: status.Context, Required: requiredName && requiredApp == 0, Source: "commit_status", State: state, ObservedSHA: sha, SourceAt: status.UpdatedAt, ObservedAt: c.clock.Now().UTC()})
	}
	for i := range all {
		all[i].Required = false
	}
	for name, appID := range required {
		best := -1
		for i := range all {
			if all[i].Name != name || appID > 0 && all[i].AppID != appID {
				continue
			}
			if best < 0 || all[i].SourceAt.After(all[best].SourceAt) || all[i].SourceAt.Equal(all[best].SourceAt) && all[i].ID > all[best].ID {
				best = i
			}
		}
		if best >= 0 {
			all[best].Required = true
		}
	}
	for name := range required {
		found := false
		for _, check := range all {
			if check.Required && check.Name == name {
				found = true
			}
		}
		if !found {
			unknown = append(unknown, "missing_required_check:"+name)
		}
	}
	return all, coderabbitCheck, unknown, nil
}

func mergeCodeRabbitState(current, next domain.CodeRabbitState) domain.CodeRabbitState {
	priority := map[domain.CodeRabbitState]int{domain.CodeRabbitAbsent: 0, domain.CodeRabbitPass: 1, domain.CodeRabbitPending: 2, domain.CodeRabbitUnknown: 3, domain.CodeRabbitInfrastructure: 4, domain.CodeRabbitActionable: 5, domain.CodeRabbitUntrusted: 6}
	if priority[next] > priority[current] {
		return next
	}
	return current
}
func mapCheck(status, conclusion string) domain.CheckState {
	if status != "completed" {
		switch status {
		case "queued":
			return domain.CheckQueued
		case "in_progress":
			return domain.CheckInProgress
		case "pending":
			return domain.CheckPending
		case "requested":
			return domain.CheckRequested
		case "waiting":
			return domain.CheckWaiting
		default:
			return domain.CheckUnknown
		}
	}
	switch conclusion {
	case "success":
		return domain.CheckSuccess
	case "neutral":
		return domain.CheckNeutral
	case "skipped":
		return domain.CheckSkipped
	case "failure":
		return domain.CheckFailure
	case "action_required":
		return domain.CheckActionRequired
	case "cancelled":
		return domain.CheckCancelled
	case "timed_out":
		return domain.CheckTimedOut
	case "stale":
		return domain.CheckStale
	default:
		return domain.CheckUnknown
	}
}

type rawReviewComment struct {
	ID         string    `json:"id"`
	DatabaseID int64     `json:"databaseId"`
	Body       string    `json:"body"`
	Path       string    `json:"path"`
	Line       int       `json:"line"`
	Outdated   bool      `json:"outdated"`
	CreatedAt  time.Time `json:"createdAt"`
	Commit     struct {
		OID string `json:"oid"`
	} `json:"commit"`
	OriginalCommit struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
	Author struct {
		Login      string `json:"login"`
		Typename   string `json:"__typename"`
		ID         string `json:"id"`
		DatabaseID int64  `json:"databaseId"`
	} `json:"author"`
}
type graphPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

const reviewQuery = `query ReadPullRequestReviews($owner:String!,$name:String!,$number:Int!,$cursor:String,$reviewCursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewDecision reviews(first:100,after:$reviewCursor){nodes{id databaseId state commit{oid} submittedAt author{login __typename ... on Bot{id databaseId}}} pageInfo{hasNextPage endCursor}} reviewThreads(first:100,after:$cursor){nodes{id isResolved isOutdated comments(first:100){totalCount nodes{id databaseId body path line outdated createdAt commit{oid} originalCommit{oid} author{login __typename ... on Bot{id databaseId}} authorAssociation} pageInfo{hasNextPage endCursor}}}pageInfo{hasNextPage endCursor}}}}}`
const threadCommentsQuery = `query ReadReviewThreadComments($id:ID!,$cursor:String){node(id:$id){... on PullRequestReviewThread{comments(first:100,after:$cursor){nodes{id databaseId body path line outdated createdAt commit{oid} originalCommit{oid} author{login __typename ... on Bot{id databaseId}} authorAssociation} pageInfo{hasNextPage endCursor}}}}}`

func (c *Client) readReviews(ctx context.Context, pr int64, head string, coderabbitCheck domain.CodeRabbitState) (string, []domain.GitHubReview, []domain.NormalizedFinding, domain.CodeRabbitState, []string, error) {
	cursor := ""
	reviewCursor := ""
	var findings []domain.NormalizedFinding
	var reviews []domain.GitHubReview
	unknown := []string{}
	cr := coderabbitCheck
	decision := ""
	seen := map[string]bool{}
	reviewsDone := false
	threadsDone := false
	completedPages := false
	for pages := 0; pages < 20; pages++ {
		var env struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewDecision string `json:"reviewDecision"`
						Reviews        struct {
							Nodes []struct {
								ID         string `json:"id"`
								DatabaseID int64  `json:"databaseId"`
								State      string `json:"state"`
								Commit     struct {
									OID string `json:"oid"`
								} `json:"commit"`
								SubmittedAt time.Time `json:"submittedAt"`
								Author      struct {
									Login      string `json:"login"`
									Typename   string `json:"__typename"`
									ID         string `json:"id"`
									DatabaseID int64  `json:"databaseId"`
								} `json:"author"`
							} `json:"nodes"`
							PageInfo graphPageInfo `json:"pageInfo"`
						} `json:"reviews"`
						ReviewThreads struct {
							Nodes []struct {
								ID         string `json:"id"`
								IsResolved bool   `json:"isResolved"`
								IsOutdated bool   `json:"isOutdated"`
								Comments   struct {
									TotalCount int                `json:"totalCount"`
									Nodes      []rawReviewComment `json:"nodes"`
									PageInfo   graphPageInfo      `json:"pageInfo"`
								} `json:"comments"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := c.graphql(ctx, "ReadPullRequestReviews", reviewQuery, map[string]any{"owner": c.cfg.RepositoryOwner, "name": c.cfg.RepositoryName, "number": pr, "cursor": nullable(cursor), "reviewCursor": nullable(reviewCursor)}, &env); err != nil {
			return "", nil, nil, "", nil, err
		}
		if len(env.Errors) > 0 {
			return "", nil, nil, "", nil, errors.New("GitHub GraphQL returned partial data with errors")
		}
		if env.Data.Repository == nil || env.Data.Repository.PullRequest == nil {
			return "", nil, nil, "", nil, errors.New("GitHub GraphQL response missing pull request")
		}
		p := env.Data.Repository.PullRequest
		if !reviewsDone {
			for _, r := range p.Reviews.Nodes {
				reviews = append(reviews, domain.GitHubReview{DatabaseID: r.DatabaseID, NodeID: r.ID, State: r.State, CommitSHA: r.Commit.OID, SourceAt: r.SubmittedAt, Actor: domain.ActorIdentity{DatabaseID: r.Author.DatabaseID, NodeID: r.Author.ID, Login: r.Author.Login, Type: r.Author.Typename}})
			}
			if p.Reviews.PageInfo.HasNextPage {
				if p.Reviews.PageInfo.EndCursor == "" {
					return "", nil, nil, "", nil, errors.New("review pagination cursor missing")
				}
				reviewCursor = p.Reviews.PageInfo.EndCursor
			} else {
				reviewsDone = true
			}
		}
		decision = p.ReviewDecision
		if !threadsDone {
			for _, t := range p.ReviewThreads.Nodes {
				comments, err := c.completeThreadComments(ctx, t.ID, t.Comments.Nodes, t.Comments.PageInfo)
				if err != nil {
					return "", nil, nil, "", nil, err
				}
				for _, m := range comments {
					id := strconv.FormatInt(m.DatabaseID, 10)
					if id == "0" {
						id = m.ID
					}
					if seen[id] {
						continue
					}
					seen[id] = true
					dig := sha256.Sum256([]byte(m.Body))
					trusted := coderabbitCheck != domain.CodeRabbitAbsent && coderabbitCheck != domain.CodeRabbitUnknown && c.cfg.CodeRabbitAppID > 0 && m.Author.DatabaseID == c.cfg.CodeRabbitActorID && m.Author.ID == c.cfg.CodeRabbitNodeID && m.Author.Typename == "Bot"
					if trusted && m.Commit.OID != head {
						unknown = append(unknown, "coderabbit_comment_head_binding_unavailable:"+id)
						cr = mergeCodeRabbitState(cr, domain.CodeRabbitUnknown)
						trusted = false
					}
					if strings.Contains(strings.ToLower(m.Author.Login), "coderabbit") && !trusted {
						unknown = append(unknown, "coderabbit_comment_app_provenance_unavailable:"+id)
						cr = mergeCodeRabbitState(cr, domain.CodeRabbitUntrusted)
					}
					if trusted {
						if t.IsResolved || t.IsOutdated || m.Outdated {
							if cr == domain.CodeRabbitAbsent {
								cr = domain.CodeRabbitPass
							}
						} else {
							cr = domain.CodeRabbitActionable
						}
					}
					if trusted {
						findings = append(findings, domain.NormalizedFinding{Source: "coderabbit_review_comment", SourceID: id, ThreadID: t.ID, File: m.Path, Line: m.Line, Classification: "source_unspecified", BodyDigest: hex.EncodeToString(dig[:]), Resolved: t.IsResolved, Outdated: t.IsOutdated || m.Outdated, HeadSHA: m.Commit.OID, SourceAt: m.CreatedAt, ObservedAt: c.clock.Now().UTC()})
					} else {
						unknown = append(unknown, "untrusted_review_comment:"+id)
					}
				}
			}
		}
		if !p.ReviewThreads.PageInfo.HasNextPage {
			threadsDone = true
		}
		if threadsDone && reviewsDone {
			completedPages = true
			break
		}
		if !threadsDone && p.ReviewThreads.PageInfo.EndCursor == "" {
			return "", nil, nil, "", nil, errors.New("GraphQL pagination cursor missing")
		}
		if !threadsDone {
			cursor = p.ReviewThreads.PageInfo.EndCursor
		}
	}
	if !completedPages {
		return "", nil, nil, "", nil, errors.New("review-thread pagination exceeded bounded limit")
	}
	return decision, reviews, findings, cr, unknown, nil
}

func (c *Client) completeThreadComments(ctx context.Context, threadID string, initial []rawReviewComment, pageInfo graphPageInfo) ([]rawReviewComment, error) {
	comments := append([]rawReviewComment(nil), initial...)
	cursor := pageInfo.EndCursor
	for page := 1; pageInfo.HasNextPage && page <= 20; page++ {
		if cursor == "" {
			return nil, errors.New("review-comment pagination cursor missing")
		}
		var env struct {
			Data struct {
				Node *struct {
					Comments struct {
						Nodes    []rawReviewComment `json:"nodes"`
						PageInfo graphPageInfo      `json:"pageInfo"`
					} `json:"comments"`
				} `json:"node"`
			} `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		if err := c.graphql(ctx, "ReadReviewThreadComments", threadCommentsQuery, map[string]any{"id": threadID, "cursor": cursor}, &env); err != nil {
			return nil, err
		}
		if len(env.Errors) > 0 {
			return nil, errors.New("GitHub GraphQL returned comment pagination errors")
		}
		if env.Data.Node == nil {
			return nil, errors.New("GitHub GraphQL response missing review thread")
		}
		comments = append(comments, env.Data.Node.Comments.Nodes...)
		pageInfo = env.Data.Node.Comments.PageInfo
		cursor = pageInfo.EndCursor
	}
	if pageInfo.HasNextPage {
		return nil, errors.New("review-comment pagination exceeded bounded limit")
	}
	return comments, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
