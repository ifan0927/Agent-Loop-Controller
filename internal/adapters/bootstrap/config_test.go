package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
)

func TestLoadBuildsOfflineSanitizedReadiness(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, secretPath := writeFixture(t, root, "github-app-profile:fixture", 7)
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != LegacyVersion || loaded.RegistryPath == "" {
		t.Fatalf("legacy bootstrap=%+v", loaded)
	}
	raw, err := json.Marshal(loaded.Readiness())
	if err != nil {
		t.Fatal(err)
	}
	output := string(raw)
	if !strings.Contains(output, `"offline":true`) || !strings.Contains(output, `"profile_id":"repository-profile:owner/repo"`) {
		t.Fatalf("readiness=%s", output)
	}
	if strings.Contains(output, secretPath) || strings.Contains(output, "Authorization") || strings.Contains(output, "not-for-output") || strings.Contains(output, root) {
		t.Fatalf("readiness leaked configuration or credential data: %s", output)
	}
}

func TestLoadVersionTwoBuildsInlineRepositoryRegistry(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, secretPath := writeV2Fixture(t, root, "github-app-profile:fixture", 7)
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != CurrentVersion || loaded.RegistryPath != "" {
		t.Fatalf("version two bootstrap=%+v", loaded)
	}
	binding, err := loaded.Registry.Resolve("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if binding.GitHubAppProfileRef != "github-app-profile:fixture" || binding.ExpectedRepositoryID != 9 {
		t.Fatalf("binding=%+v", binding)
	}
	raw, err := json.Marshal(loaded.Readiness())
	if err != nil {
		t.Fatal(err)
	}
	if output := string(raw); strings.Contains(output, secretPath) || strings.Contains(output, root) {
		t.Fatalf("readiness leaked configuration or credential data: %s", output)
	}
}

func TestLoadVersionTwoRejectsMixedOrInvalidInlineRepositoryTopology(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, _ := writeV2Fixture(t, root, "github-app-profile:fixture", 7)
	config := readJSONFixture(t, configPath)

	config["repository_registry_file"] = filepath.Join(root, "registry.json")
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "invalid_config") {
		t.Fatalf("mixed topology error=%v", err)
	}

	delete(config, "repository_registry_file")
	delete(config, "repositories")
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "missing_reference") {
		t.Fatalf("missing inline registry error=%v", err)
	}

	config["version"] = LegacyVersion
	config["repositories"] = []any{}
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "invalid_config") {
		t.Fatalf("legacy inline registry error=%v", err)
	}
}

func TestLoadVersionTwoRejectsUnknownInlineRepositoryFieldAndAuthorityConflict(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, _ := writeV2Fixture(t, root, "github-app-profile:fixture", 7)
	config := readJSONFixture(t, configPath)
	repositories := config["repositories"].([]any)
	repository := repositories[0].(map[string]any)
	repository["unexpected"] = true
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "invalid_config") {
		t.Fatalf("unknown inline repository field error=%v", err)
	}

	delete(repository, "unexpected")
	profiles := config["github_app_profiles"].([]any)
	profile := profiles[0].(map[string]any)
	profileConfig := profile["config"].(map[string]any)
	profileConfig["app_id"] = float64(8)
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "identity_conflict") {
		t.Fatalf("inline authority conflict error=%v", err)
	}
}

func TestLoadRejectsMissingProfileAndIdentityConflict(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, _ := writeFixture(t, root, "github-app-profile:missing", 7)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "missing_reference") {
		t.Fatalf("missing profile error=%v", err)
	}
	root = canonicalTempDir(t)
	configPath, _ = writeFixture(t, root, "github-app-profile:fixture", 8)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "identity_conflict") {
		t.Fatalf("identity conflict error=%v", err)
	}
}

func TestLoadRejectsSymlinkedConfigWithoutExposingPath(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, secretPath := writeFixture(t, root, "github-app-profile:fixture", 7)
	linkPath := filepath.Join(root, "link.json")
	if err := os.Symlink(configPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(linkPath); err == nil || strings.Contains(err.Error(), secretPath) || !strings.Contains(err.Error(), "unsafe_path") {
		t.Fatalf("symlink error=%v", err)
	}
}

func writeFixture(t *testing.T, root, profileID string, appID int64) (string, string) {
	t.Helper()
	paths := []string{filepath.Join(root, "origin"), filepath.Join(root, "source"), filepath.Join(root, "runs"), filepath.Join(root, "worktrees")}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secretPath := filepath.Join(root, "private.pem")
	if err := os.WriteFile(secretPath, []byte("Authorization: Bearer not-for-output"), 0o600); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(root, "registry.json")
	registry := localregistry.File{Version: 1, Repositories: []localregistry.Repository{{Owner: "owner", Name: "repo", OriginPath: paths[0], SourcePath: paths[1], RunRoot: paths[2], WorktreeRoot: paths[3], BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"}, GitHubAppProfileRef: profileID, GitHubAppID: 7, GitHubInstallationID: 8, ExpectedRepositoryID: 9, OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 1, NodeID: "node", Login: "ifan0927", Type: "User"}}}}}}
	rawRegistry, _ := json.Marshal(registry)
	if err := os.WriteFile(registryPath, rawRegistry, 0o600); err != nil {
		t.Fatal(err)
	}
	github := map[string]any{"api_base_url": "https://api.github.com", "graphql_url": "https://api.github.com/graphql", "app_id": appID, "installation_id": 8, "repository_owner": "owner", "repository_name": "repo", "repository_id": 9, "private_key_file": secretPath, "http_timeout": "2s", "token_refresh_skew": "5m", "api_version": "2022-11-28"}
	config := map[string]any{"version": 1, "controller": map[string]any{"database_path": filepath.Join(root, "controller.db"), "codex_binary": "codex", "run_timeout": "30m"}, "linear": map[string]any{"api_url": "https://api.linear.app/graphql", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN", "authorization_scheme": "bearer", "team_key": "IFAN", "http_timeout": "2s", "max_response_bytes": 4096, "label_page_size": 10, "max_label_pages": 1}, "repository_registry_file": registryPath, "github_app_profiles": []map[string]any{{"id": "github-app-profile:fixture", "config": github}}}
	rawConfig, _ := json.Marshal(config)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, rawConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, secretPath
}

func writeV2Fixture(t *testing.T, root, profileID string, appID int64) (string, string) {
	t.Helper()
	paths := []string{filepath.Join(root, "origin"), filepath.Join(root, "source"), filepath.Join(root, "runs"), filepath.Join(root, "worktrees")}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secretPath := filepath.Join(root, "private.pem")
	if err := os.WriteFile(secretPath, []byte("Authorization: Bearer not-for-output"), 0o600); err != nil {
		t.Fatal(err)
	}
	repositories := []localregistry.Repository{{Owner: "owner", Name: "repo", OriginPath: paths[0], SourcePath: paths[1], RunRoot: paths[2], WorktreeRoot: paths[3], BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"}, GitHubAppProfileRef: profileID, GitHubAppID: 7, GitHubInstallationID: 8, ExpectedRepositoryID: 9, OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 1, NodeID: "node", Login: "ifan0927", Type: "User"}}}}}
	github := map[string]any{"api_base_url": "https://api.github.com", "graphql_url": "https://api.github.com/graphql", "app_id": appID, "installation_id": 8, "repository_owner": "owner", "repository_name": "repo", "repository_id": 9, "private_key_file": secretPath, "http_timeout": "2s", "token_refresh_skew": "5m", "api_version": "2022-11-28"}
	config := map[string]any{"version": CurrentVersion, "controller": map[string]any{"database_path": filepath.Join(root, "controller.db"), "codex_binary": "codex", "run_timeout": "30m"}, "linear": map[string]any{"api_url": "https://api.linear.app/graphql", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN", "authorization_scheme": "bearer", "team_key": "IFAN", "http_timeout": "2s", "max_response_bytes": 4096, "label_page_size": 10, "max_label_pages": 1}, "repositories": repositories, "github_app_profiles": []map[string]any{{"id": "github-app-profile:fixture", "config": github}}}
	configPath := filepath.Join(root, "controller.json")
	writeJSONFixture(t, configPath, config)
	return configPath, secretPath
}

func readJSONFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
