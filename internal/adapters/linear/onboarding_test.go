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

func TestRepositoryLabelLookupAndCreateBindExactTeamIdentity(t *testing.T) {
	const token = "linear-onboarding-secret"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("unexpected authorization")
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Variables["label"] != "repo:repository" {
			t.Fatalf("variables=%v", body.Variables)
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(body.Query, "ControllerOnboardingLabel") {
			_, _ = w.Write([]byte(`{"data":{"teams":{"nodes":[{"id":"team-1","key":"IFAN"}]},"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`))
			return
		}
		if body.Variables["teamID"] != "team-1" {
			t.Fatalf("create variables=%v", body.Variables)
		}
		_, _ = w.Write([]byte(`{"data":{"issueLabelCreate":{"success":true,"issueLabel":{"id":"label-1","name":"repo:repository","team":{"id":"team-1","key":"IFAN"}}}}}`))
	}))
	defer server.Close()
	now := time.Date(2026, 8, 25, 4, 0, 0, 0, time.UTC)
	client, err := New(testConfig(server.URL), &staticCredentials{token: token}, fixedClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := client.LookupRepositoryLabel(context.Background(), "repo:repository")
	if err != nil || missing.Found || missing.TeamID != "team-1" || missing.TeamKey != "IFAN" || len(missing.EvidenceDigest) != 64 {
		t.Fatalf("lookup=%+v err=%v", missing, err)
	}
	created, err := client.CreateRepositoryLabel(context.Background(), missing.TeamID, missing.Name)
	if err != nil || !created.Found || created.LabelID != "label-1" || created.TeamID != missing.TeamID || !created.ObservedAt.Equal(now) || len(created.EvidenceDigest) != 64 {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestRepositoryLabelRejectsEmptySlugBeforeCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid label reached Linear")
	}))
	defer server.Close()
	credentials := &staticCredentials{token: "secret"}
	client, err := New(testConfig(server.URL), credentials, fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LookupRepositoryLabel(context.Background(), "repo:"); err == nil || len(credentials.refs) != 0 {
		t.Fatalf("error=%v credential_reads=%v", err, credentials.refs)
	}
}
