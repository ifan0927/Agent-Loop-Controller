package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestSquashMergeUsesExactHeadAndObservesMergedPullRequest(t *testing.T) {
	_, key := testKey(t)
	request := application.SquashMergeRequest{PullRequest: 7, HeadBranch: "ifan/one", BaseBranch: "main", ExpectedHeadSHA: "head", ExpectedBaseSHA: "base", OwnershipKey: "key"}
	var reads, merges int
	mux := http.NewServeMux()
	installMergeToken(t, mux)
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		reads++
		if reads == 1 {
			fmt.Fprint(w, mergePullRequestJSON(false))
			return
		}
		fmt.Fprint(w, mergePullRequestJSON(true))
	})
	mux.HandleFunc("/repos/owner/repo/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("unexpected method %s", r.Method)
		}
		var payload struct {
			SHA         string `json:"sha"`
			MergeMethod string `json:"merge_method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.SHA != request.ExpectedHeadSHA || payload.MergeMethod != "squash" {
			t.Fatalf("payload=%+v err=%v", payload, err)
		}
		merges++
		fmt.Fprint(w, `{"sha":"merge","merged":true,"message":"Pull Request successfully merged"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := mergeClient(t, key, server)
	got, err := client.SquashMerge(context.Background(), request)
	if err != nil || !got.Merged || got.MergeSHA != "merge" || got.MergedAt.IsZero() || reads != 2 || merges != 1 {
		t.Fatalf("pr=%+v err=%v reads=%d merges=%d", got, err, reads, merges)
	}
}

func TestSquashMergeFailsClosedBeforeWriteWhenHeadOrBaseDrifts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(application.SquashMergeRequest) string
	}{
		{name: "head drift", mutate: func(r application.SquashMergeRequest) string {
			return mergePullRequestJSONWith(r.HeadBranch, "other-head", r.BaseBranch, r.ExpectedBaseSHA, false)
		}},
		{name: "base drift", mutate: func(r application.SquashMergeRequest) string {
			return mergePullRequestJSONWith(r.HeadBranch, r.ExpectedHeadSHA, r.BaseBranch, "other-base", false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, key := testKey(t)
			request := application.SquashMergeRequest{PullRequest: 7, HeadBranch: "ifan/one", BaseBranch: "main", ExpectedHeadSHA: "head", ExpectedBaseSHA: "base", OwnershipKey: "key"}
			merges := 0
			mux := http.NewServeMux()
			installMergeToken(t, mux)
			mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
			})
			mux.HandleFunc("/repos/owner/repo/pulls/7", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.mutate(request)) })
			mux.HandleFunc("/repos/owner/repo/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) { merges++ })
			server := httptest.NewServer(mux)
			defer server.Close()
			if _, err := mergeClient(t, key, server).SquashMerge(context.Background(), request); err == nil || merges != 0 {
				t.Fatalf("err=%v merges=%d", err, merges)
			}
		})
	}
}

func TestSquashMergeClassifiesForbiddenAsRejected(t *testing.T) {
	_, key := testKey(t)
	request := application.SquashMergeRequest{PullRequest: 7, HeadBranch: "ifan/one", BaseBranch: "main", ExpectedHeadSHA: "head", ExpectedBaseSHA: "base", OwnershipKey: "key"}
	merges := 0
	mux := http.NewServeMux()
	installMergeToken(t, mux)
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/7", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	mux.HandleFunc("/repos/owner/repo/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) { merges++ })
	server := httptest.NewServer(mux)
	defer server.Close()
	_, err := mergeClient(t, key, server).SquashMerge(context.Background(), request)
	var rejected *application.MergeRejectedError
	if !errors.As(err, &rejected) || merges != 0 {
		t.Fatalf("err=%v merges=%d", err, merges)
	}
}

func installMergeToken(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"fixture-token","expires_at":"2099-07-11T01:00:00Z","permissions":{"metadata":"read","contents":"write","pull_requests":"write","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
	})
}

func mergeClient(t *testing.T, key string, server *httptest.Server) *Client {
	t.Helper()
	cfg := validConfig(key)
	cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID, cfg.PullRequestsWrite, cfg.SquashMergeWrite = server.URL, server.URL+"/graphql", 99, true, true
	client, err := New(cfg, fixedClock{time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	return client
}

func mergePullRequestJSON(merged bool) string {
	return mergePullRequestJSONWith("ifan/one", "head", "main", "base", merged)
}

func mergePullRequestJSONWith(headBranch, head, baseBranch, base string, merged bool) string {
	state, merge, mergedAt := "open", "", ""
	if merged {
		state, merge, mergedAt = "closed", "merge", `,"merged_at":"2026-07-13T01:00:00Z"`
	}
	return fmt.Sprintf(`{"id":70,"number":7,"html_url":"https://example.invalid/pull/7","node_id":"PR_7","state":%q,"merged":%t,"merge_commit_sha":%q%s,"body":"## Summary\\n\\n<!-- controller-run:key -->\\n","head":{"ref":%q,"sha":%q,"repo":{"id":99}},"base":{"ref":%q,"sha":%q,"repo":{"id":99}}}`, state, merged, merge, mergedAt, headBranch, head, baseBranch, base)
}
