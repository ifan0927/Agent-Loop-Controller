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

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
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
		write(w, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","completed_at":"2026-07-11T00:00:00Z","app":{"id":1}}]}`)
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
	paths := []string{filepath.Join(dir, "origin"), filepath.Join(dir, "source"), filepath.Join(dir, "runs"), filepath.Join(dir, "worktrees")}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registryPath := filepath.Join(dir, "registry.json")
	registry := localregistry.File{Version: 1, Repositories: []localregistry.Repository{{Owner: "owner", Name: "repo", OriginPath: paths[0], SourcePath: paths[1], RunRoot: paths[2], WorktreeRoot: paths[3], BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"}, GitHubAppProfileRef: "github-app-profile:fixture", GitHubAppID: 1, GitHubInstallationID: 2, ExpectedRepositoryID: 99, OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Login: "ifan0927", Type: "User"}}}}}}
	registryRaw, _ := json.Marshal(registry)
	if err := os.WriteFile(registryPath, registryRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	githubConfig := map[string]any{"api_base_url": server.URL, "graphql_url": server.URL + "/graphql", "app_id": 1, "installation_id": 2, "repository_owner": "owner", "repository_name": "repo", "repository_id": 99, "private_key_file": keyPath, "http_timeout": "2s", "token_refresh_skew": "5m", "api_version": "2022-11-28"}
	config := map[string]any{"version": 1, "controller": map[string]any{"database_path": dbPath, "codex_binary": "codex", "run_timeout": "30m"}, "linear": map[string]any{"api_url": "https://api.linear.app/graphql", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN", "authorization_scheme": "bearer", "team_key": "IFAN", "http_timeout": "2s", "max_response_bytes": 4096, "label_page_size": 10, "max_label_pages": 1}, "repository_registry_file": registryPath, "github_app_profiles": []map[string]any{{"id": "github-app-profile:fixture", "config": githubConfig}}}
	raw, _ := json.Marshal(config)
	configPath := filepath.Join(dir, "controller.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	read, pipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = pipeWrite
	callErr := githubRead([]string{"--config", configPath, "--pr", "1", "--expected-head", "head", "--run-id", "run", "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User", "--repository", "owner/repo", "--expected-state", "received", "--idempotency-key", "key"})
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
	if len(rendered) != 3 || rendered["reconciled_head"] != "head" || rendered["reconciliation_status"] != "pass" || rendered["current_state"] != "received" {
		t.Fatalf("GitHub CLI leaked non-contract fields: %s", output)
	}
	baseline := requests.Load()
	if err := githubRead([]string{"--config", configPath, "--pr", "1", "--expected-head", "head", "--run-id", "run", "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User", "--repository", "owner/repo", "--expected-state", "executing", "--idempotency-key", "key"}); err == nil {
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
