package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type staticCredentials struct {
	token string
	err   error
	refs  []string
}

func (s *staticCredentials) Resolve(_ context.Context, ref string) (string, error) {
	s.refs = append(s.refs, ref)
	return s.token, s.err
}

func TestReadIssueNormalizesBoundedPaginatedLabels(t *testing.T) {
	const token = "linear-secret-token"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("unexpected Linear request authorization")
		}
		var request struct {
			Variables struct {
				Identifier string  `json:"identifier"`
				After      *string `json:"after"`
				First      int     `json:"first"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Variables.Identifier != "IFAN-4" || request.Variables.First != 1 {
			t.Fatalf("unexpected request variables: %+v", request.Variables)
		}
		w.Header().Set("X-Request-ID", "00000000-0000-4000-8000-00000000000"+string(rune('1'+calls)))
		w.Header().Set("X-RateLimit-Requests-Limit", "2500")
		w.Header().Set("X-RateLimit-Requests-Remaining", "2499")
		w.Header().Set("X-RateLimit-Requests-Reset", "1783814400000")
		if calls == 0 {
			if request.Variables.After != nil {
				t.Fatal("first label page included a cursor")
			}
			writeIssueResponse(t, w, true, "next", []label{{ID: "label-b", Name: "repo:brand"}})
		} else {
			if request.Variables.After == nil || *request.Variables.After != "next" {
				t.Fatal("second label page did not use the server cursor")
			}
			writeIssueResponse(t, w, false, "", []label{{ID: "label-a", Name: "agent:codex"}})
		}
		calls++
	}))
	defer server.Close()

	credentials := &staticCredentials{token: token}
	client, err := New(testConfig(server.URL), credentials, fixedClock{time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	source, observations, err := client.ReadIssue(context.Background(), "IFAN-4")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(observations) != 2 || len(credentials.refs) != 1 {
		t.Fatalf("calls=%d observations=%d credentials=%d", calls, len(observations), len(credentials.refs))
	}
	if source.Provider != "linear" || source.Identifier != "IFAN-4" || source.SourceRevision != "2026-07-12T00:00:00Z" || !source.Cycle.IsActive {
		t.Fatalf("unexpected source: %+v", source)
	}
	if len(source.Labels) != 2 || source.Labels[0].ID != "label-a" || source.Labels[1].ID != "label-b" {
		t.Fatalf("labels were not deterministic: %+v", source.Labels)
	}
	for _, observation := range observations {
		if observation.Operation != "read_issue" || observation.ResponseDigest == "" || observation.RequestID == "" || observation.RateLimitLimit != 2500 || observation.RateLimitRemaining != 2499 || observation.RateLimitReset.IsZero() {
			t.Fatalf("unexpected observation: %+v", observation)
		}
		if strings.Contains(fmtObservation(observation), token) {
			t.Fatal("credential leaked into observation")
		}
	}
}

func TestReadIssueFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		config  func(*Config)
		respond func(t *testing.T, w http.ResponseWriter)
		want    string
	}{
		{
			name: "partial GraphQL errors",
			respond: func(t *testing.T, w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"data":{"issue":null},"errors":[{"message":"secret external body"}]}`))
			},
			want: "GraphQL response contains errors",
		},
		{
			name: "missing cycle",
			respond: func(t *testing.T, w http.ResponseWriter) {
				response := issueResponse(false, "", nil)
				response.Data.Issue["cycle"] = nil
				writeJSON(t, w, response)
			},
			want: "incomplete or mismatched",
		},
		{
			name: "missing labels",
			respond: func(t *testing.T, w http.ResponseWriter) {
				response := issueResponse(false, "", nil)
				delete(response.Data.Issue, "labels")
				writeJSON(t, w, response)
			},
			want: "labels response is incomplete",
		},
		{
			name: "missing label page state",
			respond: func(t *testing.T, w http.ResponseWriter) {
				response := issueResponse(false, "", nil)
				response.Data.Issue["labels"] = map[string]any{"nodes": []map[string]string{}, "pageInfo": map[string]any{}}
				writeJSON(t, w, response)
			},
			want: "labels response is incomplete",
		},
		{
			name: "malformed JSON",
			respond: func(t *testing.T, w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"data":`))
			},
			want: "invalid Linear response JSON",
		},
		{
			name: "wrong issue identity",
			respond: func(t *testing.T, w http.ResponseWriter) {
				response := issueResponse(false, "", nil)
				response.Data.Issue["identifier"] = "IFAN-99"
				writeJSON(t, w, response)
			},
			want: "incomplete or mismatched",
		},
		{
			name:   "oversized response",
			config: func(config *Config) { config.MaxResponseBytes = 1024 },
			respond: func(t *testing.T, w http.ResponseWriter) {
				_, _ = w.Write([]byte(strings.Repeat("x", 1025)))
			},
			want: "exceeds configured limit",
		},
		{
			name:   "pagination limit",
			config: func(config *Config) { config.MaxLabelPages = 1 },
			respond: func(t *testing.T, w http.ResponseWriter) {
				writeIssueResponse(t, w, true, "next", nil)
			},
			want: "pagination limit exceeded",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { test.respond(t, w) }))
			defer server.Close()
			config := testConfig(server.URL)
			if test.config != nil {
				test.config(&config)
			}
			client, err := New(config, &staticCredentials{token: "linear-secret-token"}, fixedClock{time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			_, observations, err := client.ReadIssue(context.Background(), "IFAN-4")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
			if len(observations) != 1 || strings.Contains(err.Error(), "secret") || strings.Contains(fmtObservation(observations[0]), "secret") {
				t.Fatalf("unsafe or missing observation: %+v error=%v", observations, err)
			}
		})
	}
}

func TestReadIssueRejectsMissingCredentialsAndUnsafeIdentifier(t *testing.T) {
	config := testConfig("http://127.0.0.1:9999")
	if _, err := New(config, nil, fixedClock{}); err == nil {
		t.Fatal("expected missing credential source rejection")
	}
	client, err := New(config, &staticCredentials{token: "linear-secret-token"}, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	_, observations, err := client.ReadIssue(context.Background(), "IFAN-4; mutation")
	if err == nil || len(observations) != 0 || strings.Contains(err.Error(), "mutation") {
		t.Fatalf("error=%v observations=%+v", err, observations)
	}
}

func TestReadIssueSupportsConfiguredPersonalAPIKeyAuthorization(t *testing.T) {
	const token = "linear-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != token {
			t.Fatalf("Authorization=%q", got)
		}
		writeIssueResponse(t, w, false, "", nil)
	}))
	defer server.Close()
	config := testConfig(server.URL)
	config.AuthorizationScheme = "api_key"
	client, err := New(config, &staticCredentials{token: token}, fixedClock{time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.ReadIssue(context.Background(), "IFAN-4"); err != nil {
		t.Fatal(err)
	}
}

func TestReadIssueDropsUntrustedRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "linear-secret-token")
		writeIssueResponse(t, w, false, "", nil)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), &staticCredentials{token: "linear-secret-token"}, fixedClock{time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, observations, err := client.ReadIssue(context.Background(), "IFAN-4")
	if err != nil || len(observations) != 1 || observations[0].RequestID != "" || strings.Contains(fmtObservation(observations[0]), "secret") {
		t.Fatalf("error=%v observations=%+v", err, observations)
	}
}

func TestSanitizeRequestIDRequiresRFC4122VersionAndVariant(t *testing.T) {
	for _, value := range []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-8000-b000-000000000001",
	} {
		if got := sanitizeRequestID(value); got != value {
			t.Fatalf("value=%q got=%q", value, got)
		}
	}
	for _, value := range []string{
		"00000000-0000-0000-8000-000000000001",
		"00000000-0000-4000-7000-000000000001",
	} {
		if got := sanitizeRequestID(value); got != "" {
			t.Fatalf("value=%q got=%q", value, got)
		}
	}
}

func TestReadIssueRejectsPaginationOverlapAndSourceDrift(t *testing.T) {
	tests := []struct {
		name   string
		second func(issueEnvelope) issueEnvelope
		want   string
	}{
		{
			name: "repeated label",
			second: func(response issueEnvelope) issueEnvelope {
				response.Data.Issue["labels"] = map[string]any{"nodes": []map[string]string{{"id": "label-a", "name": "agent:codex"}}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}
				return response
			},
			want: "label appeared more than once",
		},
		{
			name: "updated source",
			second: func(response issueEnvelope) issueEnvelope {
				response.Data.Issue["updatedAt"] = "2026-07-12T00:00:01Z"
				return response
			},
			want: "changed while labels were fetched",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				response := issueResponse(calls == 0, "next", []label{{ID: "label-a", Name: "agent:codex"}})
				if calls == 1 {
					response = test.second(response)
				}
				calls++
				writeJSON(t, w, response)
			}))
			defer server.Close()
			client, err := New(testConfig(server.URL), &staticCredentials{token: "linear-secret-token"}, fixedClock{time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			_, observations, err := client.ReadIssue(context.Background(), "IFAN-4")
			if err == nil || !strings.Contains(err.Error(), test.want) || len(observations) != 2 {
				t.Fatalf("error=%v observations=%+v", err, observations)
			}
		})
	}
}

type label struct {
	ID   string
	Name string
}

type issueEnvelope struct {
	Data struct {
		Issue map[string]any `json:"issue"`
	} `json:"data"`
}

func issueResponse(hasNext bool, endCursor string, labels []label) issueEnvelope {
	nodes := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		nodes = append(nodes, map[string]string{"id": label.ID, "name": label.Name})
	}
	response := issueEnvelope{}
	response.Data.Issue = map[string]any{
		"id": "issue-id", "identifier": "IFAN-4", "url": "https://linear.app/ifan/issue/IFAN-4/test", "title": "Read issue",
		"description": "Goal\n\nAcceptance Criteria\n\nOut of Scope", "createdAt": "2026-07-11T00:00:00Z", "updatedAt": "2026-07-12T00:00:00Z", "branchName": "ifan/ifan-4-linear-read",
		"team":   map[string]any{"id": "team-id", "key": "IFAN", "name": "I-Fan"},
		"state":  map[string]any{"id": "state-id", "name": "Todo", "type": "backlog"},
		"cycle":  map[string]any{"id": "cycle-id", "number": 3, "startsAt": "2026-07-01T00:00:00Z", "endsAt": "2026-07-15T00:00:00Z", "isActive": true},
		"labels": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor}},
	}
	return response
}

func writeIssueResponse(t *testing.T, w http.ResponseWriter, hasNext bool, endCursor string, labels []label) {
	t.Helper()
	writeJSON(t, w, issueResponse(hasNext, endCursor, labels))
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func testConfig(serverURL string) Config {
	return Config{APIURL: serverURL + "/graphql", CredentialSourceRef: EnvironmentCredentialSourceRef, AuthorizationScheme: "bearer", TeamKey: "IFAN", HTTPTimeout: time.Second, MaxResponseBytes: 4096, LabelPageSize: 1, MaxLabelPages: 2}
}

func fmtObservation(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
