package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestReplyToReviewCommentUsesOnlyRootReplyEndpointAndConfiguredApp(t *testing.T) {
	_, key := testKey(t)
	head := strings.Repeat("a", 40)
	marker, digest, err := domain.ReviewReplyMarker("run", 7, "THREAD", 9, "COMMENT", strings.Repeat("b", 64), head)
	if err != nil {
		t.Fatal(err)
	}
	body, err := domain.ReviewReplyBody(head, marker)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	installMergeToken(t, mux)
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/7/comments/9/replies", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s", r.Method)
		}
		var payload struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Body != body {
			t.Fatalf("payload=%+v err=%v", payload, err)
		}
		fmt.Fprintf(w, `{"id":10,"node_id":"COMMENT_10","in_reply_to_id":9,"body":%q,"created_at":"2026-07-14T00:00:00Z","user":{"id":2,"node_id":"BOT_2","login":"ifan-loop[bot]","type":"Bot"},"performed_via_github_app":{"id":1}}`, body)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	cfg := validConfig(key)
	cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID, cfg.ReviewCommentsWrite = server.URL, server.URL+"/graphql", 99, true
	client, err := New(cfg, fixedClock{time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	reply, err := client.ReplyToReviewComment(context.Background(), application.ReplyToReviewCommentRequest{PullRequestNumber: 7, RootCommentID: 9, Body: body, MarkerDigest: digest})
	if err != nil || reply.DatabaseID != 10 || reply.ReplyToID != 9 || reply.MarkerDigest != digest || reply.Actor.AppID != 1 {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
}

func TestReplyToReviewCommentMapsAuthoritativeAndRetryableFailures(t *testing.T) {
	head := strings.Repeat("a", 40)
	marker, digest, err := domain.ReviewReplyMarker("run", 7, "THREAD", 9, "COMMENT", strings.Repeat("b", 64), head)
	if err != nil {
		t.Fatal(err)
	}
	body, err := domain.ReviewReplyBody(head, marker)
	if err != nil {
		t.Fatal(err)
	}
	request := application.ReplyToReviewCommentRequest{PullRequestNumber: 7, RootCommentID: 9, Body: body, MarkerDigest: digest}
	for _, tc := range []struct {
		name      string
		status    int
		rejected  bool
		retryable bool
	}{
		{"forbidden", http.StatusForbidden, true, false},
		{"not found", http.StatusNotFound, true, false},
		{"rate limited", http.StatusTooManyRequests, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) }))
			defer server.Close()
			client := &Client{cfg: Config{APIBaseURL: server.URL, RepositoryOwner: "owner", RepositoryName: "repo", ReviewCommentsWrite: true}, http: server.Client(), clock: fixedClock{time.Now()}, token: "fixture"}
			_, got := client.ReplyToReviewComment(context.Background(), request)
			var rejected *application.ReviewReplyRejectedError
			if errors.As(got, &rejected) != tc.rejected {
				t.Fatalf("err=%v rejected=%t", got, tc.rejected)
			}
			var status *statusError
			if errors.As(got, &status) != tc.retryable || tc.retryable && status.status != tc.status {
				t.Fatalf("err=%v status=%+v", got, status)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("fixture network unavailable") })
		client := &Client{cfg: Config{APIBaseURL: "http://fixture.invalid", RepositoryOwner: "owner", RepositoryName: "repo", ReviewCommentsWrite: true}, http: &http.Client{Transport: transport}, clock: fixedClock{time.Now()}, token: "fixture"}
		_, got := client.ReplyToReviewComment(context.Background(), request)
		var rejected *application.ReviewReplyRejectedError
		if got == nil || errors.As(got, &rejected) {
			t.Fatalf("network err=%v", got)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestFindReviewCommentRepliesTreatsUnprovableAbsenceAsInconclusive(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(int) string
	}{
		{"bounded pagination exhausted", func(int) string {
			return "[" + strings.Repeat(`{"id":1,"node_id":"N","in_reply_to_id":9,"body":"","user":{}},`, 99) + `{"id":1,"node_id":"N","in_reply_to_id":9,"body":"","user":{}}]`
		}},
		{"non-array response", func(int) string { return `null` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, key := testKey(t)
			mux := http.NewServeMux()
			installMergeToken(t, mux)
			calls := 0
			mux.HandleFunc("/repos/owner/repo/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
				calls++
				fmt.Fprint(w, tc.body(calls))
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			cfg := validConfig(key)
			cfg.APIBaseURL, cfg.GraphQLURL, cfg.RepositoryID = server.URL, server.URL+"/graphql", 99
			client, err := New(cfg, fixedClock{time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client.http = server.Client()
			_, err = client.FindReviewCommentReplies(context.Background(), 7, 9)
			var inconclusive *application.ReviewReplyInconclusiveError
			if !errors.As(err, &inconclusive) || tc.name == "bounded pagination exhausted" && calls != 20 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
