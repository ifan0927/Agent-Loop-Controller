package githubapp

import (
	"context"
	"fmt"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
		write(w, `{"number":1,"html_url":"https://example.invalid/pr/1","node_id":"PR","state":"open","merged":false,"body":"body","head":{"ref":"feature","sha":"headsha"},"base":{"ref":"main","sha":"basesha"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/headsha/check-runs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) { write(w, `{"contexts":["test"],"checks":[]}`) })
	mux.HandleFunc("/repos/owner/repo/commits/headsha/status", func(w http.ResponseWriter, r *http.Request) { write(w, `{"statuses":[]}`) })
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED","reviewThreads":{"nodes":[{"id":"THREAD","isResolved":false,"isOutdated":false,"comments":{"nodes":[{"id":"COMMENT","databaseId":10,"body":"finding","path":"x.go","line":2,"outdated":false,"createdAt":"2026-07-11T00:00:00Z","author":{"login":"coderabbitai[bot]","__typename":"Bot","id":"BOT","databaseId":7}}]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	})
	return httptest.NewServer(mux)
}
