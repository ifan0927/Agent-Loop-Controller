package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const (
	candidateTeamID        = "123e4567-e89b-42d3-a456-426614174000"
	candidateTodoStateID   = "123e4567-e89b-42d3-a456-426614174001"
	candidateStartStateID  = "123e4567-e89b-42d3-a456-426614174002"
	candidateUntrustedText = "untrusted-title-description-comments"
)

func TestListTodoCandidatesReturnsSanitizedBoundedSnapshot(t *testing.T) {
	const token = "linear-secret-token"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("unexpected candidate request authorization")
		}
		var request struct {
			Query     string `json:"query"`
			Variables struct {
				TeamID      string   `json:"teamID"`
				StateIDs    []string `json:"stateIDs"`
				TodoStateID string   `json:"todoStateID"`
				After       *string  `json:"after"`
				First       int      `json:"first"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		for _, filter := range []string{"agent:codex", "agent:hermes", "repo:", "isActive", "todoStateID"} {
			if !strings.Contains(request.Query, filter) {
				t.Fatalf("missing server-side filter %q in %s", filter, request.Query)
			}
		}
		if !strings.Contains(request.Query, "$stateIDs: [ID!]!") || !strings.Contains(request.Query, "$todoStateID: ID!") {
			t.Fatalf("candidate query does not use immutable ID variable definitions: %s", request.Query)
		}
		if !strings.HasPrefix(request.Query, "query ") || strings.Contains(request.Query, "title") || strings.Contains(request.Query, "description") || strings.Contains(request.Query, "comments") ||
			request.Variables.TeamID != candidateTeamID || request.Variables.TodoStateID != candidateTodoStateID || len(request.Variables.StateIDs) != 2 || request.Variables.First != 2 {
			t.Fatalf("unsafe or invalid candidate request: %+v", request)
		}
		if calls == 0 && request.Variables.After != nil {
			t.Fatal("first candidate page unexpectedly had a cursor")
		}
		if calls == 1 && (request.Variables.After == nil || *request.Variables.After != "cursor-1") {
			t.Fatal("second candidate page did not use the server cursor")
		}
		w.Header().Set("X-Request-ID", "00000000-0000-4000-8000-00000000000"+string(rune('1'+calls)))
		if calls == 0 {
			writeCandidateResponse(t, w, candidateResponse([]map[string]any{candidateNode("issue-b", "IFAN-8", 2, []map[string]string{{"id": "repo-b", "name": "repo:brand"}, {"id": "codex", "name": "agent:codex"}})}, true, "cursor-1"))
		} else {
			writeCandidateResponse(t, w, candidateResponse([]map[string]any{candidateNode("issue-a", "IFAN-7", 1, []map[string]string{{"id": "repo-a", "name": "repo:backend"}, {"id": "repo-c", "name": "repo:other"}, {"id": "codex", "name": "agent:codex"}})}, false, ""))
		}
		calls++
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), &staticCredentials{token: token}, fixedClock{time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	scan, observations, err := client.ListTodoCandidates(context.Background(), candidateAuthority(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(scan.Candidates) != 2 || scan.Candidates[0].IssueID != fixtureUUID("issue-b") || scan.Candidates[1].IssueID != fixtureUUID("issue-a") || scan.Digest == "" || scan.ObservedAt.IsZero() {
		t.Fatalf("unexpected scan: %+v calls=%d", scan, calls)
	}
	if scan.Candidates[1].Priority != 1 || len(scan.Candidates[1].RepositoryLabels) != 2 || scan.Candidates[1].SourceRevision != "2026-07-14T00:00:00Z" || scan.Candidates[1].SourceDigest == "" {
		t.Fatalf("candidate did not preserve normalized metadata: %+v", scan.Candidates[1])
	}
	if strings.Contains(fmtObservation(scan), candidateUntrustedText) {
		t.Fatalf("untrusted prose leaked into scan: %+v", scan)
	}
	if len(observations) != 2 || observations[0].Operation != "list_todo_candidates" || observations[0].Page != 1 || observations[0].Count != 1 || observations[1].Page != 2 || observations[1].Count != 1 {
		t.Fatalf("unexpected observations: %+v", observations)
	}
	for _, observation := range observations {
		if observation.ResponseDigest == "" || observation.RequestID == "" || strings.Contains(fmtObservation(observation), token) {
			t.Fatalf("unsafe observation: %+v", observation)
		}
	}
}

func TestListTodoCandidatesAcceptsZeroOneAndPriorityValues(t *testing.T) {
	for _, test := range []struct {
		name       string
		nodes      []map[string]any
		wantValues []int
	}{
		{name: "zero", nodes: nil},
		{name: "one no priority", nodes: []map[string]any{candidateNode("issue-zero", "IFAN-1", 0, requiredLabels())}, wantValues: []int{0}},
		{name: "multiple priority values", nodes: []map[string]any{candidateNode("issue-four", "IFAN-4", 4, requiredLabels()), candidateNode("issue-one", "IFAN-2", 1, requiredLabels())}, wantValues: []int{4, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeCandidateResponse(t, w, candidateResponse(test.nodes, false, ""))
			}))
			defer server.Close()
			client, err := New(testConfig(server.URL), &staticCredentials{token: "linear-secret-token"}, fixedClock{time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			scan, observations, err := client.ListTodoCandidates(context.Background(), candidateAuthority(3, 1))
			if err != nil || len(observations) != 1 || len(scan.Candidates) != len(test.wantValues) {
				t.Fatalf("scan=%+v observations=%+v error=%v", scan, observations, err)
			}
			seen := make(map[int]bool)
			for _, candidate := range scan.Candidates {
				seen[candidate.Priority] = true
			}
			for _, priority := range test.wantValues {
				if !seen[priority] {
					t.Fatalf("priority %d was lost: %+v", priority, scan.Candidates)
				}
			}
		})
	}
}

func TestListTodoCandidatesFailsClosed(t *testing.T) {
	secret := "untrusted-title-and-body"
	tests := []struct {
		name      string
		authority application.LinearTodoCandidateAuthority
		response  func() map[string]any
		want      string
	}{
		{name: "partial graphql error", authority: candidateAuthority(2, 1), response: func() map[string]any {
			return map[string]any{"data": candidateResponse(nil, false, "")["data"], "errors": []map[string]string{{"message": secret}}}
		}, want: "GraphQL response contains errors"},
		{name: "non current cycle", authority: candidateAuthority(2, 1), response: func() map[string]any {
			value := candidateResponse([]map[string]any{candidateNode("issue", "IFAN-1", 1, requiredLabels())}, false, "")
			value["data"].(map[string]any)["team"].(map[string]any)["issues"].(map[string]any)["nodes"].([]map[string]any)[0]["cycle"].(map[string]any)["isActive"] = false
			return value
		}, want: "outside configured filters"},
		{name: "required label absent", authority: candidateAuthority(2, 1), response: func() map[string]any {
			return candidateResponse([]map[string]any{candidateNode("issue", "IFAN-1", 1, []map[string]string{{"id": "repo", "name": "repo:brand"}})}, false, "")
		}, want: "outside configured filters"},
		{name: "excluded label present", authority: candidateAuthority(2, 1), response: func() map[string]any {
			return candidateResponse([]map[string]any{candidateNode("issue", "IFAN-1", 1, append(requiredLabels(), map[string]string{"id": "hermes", "name": "agent:hermes"}))}, false, "")
		}, want: "outside configured filters"},
		{name: "no repository label", authority: candidateAuthority(2, 1), response: func() map[string]any {
			return candidateResponse([]map[string]any{candidateNode("issue", "IFAN-1", 1, []map[string]string{{"id": "codex", "name": "agent:codex"}})}, false, "")
		}, want: "outside configured filters"},
		{name: "wrong team", authority: candidateAuthority(2, 1), response: func() map[string]any {
			value := candidateResponse(nil, false, "")
			value["data"].(map[string]any)["team"].(map[string]any)["key"] = "OTHER"
			return value
		}, want: "authority does not match"},
		{name: "wrong todo state type", authority: candidateAuthority(2, 1), response: func() map[string]any {
			value := candidateResponse(nil, false, "")
			states := value["data"].(map[string]any)["team"].(map[string]any)["states"].(map[string]any)["nodes"].([]map[string]any)
			states[0]["type"] = "started"
			return value
		}, want: "Todo workflow authority"},
		{name: "wrong todo state id", authority: candidateAuthority(2, 1), response: func() map[string]any {
			value := candidateResponse(nil, false, "")
			states := value["data"].(map[string]any)["team"].(map[string]any)["states"].(map[string]any)["nodes"].([]map[string]any)
			states[0]["id"] = candidateStartStateID
			return value
		}, want: "duplicate workflow state"},
		{name: "invalid issue id", authority: candidateAuthority(2, 1), response: func() map[string]any {
			value := candidateResponse([]map[string]any{candidateNode("issue", "IFAN-1", 1, requiredLabels())}, false, "")
			value["data"].(map[string]any)["team"].(map[string]any)["issues"].(map[string]any)["nodes"].([]map[string]any)[0]["id"] = "invalid-issue-id"
			return value
		}, want: "incomplete or outside"},
		{name: "invalid cycle id", authority: candidateAuthority(2, 1), response: func() map[string]any {
			value := candidateResponse([]map[string]any{candidateNode("issue", "IFAN-1", 1, requiredLabels())}, false, "")
			value["data"].(map[string]any)["team"].(map[string]any)["issues"].(map[string]any)["nodes"].([]map[string]any)[0]["cycle"].(map[string]any)["id"] = "invalid-cycle-id"
			return value
		}, want: "incomplete or outside"},
		{name: "invalid label id", authority: candidateAuthority(2, 1), response: func() map[string]any {
			value := candidateResponse([]map[string]any{candidateNode("issue", "IFAN-1", 1, requiredLabels())}, false, "")
			labels := value["data"].(map[string]any)["team"].(map[string]any)["issues"].(map[string]any)["nodes"].([]map[string]any)[0]["labels"].(map[string]any)["nodes"].([]map[string]string)
			labels[0]["id"] = "invalid-label-id"
			return value
		}, want: "incomplete label"},
		{name: "missing cursor", authority: candidateAuthority(2, 2), response: func() map[string]any { return candidateResponse(nil, true, "") }, want: "pagination limit exceeded"},
		{name: "response overflow", authority: candidateAuthority(2, 1), response: func() map[string]any { return map[string]any{"data": strings.Repeat("x", 2048)} }, want: "exceeds configured limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeCandidateResponse(t, w, test.response()) }))
			defer server.Close()
			config := testConfig(server.URL)
			if test.name == "response overflow" {
				config.MaxResponseBytes = 1024
			}
			client, err := New(config, &staticCredentials{token: "linear-secret-token"}, fixedClock{time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			scan, observations, err := client.ListTodoCandidates(context.Background(), test.authority)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(scan.Candidates) != 0 || strings.Contains(err.Error(), secret) || strings.Contains(fmtObservation(observations), secret) {
				t.Fatalf("scan=%+v observations=%+v error=%v", scan, observations, err)
			}
		})
	}
}

func TestListTodoCandidatesClassifiesRateLimitWithoutReturningCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("untrusted-rate-limit-body"))
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), &staticCredentials{token: "linear-secret-token"}, fixedClock{time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	scan, observations, err := client.ListTodoCandidates(context.Background(), candidateAuthority(2, 1))
	if err == nil || !strings.Contains(err.Error(), "rate limited") || len(scan.Candidates) != 0 || len(observations) != 1 || observations[0].ErrorClass != "rate_limited" || strings.Contains(fmtObservation(observations), "untrusted-rate-limit-body") {
		t.Fatalf("scan=%+v observations=%+v error=%v", scan, observations, err)
	}
}

func TestListTodoCandidatesRejectsPaginationOverlapCursorAndAuthorityDrift(t *testing.T) {
	tests := []struct {
		name   string
		second func(map[string]any) map[string]any
		want   string
	}{
		{name: "overlap", second: func(value map[string]any) map[string]any {
			value["data"].(map[string]any)["team"].(map[string]any)["issues"].(map[string]any)["nodes"] = []map[string]any{candidateNode("issue-a", "IFAN-1", 1, requiredLabels())}
			return value
		}, want: "appeared more than once"},
		{name: "repeated cursor", second: func(value map[string]any) map[string]any {
			value["data"].(map[string]any)["team"].(map[string]any)["issues"].(map[string]any)["pageInfo"].(map[string]any)["endCursor"] = "cursor-1"
			return value
		}, want: "cursor appeared more than once"},
		{name: "workflow source drift", second: func(value map[string]any) map[string]any {
			states := value["data"].(map[string]any)["team"].(map[string]any)["states"].(map[string]any)["nodes"].([]map[string]any)
			states[0]["name"] = "Changed"
			return value
		}, want: "Todo workflow authority"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls == 0 {
					writeCandidateResponse(t, w, candidateResponse([]map[string]any{candidateNode("issue-a", "IFAN-1", 1, requiredLabels())}, true, "cursor-1"))
				} else {
					writeCandidateResponse(t, w, test.second(candidateResponse([]map[string]any{candidateNode("issue-b", "IFAN-2", 2, requiredLabels())}, true, "cursor-2")))
				}
				calls++
			}))
			defer server.Close()
			client, err := New(testConfig(server.URL), &staticCredentials{token: "linear-secret-token"}, fixedClock{time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			scan, observations, err := client.ListTodoCandidates(context.Background(), candidateAuthority(3, 3))
			if err == nil || !strings.Contains(err.Error(), test.want) || len(scan.Candidates) != 0 || len(observations) != 2 {
				t.Fatalf("scan=%+v observations=%+v error=%v", scan, observations, err)
			}
		})
	}
}

func TestListTodoCandidatesRejectsInvalidAuthorityWithoutResolvingCredentials(t *testing.T) {
	credentials := &staticCredentials{token: "linear-secret-token"}
	client, err := New(testConfig("http://127.0.0.1:9999"), credentials, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	authority := candidateAuthority(2, 1)
	authority.TodoState.ID = "Todo"
	if _, observations, err := client.ListTodoCandidates(context.Background(), authority); err == nil || len(observations) != 0 || len(credentials.refs) != 0 || strings.Contains(err.Error(), "Todo") {
		t.Fatalf("error=%v observations=%+v credentials=%+v", err, observations, credentials.refs)
	}
}

func TestFinalizedCandidateScanUsesCanonicalIdentityOrderForDigest(t *testing.T) {
	first := application.LinearTodoCandidate{IssueID: fixtureUUID("issue-b"), Identifier: "IFAN-2", SourceDigest: "b"}
	second := application.LinearTodoCandidate{IssueID: fixtureUUID("issue-a"), Identifier: "IFAN-1", SourceDigest: "a"}
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	forward := finalizedCandidateScan([]application.LinearTodoCandidate{first, second}, now)
	reversed := finalizedCandidateScan([]application.LinearTodoCandidate{second, first}, now)
	if forward.Digest == "" || forward.Digest != reversed.Digest || forward.Candidates[0].IssueID != fixtureUUID("issue-b") || reversed.Candidates[0].IssueID != fixtureUUID("issue-b") {
		t.Fatalf("scan digest or order was not canonical: forward=%+v reversed=%+v", forward, reversed)
	}
}

func candidateAuthority(maxCandidates, maxPages int) application.LinearTodoCandidateAuthority {
	return application.LinearTodoCandidateAuthority{TeamID: candidateTeamID, TeamKey: "IFAN", TodoState: application.LinearState{ID: candidateTodoStateID, Name: "Todo", Type: "unstarted"}, InProgressState: application.LinearState{ID: candidateStartStateID, Name: "In Progress", Type: "started"}, MaxCandidates: maxCandidates, MaxPages: maxPages}
}

func candidateResponse(nodes []map[string]any, hasNext bool, cursor string) map[string]any {
	if nodes == nil {
		nodes = []map[string]any{}
	}
	return map[string]any{"data": map[string]any{"team": map[string]any{
		"id": candidateTeamID, "key": "IFAN",
		"states": map[string]any{"nodes": []map[string]any{{"id": candidateTodoStateID, "name": "Todo", "type": "unstarted"}, {"id": candidateStartStateID, "name": "In Progress", "type": "started"}}},
		"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor}},
	}}}
}

func candidateNode(id, identifier string, priority int, labels []map[string]string) map[string]any {
	return map[string]any{"id": fixtureUUID(id), "identifier": identifier, "priority": priority, "branchName": "ifan/" + strings.ToLower(identifier), "title": candidateUntrustedText, "description": candidateUntrustedText,
		"createdAt": "2026-07-13T00:00:00Z", "updatedAt": "2026-07-14T00:00:00Z", "team": map[string]any{"id": candidateTeamID, "key": "IFAN"},
		"state":  map[string]any{"id": candidateTodoStateID, "name": "Todo", "type": "unstarted"},
		"cycle":  map[string]any{"id": fixtureUUID("cycle-id"), "number": 1, "startsAt": "2026-07-01T00:00:00Z", "endsAt": "2026-07-31T00:00:00Z", "isActive": true},
		"labels": map[string]any{"nodes": fixtureLabels(labels), "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}},
	}
}

func fixtureUUID(value string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("candidate-fixture:"+value)).String()
}

func fixtureLabels(labels []map[string]string) []map[string]string {
	result := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		result = append(result, map[string]string{"id": fixtureUUID("label:" + label["id"]), "name": label["name"]})
	}
	return result
}

func requiredLabels() []map[string]string {
	return []map[string]string{{"id": "codex", "name": "agent:codex"}, {"id": "repo", "name": "repo:brand"}}
}

func writeCandidateResponse(t *testing.T, w http.ResponseWriter, response map[string]any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Fatal(err)
	}
}
