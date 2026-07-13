package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestOpenPullRequestCreatesOnlyAfterExactLookup(t *testing.T) {
	_, key := testKey(t)
	request := pullRequestRequest(t)
	var creates atomic.Int32
	mux := http.NewServeMux()
	installWriteToken(t, mux)
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if got := r.URL.Query().Get("head"); got != "owner:ifan/one" {
				t.Errorf("head=%q", got)
			}
			fmt.Fprint(w, `[]`)
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s", r.Method)
		}
		var payload struct{ Title, Head, Base, Body string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Title != request.Title || payload.Head != request.HeadBranch || payload.Base != request.BaseBranch || payload.Body != request.Body {
			t.Fatalf("payload=%+v err=%v", payload, err)
		}
		creates.Add(1)
		fmt.Fprint(w, rawPullRequestJSON(request, true))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := writeClient(t, key, server)
	got, err := client.OpenPullRequest(context.Background(), request)
	if err != nil || got.Number != 7 || got.DatabaseID != 70 || creates.Load() != 1 {
		t.Fatalf("pr=%+v err=%v creates=%d", got, err, creates.Load())
	}
}

func TestOpenPullRequestAdoptsOnlyMatchingImmutableIntent(t *testing.T) {
	_, key := testKey(t)
	request := pullRequestRequest(t)
	var creates atomic.Int32
	mux := http.NewServeMux()
	installWriteToken(t, mux)
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			creates.Add(1)
			return
		}
		fmt.Fprintf(w, `[%s]`, rawPullRequestJSON(request, true))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	got, err := writeClient(t, key, server).OpenPullRequest(context.Background(), request)
	if err != nil || got.Number != 7 || creates.Load() != 0 {
		t.Fatalf("pr=%+v err=%v creates=%d", got, err, creates.Load())
	}
}

func TestOpenPullRequestRejectsPartialOrMismatchedAdoption(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
		mutate   func(*application.PullRequestOpenRequest)
	}{
		{name: "partial response", complete: false},
		{name: "body digest mismatch", complete: true, mutate: func(r *application.PullRequestOpenRequest) { r.Body += " altered" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, key := testKey(t)
			request := pullRequestRequest(t)
			response := request
			if tc.mutate != nil {
				tc.mutate(&response)
			}
			var creates atomic.Int32
			mux := http.NewServeMux()
			installWriteToken(t, mux)
			mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
			})
			mux.HandleFunc("/repos/owner/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					creates.Add(1)
				}
				fmt.Fprintf(w, `[%s]`, rawPullRequestJSON(response, tc.complete))
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			if _, err := writeClient(t, key, server).OpenPullRequest(context.Background(), request); err == nil || creates.Load() != 0 {
				t.Fatalf("err=%v creates=%d", err, creates.Load())
			}
		})
	}
}

func TestOpenPullRequestReplaysPOSTBodyAfterOne401Refresh(t *testing.T) {
	_, key := testKey(t)
	request := pullRequestRequest(t)
	var mints, posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		n := mints.Add(1)
		fmt.Fprintf(w, `{"token":"fixture-token-%d","expires_at":"2099-07-11T01:00:00Z","permissions":{"metadata":"read","contents":"read","pull_requests":"write","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`, n)
	})
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `[]`)
			return
		}
		var payload struct{ Title, Head, Base, Body string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Title != request.Title || payload.Head != request.HeadBranch || payload.Base != request.BaseBranch || payload.Body != request.Body {
			t.Fatalf("payload=%+v err=%v", payload, err)
		}
		if posts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, rawPullRequestJSON(request, true))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if _, err := writeClient(t, key, server).OpenPullRequest(context.Background(), request); err != nil || posts.Load() != 2 || mints.Load() != 2 {
		t.Fatalf("err=%v posts=%d mints=%d", err, posts.Load(), mints.Load())
	}
}

func pullRequestRequest(t *testing.T) application.PullRequestOpenRequest {
	t.Helper()
	body := "## Summary\n\nSafe write\n\n<!-- controller-run:key -->\n"
	digest := sha256.Sum256([]byte(body))
	request := application.PullRequestOpenRequest{Title: "IFAN-1: Safe write", HeadBranch: "ifan/one", BaseBranch: "main", CandidateSHA: "head", BaseSHA: "base", Body: body, BodyDigest: hex.EncodeToString(digest[:]), OwnershipKey: "key"}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	return request
}

func installWriteToken(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("token method=%s", r.Method)
		}
		fmt.Fprint(w, `{"token":"fixture-token","expires_at":"2099-07-11T01:00:00Z","permissions":{"metadata":"read","contents":"read","pull_requests":"write","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
	})
}

func writeClient(t *testing.T, key string, server *httptest.Server) *Client {
	t.Helper()
	cfg := validConfig(key)
	cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID, cfg.PullRequestsWrite = server.URL, server.URL+"/graphql", 99, true
	client, err := New(cfg, fixedClock{time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	return client
}

func rawPullRequestJSON(request application.PullRequestOpenRequest, complete bool) string {
	nodeID := `"node_id":"PR_7",`
	if !complete {
		nodeID = ""
	}
	return fmt.Sprintf(`{"id":70,"number":7,"html_url":"https://example.invalid/pull/7",%s"state":"open","merged":false,"body":%q,"head":{"ref":%q,"sha":%q,"repo":{"id":99}},"base":{"ref":%q,"sha":%q,"repo":{"id":99}}}`, nodeID, request.Body, request.HeadBranch, request.CandidateSHA, request.BaseBranch, request.BaseSHA)
}
