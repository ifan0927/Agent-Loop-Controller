package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRepositoryReadinessUsesOnlyTokenAndRepositoryReads(t *testing.T) {
	_, key := testKey(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/app/installations/2/access_tokens":
			fmt.Fprint(w, `{"token":"installation-secret","expires_at":"2026-08-24T03:00:00Z","permissions":{"metadata":"read","contents":"read","checks":"read","statuses":"read","administration":"read","pull_requests":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":99,"node_id":"REPO_99","name":"repo","owner":{"login":"owner"}}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID = server.URL, server.URL+"/graphql", 99
	client, err := New(cfg, fixedClock{time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile := application.LocalRepository{CanonicalRepository: "owner/repo", ProfileDigest: strings.Repeat("a", 64), RepositoryBindingDigest: strings.Repeat("b", 64), GitHubAppID: 1, GitHubInstallationID: 2, ExpectedRepositoryID: 99}
	results, err := client.ObserveRepositoryGitHub(context.Background(), profile)
	if err != nil || results[0].Status != domain.RepositoryReady || results[1].Status != domain.RepositoryReady || results[0].Identity != "99:REPO_99" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	want := []string{"POST /app/installations/2/access_tokens", "GET /repos/owner/repo"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestObserveInitialBaseRevalidatesExactRepositoryAndCommit(t *testing.T) {
	_, key := testKey(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/app/installations/2/access_tokens":
			fmt.Fprint(w, `{"token":"installation-secret","expires_at":"2026-08-24T03:00:00Z","permissions":{"metadata":"read","contents":"read","checks":"read","statuses":"read","administration":"read","pull_requests":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":99,"node_id":"REPO_99","name":"repo","owner":{"login":"owner"}}`)
		case "/repos/owner/repo/git/ref/heads/main":
			fmt.Fprint(w, `{"ref":"refs/heads/main","object":{"sha":"0123456789012345678901234567890123456789","type":"commit"}}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID = server.URL, server.URL+"/graphql", 99
	client, err := New(cfg, fixedClock{time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := client.ObserveInitialBase(context.Background(), "owner/repo", "main", "0123456789012345678901234567890123456789")
	if result.Status != BaseRefReady || len(result.EvidenceDigest) != 64 {
		t.Fatalf("result=%+v", result)
	}
	want := []string{"POST /app/installations/2/access_tokens", "GET /repos/owner/repo", "GET /repos/owner/repo/git/ref/heads/main"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}
