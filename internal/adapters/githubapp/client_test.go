package githubapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type adjustableClock struct{ value time.Time }

func (c *adjustableClock) Now() time.Time { return c.value }

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

func TestInstallationTokenRefreshContinuesBeyondSevenDays(t *testing.T) {
	_, key := testKey(t)
	clock := &adjustableClock{value: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}
	var mints atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		n := mints.Add(1)
		fmt.Fprintf(w, `{"token":"token-%d","expires_at":"%s","permissions":{"metadata":"read","contents":"read","pull_requests":"read","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`, n, clock.Now().Add(time.Hour).Format(time.RFC3339))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := &Client{cfg: Config{APIBaseURL: server.URL, AppID: 1, InstallationID: 2, RepositoryOwner: "owner", RepositoryName: "repo", RepositoryID: 99, PrivateKeyFile: key, TokenRefreshSkew: 5 * time.Minute, APIVersion: "2022-11-28"}, http: server.Client(), clock: clock}
	for day := 0; day <= 8; day++ {
		if err := client.ensureToken(context.Background(), false); err != nil {
			t.Fatalf("day=%d: %v", day, err)
		}
		clock.value = clock.value.Add(24 * time.Hour)
	}
	if got := mints.Load(); got != 9 {
		t.Fatalf("token mints=%d", got)
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
			fmt.Fprint(w, `{"total_count":101,"check_runs":[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"id":%d,"name":"n%d","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:00:00Z","app":{"id":1}}`, i+1, i)
			}
			fmt.Fprint(w, `]}`)
			return
		}
		fmt.Fprint(w, `{"total_count":101,"check_runs":[{"id":101,"name":"required","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:01:00Z","app":{"id":1}}]}`)
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

func TestCheckRunPaginationFailsClosedOnIncompleteOrDriftingCounts(t *testing.T) {
	tests := []struct {
		name string
		page func(http.ResponseWriter, int)
	}{
		{"truncated short page", func(w http.ResponseWriter, page int) { writeCheckRunFixturePage(w, 2, page, 1) }},
		{"missing newer same-context failure", func(w http.ResponseWriter, page int) {
			if page == 1 {
				writeCheckRunFixturePage(w, 101, page, 100)
				return
			}
			fmt.Fprint(w, `{"total_count":101,"check_runs":[]}`)
		}},
		{"total count drift", func(w http.ResponseWriter, page int) {
			if page == 1 {
				writeCheckRunFixturePage(w, 101, page, 100)
				return
			}
			writeCheckRunFixturePage(w, 102, page, 1)
		}},
		{"over twenty pages", func(w http.ResponseWriter, page int) { writeCheckRunFixturePage(w, 2001, page, 100) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"contexts":["test"],"checks":[]}`) })
			mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				test.page(w, page)
			})
			mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"total_count":0,"statuses":[]}`) })
			server := httptest.NewServer(mux)
			defer server.Close()
			client := &Client{cfg: Config{APIBaseURL: server.URL, RepositoryOwner: "owner", RepositoryName: "repo", APIVersion: "2022-11-28"}, http: server.Client(), clock: fixedClock{time.Now()}, token: "token"}
			if _, _, err := client.readChecks(context.Background(), "head", "main"); err == nil {
				t.Fatal("incomplete check-run pagination was accepted")
			}
		})
	}
}

func writeCheckRunFixturePage(w http.ResponseWriter, total, page, count int) {
	fmt.Fprintf(w, `{"total_count":%d,"check_runs":[`, total)
	for index := 0; index < count; index++ {
		if index > 0 {
			fmt.Fprint(w, ",")
		}
		id := int64(page-1)*100 + int64(index) + 1
		name, conclusion := fmt.Sprintf("check-%d", id), "success"
		if id == 1 || id == 101 {
			name = "test"
		}
		if id == 101 {
			conclusion = "failure"
		}
		fmt.Fprintf(w, `{"id":%d,"name":%q,"status":"completed","conclusion":%q,"completed_at":"2026-07-11T00:01:00Z","app":{"id":8}}`, id, name, conclusion)
	}
	fmt.Fprint(w, `]}`)
}

func TestRequiredCheckIdentityValidationRejectsMalformed2xxEvidence(t *testing.T) {
	validProtection := `{"contexts":["test"],"checks":[]}`
	validCheck := `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:01:00Z","app":{"id":8}}]}`
	validStatuses := `{"total_count":0,"statuses":[]}`
	tests := []struct {
		name, protection, checks, statuses string
	}{
		{"empty protection context", `{"contexts":[""],"checks":[]}`, `{"total_count":0,"check_runs":[]}`, validStatuses},
		{"noncanonical protection context", `{"contexts":[" test"],"checks":[]}`, `{"total_count":0,"check_runs":[]}`, validStatuses},
		{"conflicting context app binding", `{"contexts":["test"],"checks":[{"context":"test","app_id":8}]}`, validCheck, validStatuses},
		{"duplicate app binding", `{"contexts":[],"checks":[{"context":"test","app_id":8},{"context":"test","app_id":8}]}`, validCheck, validStatuses},
		{"invalid app binding", `{"contexts":[],"checks":[{"context":"test","app_id":0}]}`, validCheck, validStatuses},
		{"check run id zero", validProtection, `{"total_count":1,"check_runs":[{"id":0,"name":"test","status":"queued","app":{"id":8}}]}`, validStatuses},
		{"check run name empty", validProtection, `{"total_count":1,"check_runs":[{"id":1,"name":"","status":"queued","app":{"id":8}}]}`, validStatuses},
		{"check run app missing", validProtection, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"queued","app":{"id":0}}]}`, validStatuses},
		{"completed check timestamp missing", validProtection, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","app":{"id":8}}]}`, validStatuses},
		{"in progress timestamp missing", validProtection, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"in_progress","app":{"id":8}}]}`, validStatuses},
		{"status total count missing despite passing check", validProtection, validCheck, `{"statuses":[]}`},
		{"status id zero", validProtection, validCheck, `{"total_count":1,"statuses":[{"id":0,"context":"test","state":"success","updated_at":"2026-07-11T00:02:00Z"}]}`},
		{"status context empty", validProtection, validCheck, `{"total_count":1,"statuses":[{"id":2,"context":"","state":"success","updated_at":"2026-07-11T00:02:00Z"}]}`},
		{"status timestamp missing", validProtection, validCheck, `{"total_count":1,"statuses":[{"id":2,"context":"test","state":"success"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, closeServer := checkEvidenceFixtureClient(t, test.protection, test.checks, test.statuses)
			defer closeServer()
			if _, _, err := client.readChecks(context.Background(), "head", "main"); err == nil {
				t.Fatal("malformed 2xx check evidence was accepted")
			}
		})
	}
}

func TestQueuedCheckRunAllowsNullTimestampsWithImmutableIdentity(t *testing.T) {
	client, closeServer := checkEvidenceFixtureClient(t, `{"contexts":["test"],"checks":[]}`, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"queued","started_at":null,"completed_at":null,"app":{"id":8}}]}`, `{"total_count":0,"statuses":[]}`)
	defer closeServer()
	checks, unknown, err := client.readChecks(context.Background(), "head", "main")
	if err != nil {
		t.Fatal(err)
	}
	evidence := domain.GitHubReadEvidence{PullRequest: domain.PullRequest{HeadSHA: "head"}, Checks: checks, UnknownEvents: unknown}
	if evidence.RequiredChecksStatus() != domain.ReconciliationPending || len(checks) != 1 || checks[0].ID != "1" || checks[0].SourceAt != (time.Time{}) {
		t.Fatalf("checks=%+v unknown=%v status=%s", checks, unknown, evidence.RequiredChecksStatus())
	}
}

func checkEvidenceFixtureClient(t *testing.T, protection, checks, statuses string) (*Client, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, protection) })
	mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, checks) })
	mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, statuses) })
	server := httptest.NewServer(mux)
	client := &Client{cfg: Config{APIBaseURL: server.URL, RepositoryOwner: "owner", RepositoryName: "repo", APIVersion: "2022-11-28"}, http: server.Client(), clock: fixedClock{time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)}, token: "token"}
	return client, server.Close
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
	want := map[string]domain.ReconciliationStatus{"required_checks_pass": domain.ReconciliationPass, "required_checks_pending": domain.ReconciliationPending, "actionable_check_failure": domain.ReconciliationActionable, "missing_required_check": domain.ReconciliationPending, "unknown_check_state": domain.ReconciliationInfrastructure}[name]
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
		fmt.Fprint(w, `{"total_count":3,"check_runs":[{"id":3,"name":"test","status":"in_progress","conclusion":"","started_at":"2026-07-11T00:04:00Z","completed_at":null,"app":{"id":8}},{"id":2,"name":"test","status":"completed","conclusion":"failure","started_at":"2026-07-11T00:02:00Z","completed_at":"2026-07-11T00:03:00Z","app":{"id":8}},{"id":1,"name":"test","status":"completed","conclusion":"success","started_at":"2026-07-11T00:00:00Z","completed_at":"2026-07-11T00:01:00Z","app":{"id":8}}]}`)
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

func TestUnboundRequiredContextAggregatesCheckRunAndCommitStatus(t *testing.T) {
	tests := []struct {
		name, protection, checkState, checkConclusion, statusState string
		wantStatus                                                 domain.ReconciliationStatus
		wantRequired                                               int
	}{
		{"check success status failure", `{"contexts":["test"],"checks":[]}`, "completed", "success", "failure", domain.ReconciliationActionable, 2},
		{"check failure status success", `{"contexts":["test"],"checks":[]}`, "completed", "failure", "success", domain.ReconciliationActionable, 2},
		{"check pending status pending", `{"contexts":["test"],"checks":[]}`, "in_progress", "", "pending", domain.ReconciliationPending, 2},
		{"check pending status success", `{"contexts":["test"],"checks":[]}`, "in_progress", "", "success", domain.ReconciliationPending, 2},
		{"both success", `{"contexts":["test"],"checks":[]}`, "completed", "success", "success", domain.ReconciliationPass, 2},
		{"both unknown", `{"contexts":["test"],"checks":[]}`, "completed", "new_state", "new_state", domain.ReconciliationInfrastructure, 2},
		{"app bound excludes status", `{"contexts":[],"checks":[{"context":"test","app_id":8}]}`, "completed", "success", "failure", domain.ReconciliationPass, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, test.protection) })
			mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":10,"name":"test","status":%q,"conclusion":%q,"started_at":"2026-07-11T00:00:00Z","completed_at":"2026-07-11T00:01:00Z","app":{"id":8}}]}`, test.checkState, test.checkConclusion)
			})
			mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"total_count":1,"statuses":[{"id":20,"context":"test","state":%q,"updated_at":"2026-07-11T00:02:00Z"}]}`, test.statusState)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			client := &Client{cfg: Config{APIBaseURL: server.URL, RepositoryOwner: "owner", RepositoryName: "repo", APIVersion: "2022-11-28", InstallationID: 2}, http: server.Client(), clock: fixedClock{time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)}, token: "token"}
			checks, unknown, err := client.readChecks(context.Background(), "head", "main")
			if err != nil {
				t.Fatal(err)
			}
			required := 0
			for _, check := range checks {
				if check.Required {
					required++
				}
			}
			evidence := domain.GitHubReadEvidence{PullRequest: domain.PullRequest{HeadSHA: "head"}, Checks: checks, UnknownEvents: unknown}
			if evidence.RequiredChecksStatus() != test.wantStatus || required != test.wantRequired {
				t.Fatalf("checks=%+v unknown=%v status=%s required=%d", checks, unknown, evidence.RequiredChecksStatus(), required)
			}
			if test.wantStatus == domain.ReconciliationInfrastructure && len(unknown) != 2 {
				t.Fatalf("unknown telemetry=%v", unknown)
			}
			checks2, unknown2, err := client.readChecks(context.Background(), "head", "main")
			if err != nil || !reflect.DeepEqual(checks, checks2) || !reflect.DeepEqual(unknown, unknown2) {
				t.Fatalf("nondeterministic checks: first=%+v/%v second=%+v/%v err=%v", checks, unknown, checks2, unknown2, err)
			}
		})
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

func TestReviewReadRejectsMissingPageMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}}`)
	}))
	defer srv.Close()
	c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
	if _, _, err := c.readReviews(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("err=%v", err)
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

func TestReviewThreadQueryHasBalancedBraces(t *testing.T) {
	if opens, closes := strings.Count(reviewThreadQuery, "{"), strings.Count(reviewThreadQuery, "}"); opens != closes {
		t.Fatalf("review thread query braces are unbalanced: opens=%d closes=%d", opens, closes)
	}
	if !strings.Contains(reviewThreadQuery, "replyTo{id databaseId} originalCommit{oid}") || !strings.Contains(reviewThreadQuery, "pageInfo{hasNextPage endCursor}}} pageInfo{hasNextPage endCursor}") {
		t.Fatal("review thread query does not match GitHub connection and comment evidence shape")
	}
}

func TestReviewThreadReadCollectsCompleteTopologyWithoutSerializingBodies(t *testing.T) {
	const body = "Authorization: Bearer inline-body-secret \u00e9"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(request.Query, "reviewThreads(first:100") || !strings.Contains(request.Query, "comments(first:100)") || !strings.Contains(request.Query, "originalCommit{oid}") {
			http.Error(w, "review thread selection is incomplete", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"THREAD_1","isResolved":false,"isOutdated":false,"path":"internal/a.go","line":7,"comments":{"nodes":[{"id":"COMMENT_1","databaseId":1,"replyTo":null,"originalCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"author":{"login":"trusted","__typename":"User","id":"USER_1","databaseId":11},"pullRequestReview":{"id":"REVIEW_1","databaseId":21,"state":"CHANGES_REQUESTED","commit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"submittedAt":"2026-07-14T01:00:00Z","author":{"login":"trusted","__typename":"User","id":"USER_1","databaseId":11}},"body":%q,"createdAt":"2026-07-14T01:01:00Z","updatedAt":"2026-07-14T01:02:00Z"},{"id":"COMMENT_2","databaseId":2,"replyTo":{"id":"COMMENT_1","databaseId":1},"author":{"login":"lookalike","__typename":"Bot","id":"BOT_2","databaseId":12},"pullRequestReview":{"id":"REVIEW_1","databaseId":21,"state":"CHANGES_REQUESTED","commit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"submittedAt":"2026-07-14T01:00:00Z","author":{"login":"trusted","__typename":"User","id":"USER_1","databaseId":11}},"body":"reply","createdAt":"2026-07-14T01:03:00Z","updatedAt":"2026-07-14T01:04:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}},{"id":"THREAD_2","isResolved":true,"isOutdated":true,"path":"","line":null,"comments":{"nodes":[{"id":"COMMENT_3","databaseId":3,"replyTo":null,"originalCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"author":null,"pullRequestReview":{"id":"REVIEW_2","databaseId":22,"state":"COMMENTED","commit":{"oid":"dddddddddddddddddddddddddddddddddddddddd"},"submittedAt":"2026-07-14T01:05:00Z","author":{"login":"app","__typename":"App","id":"APP_3","databaseId":13}},"body":"deleted author","createdAt":"2026-07-14T01:06:00Z","updatedAt":"2026-07-14T01:07:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`, body)
	}))
	defer srv.Close()
	var observations []application.GitHubRequestObservation
	c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "fixture-installation-token", observe: func(observation application.GitHubRequestObservation) {
		observations = append(observations, observation)
	}}
	threads, handoff, err := c.readReviewThreads(context.Background(), 1)
	if err != nil || len(threads) != 2 {
		t.Fatalf("unexpected thread count or error: %d %v", len(threads), err)
	}
	root, reply := threads[0].Comments[0], threads[0].Comments[1]
	sum := sha256.Sum256([]byte(body))
	if len(handoff.Comments) != 3 || handoff.Comments[0].Body != body || root.BodyDigest != fmt.Sprintf("%x", sum) || reply.ReplyToNodeID != root.NodeID || reply.ReplyToDatabaseID != root.DatabaseID || reply.Author == nil || reply.Author.Type != "Bot" || threads[1].Comments[0].Author != nil || threads[1].Comments[0].Review.Actor.Type != "App" || !threads[1].Resolved || !threads[1].Outdated || threads[1].Line != nil {
		t.Fatalf("unexpected topology: %+v", threads)
	}
	raw, err := json.Marshal(domain.GitHubReadEvidence{ReviewThreads: threads})
	if err != nil || strings.Contains(string(raw), body) || strings.Contains(string(raw), "inline-body-secret") {
		t.Fatalf("raw inline body leaked into evidence: %s err=%v", raw, err)
	}
	combined := fmt.Sprint(err, observations, string(raw))
	for _, forbidden := range []string{"inline-body-secret", "Authorization: Bearer", "fixture-installation-token", "BEGIN PRIVATE KEY"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("sensitive value leaked from a GitHub read observation: %q", forbidden)
		}
	}
}

func TestReviewThreadReadPaginationAndFailureBounds(t *testing.T) {
	t.Run("zero threads complete", func(t *testing.T) {
		srv := reviewThreadServer(t, func(int, map[string]any) string {
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`
		})
		defer srv.Close()
		c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
		threads, _, err := c.readReviewThreads(context.Background(), 1)
		if err != nil || len(threads) != 0 {
			t.Fatalf("threads=%+v err=%v", threads, err)
		}
	})
	t.Run("thread pagination", func(t *testing.T) {
		srv := reviewThreadServer(t, func(call int, variables map[string]any) string {
			if call == 1 {
				return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"thread-1"}}}}}}`
			}
			if variables["threadCursor"] != "thread-1" {
				t.Errorf("threadCursor=%v", variables["threadCursor"])
			}
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`
		})
		defer srv.Close()
		c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
		if _, _, err := c.readReviewThreads(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing outer page metadata", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`},
		{"null outer page metadata", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":null}}}}}`},
		{"missing outer cursor", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":""}}}}}}`},
		{"partial GraphQL error", `{"data":{"repository":{"pullRequest":null}},"errors":[{"message":"partial"}]}`},
		{"missing thread ID", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"","isResolved":false,"isOutdated":false,"originalCommit":{"oid":"original"},"comments":{"nodes":[{"id":"COMMENT","databaseId":1,"pullRequestReview":{"id":"REVIEW","databaseId":2,"state":"COMMENTED","commit":{"oid":"commit"},"submittedAt":"2026-07-14T01:00:00Z"},"body":"x","createdAt":"2026-07-14T01:00:00Z","updatedAt":"2026-07-14T01:00:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`},
		{"missing nested page metadata", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"THREAD","isResolved":false,"isOutdated":false,"originalCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"comments":{"nodes":[]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`},
		{"null nested page metadata", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"THREAD","isResolved":false,"isOutdated":false,"originalCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"comments":{"nodes":[],"pageInfo":null}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`},
		{"nested pagination continuation", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"THREAD","isResolved":false,"isOutdated":false,"originalCommit":{"oid":"original"},"comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"nested-1"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := reviewThreadServer(t, func(int, map[string]any) string { return tc.body })
			defer srv.Close()
			c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
			if _, _, err := c.readReviewThreads(context.Background(), 1); err == nil {
				t.Fatal("incomplete review-thread response was accepted")
			}
		})
	}
	t.Run("outer pagination overflow", func(t *testing.T) {
		srv := reviewThreadServer(t, func(call int, _ map[string]any) string {
			return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"thread-%d"}}}}}}`, call)
		})
		defer srv.Close()
		c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
		if _, _, err := c.readReviewThreads(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestReviewTopologyDigestChangesWhenCommentBodyChanges(t *testing.T) {
	first := domain.GitHubReviewThread{NodeID: "THREAD", Comments: []domain.GitHubReviewComment{{NodeID: "COMMENT", BodyDigest: domain.TrustedReviewFeedbackDigest("before")}}}
	edited := first
	edited.Comments = append([]domain.GitHubReviewComment(nil), first.Comments...)
	edited.Comments[0].BodyDigest = domain.TrustedReviewFeedbackDigest("after")
	if reviewTopologyDigest(nil, []domain.GitHubReviewThread{first}, nil) == reviewTopologyDigest(nil, []domain.GitHubReviewThread{edited}, nil) {
		t.Fatal("comment body edit did not change topology digest")
	}
}

func TestReviewThreadReadRejectsIncompleteImmutableReviewEvidence(t *testing.T) {
	valid := reviewThreadResponse("body")
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"short original commit", strings.Replace(valid, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "short", 1), "original commit"},
		{"short review commit", strings.Replace(valid, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "short", 1), "review identity"},
		{"unknown review state", strings.Replace(valid, "\"COMMENTED\"", "\"FUTURE\"", 1), "review identity"},
		{"unknown comment author type", strings.Replace(valid, "\"__typename\":\"User\"", "\"__typename\":\"UnknownActor\"", 1), "author identity"},
		{"unknown review author type", strings.Replace(valid, "\"__typename\":\"User\"", "\"__typename\":\"UnknownActor\"", 2), "author identity"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := reviewThreadServer(t, func(int, map[string]any) string { return tc.body })
			defer srv.Close()
			c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
			if _, _, err := c.readReviewThreads(context.Background(), 1); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	t.Run("null review author remains explicit deleted-author evidence", func(t *testing.T) {
		body := strings.Replace(valid, `"submittedAt":"2026-07-14T01:00:00Z","author":{"login":"author","__typename":"User","id":"USER","databaseId":2}`, `"submittedAt":"2026-07-14T01:00:00Z","author":null`, 1)
		srv := reviewThreadServer(t, func(int, map[string]any) string { return body })
		defer srv.Close()
		c := &Client{cfg: Config{GraphQLURL: srv.URL, APIVersion: "2022-11-28"}, http: srv.Client(), clock: fixedClock{time.Now()}, token: "token"}
		threads, _, err := c.readReviewThreads(context.Background(), 1)
		if err != nil || len(threads) != 1 || threads[0].Comments[0].Review.Actor.Type != "" {
			t.Fatalf("thread=%+v err=%v", threads, err)
		}
	})
}

func TestReadRejectsReviewThreadTopologyDrift(t *testing.T) {
	_, key := testKey(t)
	var threadReads atomic.Int32
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"token":"fixture-installation-secret","expires_at":"2026-07-11T01:00:00Z","permissions":{"metadata":"read","contents":"read","pull_requests":"read","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"id":101,"number":1,"html_url":"https://example.invalid/pr/1","node_id":"PR","state":"open","merged":false,"body":"body","head":{"ref":"feature","sha":"headsha","repo":{"id":99}},"base":{"ref":"main","sha":"basesha","repo":{"id":99}}}`)
	})
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"contexts":["test"],"checks":[]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/headsha/check-runs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:00:00Z","app":{"id":8}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/headsha/status", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"total_count":0,"statuses":[]}`)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
			return
		}
		if strings.Contains(request.Query, "reviewThreads") {
			body := "first body"
			if threadReads.Add(1) > 1 {
				body = "edited body"
			}
			write(w, reviewThreadResponse(body))
			return
		}
		write(w, `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID = server.URL, server.URL+"/graphql", 99
	client, err := New(cfg, fixedClock{time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(context.Background(), 1, "headsha"); err == nil || !strings.Contains(err.Error(), "review topology drifted") {
		t.Fatalf("err=%v", err)
	}
}

func TestReadTreatsCheckTopologyMovementAsExactHeadPending(t *testing.T) {
	_, key := testKey(t)
	var checkReads atomic.Int32
	var protectionReads atomic.Int32
	var driftProtection atomic.Bool
	var strictOnlyDrift atomic.Bool
	var pullRequestReads atomic.Int32
	var pullRequestLifecycle atomic.Int32
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"token":"fixture-installation-secret","expires_at":"2026-07-11T01:00:00Z","permissions":{"metadata":"read","contents":"read","pull_requests":"read","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/1", func(w http.ResponseWriter, _ *http.Request) {
		read := pullRequestReads.Add(1)
		if read > 1 && pullRequestLifecycle.Load() == 1 {
			write(w, `{"id":101,"number":1,"html_url":"https://example.invalid/pr/1","node_id":"PR","state":"closed","merged":false,"merge_commit_sha":"synthetic-close-sha","body":"body","head":{"ref":"feature","sha":"headsha","repo":{"id":99}},"base":{"ref":"main","sha":"basesha","repo":{"id":99}}}`)
			return
		}
		if read > 1 && pullRequestLifecycle.Load() == 2 {
			write(w, `{"id":101,"number":1,"html_url":"https://example.invalid/pr/1","node_id":"PR","state":"closed","merged":true,"merge_commit_sha":"merge-sha","merged_at":"2026-07-11T00:30:00Z","body":"body","head":{"ref":"feature","sha":"headsha","repo":{"id":99}},"base":{"ref":"main","sha":"basesha","repo":{"id":99}}}`)
			return
		}
		write(w, `{"id":101,"number":1,"html_url":"https://example.invalid/pr/1","node_id":"PR","state":"open","merged":false,"body":"body","head":{"ref":"feature","sha":"headsha","repo":{"id":99}},"base":{"ref":"main","sha":"basesha","repo":{"id":99}}}`)
	})
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, _ *http.Request) {
		read := protectionReads.Add(1)
		if driftProtection.Load() && read > 1 {
			if strictOnlyDrift.Load() {
				write(w, `{"strict":true,"contexts":["test"],"checks":[]}`)
				return
			}
			write(w, `{"contexts":["lint"],"checks":[]}`)
			return
		}
		write(w, `{"contexts":["test"],"checks":[]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/headsha/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		if checkReads.Add(1) == 1 {
			write(w, `{"total_count":0,"check_runs":[]}`)
			return
		}
		write(w, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"in_progress","conclusion":null,"started_at":"2026-07-11T00:00:00Z","completed_at":null,"app":{"id":8}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/headsha/status", func(w http.ResponseWriter, _ *http.Request) { write(w, `{"total_count":0,"statuses":[]}`) })
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(request.Query, "reviewThreads") {
			write(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
			return
		}
		write(w, `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID = server.URL, server.URL+"/graphql", 99
	client, err := New(cfg, fixedClock{time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := client.Read(context.Background(), 1, "headsha")
	if err != nil || !evidence.RequiredChecksWaiting() || len(evidence.Checks) != 1 || evidence.Checks[0].State != domain.CheckInProgress {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
	for _, lifecycle := range []int32{1, 2} {
		pullRequestLifecycle.Store(lifecycle)
		pullRequestReads.Store(0)
		protectionReads.Store(0)
		checkReads.Store(0)
		if _, err := client.Read(context.Background(), 1, "headsha"); err == nil || !strings.Contains(err.Error(), "pull request drifted") {
			t.Fatalf("lifecycle=%d err=%v", lifecycle, err)
		}
	}
	pullRequestLifecycle.Store(0)
	pullRequestReads.Store(0)
	driftProtection.Store(true)
	protectionReads.Store(0)
	checkReads.Store(0)
	if _, err := client.Read(context.Background(), 1, "headsha"); err == nil || !strings.Contains(err.Error(), "required check protection drifted") {
		t.Fatalf("protection drift err=%v", err)
	}
	strictOnlyDrift.Store(true)
	protectionReads.Store(0)
	checkReads.Store(0)
	if _, err := client.Read(context.Background(), 1, "headsha"); err == nil || !strings.Contains(err.Error(), "required check protection drifted") {
		t.Fatalf("strict-only protection drift err=%v", err)
	}
}

func reviewThreadResponse(body string) string {
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"THREAD","isResolved":false,"isOutdated":false,"path":"internal/a.go","line":7,"comments":{"nodes":[{"id":"COMMENT","databaseId":1,"replyTo":null,"originalCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"author":{"login":"author","__typename":"User","id":"USER","databaseId":2},"pullRequestReview":{"id":"REVIEW","databaseId":3,"state":"COMMENTED","commit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"submittedAt":"2026-07-14T01:00:00Z","author":{"login":"author","__typename":"User","id":"USER","databaseId":2}},"body":%q,"createdAt":"2026-07-14T01:00:00Z","updatedAt":"2026-07-14T01:00:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`, body)
}

func reviewThreadServer(t *testing.T, reply func(int, map[string]any) string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reply(int(calls.Add(1)), request.Variables))
	}))
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
		write(w, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:00:00Z","app":{"id":8}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) { write(w, `{"contexts":["test"],"checks":[]}`) })
	mux.HandleFunc("/repos/owner/repo/commits/headsha/status", func(w http.ResponseWriter, r *http.Request) { write(w, `{"total_count":0,"statuses":[]}`) })
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
			return
		}
		if !strings.Contains(request.Query, "... on User{id databaseId}") {
			http.Error(w, "invalid review selection", http.StatusBadRequest)
			return
		}
		if strings.Contains(request.Query, "reviewThreads") {
			write(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
			return
		}
		write(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED","reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	})
	return httptest.NewServer(mux)
}
