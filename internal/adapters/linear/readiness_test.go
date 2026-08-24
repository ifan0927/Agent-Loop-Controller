package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRepositoryReadinessUsesBoundedReadQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer linear-secret" {
			t.Fatalf("method=%s authorization=%q", request.Method, request.Header.Get("Authorization"))
		}
		var payload struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(payload.Query), "mutation") || !strings.Contains(payload.Query, "issueLabels") {
			t.Fatalf("unsafe readiness query: %s", payload.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issueLabels":{"nodes":[{"id":"LABEL_1","name":"repo:one","team":{"key":"IFAN"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`))
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), &staticCredentials{token: "linear-secret"}, fixedClock{time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	profile := application.LocalRepository{LinearLabel: "repo:one", ProfileDigest: strings.Repeat("a", 64), RepositoryBindingDigest: strings.Repeat("b", 64)}
	result, err := client.ObserveRepositoryLinear(context.Background(), profile)
	if err != nil || result.Status != domain.RepositoryReady || result.Identity != "LABEL_1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
