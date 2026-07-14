package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVersionedFixtureScenarioIndex(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Version   int      `json:"version"`
		Sanitized bool     `json:"sanitized"`
		Scenarios []string `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	if index.Version != 1 || !index.Sanitized || len(index.Scenarios) < 18 {
		t.Fatalf("invalid fixture index: %+v", index)
	}
	seen := map[string]bool{}
	for _, name := range index.Scenarios {
		if seen[name] || name == "" {
			t.Fatalf("duplicate/blank scenario %q", name)
		}
		seen[name] = true
		t.Run(name, func(t *testing.T) { replayDeclaredScenario(t, name) })
	}
}

func TestConfigRejectsRemovedCodeRabbitFields(t *testing.T) {
	_, key := testKey(t)
	cfg := validConfig(key)
	raw := map[string]any{
		"api_base_url":        cfg.APIBaseURL,
		"graphql_url":         cfg.GraphQLURL,
		"app_id":              cfg.AppID,
		"installation_id":     cfg.InstallationID,
		"repository_owner":    cfg.RepositoryOwner,
		"repository_name":     cfg.RepositoryName,
		"repository_id":       cfg.RepositoryID,
		"private_key_file":    cfg.PrivateKeyFile,
		"http_timeout":        cfg.HTTPTimeout.String(),
		"token_refresh_skew":  cfg.TokenRefreshSkew.String(),
		"api_version":         cfg.APIVersion,
		"pull_requests_write": cfg.PullRequestsWrite,
		"squash_merge_write":  cfg.SquashMergeWrite,
	}
	raw["coderabbit_actor_id"] = 1
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeConfigWithoutPrivateKey(bytes.NewReader(encoded)); err == nil {
		t.Fatal("deprecated CodeRabbit configuration was accepted")
	}
}

func replayDeclaredScenario(t *testing.T, name string) {
	t.Helper()
	switch name {
	case "valid_installation_token_metadata", "wrong_installation", "wrong_repository", "token_expiry_refresh", "single_401_refresh", "repeated_401":
		replayAuthScenario(t, name)
	case "permission_403", "rate_limit", "malformed_json", "graphql_partial_data_errors":
		replayTransportScenario(t, name)
	case "pagination":
		replayPaginationScenario(t)
	case "pr_open", "pr_closed_unmerged", "pr_squash_merged", "head_match", "head_mismatch", "base_match", "base_mismatch":
		replayPRScenario(t, name)
	case "required_checks_pass", "required_checks_pending", "actionable_check_failure", "missing_required_check", "unknown_check_state":
		replayCheckScenario(t, name)
	default:
		t.Fatalf("declared scenario has no replay assertion: %s", name)
	}
}

func replayAuthScenario(t *testing.T, name string) {
	_, key := testKey(t)
	var mints, reads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var scope struct {
			RepositoryIDs []int64 `json:"repository_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&scope); err != nil || len(scope.RepositoryIDs) != 1 || scope.RepositoryIDs[0] != 99 {
			http.Error(w, "invalid repository scope", http.StatusBadRequest)
			return
		}
		n := mints.Add(1)
		repoID := 99
		if name == "wrong_repository" {
			repoID = 100
		}
		expiry := "2026-07-11T01:00:00Z"
		if name == "token_expiry_refresh" {
			expiry = "2026-07-11T00:03:00Z"
		}
		fmt.Fprintf(w, `{"token":"token-%d","expires_at":"%s","permissions":{"metadata":"read","contents":"read","pull_requests":"read","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":%d,"name":"repo","owner":{"login":"owner"}}]}`, n, expiry, repoID)
	})
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		attempt := reads.Add(1)
		if name == "repeated_401" || name == "single_401_refresh" && attempt == 1 {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"id":99,"node_id":"R","name":"repo","owner":{"login":"owner"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installation := int64(2)
	if name == "wrong_installation" {
		installation = 3
	}
	c := &Client{cfg: Config{APIBaseURL: srv.URL, AppID: 1, InstallationID: installation, RepositoryOwner: "owner", RepositoryName: "repo", RepositoryID: 99, PrivateKeyFile: key, TokenRefreshSkew: 5 * time.Minute, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}}
	err := c.ensureToken(context.Background(), false)
	if name == "wrong_installation" || name == "wrong_repository" {
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if name == "token_expiry_refresh" {
		if err := c.ensureToken(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		if mints.Load() != 2 {
			t.Fatalf("refresh mints=%d", mints.Load())
		}
		return
	}
	if name == "valid_installation_token_metadata" {
		if c.token == "" || c.expires.IsZero() {
			t.Fatal("token metadata missing")
		}
		return
	}
	var repo map[string]any
	err = c.rest(context.Background(), "repository", "GET", "/repos/owner/repo", nil, &repo, true)
	if name == "repeated_401" {
		if err == nil || mints.Load() != 2 {
			t.Fatalf("repeated 401 err=%v mints=%d", err, mints.Load())
		}
		return
	}
	if err != nil || mints.Load() != 2 || reads.Load() != 2 {
		t.Fatalf("single refresh err=%v mints=%d reads=%d", err, mints.Load(), reads.Load())
	}
}

func replayTransportScenario(t *testing.T, name string) {
	status := 200
	body := `{}`
	headers := map[string]string{}
	category := "REST"
	want := ""
	switch name {
	case "permission_403":
		status = 403
		want = "permission_denied"
	case "rate_limit":
		status = 403
		headers["X-RateLimit-Remaining"] = "0"
		want = "rate_limited"
	case "malformed_json":
		body = `{`
		want = "malformed_json"
	case "graphql_partial_data_errors":
		category = "GraphQL"
		body = `{"data":{},"errors":[{"message":"partial"}]}`
		want = "graphql_errors"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	var observed application.GitHubRequestObservation
	c := &Client{cfg: Config{APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, observe: func(o application.GitHubRequestObservation) { observed = o }}
	var out map[string]any
	_ = c.do(context.Background(), name, category, "GET", srv.URL, nil, "Bearer fixture", &out, true)
	if observed.ErrorClass != want {
		t.Fatalf("class=%s want=%s", observed.ErrorClass, want)
	}
}

func replayPaginationScenario(t *testing.T) {
	var pages atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"contexts":["required"],"checks":[]}`) })
	mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		page := pages.Add(1)
		if page == 1 {
			fmt.Fprint(w, `{"check_runs":[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"id":%d,"name":"n%d","status":"completed","conclusion":"success","app":{"id":1}}`, i+1, i)
			}
			fmt.Fprint(w, `]}`)
			return
		}
		fmt.Fprint(w, `{"check_runs":[]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"total_count":0,"statuses":[]}`) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{cfg: Config{APIBaseURL: srv.URL, RepositoryOwner: "owner", RepositoryName: "repo", APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
	if _, _, err := c.readChecks(context.Background(), "head", "main"); err != nil {
		t.Fatal(err)
	}
	if pages.Load() != 2 {
		t.Fatalf("pages=%d", pages.Load())
	}
}

func replayPRScenario(t *testing.T, name string) {
	repo := domain.RepositoryIdentity{ID: 1, NodeID: "R", Owner: "o", Name: "r"}
	pr := domain.PullRequest{Number: 1, DatabaseID: 2, NodeID: "P", URL: "u", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", OwnershipKey: "key", BodyDigest: "body", State: "open"}
	if name == "pr_closed_unmerged" {
		pr.State = "closed"
	}
	if name == "pr_squash_merged" {
		pr.State = "closed"
		pr.Merged = true
		pr.MergeSHA = "merge"
	}
	got := domain.GitHubReadEvidence{Repository: repo, PullRequest: pr}
	expected := pr
	switch name {
	case "head_mismatch":
		got.PullRequest.HeadSHA = "wrong"
	case "base_mismatch":
		got.PullRequest.BaseSHA = "wrong"
	}
	err := application.ReconcileGitHubRead(repo, expected, "feature", "main", "head", "base", "key", "body", got)
	wantError := strings.HasSuffix(name, "mismatch")
	if (err != nil) != wantError {
		t.Fatalf("%s err=%v", name, err)
	}
}

func replayCheckScenario(t *testing.T, name string) {
	state := map[string]domain.CheckState{"required_checks_pass": domain.CheckSuccess, "required_checks_pending": domain.CheckPending, "actionable_check_failure": domain.CheckFailure, "unknown_check_state": domain.CheckUnknown}[name]
	checks := []domain.GitHubCheck{}
	unknown := []string{}
	if name == "missing_required_check" {
		unknown = []string{"missing_required_check:test"}
	} else {
		checks = []domain.GitHubCheck{{Name: "test", Required: true, State: state, ObservedSHA: "head"}}
	}
	e := domain.GitHubReadEvidence{PullRequest: domain.PullRequest{HeadSHA: "head"}, Checks: checks, UnknownEvents: unknown}
	got := e.RequiredChecksStatus()
	want := map[string]domain.ReconciliationStatus{"required_checks_pass": domain.ReconciliationPass, "required_checks_pending": domain.ReconciliationPending, "actionable_check_failure": domain.ReconciliationActionable, "missing_required_check": domain.ReconciliationInfrastructure, "unknown_check_state": domain.ReconciliationInfrastructure}[name]
	if got != want {
		t.Fatalf("status=%s want=%s", got, want)
	}
}

func TestCheckStateMapping(t *testing.T) {
	cases := []struct {
		status, conclusion string
		want               domain.CheckState
	}{{"queued", "", domain.CheckQueued}, {"in_progress", "", domain.CheckInProgress}, {"pending", "", domain.CheckPending}, {"requested", "", domain.CheckRequested}, {"waiting", "", domain.CheckWaiting}, {"completed", "success", domain.CheckSuccess}, {"completed", "neutral", domain.CheckNeutral}, {"completed", "skipped", domain.CheckSkipped}, {"completed", "failure", domain.CheckFailure}, {"completed", "action_required", domain.CheckActionRequired}, {"completed", "cancelled", domain.CheckCancelled}, {"completed", "timed_out", domain.CheckTimedOut}, {"completed", "stale", domain.CheckStale}, {"completed", "new_state", domain.CheckUnknown}}
	for _, tc := range cases {
		if got := mapCheck(tc.status, tc.conclusion); got != tc.want {
			t.Fatalf("%s/%s=%s want %s", tc.status, tc.conclusion, got, tc.want)
		}
	}
}

func TestPullRequestNormalizationStatesAndOwnership(t *testing.T) {
	open := rawPR{ID: 1, Number: 2, NodeID: "P", State: "closed", Body: "x\n<!-- controller-run:key -->"}
	open.Head.Ref = "feature"
	open.Head.SHA = "head"
	open.Base.Ref = "main"
	open.Base.SHA = "base"
	got := open.normalized()
	if got.Merged || got.State != "closed" || got.OwnershipKey != "key" || got.DatabaseID != 1 {
		t.Fatalf("closed-unmerged=%+v", got)
	}
	open.Merged = true
	open.MergeSHA = "merge"
	open.MergedAt = time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	got = open.normalized()
	if !got.Merged || got.MergeSHA != "merge" || got.MergedAt.IsZero() {
		t.Fatalf("squash-merged=%+v", got)
	}
}

func TestLatestCheckRunWinsDeterministically(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"contexts":[],"checks":[{"context":"test","app_id":8}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"check_runs":[{"id":3,"name":"test","status":"in_progress","conclusion":"","started_at":"2026-07-11T00:04:00Z","completed_at":null,"app":{"id":8}},{"id":2,"name":"test","status":"completed","conclusion":"failure","started_at":"2026-07-11T00:02:00Z","completed_at":"2026-07-11T00:03:00Z","app":{"id":8}},{"id":1,"name":"test","status":"completed","conclusion":"success","started_at":"2026-07-11T00:00:00Z","completed_at":"2026-07-11T00:01:00Z","app":{"id":8}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"total_count":0,"statuses":[]}`) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{cfg: Config{APIBaseURL: srv.URL, RepositoryOwner: "owner", RepositoryName: "repo", APIVersion: "2022-11-28", InstallationID: 2}, http: srv.Client(), clock: fixedClock{time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)}, token: "token"}
	checks, _, err := c.readChecks(context.Background(), "head", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].State != domain.CheckInProgress {
		t.Fatalf("checks=%+v", checks)
	}
}

func TestReviewConnectionPaginates(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		call := calls.Add(1)
		if call == 1 {
			fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED","reviews":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"review-1"}}}}}}`)
			return
		}
		if request.Variables["reviewCursor"] != "review-1" {
			t.Errorf("reviewCursor=%v", request.Variables["reviewCursor"])
		}
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED","reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	}))
	defer srv.Close()
	c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
	if _, _, err := c.readReviews(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestReviewReadIncludesImmutableUserIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(request.Query, "... on User{id databaseId}") {
			http.Error(w, "user identity fields missing", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"APPROVED","reviews":{"nodes":[{"id":"PRR_55","databaseId":55,"state":"APPROVED","commit":{"oid":"head"},"submittedAt":"2026-07-13T01:00:00Z","author":{"login":"ifan0927","__typename":"User","id":"USER_33","databaseId":33}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	}))
	defer srv.Close()
	c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
	reviews, _, err := c.readReviews(context.Background(), 1)
	if err != nil || len(reviews) != 1 || reviews[0].Actor.DatabaseID != 33 || reviews[0].Actor.NodeID != "USER_33" || reviews[0].Actor.Type != "User" {
		t.Fatalf("reviews=%+v err=%v", reviews, err)
	}
}

func TestHTTPFailureClassificationAndBounds(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		class   string
	}{{"permission", 403, nil, `{}`, "permission_denied"}, {"rate", 403, map[string]string{"X-RateLimit-Remaining": "0"}, `{}`, "rate_limited"}, {"not-found", 404, nil, `{}`, "not_found"}, {"malformed", 200, nil, `{`, "malformed_json"}, {"graphql-errors", 200, nil, `{"data":{},"errors":[{"message":"limited"}]}`, "graphql_errors"}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			var obs []application.GitHubRequestObservation
			c := &Client{cfg: Config{APIVersion: "2022-11-28", InstallationID: 2}, http: srv.Client(), clock: fixedClock{time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}, observe: func(o application.GitHubRequestObservation) { obs = append(obs, o) }}
			var out map[string]any
			_ = c.do(context.Background(), "test", map[bool]string{true: "GraphQL", false: "REST"}[tc.name == "graphql-errors"], "GET", srv.URL, nil, "Bearer fixture", &out, true)
			if len(obs) != 1 || obs[0].ErrorClass != tc.class {
				t.Fatalf("observations=%+v", obs)
			}
		})
	}
}

func TestFixtureReplayAndRestartMint(t *testing.T) {
	_, key := testKey(t)
	var mint atomic.Int32
	server := fixtureServer(t, &mint, false)
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL = server.URL
	cfg.GraphQLURL = server.URL + "/graphql"
	cfg.RepositoryID = 99
	clock := fixedClock{time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}
	for i := 0; i < 2; i++ {
		client, err := New(cfg, clock, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.Read(context.Background(), 1, "headsha")
		if err != nil {
			t.Fatal(err)
		}
		if got.Repository.ID != 99 || got.PullRequest.HeadSHA != "headsha" || len(got.Reviews) != 0 || len(got.Findings) != 0 {
			t.Fatalf("unexpected replay: %+v", got)
		}
	}
	if mint.Load() != 2 {
		t.Fatalf("restart mint count=%d", mint.Load())
	}
}

func Test401RefreshOnceAndRepeatedFailure(t *testing.T) {
	_, key := testKey(t)
	var mint atomic.Int32
	server := fixtureServer(t, &mint, true)
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL = server.URL
	cfg.GraphQLURL = server.URL + "/graphql"
	cfg.RepositoryID = 99
	client, _ := New(cfg, fixedClock{time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}, nil)
	if _, err := client.Read(context.Background(), 1, "headsha"); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err=%v", err)
	}
	if mint.Load() != 2 {
		t.Fatalf("mint count=%d", mint.Load())
	}
}

func TestSecretSafeObservationsAndErrors(t *testing.T) {
	_, key := testKey(t)
	var mint atomic.Int32
	server := fixtureServer(t, &mint, false)
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL = server.URL
	cfg.GraphQLURL = server.URL + "/graphql"
	cfg.RepositoryID = 99
	var observations []application.GitHubRequestObservation
	client, _ := New(cfg, fixedClock{time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}, func(o application.GitHubRequestObservation) { observations = append(observations, o) })
	_, err := client.Read(context.Background(), 1, "wrong")
	combined := fmt.Sprint(err, observations)
	for _, secret := range []string{"fixture-installation-secret", "BEGIN PRIVATE KEY", "Bearer "} {
		if strings.Contains(combined, secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
}

func fixtureServer(t *testing.T, mint *atomic.Int32, always401 bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, s string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, s)
	}
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mint.Add(1)
		write(w, `{"token":"fixture-installation-secret","expires_at":"2026-07-11T01:00:00Z","permissions":{"metadata":"read","contents":"read","pull_requests":"read","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		if always401 {
			w.WriteHeader(401)
			return
		}
		write(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"id":101,"number":1,"html_url":"https://example.invalid/pr/1","node_id":"PR","state":"open","merged":false,"body":"body","head":{"ref":"feature","sha":"headsha","repo":{"id":99}},"base":{"ref":"main","sha":"basesha","repo":{"id":99}}}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/headsha/check-runs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:00:00Z","app":{"id":8}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) { write(w, `{"contexts":["test"],"checks":[]}`) })
	mux.HandleFunc("/repos/owner/repo/commits/headsha/status", func(w http.ResponseWriter, r *http.Request) { write(w, `{"statuses":[]}`) })
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
			return
		}
		if !strings.Contains(request.Query, "... on User{id databaseId}") || strings.Contains(request.Query, "reviewThreads") {
			http.Error(w, "invalid review selection", http.StatusBadRequest)
			return
		}
		write(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED","reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	})
	return httptest.NewServer(mux)
}
