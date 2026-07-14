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
	if loaded.Version != VersionTwo || loaded.RegistryPath != "" {
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

func TestLoadVersionThreeDisabledAutomationIsCompatibleAndOffline(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, secretPath := writeV2Fixture(t, root, "github-app-profile:fixture", 7)
	config := readJSONFixture(t, configPath)
	config["version"] = CurrentVersion
	config["automation"] = map[string]any{"linear_todo_admission": map[string]any{
		"enabled": false,
		// Disabled authority deliberately does not validate or resolve operational values.
		"credential_source_ref": "secret://unavailable/not-read",
		"poll_interval":         "not-a-duration",
	}}
	writeJSONFixture(t, configPath, config)
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Automation.LinearTodoAdmission.Enabled {
		t.Fatal("disabled automation became enabled")
	}
	inspect, err := json.Marshal(loaded.Readiness())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inspect), "credential_source_ref") || strings.Contains(string(inspect), "unavailable") || strings.Contains(string(inspect), secretPath) {
		t.Fatalf("disabled inspect leaked configuration: %s", inspect)
	}

	config["automation"].(map[string]any)["unexpected"] = true
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "invalid_config") {
		t.Fatalf("disabled unknown field error=%v", err)
	}
}

func TestLoadVersionTwoRejectsAutomationRatherThanSilentlyEnablingIt(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, _ := writeV2Fixture(t, root, "github-app-profile:fixture", 7)
	config := readJSONFixture(t, configPath)
	config["automation"] = map[string]any{"linear_todo_admission": map[string]any{"enabled": false}}
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "invalid_config") {
		t.Fatalf("v2 automation error=%v", err)
	}
}

func TestLoadVersionThreeEnabledAutomationValidatesAuthorityAndSanitizesInspect(t *testing.T) {
	root := canonicalTempDir(t)
	configPath, secretPath := writeV2Fixture(t, root, "github-app-profile:fixture", 7)
	config := readJSONFixture(t, configPath)
	config["version"] = CurrentVersion
	config["automation"] = map[string]any{"linear_todo_admission": validAdmissionFixture()}
	writeJSONFixture(t, configPath, config)

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Automation.LinearTodoAdmission.Enabled || loaded.Automation.LinearTodoAdmission.MaxActiveRuns != 1 {
		t.Fatalf("automation=%+v", loaded.Automation.LinearTodoAdmission)
	}
	first, err := json.Marshal(loaded.Readiness())
	if err != nil {
		t.Fatal(err)
	}
	loadedAgain, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(loadedAgain.Readiness())
	if string(first) != string(second) {
		t.Fatalf("inspect is not stable:\n%s\n%s", first, second)
	}
	for _, forbidden := range []string{secretPath, "secret://", "123e4567-e89b-42d3-a456-426614174000", "123e4567-e89b-42d3-a456-426614174001", root} {
		if strings.Contains(string(first), forbidden) {
			t.Fatalf("inspect leaked %q: %s", forbidden, first)
		}
	}
	for _, required := range []string{`"enabled":true`, `"max_active_runs":1`, `"login":"ifan0927"`, `"profile_digest"`} {
		if !strings.Contains(string(first), required) {
			t.Fatalf("inspect omitted %q: %s", required, first)
		}
	}
}

func TestLoadVersionThreeEnabledAutomationRejectsMissingInvalidAndUnknownFields(t *testing.T) {
	for _, field := range []string{"enabled", "team_id", "team_key", "todo_state", "in_progress_state", "poll_interval", "scheduler_lease_ttl", "scheduler_lease_renewal_interval", "max_candidates", "max_pages", "max_active_runs", "requester", "notification_mode", "credential_source_ref"} {
		t.Run("missing_"+field, func(t *testing.T) {
			configPath, config := enabledAutomationConfig(t)
			admission := config["automation"].(map[string]any)["linear_todo_admission"].(map[string]any)
			delete(admission, field)
			writeJSONFixture(t, configPath, config)
			if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "invalid_config") {
				t.Fatalf("missing %s error=%v", field, err)
			}
		})
	}
	for _, mutate := range []func(map[string]any){
		func(a map[string]any) { a["unknown"] = true },
		func(a map[string]any) { a["team_id"] = "not-a-uuid" },
		func(a map[string]any) { a["team_key"] = "OTHER" },
		func(a map[string]any) { a["todo_state"].(map[string]any)["id"] = "not-a-uuid" },
		func(a map[string]any) { a["todo_state"].(map[string]any)["name"] = "Backlog" },
		func(a map[string]any) { a["in_progress_state"].(map[string]any)["type"] = "unstarted" },
		func(a map[string]any) {
			a["in_progress_state"].(map[string]any)["id"] = a["todo_state"].(map[string]any)["id"]
		},
		func(a map[string]any) { a["poll_interval"] = "10s" },
		func(a map[string]any) { a["poll_interval"] = "2h" },
		func(a map[string]any) { a["scheduler_lease_ttl"] = "20s" },
		func(a map[string]any) { a["scheduler_lease_renewal_interval"] = "3s" },
		func(a map[string]any) { a["scheduler_lease_renewal_interval"] = "45s" },
		func(a map[string]any) { a["scheduler_lease_renewal_interval"] = "2562047h47m16.854775807s" },
		func(a map[string]any) { a["max_candidates"] = 0 },
		func(a map[string]any) { a["max_candidates"] = 101 },
		func(a map[string]any) { a["max_pages"] = 0 },
		func(a map[string]any) { a["max_pages"] = 21 },
		func(a map[string]any) { a["max_active_runs"] = 0 },
		func(a map[string]any) { a["max_active_runs"] = 2 },
		func(a map[string]any) { a["notification_mode"] = "remote" },
		func(a map[string]any) { a["credential_source_ref"] = "admission-secret-value" },
	} {
		configPath, config := enabledAutomationConfig(t)
		mutate(config["automation"].(map[string]any)["linear_todo_admission"].(map[string]any))
		writeJSONFixture(t, configPath, config)
		if _, err := Load(configPath); err == nil {
			t.Fatal("invalid enabled automation was accepted")
		}
	}
	for _, field := range []string{"id", "name", "type"} {
		configPath, config := enabledAutomationConfig(t)
		delete(config["automation"].(map[string]any)["linear_todo_admission"].(map[string]any)["todo_state"].(map[string]any), field)
		writeJSONFixture(t, configPath, config)
		if _, err := Load(configPath); err == nil {
			t.Fatalf("missing todo state %s was accepted", field)
		}
	}

	configPath, config := enabledAutomationConfig(t)
	config["automation"].(map[string]any)["unknown"] = true
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "invalid_config") {
		t.Fatalf("unknown automation field error=%v", err)
	}

	configPath, config = enabledAutomationConfig(t)
	config["linear"].(map[string]any)["team_key"] = "OTHER"
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "identity_conflict") {
		t.Fatalf("Linear profile team mismatch error=%v", err)
	}
}

func TestLoadVersionThreeEnabledAutomationRejectsRequesterLookalikesAndBots(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(r map[string]any) { r["database_id"] = 2 },
		func(r map[string]any) { r["node_id"] = "lookalike" },
		func(r map[string]any) { r["login"] = "other" },
		func(r map[string]any) { r["type"] = "Bot" },
	} {
		configPath, config := enabledAutomationConfig(t)
		admission := config["automation"].(map[string]any)["linear_todo_admission"].(map[string]any)
		mutate(admission["requester"].(map[string]any))
		writeJSONFixture(t, configPath, config)
		if _, err := Load(configPath); err == nil {
			t.Fatalf("requester mismatch error=%v", err)
		}
	}
	for _, field := range []string{"database_id", "node_id", "login", "type"} {
		configPath, config := enabledAutomationConfig(t)
		delete(config["automation"].(map[string]any)["linear_todo_admission"].(map[string]any)["requester"].(map[string]any), field)
		writeJSONFixture(t, configPath, config)
		if _, err := Load(configPath); err == nil {
			t.Fatalf("missing requester %s was accepted", field)
		}
	}

	configPath, config := enabledAutomationConfig(t)
	const secret = "not-a-secret-reference-value"
	config["automation"].(map[string]any)["linear_todo_admission"].(map[string]any)["credential_source_ref"] = secret
	writeJSONFixture(t, configPath, config)
	if _, err := Load(configPath); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("credential reference error=%v", err)
	}
}

func enabledAutomationConfig(t *testing.T) (string, map[string]any) {
	t.Helper()
	root := canonicalTempDir(t)
	configPath, _ := writeV2Fixture(t, root, "github-app-profile:fixture", 7)
	config := readJSONFixture(t, configPath)
	config["version"] = CurrentVersion
	config["automation"] = map[string]any{"linear_todo_admission": validAdmissionFixture()}
	return configPath, config
}

func validAdmissionFixture() map[string]any {
	return map[string]any{
		"enabled": true, "team_id": "123e4567-e89b-42d3-a456-426614174000", "team_key": "IFAN",
		"todo_state":        map[string]any{"id": "123e4567-e89b-42d3-a456-426614174001", "name": "Todo", "type": "unstarted"},
		"in_progress_state": map[string]any{"id": "123e4567-e89b-42d3-a456-426614174002", "name": "In Progress", "type": "started"},
		"poll_interval":     "5m", "scheduler_lease_ttl": "1m", "scheduler_lease_renewal_interval": "20s",
		"max_candidates": 20, "max_pages": 5, "max_active_runs": 1,
		"requester":         map[string]any{"database_id": 1, "node_id": "node", "login": "ifan0927", "type": "User"},
		"notification_mode": "local_outbox", "credential_source_ref": "secret://env/ADMISSION_TOKEN",
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
	config := map[string]any{"version": VersionTwo, "controller": map[string]any{"database_path": filepath.Join(root, "controller.db"), "codex_binary": "codex", "run_timeout": "30m"}, "linear": map[string]any{"api_url": "https://api.linear.app/graphql", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN", "authorization_scheme": "bearer", "team_key": "IFAN", "http_timeout": "2s", "max_response_bytes": 4096, "label_page_size": 10, "max_label_pages": 1}, "repositories": repositories, "github_app_profiles": []map[string]any{{"id": "github-app-profile:fixture", "config": github}}}
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
