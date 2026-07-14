package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestMoveReservedIssueToStartedUsesOnlyExactIssueUpdate(t *testing.T) {
	const token = "linear-start-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("unexpected mutation request authorization")
		}
		var request struct {
			Query     string `json:"query"`
			Variables struct {
				IssueID string `json:"issueID"`
				StateID string `json:"stateID"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(request.Query, "issueUpdate") || !strings.Contains(request.Query, "$issueID: ID!") || !strings.Contains(request.Query, "$stateID: ID!") || strings.Contains(request.Query, "priority") || strings.Contains(request.Query, "description") || request.Variables.IssueID != candidateTeamID || request.Variables.StateID != candidateStartStateID {
			t.Fatalf("unsafe or invalid mutation query: %+v", request)
		}
		writeStartMutation(t, w, candidateTeamID, candidateStartStateID, "In Progress", "started")
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), &staticCredentials{token: token}, fixedClock{time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	result, observations, err := client.MoveReservedIssueToStarted(context.Background(), application.LinearIssueStartMutation{IssueID: candidateTeamID, TargetStateID: candidateStartStateID})
	if err != nil || result.IssueID != candidateTeamID || result.State.ID != candidateStartStateID || len(observations) != 1 || observations[0].Operation != "move_reserved_issue_to_started" || observations[0].ResponseDigest == "" || strings.Contains(fmtObservation(observations), token) {
		t.Fatalf("result=%+v observations=%+v err=%v", result, observations, err)
	}
}

func TestMoveReservedIssueToStartedRefreshesExactlyOnceAfter401(t *testing.T) {
	credentials := &rotatingCredentials{tokens: []string{"stale-token", "fresh-token"}}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.Header.Get("Authorization") != "Bearer stale-token" {
				t.Fatal("first mutation did not use stale token")
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if calls != 2 || r.Header.Get("Authorization") != "Bearer fresh-token" {
			t.Fatalf("unexpected refresh attempt: calls=%d authorization=%q", calls, r.Header.Get("Authorization"))
		}
		writeStartMutation(t, w, candidateTeamID, candidateStartStateID, "In Progress", "started")
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), credentials, fixedClock{time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, observations, err := client.MoveReservedIssueToStarted(context.Background(), application.LinearIssueStartMutation{IssueID: candidateTeamID, TargetStateID: candidateStartStateID})
	if err != nil || calls != 2 || len(observations) != 2 || credentials.calls != 2 || observations[0].ErrorClass != "unauthorized" {
		t.Fatalf("calls=%d credential_calls=%d observations=%+v err=%v", calls, credentials.calls, observations, err)
	}
}

func TestMoveReservedIssueToStartedClassifiesAmbiguousAndPartialFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		class     string
		ambiguous bool
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, class: "rate_limited", ambiguous: true},
		{name: "server", status: http.StatusBadGateway, class: "server_error", ambiguous: true},
		{name: "forbidden", status: http.StatusForbidden, class: "forbidden"},
		{name: "missing", status: http.StatusNotFound, class: "not_found"},
		{name: "partial", status: http.StatusOK, body: `{"data":{"issueUpdate":{"success":true,"issue":null}}}`, class: "partial_mutation"},
		{name: "malformed", status: http.StatusOK, body: `{`, class: "malformed_json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := New(testConfig(server.URL), &staticCredentials{token: "linear-start-secret"}, fixedClock{time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			_, observations, err := client.MoveReservedIssueToStarted(context.Background(), application.LinearIssueStartMutation{IssueID: candidateTeamID, TargetStateID: candidateStartStateID})
			var mutationError *application.LinearIssueStartMutationError
			if !errors.As(err, &mutationError) || mutationError.Class != test.class || mutationError.Ambiguous != test.ambiguous || len(observations) != 1 || strings.Contains(fmtObservation(observations), "linear-start-secret") {
				t.Fatalf("err=%v observations=%+v", err, observations)
			}
		})
	}
}

type rotatingCredentials struct {
	tokens []string
	calls  int
}

func (c *rotatingCredentials) Resolve(_ context.Context, _ string) (string, error) {
	if c.calls >= len(c.tokens) {
		return "", errors.New("unexpected credential resolution")
	}
	token := c.tokens[c.calls]
	c.calls++
	return token, nil
}

func writeStartMutation(t *testing.T, w http.ResponseWriter, issueID, stateID, name, stateType string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": issueID, "state": map[string]any{"id": stateID, "name": name, "type": stateType}}}}}); err != nil {
		t.Fatal(err)
	}
}
