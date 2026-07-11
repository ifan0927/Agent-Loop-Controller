package githubapp

import (
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
	if index.Version != 1 || !index.Sanitized || len(index.Scenarios) < 30 {
		t.Fatalf("invalid fixture index: %+v", index)
	}
	seen := map[string]bool{}
	for _, name := range index.Scenarios {
		if seen[name] || name == "" {
			t.Fatalf("duplicate/blank scenario %q", name)
		}
		seen[name] = true
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
		fmt.Fprint(w, `{"check_runs":[{"id":2,"name":"test","status":"completed","conclusion":"failure","started_at":"2026-07-11T00:02:00Z","completed_at":"2026-07-11T00:03:00Z","app":{"id":8}},{"id":1,"name":"test","status":"completed","conclusion":"success","started_at":"2026-07-11T00:00:00Z","completed_at":"2026-07-11T00:01:00Z","app":{"id":8}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"total_count":0,"statuses":[]}`) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{cfg: Config{APIBaseURL: srv.URL, RepositoryOwner: "owner", RepositoryName: "repo", APIVersion: "2022-11-28", CodeRabbitAppID: 8, InstallationID: 2}, http: srv.Client(), clock: fixedClock{time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)}, token: "token"}
	checks, cr, _, err := c.readChecks(context.Background(), "head", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].State != domain.CheckFailure || cr != domain.CodeRabbitActionable {
		t.Fatalf("checks=%+v coderabbit=%s", checks, cr)
	}
}

func TestCodeRabbitStateAggregationIsConservative(t *testing.T) {
	states := []domain.CodeRabbitState{domain.CodeRabbitPass, domain.CodeRabbitPending, domain.CodeRabbitInfrastructure, domain.CodeRabbitActionable}
	got := domain.CodeRabbitAbsent
	for _, state := range states {
		got = mergeCodeRabbitState(got, state)
	}
	if got != domain.CodeRabbitActionable {
		t.Fatalf("got %s", got)
	}
	got = domain.CodeRabbitAbsent
	for i := len(states) - 1; i >= 0; i-- {
		got = mergeCodeRabbitState(got, states[i])
	}
	if got != domain.CodeRabbitActionable {
		t.Fatalf("reverse got %s", got)
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
	cfg.CodeRabbitActorID = 7
	cfg.CodeRabbitNodeID = "BOT"
	cfg.CodeRabbitAppID = 8
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
		if got.Repository.ID != 99 || got.PullRequest.HeadSHA != "headsha" || got.CodeRabbit != domain.CodeRabbitActionable || len(got.Findings) != 1 {
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
		write(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED","reviewThreads":{"nodes":[{"id":"THREAD","isResolved":false,"isOutdated":false,"comments":{"totalCount":1,"nodes":[{"id":"COMMENT","databaseId":10,"body":"finding","path":"x.go","line":2,"outdated":false,"createdAt":"2026-07-11T00:00:00Z","author":{"login":"coderabbitai[bot]","__typename":"Bot","id":"BOT","databaseId":7}}]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	})
	return httptest.NewServer(mux)
}
