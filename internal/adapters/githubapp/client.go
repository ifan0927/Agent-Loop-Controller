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
	"unicode"
)

const maxBody = 4 << 20
const maxRequestsPerRead = 10000

type Observer func(application.GitHubRequestObservation)
type Client struct {
	cfg               Config
	http              *http.Client
	clock             Clock
	observe           Observer
	mu                sync.Mutex
	budgetMu          sync.Mutex
	requestCount      int
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
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.HTTPTimeout}, clock: clock, observe: observer, repo: domain.RepositoryIdentity{ID: cfg.RepositoryID, Owner: cfg.RepositoryOwner, Name: cfg.RepositoryName}}, nil
}
func (c *Client) Read(ctx context.Context, pr int64, expectedHead string) (domain.GitHubReadEvidence, error) {
	return c.read(ctx, pr, expectedHead, nil)
}

// ReadWithInlineReviewBodies keeps raw inline bodies in a separate bounded
// handoff. Callers must never serialize this value as general GitHub evidence.
func (c *Client) ReadWithInlineReviewBodies(ctx context.Context, pr int64, expectedHead string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, error) {
	var handoff domain.InlineReviewBodyHandoff
	evidence, err := c.read(ctx, pr, expectedHead, &handoff)
	return evidence, handoff, err
}

func (c *Client) read(ctx context.Context, pr int64, expectedHead string, handoff *domain.InlineReviewBodyHandoff) (domain.GitHubReadEvidence, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.budgetMu.Lock()
	c.requestCount = 0
	c.budgetMu.Unlock()
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
	e := domain.GitHubReadEvidence{Repository: identity, PullRequest: raw.normalized(), ObservedAt: c.clock.Now().UTC()}
	if raw.Base.Repo.ID != c.cfg.RepositoryID || raw.Head.Repo.ID != c.cfg.RepositoryID {
		return e, errors.New("pull request head/base repository identity mismatch")
	}
	if e.PullRequest.DatabaseID < 1 || e.PullRequest.NodeID == "" {
		return e, errors.New("pull request identity is incomplete")
	}
	if e.PullRequest.HeadSHA != expectedHead {
		return e, errors.New("pull request head SHA mismatch")
	}
	firstChecks, err := c.readChecksSnapshot(ctx, expectedHead, e.PullRequest.BaseBranch)
	if err != nil {
		return e, err
	}
	checks, unknown := firstChecks.checks, firstChecks.unknown
	e.Checks = checks
	e.UnknownEvents = unknown
	reviews, unknown2, err := c.readReviews(ctx, pr)
	if err != nil {
		return e, err
	}
	threads, _, err := c.readReviewThreads(ctx, pr)
	if err != nil {
		return e, err
	}
	reviews2, unknown3, err := c.readReviews(ctx, pr)
	if err != nil {
		return e, err
	}
	threads2, _, err := c.readReviewThreads(ctx, pr)
	if err != nil {
		return e, err
	}
	if reviewTopologyDigest(reviews, threads, unknown2) != reviewTopologyDigest(reviews2, threads2, unknown3) {
		return e, errors.New("review topology drifted while collecting GitHub evidence")
	}
	reviews, threads, unknown2 = reviews2, threads2, unknown3
	secondChecks, err := c.readChecksSnapshot(ctx, expectedHead, e.PullRequest.BaseBranch)
	if err != nil {
		return e, err
	}
	checks2, unknownChecks2 := secondChecks.checks, secondChecks.unknown
	if firstChecks.protectionDigest != secondChecks.protectionDigest {
		return e, errors.New("required check protection drifted while collecting GitHub evidence")
	}
	// Check runs are expected to appear and advance immediately after a push or
	// PR creation. Repository, PR, exact head, protection, and completeness are
	// revalidated by both bounded reads, so use the newer snapshot when only the
	// check topology moved. Review topology remains immutable within one read
	// because it can carry human authorization or repair input.
	checks, unknown = checks2, unknownChecks2
	e.Checks = checks
	e.UnknownEvents = unknown
	reviews3, unknown4, err := c.readReviews(ctx, pr)
	if err != nil {
		return e, err
	}
	threads3, bodies3, err := c.readReviewThreads(ctx, pr)
	if err != nil {
		return e, err
	}
	if reviewTopologyDigest(reviews2, threads2, unknown3) != reviewTopologyDigest(reviews3, threads3, unknown4) {
		return e, errors.New("review topology drifted after final check collection")
	}
	reviews, threads, unknown2 = reviews3, threads3, unknown4
	e.Reviews = reviews
	e.ReviewThreads = threads
	e.UnknownEvents = append(unknown, unknown2...)
	var final rawPR
	if err := c.rest(ctx, "pull_request_final", "GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, pr), nil, &final, true); err != nil {
		return e, err
	}
	finalPR := final.normalized()
	if final.Head.Repo.ID != c.cfg.RepositoryID || final.Base.Repo.ID != c.cfg.RepositoryID || finalPR.Number != e.PullRequest.Number || finalPR.DatabaseID != e.PullRequest.DatabaseID || finalPR.NodeID != e.PullRequest.NodeID || finalPR.HeadSHA != e.PullRequest.HeadSHA || finalPR.BaseSHA != e.PullRequest.BaseSHA || finalPR.HeadBranch != e.PullRequest.HeadBranch || finalPR.BaseBranch != e.PullRequest.BaseBranch || finalPR.BodyDigest != e.PullRequest.BodyDigest || finalPR.State != e.PullRequest.State || finalPR.Merged != e.PullRequest.Merged || finalPR.MergeSHA != e.PullRequest.MergeSHA || !finalPR.MergedAt.UTC().Equal(e.PullRequest.MergedAt.UTC()) {
		return e, errors.New("pull request drifted while collecting GitHub evidence")
	}
	if handoff != nil {
		*handoff = bodies3
	}
	return e, nil
}

func reviewTopologyDigest(reviews []domain.GitHubReview, threads []domain.GitHubReviewThread, unknown []string) string {
	reviewCopies := append([]domain.GitHubReview(nil), reviews...)
	threadCopies := append([]domain.GitHubReviewThread(nil), threads...)
	for i := range threadCopies {
		threadCopies[i].Comments = append([]domain.GitHubReviewComment(nil), threadCopies[i].Comments...)
		sort.Slice(threadCopies[i].Comments, func(left, right int) bool {
			return threadCopies[i].Comments[left].NodeID < threadCopies[i].Comments[right].NodeID
		})
	}
	sort.Slice(reviewCopies, func(left, right int) bool { return reviewCopies[left].NodeID < reviewCopies[right].NodeID })
	sort.Slice(threadCopies, func(left, right int) bool { return threadCopies[left].NodeID < threadCopies[right].NodeID })
	unknownCopies := append([]string(nil), unknown...)
	sort.Strings(unknownCopies)
	raw, _ := json.Marshal(struct {
		Reviews []domain.GitHubReview
		Threads []domain.GitHubReviewThread
		Unknown []string
	}{reviewCopies, threadCopies, unknownCopies})
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
	writable := map[string]bool{"pull_requests": c.cfg.PullRequestsWrite, "contents": c.cfg.SquashMergeWrite}
	for name, level := range out.Permissions {
		if level != "read" && !(writable[name] && level == "write") {
			return fmt.Errorf("installation permission %s exceeds controller policy", name)
		}
	}
	for _, name := range []string{"metadata", "contents", "checks", "statuses", "administration", "pull_requests"} {
		want := "read"
		if writable[name] {
			want = "write"
		}
		if out.Permissions[name] != want {
			return fmt.Errorf("installation permission %s does not match controller policy", name)
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
	payload, err := requestPayload(body)
	if err != nil {
		return err
	}
	err = c.do(ctx, op, "REST", method, c.cfg.APIBaseURL+path, bytes.NewReader(payload), "Bearer "+c.token, out, true)
	var se *statusError
	if retry && errors.As(err, &se) && se.status == 401 {
		if e := c.ensureToken(ctx, true); e != nil {
			return e
		}
		return c.do(ctx, op, "REST", method, c.cfg.APIBaseURL+path, bytes.NewReader(payload), "Bearer "+c.token, out, true)
	}
	return err
}

func requestPayload(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	payload, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	if err != nil {
		return nil, errors.New("read GitHub request body")
	}
	if len(payload) > maxBody {
		return nil, errors.New("GitHub request exceeds body limit")
	}
	return payload, nil
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
	c.budgetMu.Lock()
	if c.requestCount >= maxRequestsPerRead {
		c.budgetMu.Unlock()
		return errors.New("GitHub request budget exceeded")
	}
	c.requestCount++
	c.budgetMu.Unlock()
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

type checkReadSnapshot struct {
	checks           []domain.GitHubCheck
	unknown          []string
	protectionDigest string
}

func (c *Client) readChecks(ctx context.Context, sha, base string) ([]domain.GitHubCheck, []string, error) {
	snapshot, err := c.readChecksSnapshot(ctx, sha, base)
	return snapshot.checks, snapshot.unknown, err
}

func (c *Client) readChecksSnapshot(ctx context.Context, sha, base string) (checkReadSnapshot, error) {
	var all []domain.GitHubCheck
	var unknown []string
	var protection struct {
		Strict   bool     `json:"strict"`
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
			AppID   int64  `json:"app_id"`
		} `json:"checks"`
	}
	if err := c.rest(ctx, "required_checks", "GET", fmt.Sprintf("/repos/%s/%s/branches/%s/protection/required_status_checks", c.cfg.RepositoryOwner, c.cfg.RepositoryName, url.PathEscape(base)), nil, &protection, true); err != nil {
		return checkReadSnapshot{}, err
	}
	required := map[string]int64{}
	for _, name := range protection.Contexts {
		if !canonicalCheckContext(name) {
			return checkReadSnapshot{}, errors.New("required check protection context is invalid")
		}
		if _, duplicate := required[name]; duplicate {
			return checkReadSnapshot{}, errors.New("required check protection binding is duplicated")
		}
		required[name] = 0
	}
	for _, check := range protection.Checks {
		if !canonicalCheckContext(check.Context) || check.AppID < 1 {
			return checkReadSnapshot{}, errors.New("required check protection app binding is invalid")
		}
		if _, duplicate := required[check.Context]; duplicate {
			return checkReadSnapshot{}, errors.New("required check protection binding conflicts")
		}
		required[check.Context] = check.AppID
	}
	requiredPairs := make([]string, 0, len(required))
	for name, appID := range required {
		requiredPairs = append(requiredPairs, fmt.Sprintf("%s\x00%d", name, appID))
	}
	sort.Strings(requiredPairs)
	protectionRaw, _ := json.Marshal(struct {
		Strict bool
		Pairs  []string
	}{Strict: protection.Strict, Pairs: requiredPairs})
	protectionSum := sha256.Sum256(protectionRaw)
	protectionDigest := hex.EncodeToString(protectionSum[:])
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
	seenCheckRunIDs := map[int64]struct{}{}
	expectedCheckRunCount, collectedCheckRuns := -1, 0
	page := 1
	for page <= 20 {
		var raw struct {
			TotalCount *int       `json:"total_count"`
			CheckRuns  []checkRun `json:"check_runs"`
		}
		path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, sha, page)
		if err := c.rest(ctx, "check_runs", "GET", path, nil, &raw, true); err != nil {
			return checkReadSnapshot{}, err
		}
		if raw.TotalCount == nil || *raw.TotalCount < 0 {
			return checkReadSnapshot{}, errors.New("check-run total count is invalid")
		}
		if expectedCheckRunCount < 0 {
			expectedCheckRunCount = *raw.TotalCount
		} else if expectedCheckRunCount != *raw.TotalCount {
			return checkReadSnapshot{}, errors.New("check-run total count drifted during pagination")
		}
		if len(raw.CheckRuns) > 100 || collectedCheckRuns+len(raw.CheckRuns) > expectedCheckRunCount {
			return checkReadSnapshot{}, errors.New("check-run pagination count is inconsistent")
		}
		for _, r := range raw.CheckRuns {
			if r.ID < 1 || !canonicalCheckContext(r.Name) || r.App.ID < 1 || r.Status == "completed" && r.CompletedAt.IsZero() || r.Status == "in_progress" && r.StartedAt.IsZero() {
				return checkReadSnapshot{}, errors.New("check-run identity evidence is incomplete")
			}
			if _, duplicate := seenCheckRunIDs[r.ID]; duplicate {
				return checkReadSnapshot{}, errors.New("check-run identity is duplicated")
			}
			seenCheckRunIDs[r.ID] = struct{}{}
			key := fmt.Sprintf("%s\x00%d", r.Name, r.App.ID)
			previous, ok := latestRuns[key]
			if !ok || r.ID > previous.ID || r.ID == previous.ID && r.StartedAt.After(previous.StartedAt) || r.ID == previous.ID && r.StartedAt.Equal(previous.StartedAt) && r.CompletedAt.After(previous.CompletedAt) {
				latestRuns[key] = r
			}
		}
		collectedCheckRuns += len(raw.CheckRuns)
		if collectedCheckRuns == expectedCheckRunCount {
			break
		}
		if len(raw.CheckRuns) < 100 {
			return checkReadSnapshot{}, errors.New("check-run pagination ended before total count")
		}
		page++
	}
	if page > 20 || collectedCheckRuns != expectedCheckRunCount {
		return checkReadSnapshot{}, errors.New("check-run pagination exceeded bounded limit")
	}
	for _, r := range latestRuns {
		state := mapCheck(r.Status, r.Conclusion)
		sourceAt := r.CompletedAt
		if sourceAt.IsZero() {
			sourceAt = r.StartedAt
		}
		all = append(all, domain.GitHubCheck{ID: strconv.FormatInt(r.ID, 10), Name: r.Name, Source: "check_run", AppID: r.App.ID, State: state, ObservedSHA: sha, SourceAt: sourceAt, ObservedAt: c.clock.Now().UTC()})
	}
	type commitStatus struct {
		ID        int64     `json:"id"`
		Context   string    `json:"context"`
		State     string    `json:"state"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	type statusPage struct {
		TotalCount *int           `json:"total_count"`
		Statuses   []commitStatus `json:"statuses"`
	}
	var statuses []commitStatus
	seenStatusIDs := map[int64]struct{}{}
	expectedStatusCount := -1
	for page := 1; page <= 20; page++ {
		var current statusPage
		if err := c.rest(ctx, "commit_statuses", "GET", fmt.Sprintf("/repos/%s/%s/commits/%s/status?per_page=100&page=%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, sha, page), nil, &current, true); err != nil {
			return checkReadSnapshot{}, err
		}
		if current.TotalCount == nil || *current.TotalCount < 0 {
			return checkReadSnapshot{}, errors.New("commit-status total count is invalid")
		}
		if page == 1 {
			expectedStatusCount = *current.TotalCount
		} else if *current.TotalCount != expectedStatusCount {
			return checkReadSnapshot{}, errors.New("commit-status total count drifted during pagination")
		}
		if len(statuses)+len(current.Statuses) > expectedStatusCount {
			return checkReadSnapshot{}, errors.New("commit-status pagination count is inconsistent")
		}
		for _, status := range current.Statuses {
			if status.ID < 1 || !canonicalCheckContext(status.Context) || status.UpdatedAt.IsZero() {
				return checkReadSnapshot{}, errors.New("commit-status identity evidence is incomplete")
			}
			if _, duplicate := seenStatusIDs[status.ID]; duplicate {
				return checkReadSnapshot{}, errors.New("commit-status identity is duplicated")
			}
			seenStatusIDs[status.ID] = struct{}{}
		}
		statuses = append(statuses, current.Statuses...)
		if len(statuses) >= expectedStatusCount || len(current.Statuses) < 100 {
			break
		}
		if page == 20 {
			return checkReadSnapshot{}, errors.New("commit-status pagination exceeded bounded limit")
		}
	}
	if len(statuses) != expectedStatusCount {
		return checkReadSnapshot{}, errors.New("commit-status pagination was incomplete")
	}
	latestStatuses := map[string]struct {
		ID             int64
		Context, State string
		UpdatedAt      time.Time
	}{}
	for _, status := range statuses {
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
		all = append(all, domain.GitHubCheck{ID: "status:" + strconv.FormatInt(status.ID, 10), Name: status.Context, Source: "commit_status", State: state, ObservedSHA: sha, SourceAt: status.UpdatedAt, ObservedAt: c.clock.Now().UTC()})
	}
	for i := range all {
		all[i].Required = false
	}
	for name, appID := range required {
		bestRun, bestStatus := -1, -1
		for i := range all {
			if all[i].Name != name {
				continue
			}
			if all[i].Source == "check_run" && (appID == 0 || all[i].AppID == appID) && (bestRun < 0 || laterGitHubCheck(all[i], all[bestRun])) {
				bestRun = i
			}
			if appID == 0 && all[i].Source == "commit_status" && (bestStatus < 0 || laterGitHubCheck(all[i], all[bestStatus])) {
				bestStatus = i
			}
		}
		if bestRun >= 0 {
			all[bestRun].Required = true
		}
		if bestStatus >= 0 {
			all[bestStatus].Required = true
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
	for _, check := range all {
		if !check.Required || check.State != domain.CheckUnknown {
			continue
		}
		if check.Source == "commit_status" {
			unknown = append(unknown, "unknown_commit_status:"+check.Name+":"+check.ID)
		} else {
			unknown = append(unknown, "unknown_check_state:"+check.Name+":"+strconv.FormatInt(check.AppID, 10)+":"+check.ID)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		if all[i].Source != all[j].Source {
			return all[i].Source < all[j].Source
		}
		if all[i].AppID != all[j].AppID {
			return all[i].AppID < all[j].AppID
		}
		return all[i].ID < all[j].ID
	})
	sort.Strings(unknown)
	return checkReadSnapshot{checks: all, unknown: unknown, protectionDigest: protectionDigest}, nil
}

func laterGitHubCheck(candidate, current domain.GitHubCheck) bool {
	if !candidate.SourceAt.Equal(current.SourceAt) {
		return candidate.SourceAt.After(current.SourceAt)
	}
	candidateID, candidateErr := strconv.ParseInt(strings.TrimPrefix(candidate.ID, "status:"), 10, 64)
	currentID, currentErr := strconv.ParseInt(strings.TrimPrefix(current.ID, "status:"), 10, 64)
	if candidateErr == nil && currentErr == nil {
		return candidateID > currentID
	}
	return candidate.ID > current.ID
}

func canonicalCheckContext(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && strings.IndexFunc(value, unicode.IsControl) < 0
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

type graphPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

const reviewQuery = `query ReadPullRequestReviews($owner:String!,$name:String!,$number:Int!,$reviewCursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviews(first:100,after:$reviewCursor){nodes{id databaseId state commit{oid} submittedAt author{login __typename ... on User{id databaseId} ... on Bot{id databaseId}}} pageInfo{hasNextPage endCursor}}}}}`

const reviewThreadQuery = `query ReadPullRequestReviewThreads($owner:String!,$name:String!,$number:Int!,$threadCursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$threadCursor){nodes{id isResolved isOutdated path line comments(first:100){nodes{id databaseId replyTo{id databaseId} originalCommit{oid} author{login __typename ... on User{id databaseId} ... on Bot{id databaseId}} pullRequestReview{id databaseId state commit{oid} submittedAt author{login __typename ... on User{id databaseId} ... on Bot{id databaseId}}} body createdAt updatedAt} pageInfo{hasNextPage endCursor}}} pageInfo{hasNextPage endCursor}}}}}`

func (c *Client) readReviews(ctx context.Context, pr int64) ([]domain.GitHubReview, []string, error) {
	reviewCursor := ""
	var reviews []domain.GitHubReview
	for pages := 0; pages < 20; pages++ {
		var env struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						Reviews struct {
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
							PageInfo *graphPageInfo `json:"pageInfo"`
						} `json:"reviews"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := c.graphql(ctx, "ReadPullRequestReviews", reviewQuery, map[string]any{"owner": c.cfg.RepositoryOwner, "name": c.cfg.RepositoryName, "number": pr, "reviewCursor": nullable(reviewCursor)}, &env); err != nil {
			return nil, nil, err
		}
		if len(env.Errors) > 0 {
			return nil, nil, errors.New("GitHub GraphQL returned partial data with errors")
		}
		if env.Data.Repository == nil || env.Data.Repository.PullRequest == nil {
			return nil, nil, errors.New("GitHub GraphQL response missing pull request")
		}
		p := env.Data.Repository.PullRequest
		for _, r := range p.Reviews.Nodes {
			reviews = append(reviews, domain.GitHubReview{DatabaseID: r.DatabaseID, NodeID: r.ID, State: r.State, CommitSHA: r.Commit.OID, SourceAt: r.SubmittedAt, Actor: domain.ActorIdentity{DatabaseID: r.Author.DatabaseID, NodeID: r.Author.ID, Login: r.Author.Login, Type: r.Author.Typename}})
		}
		if p.Reviews.PageInfo == nil {
			return nil, nil, errors.New("review pagination metadata is missing")
		}
		if !p.Reviews.PageInfo.HasNextPage {
			return reviews, nil, nil
		}
		if p.Reviews.PageInfo.EndCursor == "" {
			return nil, nil, errors.New("review pagination cursor missing")
		}
		reviewCursor = p.Reviews.PageInfo.EndCursor
	}
	return nil, nil, errors.New("review pagination exceeded bounded limit")
}

func (c *Client) readReviewThreads(ctx context.Context, pr int64) ([]domain.GitHubReviewThread, domain.InlineReviewBodyHandoff, error) {
	threadCursor := ""
	var threads []domain.GitHubReviewThread
	var handoff domain.InlineReviewBodyHandoff
	for pages := 0; pages < 20; pages++ {
		var env struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads struct {
							Nodes    []rawReviewThread `json:"nodes"`
							PageInfo *graphPageInfo    `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := c.graphql(ctx, "ReadPullRequestReviewThreads", reviewThreadQuery, map[string]any{"owner": c.cfg.RepositoryOwner, "name": c.cfg.RepositoryName, "number": pr, "threadCursor": nullable(threadCursor)}, &env); err != nil {
			return nil, domain.InlineReviewBodyHandoff{}, err
		}
		if len(env.Errors) > 0 {
			return nil, domain.InlineReviewBodyHandoff{}, errors.New("GitHub GraphQL returned partial data with errors")
		}
		if env.Data.Repository == nil || env.Data.Repository.PullRequest == nil {
			return nil, domain.InlineReviewBodyHandoff{}, errors.New("GitHub GraphQL response missing pull request")
		}
		page := env.Data.Repository.PullRequest.ReviewThreads
		if page.PageInfo == nil {
			return nil, domain.InlineReviewBodyHandoff{}, errors.New("review-thread pagination metadata is missing")
		}
		if len(page.Nodes) > 100 {
			return nil, domain.InlineReviewBodyHandoff{}, errors.New("review-thread page exceeds bounded limit")
		}
		for _, raw := range page.Nodes {
			thread, bodies, err := raw.normalized()
			if err != nil {
				return nil, domain.InlineReviewBodyHandoff{}, err
			}
			threads = append(threads, thread)
			handoff.Comments = append(handoff.Comments, bodies...)
			if err := handoff.Validate(); err != nil {
				return nil, domain.InlineReviewBodyHandoff{}, err
			}
		}
		if !page.PageInfo.HasNextPage {
			return threads, handoff, nil
		}
		if page.PageInfo.EndCursor == "" {
			return nil, domain.InlineReviewBodyHandoff{}, errors.New("review-thread pagination cursor missing")
		}
		threadCursor = page.PageInfo.EndCursor
	}
	return nil, domain.InlineReviewBodyHandoff{}, errors.New("review-thread pagination exceeded bounded limit")
}

type rawReviewThread struct {
	ID       string `json:"id"`
	Resolved bool   `json:"isResolved"`
	Outdated bool   `json:"isOutdated"`
	Path     string `json:"path"`
	Line     *int   `json:"line"`
	Comments struct {
		Nodes    []rawReviewComment `json:"nodes"`
		PageInfo *graphPageInfo     `json:"pageInfo"`
	} `json:"comments"`
}

type rawReviewComment struct {
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseId"`
	ReplyTo    *struct {
		ID         string `json:"id"`
		DatabaseID int64  `json:"databaseId"`
	} `json:"replyTo"`
	OriginalCommit *struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
	Author *struct {
		Login      string `json:"login"`
		Typename   string `json:"__typename"`
		ID         string `json:"id"`
		DatabaseID int64  `json:"databaseId"`
	} `json:"author"`
	Review *struct {
		ID         string `json:"id"`
		DatabaseID int64  `json:"databaseId"`
		State      string `json:"state"`
		Commit     *struct {
			OID string `json:"oid"`
		} `json:"commit"`
		SubmittedAt time.Time `json:"submittedAt"`
		Author      *struct {
			Login      string `json:"login"`
			Typename   string `json:"__typename"`
			ID         string `json:"id"`
			DatabaseID int64  `json:"databaseId"`
		} `json:"author"`
	} `json:"pullRequestReview"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (raw rawReviewThread) normalized() (domain.GitHubReviewThread, []domain.InlineReviewBody, error) {
	if strings.TrimSpace(raw.ID) == "" {
		return domain.GitHubReviewThread{}, nil, errors.New("review thread ID is missing")
	}
	if raw.Comments.PageInfo == nil {
		return domain.GitHubReviewThread{}, nil, errors.New("review-thread comment pagination metadata is missing")
	}
	if raw.Comments.PageInfo.HasNextPage {
		return domain.GitHubReviewThread{}, nil, errors.New("review-thread comment pagination exceeds supported bound")
	}
	if len(raw.Comments.Nodes) > 100 {
		return domain.GitHubReviewThread{}, nil, errors.New("review-thread comment page exceeds bounded limit")
	}
	if len(raw.Comments.Nodes) == 0 {
		return domain.GitHubReviewThread{}, nil, errors.New("review thread has no comments")
	}
	thread := domain.GitHubReviewThread{NodeID: raw.ID, Resolved: raw.Resolved, Outdated: raw.Outdated, Path: raw.Path, Line: raw.Line, Comments: make([]domain.GitHubReviewComment, 0, len(raw.Comments.Nodes))}
	bodies := make([]domain.InlineReviewBody, 0, len(raw.Comments.Nodes))
	rootID := ""
	rootDatabaseID := int64(0)
	for _, comment := range raw.Comments.Nodes {
		normalized, body, err := comment.normalized(raw.ID)
		if err != nil {
			return domain.GitHubReviewThread{}, nil, err
		}
		if normalized.ReplyToNodeID == "" {
			if rootID != "" {
				return domain.GitHubReviewThread{}, nil, errors.New("review thread has multiple root comments")
			}
			originalCommit := comment.OriginalCommit
			if originalCommit == nil || !validFullSHA(originalCommit.OID) {
				return domain.GitHubReviewThread{}, nil, errors.New("review thread original commit is incomplete")
			}
			rootID, rootDatabaseID = normalized.NodeID, normalized.DatabaseID
			thread.OriginalCommitSHA = originalCommit.OID
		}
		thread.Comments = append(thread.Comments, normalized)
		bodies = append(bodies, body)
	}
	if rootID == "" {
		return domain.GitHubReviewThread{}, nil, errors.New("review thread root comment is missing")
	}
	for _, comment := range thread.Comments {
		if comment.ReplyToNodeID != "" && (comment.ReplyToNodeID != rootID || comment.ReplyToDatabaseID != rootDatabaseID) {
			return domain.GitHubReviewThread{}, nil, errors.New("review thread reply topology is incomplete")
		}
	}
	return thread, bodies, nil
}

func (raw rawReviewComment) normalized(threadNodeID string) (domain.GitHubReviewComment, domain.InlineReviewBody, error) {
	if raw.DatabaseID < 1 || strings.TrimSpace(raw.ID) == "" {
		return domain.GitHubReviewComment{}, domain.InlineReviewBody{}, errors.New("review comment identity is missing")
	}
	if raw.ReplyTo != nil && (raw.ReplyTo.DatabaseID < 1 || strings.TrimSpace(raw.ReplyTo.ID) == "") {
		return domain.GitHubReviewComment{}, domain.InlineReviewBody{}, errors.New("review comment reply identity is missing")
	}
	if raw.Author != nil && !validReviewActor(*raw.Author) {
		return domain.GitHubReviewComment{}, domain.InlineReviewBody{}, errors.New("review comment author identity is incomplete")
	}
	if raw.Review == nil || raw.Review.DatabaseID < 1 || strings.TrimSpace(raw.Review.ID) == "" || !validReviewState(raw.Review.State) || raw.Review.Commit == nil || !validFullSHA(raw.Review.Commit.OID) || raw.Review.SubmittedAt.IsZero() {
		return domain.GitHubReviewComment{}, domain.InlineReviewBody{}, errors.New("review comment review identity is incomplete")
	}
	if raw.Review.Author != nil && !validReviewActor(*raw.Review.Author) {
		return domain.GitHubReviewComment{}, domain.InlineReviewBody{}, errors.New("review comment review author identity is incomplete")
	}
	if raw.CreatedAt.IsZero() || raw.UpdatedAt.IsZero() {
		return domain.GitHubReviewComment{}, domain.InlineReviewBody{}, errors.New("review comment timestamps are missing")
	}
	result := domain.GitHubReviewComment{DatabaseID: raw.DatabaseID, NodeID: raw.ID, Review: domain.GitHubReview{DatabaseID: raw.Review.DatabaseID, NodeID: raw.Review.ID, State: raw.Review.State, CommitSHA: raw.Review.Commit.OID, SourceAt: raw.Review.SubmittedAt}}
	if raw.ReplyTo != nil {
		result.ReplyToDatabaseID, result.ReplyToNodeID = raw.ReplyTo.DatabaseID, raw.ReplyTo.ID
	}
	if raw.Author != nil {
		result.Author = &domain.ActorIdentity{DatabaseID: raw.Author.DatabaseID, NodeID: raw.Author.ID, Login: raw.Author.Login, Type: raw.Author.Typename}
	}
	if raw.Review.Author != nil {
		result.Review.Actor = domain.ActorIdentity{DatabaseID: raw.Review.Author.DatabaseID, NodeID: raw.Review.Author.ID, Login: raw.Review.Author.Login, Type: raw.Review.Author.Typename}
	}
	bodyDigest := sha256.Sum256([]byte(raw.Body))
	result.BodyDigest = hex.EncodeToString(bodyDigest[:])
	result.CreatedAt, result.UpdatedAt = raw.CreatedAt.UTC(), raw.UpdatedAt.UTC()
	body := domain.InlineReviewBody{ThreadNodeID: threadNodeID, CommentNodeID: raw.ID, Body: raw.Body, BodyDigest: result.BodyDigest}
	return result, body, nil
}

func validFullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func validReviewState(value string) bool {
	switch value {
	case "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED", "PENDING":
		return true
	default:
		return false
	}
}

func validReviewActor(actor struct {
	Login      string `json:"login"`
	Typename   string `json:"__typename"`
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseId"`
}) bool {
	if actor.DatabaseID < 1 || strings.TrimSpace(actor.ID) == "" || strings.TrimSpace(actor.Login) == "" {
		return false
	}
	switch actor.Typename {
	case "User", "Bot", "App":
		return true
	default:
		return false
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
