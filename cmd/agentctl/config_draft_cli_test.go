package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestManagedConfigDraftCLIIsolatedTypedApplyAndConvergence(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
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

	openOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"draft", "open"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var draft application.ConfigurationDraft
	if err := json.Unmarshal([]byte(openOutput), &draft); err != nil || draft.Revision != 1 || draft.Settings.Admission.HeavyCapacity != 2 {
		t.Fatalf("draft=%+v output=%s err=%v", draft, openOutput, err)
	}

	invalidArgs := append([]string{"draft", "set"}, requester...)
	invalidArgs = append(invalidArgs, "--draft-id", draft.DraftID, "--revision", "1", "--heavy-capacity", "33")
	if _, err := captureConfigOutput(func() error { return configCommand(invalidArgs) }); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("capacity bound err=%v", err)
	}
	rawArgs := append([]string{"draft", "set"}, requester...)
	rawArgs = append(rawArgs, "--draft-id", draft.DraftID, "--revision", "1", "--raw-json", `{}`)
	if _, err := captureConfigOutput(func() error { return configCommand(rawArgs) }); err == nil {
		t.Fatal("raw configuration flag was accepted")
	}

	setArgs := append([]string{"draft", "set"}, requester...)
	setArgs = append(setArgs, "--draft-id", draft.DraftID, "--revision", "1", "--heavy-capacity", "3")
	setOutput, err := captureConfigOutput(func() error { return configCommand(setArgs) })
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(setOutput), &draft); err != nil || draft.Revision != 2 || draft.Settings.Admission.HeavyCapacity != 3 {
		t.Fatalf("draft=%+v output=%s err=%v", draft, setOutput, err)
	}

	validateArgs := append([]string{"draft", "validate"}, requester...)
	validateArgs = append(validateArgs, "--draft-id", draft.DraftID, "--revision", "2")
	validationOutput, err := captureConfigOutput(func() error { return configCommand(validateArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var validation application.ConfigurationValidationResult
	if err := json.Unmarshal([]byte(validationOutput), &validation); err != nil || !validation.Valid {
		t.Fatalf("validation=%+v output=%s err=%v", validation, validationOutput, err)
	}

	previewArgs := append([]string{"draft", "preview"}, requester...)
	previewArgs = append(previewArgs, "--draft-id", draft.DraftID, "--revision", "2")
	previewOutput, err := captureConfigOutput(func() error { return configCommand(previewArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var preview application.ConfigurationPreview
	if err := json.Unmarshal([]byte(previewOutput), &preview); err != nil || len(preview.Changes) != 1 || preview.Changes[0].Category != application.ConfigurationPreviewHeavyCapacityIncreased {
		t.Fatalf("preview=%+v output=%s err=%v", preview, previewOutput, err)
	}

	applyArgs := append([]string{"draft", "apply"}, requester...)
	applyArgs = append(applyArgs, "--draft-id", draft.DraftID, "--revision", "2", "--preview-digest", preview.PreviewDigest, "--expected-generation-id", "1", "--expected-digest", baseline.Digest)
	applyOutput, err := captureConfigOutput(func() error { return configCommand(applyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var applied application.ConfigurationDraftApplyResult
	if err := json.Unmarshal([]byte(applyOutput), &applied); err != nil || applied.Apply.Generation.GenerationID != 2 || applied.Apply.NoOp || applied.Convergence.State != application.ConfigurationRestartRequired {
		t.Fatalf("applied=%+v output=%s err=%v", applied, applyOutput, err)
	}
	replayOutput, err := captureConfigOutput(func() error { return configCommand(applyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var replay application.ConfigurationDraftApplyResult
	if err := json.Unmarshal([]byte(replayOutput), &replay); err != nil || replay.Apply.Generation.GenerationID != applied.Apply.Generation.GenerationID || replay.Apply.Receipt.OperationID != applied.Apply.Receipt.OperationID {
		t.Fatalf("replay=%+v output=%s err=%v", replay, replayOutput, err)
	}

	heartbeat.Stop()
	current, err := loadManagedConfiguration(configPath)
	if err != nil || current.Digest != applied.Apply.Generation.Digest {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	_, convergedHeartbeat, err := startManualControllerHeartbeat(context.Background(), current.Path, current.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer convergedHeartbeat.Stop()
	statusOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var status application.ManagedConfigurationStatus
	if err := json.Unmarshal([]byte(statusOutput), &status); err != nil || status.Convergence.State != application.ConfigurationReady || status.Convergence.DesiredGenerationID != 2 || status.Convergence.EffectiveGenerationID != 2 {
		t.Fatalf("status=%+v output=%s err=%v", status, statusOutput, err)
	}

	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if generations, err := store.ListConfigurationGenerations(context.Background()); err != nil || len(generations) != 2 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
	for _, output := range []string{openOutput, setOutput, validationOutput, previewOutput, applyOutput, replayOutput, statusOutput} {
		for _, sensitive := range []string{root, databasePath, filepath.Join(root, "app.pem"), "private-key-material", "secret://", "IFAN_LOOP_LINEAR_TOKEN"} {
			if strings.Contains(output, sensitive) {
				t.Fatalf("managed config output leaked %q: %s", sensitive, output)
			}
		}
	}
}

func TestManagedConfigDraftCLIRequiresCompleteRequesterForEveryOperation(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"draft", "open"}, {"draft", "show", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "set", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1", "--heavy-capacity", "2"}, {"draft", "validate", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "preview", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "apply", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "discard", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}} {
		if err := configCommand(args); err == nil || !strings.Contains(err.Error(), "complete requester") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func writeCurrentManagedDraftConfig(t *testing.T, root string) (string, string) {
	t.Helper()
	configPath, databasePath := writeControllerStatusConfig(t, root)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	registryPath := config["repository_registry_file"].(string)
	registryRaw, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry map[string]any
	if err := json.Unmarshal(registryRaw, &registry); err != nil {
		t.Fatal(err)
	}
	config["version"] = 5
	config["repositories"] = registry["repositories"]
	delete(config, "repository_registry_file")
	config["controller"].(map[string]any)["operator"] = map[string]any{"database_id": 33, "node_id": "MDQ6VXNlcjMz", "login": "ifan0927", "type": "User"}
	config["automation"] = map[string]any{"linear_todo_admission": map[string]any{
		"enabled": false, "poll_interval": "5m", "delivery_poll_interval": "30s", "scheduler_lease_ttl": "1m", "scheduler_lease_renewal_interval": "20s", "max_candidates": 20, "max_pages": 5, "heavy_capacity": 2,
	}}
	rewritten, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, databasePath
}

func managedDraftRequesterArgs(configPath string) []string {
	return []string{"--config", configPath, "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"}
}
