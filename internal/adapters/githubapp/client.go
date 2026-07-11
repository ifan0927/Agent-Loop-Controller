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
	e.ReviewDecision = decision
	e.Reviews = reviews
	e.Findings = findings
	e.CodeRabbit = cr
	e.UnknownEvents = append(e.UnknownEvents, unknown2...)
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
	if err := c.do(ctx, "mint_installation_token", "REST", "POST", c.cfg.APIBaseURL+path, bytes.NewReader([]byte("{}")), "Bearer "+jwt, &out, false); err != nil {
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
	page := 1
	for page <= 20 {
		var raw struct {
			CheckRuns []struct {
				ID                       int64 `json:"id"`
				Name, Status, Conclusion string
				StartedAt                time.Time `json:"started_at"`
				CompletedAt              time.Time `json:"completed_at"`
				App                      struct {
					ID int64 `json:"id"`
				} `json:"app"`
			} `json:"check_runs"`
		}
		path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, sha, page)
		if err := c.rest(ctx, "check_runs", "GET", path, nil, &raw, true); err != nil {
			return nil, domain.CodeRabbitUnknown, nil, err
		}
		for _, r := range raw.CheckRuns {
			state := mapCheck(r.Status, r.Conclusion)
			if state == domain.CheckUnknown {
				unknown = append(unknown, "unknown_check_state:"+r.Status+":"+r.Conclusion)
			}
			requiredApp, requiredName := required[r.Name]
			if c.cfg.CodeRabbitAppID > 0 && r.App.ID == c.cfg.CodeRabbitAppID {
				switch state {
				case domain.CheckSuccess, domain.CheckNeutral, domain.CheckSkipped:
					coderabbitCheck = domain.CodeRabbitPass
				case domain.CheckQueued, domain.CheckInProgress, domain.CheckPending, domain.CheckRequested, domain.CheckWaiting:
					coderabbitCheck = domain.CodeRabbitPending
				case domain.CheckFailure, domain.CheckActionRequired:
					coderabbitCheck = domain.CodeRabbitActionable
				case domain.CheckCancelled, domain.CheckTimedOut, domain.CheckStale:
					coderabbitCheck = domain.CodeRabbitInfrastructure
				default:
					coderabbitCheck = domain.CodeRabbitUnknown
				}
			}
			all = append(all, domain.GitHubCheck{ID: strconv.FormatInt(r.ID, 10), Name: r.Name, Required: requiredName && (requiredApp == 0 || requiredApp == r.App.ID), Source: "check_run", AppID: r.App.ID, State: state, ObservedSHA: sha, SourceAt: r.CompletedAt, ObservedAt: c.clock.Now().UTC()})
		}
		if len(raw.CheckRuns) < 100 {
			break
		}
		page++
	}
	if page > 20 {
		return nil, domain.CodeRabbitUnknown, nil, errors.New("check-run pagination exceeded bounded limit")
	}
	var statuses struct {
		TotalCount int `json:"total_count"`
		Statuses   []struct {
			ID        int64     `json:"id"`
			Context   string    `json:"context"`
			State     string    `json:"state"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"statuses"`
	}
	if err := c.rest(ctx, "commit_statuses", "GET", fmt.Sprintf("/repos/%s/%s/commits/%s/status?per_page=100", c.cfg.RepositoryOwner, c.cfg.RepositoryName, sha), nil, &statuses, true); err != nil {
		return nil, domain.CodeRabbitUnknown, nil, err
	}
	if statuses.TotalCount > len(statuses.Statuses) {
		return nil, domain.CodeRabbitUnknown, nil, errors.New("commit-status pagination exceeded bounded response")
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

const reviewQuery = `query ReadPullRequestReviews($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewDecision reviews(first:100){totalCount nodes{id databaseId state commit{oid} submittedAt author{login __typename ... on Bot{id databaseId}}}} reviewThreads(first:100,after:$cursor){nodes{id isResolved isOutdated comments(first:100){totalCount nodes{id databaseId body path line outdated createdAt author{login __typename ... on Bot{id databaseId}} authorAssociation}}}pageInfo{hasNextPage endCursor}}}}}`

func (c *Client) readReviews(ctx context.Context, pr int64, head string, coderabbitCheck domain.CodeRabbitState) (string, []domain.GitHubReview, []domain.NormalizedFinding, domain.CodeRabbitState, []string, error) {
	cursor := ""
	var findings []domain.NormalizedFinding
	var reviews []domain.GitHubReview
	unknown := []string{}
	cr := coderabbitCheck
	decision := ""
	seen := map[string]bool{}
	reviewsLoaded := false
	completedPages := false
	for pages := 0; pages < 20; pages++ {
		var env struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewDecision string `json:"reviewDecision"`
						Reviews        struct {
							TotalCount int `json:"totalCount"`
							Nodes      []struct {
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
						} `json:"reviews"`
						ReviewThreads struct {
							Nodes []struct {
								ID         string `json:"id"`
								IsResolved bool   `json:"isResolved"`
								IsOutdated bool   `json:"isOutdated"`
								Comments   struct {
									TotalCount int `json:"totalCount"`
									Nodes      []struct {
										ID         string    `json:"id"`
										DatabaseID int64     `json:"databaseId"`
										Body       string    `json:"body"`
										Path       string    `json:"path"`
										Line       int       `json:"line"`
										Outdated   bool      `json:"outdated"`
										CreatedAt  time.Time `json:"createdAt"`
										Author     struct {
											Login      string `json:"login"`
											Typename   string `json:"__typename"`
											ID         string `json:"id"`
											DatabaseID int64  `json:"databaseId"`
										} `json:"author"`
									} `json:"nodes"`
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
		if err := c.graphql(ctx, "ReadPullRequestReviews", reviewQuery, map[string]any{"owner": c.cfg.RepositoryOwner, "name": c.cfg.RepositoryName, "number": pr, "cursor": nullable(cursor)}, &env); err != nil {
			return "", nil, nil, "", nil, err
		}
		if len(env.Errors) > 0 {
			return "", nil, nil, "", nil, errors.New("GitHub GraphQL returned partial data with errors")
		}
		if env.Data.Repository == nil || env.Data.Repository.PullRequest == nil {
			return "", nil, nil, "", nil, errors.New("GitHub GraphQL response missing pull request")
		}
		p := env.Data.Repository.PullRequest
		if !reviewsLoaded {
			if p.Reviews.TotalCount > len(p.Reviews.Nodes) {
				return "", nil, nil, "", nil, errors.New("review pagination exceeded bounded response")
			}
			for _, r := range p.Reviews.Nodes {
				reviews = append(reviews, domain.GitHubReview{DatabaseID: r.DatabaseID, NodeID: r.ID, State: r.State, CommitSHA: r.Commit.OID, SourceAt: r.SubmittedAt, Actor: domain.ActorIdentity{DatabaseID: r.Author.DatabaseID, NodeID: r.Author.ID, Login: r.Author.Login, Type: r.Author.Typename}})
			}
			reviewsLoaded = true
		}
		decision = p.ReviewDecision
		for _, t := range p.ReviewThreads.Nodes {
			if t.Comments.TotalCount > len(t.Comments.Nodes) {
				return "", nil, nil, "", nil, errors.New("review-comment pagination exceeded bounded response")
			}
			for _, m := range t.Comments.Nodes {
				id := strconv.FormatInt(m.DatabaseID, 10)
				if id == "0" {
					id = m.ID
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				dig := sha256.Sum256([]byte(m.Body))
				trusted := false
				if strings.Contains(strings.ToLower(m.Author.Login), "coderabbit") && !trusted {
					unknown = append(unknown, "coderabbit_comment_app_provenance_unavailable:"+id)
					if cr == domain.CodeRabbitAbsent {
						cr = domain.CodeRabbitUntrusted
					}
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
					findings = append(findings, domain.NormalizedFinding{Source: "coderabbit_review_comment", SourceID: id, ThreadID: t.ID, File: m.Path, Line: m.Line, Classification: "source_unspecified", BodyDigest: hex.EncodeToString(dig[:]), Resolved: t.IsResolved, Outdated: t.IsOutdated || m.Outdated, HeadSHA: head, SourceAt: m.CreatedAt, ObservedAt: c.clock.Now().UTC()})
				} else {
					unknown = append(unknown, "untrusted_review_comment:"+id)
				}
			}
		}
		if !p.ReviewThreads.PageInfo.HasNextPage {
			completedPages = true
			break
		}
		if p.ReviewThreads.PageInfo.EndCursor == "" {
			return "", nil, nil, "", nil, errors.New("GraphQL pagination cursor missing")
		}
		cursor = p.ReviewThreads.PageInfo.EndCursor
	}
	if !completedPages {
		return "", nil, nil, "", nil, errors.New("review-thread pagination exceeded bounded limit")
	}
	return decision, reviews, findings, cr, unknown, nil
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
