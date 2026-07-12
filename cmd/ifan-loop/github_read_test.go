package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestGitHubReadCLIEndToEndPersistsAndRestarts(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "fixture\n<!-- controller-run:key -->"
	bodySum := sha256.Sum256([]byte(body))
	mux := http.NewServeMux()
	var requests atomic.Int64
	write := func(w http.ResponseWriter, value string) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, value)
	}
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"token":"fixture-token","expires_at":"2099-07-11T01:00:00Z","permissions":{"metadata":"read","contents":"read","pull_requests":"read","checks":"read","statuses":"read","administration":"read"},"repositories":[{"id":99,"name":"repo","owner":{"login":"owner"}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"id":99,"node_id":"REPO","name":"repo","owner":{"login":"owner"}}`)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":101,"number":1,"html_url":"https://example.invalid/pr/1","node_id":"PR","state":"open","merged":false,"body":%q,"head":{"ref":"feature","sha":"head","repo":{"id":99}},"base":{"ref":"main","sha":"base","repo":{"id":99}}}`, body)
	})
	mux.HandleFunc("/repos/owner/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) { write(w, `{"contexts":["test"],"checks":[]}`) })
	mux.HandleFunc("/repos/owner/repo/commits/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:00:00Z","app":{"id":1}}]}`)
	})
	mux.HandleFunc("/repos/owner/repo/commits/head/status", func(w http.ResponseWriter, r *http.Request) { write(w, `{"total_count":0,"statuses":[]}`) })
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED","reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	dbPath := filepath.Join(dir, "controller.db")
	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	bindingJSON, _ := json.Marshal(application.LocalRepository{ProfileID: "repository-profile:owner/repo", ProfileSnapshotVersion: 1, ProfileDigest: "profile", RegistryVersion: 1, RegistryDigest: "registry", RepositoryBindingDigest: "binding", CanonicalRepository: "owner/repo", BaseBranch: "main", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2, AllowedOperatorLogins: []string{"ifan0927"}})
	run := application.Run{ID: "run", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: string(bindingJSON), ProfileID: "repository-profile:owner/repo", ProfileSnapshotVersion: 1, ProfileDigest: "profile", ProfileSnapshotJSON: `{}`, RegistryVersion: 1, RegistryDigest: "registry", RepositoryBindingDigest: "binding", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: filepath.Join(dir, "artifacts"), ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetWorkspace(context.Background(), "run", "base", filepath.Join(dir, "worktree")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(context.Background(), "run", "head"); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{Number: 1, URL: "https://example.invalid/pr/1", NodeID: "PR", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", BodyDigest: hex.EncodeToString(bodySum[:]), OwnershipKey: "key", State: "open"}
	if err := store.SavePullRequest(context.Background(), "run", pr); err != nil {
		t.Fatal(err)
	}
	store.Close()
	configPath := filepath.Join(dir, "github.json")
	config := map[string]any{"api_base_url": server.URL, "graphql_url": server.URL + "/graphql", "app_id": 1, "installation_id": 2, "repository_owner": "owner", "repository_name": "repo", "repository_id": 99, "private_key_file": keyPath, "http_timeout": "2s", "token_refresh_skew": "5m", "api_version": "2022-11-28", "coderabbit_actor_id": 0, "coderabbit_node_id": "", "coderabbit_app_id": 0}
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	read, pipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = pipeWrite
	callErr := githubRead([]string{"--config", configPath, "--pr", "1", "--expected-head", "head", "--db", dbPath, "--run-id", "run", "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User", "--repository", "owner/repo", "--expected-state", "received", "--idempotency-key", "key"})
	pipeWrite.Close()
	os.Stdout = original
	if callErr != nil {
		t.Fatal(callErr)
	}
	output, err := io.ReadAll(read)
	read.Close()
	if err != nil {
		t.Fatal(err)
	}
	var rendered map[string]any
	if err := json.Unmarshal(output, &rendered); err != nil {
		t.Fatal(err)
	}
	if len(rendered) != 1 || rendered["reconciled_head"] != "head" {
		t.Fatalf("GitHub CLI leaked non-contract fields: %s", output)
	}
	baseline := requests.Load()
	if err := githubRead([]string{"--config", configPath, "--pr", "1", "--expected-head", "head", "--db", dbPath, "--run-id", "run", "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User", "--repository", "owner/repo", "--expected-state", "executing", "--idempotency-key", "key"}); err == nil {
		t.Fatal("stale expected state was accepted")
	}
	if requests.Load() != baseline {
		t.Fatal("stale command reached GitHub before CAS rejection")
	}
	store, err = sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inspection, err := store.Inspect(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.PullRequest == nil || inspection.PullRequest.DatabaseID != 101 || inspection.GitHubEvidence == nil || inspection.GitHubInstallation == nil || len(inspection.GitHubRequests) == 0 {
		t.Fatalf("incomplete restarted inspection: %+v", inspection)
	}
}

var _ = time.UTC
