package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestManagedConfigMigrationCLIPreviewApplyReplayAndRestartConvergence(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, _ := writeLegacyManagedMigrationConfig(t, root, 3)
	baseline, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	_, heartbeat, err := startManualControllerHeartbeat(context.Background(), baseline.Path, baseline.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer heartbeat.Stop()
	requester := managedDraftRequesterArgs(configPath)
	if _, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) }); err != nil {
		t.Fatal(err)
	}
	previewOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"migrate", "preview"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var preview application.ConfigurationMigrationPreview
	if err := json.Unmarshal([]byte(previewOutput), &preview); err != nil || preview.SourceSchemaVersion != 3 || preview.TargetSchemaVersion != 5 || !preview.RestartRequired || preview.ExpectedGenerationID != 1 || !preview.Preservation.RepositoryBindingsPreserved {
		t.Fatalf("preview=%+v output=%s err=%v", preview, previewOutput, err)
	}
	if strings.Contains(previewOutput, root) || strings.Contains(previewOutput, "private-key-material") || strings.Contains(previewOutput, "IFAN_LOOP_LINEAR_TOKEN") {
		t.Fatalf("preview exposed private configuration: %s", previewOutput)
	}
	applyArgs := append([]string{"migrate", "apply"}, requester...)
	applyArgs = append(applyArgs,
		"--request-id", "migration-request-1",
		"--expected-generation-id", strconv.FormatInt(preview.ExpectedGenerationID, 10),
		"--expected-digest", preview.ExpectedDigest,
		"--expected-authority-version", strconv.FormatInt(preview.ExpectedAuthorityVersion, 10),
		"--source-schema-version", strconv.Itoa(preview.SourceSchemaVersion),
		"--target-schema-version", strconv.Itoa(preview.TargetSchemaVersion),
		"--candidate-digest", preview.CandidateDigest,
		"--migration-digest", preview.MigrationDigest,
		"--preview-digest", preview.PreviewDigest,
	)
	invalidated := append([]string(nil), applyArgs...)
	for index := range invalidated {
		if invalidated[index] == "--preview-digest" {
			invalidated[index+1] = strings.Repeat("f", 64)
		}
	}
	if _, err := captureConfigOutput(func() error { return configCommand(invalidated) }); err == nil || !strings.Contains(err.Error(), "preview changed") {
		t.Fatalf("invalidated preview err=%v", err)
	}
	applyOutput, err := captureConfigOutput(func() error { return configCommand(applyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var applied application.ConfigurationMigrationApplyResult
	if err := json.Unmarshal([]byte(applyOutput), &applied); err != nil || applied.Apply.Generation.GenerationID != 2 || applied.Apply.Generation.SchemaVersion != 5 || applied.Apply.Generation.ApplyKind != application.ConfigurationApplySchemaMigration || applied.Convergence.State != application.ConfigurationRestartRequired || applied.Apply.Receipt.Phase != application.OperationPhaseObserved {
		t.Fatalf("applied=%+v output=%s err=%v", applied, applyOutput, err)
	}
	if strings.Contains(applyOutput, root) || strings.Contains(applyOutput, "private-key-material") || strings.Contains(applyOutput, "IFAN_LOOP_LINEAR_TOKEN") {
		t.Fatalf("apply exposed private configuration: %s", applyOutput)
	}
	migratedBeforeReplay, err := loadManagedConfiguration(configPath)
	if err != nil || migratedBeforeReplay.Version != 5 || migratedBeforeReplay.Digest != preview.CandidateDigest {
		t.Fatalf("desired file was not schema 5 before replay: version=%d digest=%s err=%v", migratedBeforeReplay.Version, migratedBeforeReplay.Digest, err)
	}
	replayOutput, err := captureConfigOutput(func() error { return configCommand(applyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var replay application.ConfigurationMigrationApplyResult
	if err := json.Unmarshal([]byte(replayOutput), &replay); err != nil || replay.Apply.Generation.GenerationID != applied.Apply.Generation.GenerationID || replay.Apply.Receipt.OperationID != applied.Apply.Receipt.OperationID {
		t.Fatalf("replay=%+v output=%s err=%v", replay, replayOutput, err)
	}

	heartbeat.Stop()
	current, err := loadManagedConfiguration(configPath)
	if err != nil || current.Version != 5 || current.Automation.LinearTodoAdmission.HeavyCapacity != 1 {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	_, restarted, err := startManualControllerHeartbeat(context.Background(), current.Path, current.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Stop()
	statusOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var status application.ManagedConfigurationStatus
	if err := json.Unmarshal([]byte(statusOutput), &status); err != nil || status.Convergence.State != application.ConfigurationReady || status.Convergence.DesiredGenerationID != 2 || status.Convergence.EffectiveGenerationID != 2 {
		t.Fatalf("status=%+v output=%s err=%v", status, statusOutput, err)
	}
	files, err := configurationadapter.NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := files.ValidateCurrent(base)
	if err != nil {
		t.Fatal(err)
	}
	repository := validated.Repositories["owner/repo"]
	if _, _, _, err := files.MaterializeRepositoryRemoval(base, application.LocalRepository{CanonicalRepository: "owner/repo", ProfileID: repository.ProfileID, ProfileDigest: repository.ProfileDigest, RepositoryBindingDigest: repository.RepositoryBindingDigest}); err != nil {
		t.Fatalf("post-migration repository removal materialization failed: %v", err)
	}
}

func TestManagedConfigMigrationCLIRejectsIncompleteAndUnsafeInputs(t *testing.T) {
	requester := []string{"--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"}
	for _, args := range [][]string{
		{"migrate", "preview"},
		append([]string{"migrate", "apply"}, requester...),
		append(append([]string{"migrate", "preview"}, requester...), "--raw-json", `{}`),
		append(append([]string{"migrate", "apply"}, requester...), "--candidate-file", "/tmp/candidate.json"),
	} {
		if err := configCommand(args); err == nil {
			t.Fatalf("unsafe or incomplete migration input accepted: %v", args)
		}
	}
}

func TestManagedConfigMigrationPreviewFailsClosedOnLiveDriftAndCurrentSchema(t *testing.T) {
	t.Run("live drift", func(t *testing.T) {
		root := resolvedTempDir(t)
		configPath, _ := writeLegacyManagedMigrationConfig(t, root, 3)
		baseline, err := loadManagedConfiguration(configPath)
		if err != nil {
			t.Fatal(err)
		}
		_, heartbeat, err := startManualControllerHeartbeat(context.Background(), baseline.Path, baseline.Digest)
		if err != nil {
			t.Fatal(err)
		}
		defer heartbeat.Stop()
		requester := managedDraftRequesterArgs(configPath)
		if _, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) }); err != nil {
			t.Fatal(err)
		}
		var drift map[string]any
		if err := json.Unmarshal(mustReadTestFile(t, configPath), &drift); err != nil {
			t.Fatal(err)
		}
		drift["controller"].(map[string]any)["run_timeout"] = "31m"
		payload, _ := json.Marshal(drift)
		if err := os.WriteFile(configPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := captureConfigOutput(func() error { return configCommand(append([]string{"migrate", "preview"}, requester...)) }); err == nil || !strings.Contains(err.Error(), "exact desired, effective, and live convergence") {
			t.Fatalf("drift preview err=%v", err)
		}
	})

	t.Run("already current", func(t *testing.T) {
		root := resolvedTempDir(t)
		configPath, _ := writeCurrentManagedDraftConfig(t, root)
		loaded, err := loadManagedConfiguration(configPath)
		if err != nil {
			t.Fatal(err)
		}
		_, heartbeat, err := startManualControllerHeartbeat(context.Background(), loaded.Path, loaded.Digest)
		if err != nil {
			t.Fatal(err)
		}
		defer heartbeat.Stop()
		requester := managedDraftRequesterArgs(configPath)
		if _, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) }); err != nil {
			t.Fatal(err)
		}
		if _, err := captureConfigOutput(func() error { return configCommand(append([]string{"migrate", "preview"}, requester...)) }); err == nil || !strings.Contains(err.Error(), "already uses the current schema") {
			t.Fatalf("current preview err=%v", err)
		}
	})
}

func mustReadTestFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func writeLegacyManagedMigrationConfig(t *testing.T, root string, version int) (string, string) {
	t.Helper()
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config["version"] = version
	delete(config["controller"].(map[string]any), "operator")
	if version == 2 {
		delete(config, "automation")
	} else {
		admission := map[string]any{
			"enabled": true, "team_id": "123e4567-e89b-42d3-a456-426614174100", "team_key": "IFAN",
			"todo_state":        map[string]any{"id": offlineAdmissionTodoState.ID, "name": offlineAdmissionTodoState.Name, "type": offlineAdmissionTodoState.Type},
			"in_progress_state": map[string]any{"id": offlineAdmissionInProgressState.ID, "name": offlineAdmissionInProgressState.Name, "type": offlineAdmissionInProgressState.Type},
			"poll_interval":     "1m", "delivery_poll_interval": "30s", "scheduler_lease_ttl": "1m", "scheduler_lease_renewal_interval": "20s", "max_candidates": 10, "max_pages": 1,
			"requester":         map[string]any{"database_id": 33, "node_id": "MDQ6VXNlcjMz", "login": "ifan0927", "type": "User"},
			"notification_mode": "local_outbox", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN",
		}
		if version == 3 {
			admission["max_active_runs"] = 1
		}
		config["automation"] = map[string]any{"linear_todo_admission": admission}
	}
	rewritten, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, databasePath
}
