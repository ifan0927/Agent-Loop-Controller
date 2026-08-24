package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

	sourcesOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"rollback", "sources"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var sources application.ConfigurationRollbackSources
	if err := json.Unmarshal([]byte(sourcesOutput), &sources); err != nil || sources.DesiredGenerationID != 2 || len(sources.Sources) != 1 || sources.Sources[0].GenerationID != 1 || sources.Sources[0].Digest != baseline.Digest {
		t.Fatalf("sources=%+v output=%s err=%v", sources, sourcesOutput, err)
	}
	rollbackOpenArgs := append([]string{"rollback", "open"}, requester...)
	rollbackOpenArgs = append(rollbackOpenArgs, "--source-generation-id", "1", "--source-digest", baseline.Digest, "--expected-generation-id", "2", "--expected-digest", applied.Apply.Generation.Digest)
	rollbackOpenOutput, err := captureConfigOutput(func() error { return configCommand(rollbackOpenArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var rollbackDraft application.ConfigurationDraft
	if err := json.Unmarshal([]byte(rollbackOpenOutput), &rollbackDraft); err != nil || rollbackDraft.DraftOrigin != application.ConfigurationDraftOriginRollback || rollbackDraft.BaseGenerationID != 2 || rollbackDraft.RollbackSourceGenerationID != 1 || rollbackDraft.Settings.Admission.HeavyCapacity != 2 {
		t.Fatalf("rollback draft=%+v output=%s err=%v", rollbackDraft, rollbackOpenOutput, err)
	}
	rollbackPreviewArgs := append([]string{"draft", "preview"}, requester...)
	rollbackPreviewArgs = append(rollbackPreviewArgs, "--draft-id", rollbackDraft.DraftID, "--revision", "1")
	rollbackPreviewOutput, err := captureConfigOutput(func() error { return configCommand(rollbackPreviewArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var rollbackPreview application.ConfigurationPreview
	if err := json.Unmarshal([]byte(rollbackPreviewOutput), &rollbackPreview); err != nil || rollbackPreview.RollbackSourceGenerationID != 1 || rollbackPreview.RollbackSourceDigest != baseline.Digest || len(rollbackPreview.Changes) != 1 || rollbackPreview.Changes[0].Category != application.ConfigurationPreviewHeavyCapacityDecreased {
		t.Fatalf("rollback preview=%+v output=%s err=%v", rollbackPreview, rollbackPreviewOutput, err)
	}
	rollbackApplyArgs := append([]string{"draft", "apply"}, requester...)
	rollbackApplyArgs = append(rollbackApplyArgs, "--draft-id", rollbackDraft.DraftID, "--revision", "1", "--preview-digest", rollbackPreview.PreviewDigest, "--expected-generation-id", "2", "--expected-digest", applied.Apply.Generation.Digest)
	rollbackApplyOutput, err := captureConfigOutput(func() error { return configCommand(rollbackApplyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var rollbackApplied application.ConfigurationDraftApplyResult
	if err := json.Unmarshal([]byte(rollbackApplyOutput), &rollbackApplied); err != nil || rollbackApplied.Apply.Generation.GenerationID != 3 || rollbackApplied.Apply.Generation.ParentID != 2 || rollbackApplied.Apply.Generation.RollbackSourceGenerationID != 1 || rollbackApplied.Apply.Generation.RollbackSourceDigest != baseline.Digest {
		t.Fatalf("rollback applied=%+v output=%s err=%v", rollbackApplied, rollbackApplyOutput, err)
	}
	rollbackReplayOutput, err := captureConfigOutput(func() error { return configCommand(rollbackApplyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var rollbackReplay application.ConfigurationDraftApplyResult
	if err := json.Unmarshal([]byte(rollbackReplayOutput), &rollbackReplay); err != nil || rollbackReplay.Apply.Generation.GenerationID != rollbackApplied.Apply.Generation.GenerationID || rollbackReplay.Apply.Receipt.OperationID != rollbackApplied.Apply.Receipt.OperationID {
		t.Fatalf("rollback replay=%+v output=%s err=%v", rollbackReplay, rollbackReplayOutput, err)
	}

	convergedHeartbeat.Stop()
	rollbackCurrent, err := loadManagedConfiguration(configPath)
	if err != nil || rollbackCurrent.Digest != rollbackApplied.Apply.Generation.Digest {
		t.Fatalf("rollback current=%+v err=%v", rollbackCurrent, err)
	}
	_, rollbackHeartbeat, err := startManualControllerHeartbeat(context.Background(), rollbackCurrent.Path, rollbackCurrent.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackHeartbeat.Stop()
	rollbackStatusOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(rollbackStatusOutput), &status); err != nil || status.Convergence.State != application.ConfigurationReady || status.Convergence.DesiredGenerationID != 3 || status.Convergence.EffectiveGenerationID != 3 {
		t.Fatalf("rollback status=%+v output=%s err=%v", status, rollbackStatusOutput, err)
	}

	noOpOpenArgs := append([]string{"rollback", "open"}, requester...)
	noOpOpenArgs = append(noOpOpenArgs, "--source-generation-id", "1", "--source-digest", baseline.Digest, "--expected-generation-id", "3", "--expected-digest", rollbackApplied.Apply.Generation.Digest)
	noOpOpenOutput, err := captureConfigOutput(func() error { return configCommand(noOpOpenArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var noOpDraft application.ConfigurationDraft
	if err := json.Unmarshal([]byte(noOpOpenOutput), &noOpDraft); err != nil || noOpDraft.Settings.Admission.HeavyCapacity != 2 || noOpDraft.BaseGenerationID != 3 {
		t.Fatalf("no-op draft=%+v output=%s err=%v", noOpDraft, noOpOpenOutput, err)
	}
	noOpPreviewArgs := append([]string{"draft", "preview"}, requester...)
	noOpPreviewArgs = append(noOpPreviewArgs, "--draft-id", noOpDraft.DraftID, "--revision", "1")
	noOpPreviewOutput, err := captureConfigOutput(func() error { return configCommand(noOpPreviewArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var noOpPreview application.ConfigurationPreview
	if err := json.Unmarshal([]byte(noOpPreviewOutput), &noOpPreview); err != nil || len(noOpPreview.Changes) != 0 {
		t.Fatalf("no-op preview=%+v output=%s err=%v", noOpPreview, noOpPreviewOutput, err)
	}
	noOpApplyArgs := append([]string{"draft", "apply"}, requester...)
	noOpApplyArgs = append(noOpApplyArgs, "--draft-id", noOpDraft.DraftID, "--revision", "1", "--preview-digest", noOpPreview.PreviewDigest, "--expected-generation-id", "3", "--expected-digest", rollbackApplied.Apply.Generation.Digest)
	noOpApplyOutput, err := captureConfigOutput(func() error { return configCommand(noOpApplyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var noOpApplied application.ConfigurationDraftApplyResult
	if err := json.Unmarshal([]byte(noOpApplyOutput), &noOpApplied); err != nil || !noOpApplied.Apply.NoOp || noOpApplied.Apply.Generation.GenerationID != 3 {
		t.Fatalf("no-op applied=%+v output=%s err=%v", noOpApplied, noOpApplyOutput, err)
	}

	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if generations, err := store.ListConfigurationGenerations(context.Background()); err != nil || len(generations) != 3 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
	for _, output := range []string{openOutput, setOutput, validationOutput, previewOutput, applyOutput, replayOutput, statusOutput, sourcesOutput, rollbackOpenOutput, rollbackPreviewOutput, rollbackApplyOutput, rollbackReplayOutput, rollbackStatusOutput, noOpOpenOutput, noOpPreviewOutput, noOpApplyOutput} {
		for _, sensitive := range []string{root, databasePath, filepath.Join(root, "app.pem"), "private-key-material", "secret://", "IFAN_LOOP_LINEAR_TOKEN"} {
			if strings.Contains(output, sensitive) {
				t.Fatalf("managed config output leaked %q: %s", sensitive, output)
			}
		}
	}
}

func TestManagedConfigRecoveryCLIProjectsAndRestoresExactDesiredBytes(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	baseline, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(desired, &document); err != nil {
		t.Fatal(err)
	}
	document["controller"].(map[string]any)["run_timeout"] = "45m"
	drift, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, drift, 0o600); err != nil {
		t.Fatal(err)
	}
	requester := managedDraftRequesterArgs(configPath)
	if _, err := captureConfigOutput(func() error {
		return configCommand(append([]string{"status"}, []string{"--config", configPath, "--requester", "intruder", "--requester-database-id", "44", "--requester-node-id", "USER_44", "--requester-type", "User"}...))
	}); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unauthorized status err=%v", err)
	}
	statusOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var status application.ManagedConfigurationStatus
	if err := json.Unmarshal([]byte(statusOutput), &status); err != nil || status.Recovery == nil || status.Recovery.Action != application.ConfigurationRecoveryActionRestore || status.Recovery.ExpectedGenerationID != 1 || status.Recovery.ExpectedDigest != baseline.Digest {
		t.Fatalf("status=%+v output=%s err=%v", status, statusOutput, err)
	}
	recoveryArgs := append([]string{"recover", "restore"}, requester...)
	recoveryArgs = append(recoveryArgs,
		"--expected-generation-id", "1",
		"--expected-digest", status.Recovery.ExpectedDigest,
		"--expected-authority-version", strconv.FormatInt(status.Recovery.ExpectedAuthorityVersion, 10),
		"--observed-digest", status.Recovery.ObservedDigest,
	)
	recoveryOutput, err := captureConfigOutput(func() error { return configCommand(recoveryArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var recovered application.ConfigurationRecoveryResult
	if err := json.Unmarshal([]byte(recoveryOutput), &recovered); err != nil || recovered.Recovery.State != application.ConfigurationRecoveryCommitted || recovered.Receipt.OperationType != application.OperationRestoreConfiguration || recovered.Receipt.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("recovered=%+v output=%s err=%v", recovered, recoveryOutput, err)
	}
	if live, err := os.ReadFile(configPath); err != nil || !bytes.Equal(live, desired) {
		t.Fatalf("live differs from desired: live=%q err=%v", live, err)
	}
	replayOutput, err := captureConfigOutput(func() error { return configCommand(recoveryArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var replayed application.ConfigurationRecoveryResult
	if err := json.Unmarshal([]byte(replayOutput), &replayed); err != nil || replayed.Receipt.OperationID != recovered.Receipt.OperationID || replayed.Recovery.AcceptedAt != recovered.Recovery.AcceptedAt {
		t.Fatalf("replay=%+v output=%s err=%v", replayed, replayOutput, err)
	}
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if generations, err := store.ListConfigurationGenerations(context.Background()); err != nil || len(generations) != 1 {
		store.Close()
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
	_ = store.Close()

	if err := os.WriteFile(configPath, drift, 0o600); err != nil {
		t.Fatal(err)
	}
	secondStatusOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var secondStatus application.ManagedConfigurationStatus
	if err := json.Unmarshal([]byte(secondStatusOutput), &secondStatus); err != nil || secondStatus.Recovery == nil || secondStatus.Recovery.ObservedDigest != status.Recovery.ObservedDigest || secondStatus.Recovery.ExpectedAuthorityVersion <= status.Recovery.ExpectedAuthorityVersion {
		t.Fatalf("second status=%+v output=%s err=%v", secondStatus, secondStatusOutput, err)
	}
	if _, err := captureConfigOutput(func() error { return configCommand(recoveryArgs) }); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("old occurrence replay err=%v", err)
	}
	for _, output := range []string{statusOutput, recoveryOutput, replayOutput, secondStatusOutput} {
		for _, sensitive := range []string{root, databasePath, string(desired), string(drift), "private-key-material", "secret://"} {
			if strings.Contains(output, sensitive) {
				t.Fatalf("recovery output leaked %q: %s", sensitive, output)
			}
		}
	}
}

func TestManagedConfigDraftCLIRequiresCompleteRequesterForEveryOperation(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"recover", "restore", "--expected-generation-id", "1", "--expected-digest", strings.Repeat("a", 64), "--expected-authority-version", "2", "--observed-digest", strings.Repeat("b", 64)}, {"rollback", "sources"}, {"rollback", "open", "--source-generation-id", "1", "--source-digest", strings.Repeat("a", 64), "--expected-generation-id", "2", "--expected-digest", strings.Repeat("b", 64)}, {"draft", "open"}, {"draft", "show", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "set", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1", "--heavy-capacity", "2"}, {"draft", "validate", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "preview", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "apply", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "discard", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}} {
		if err := configCommand(args); err == nil || !strings.Contains(err.Error(), "complete requester") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	requester := []string{"--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"}
	for _, forbidden := range [][]string{
		append(append([]string{"rollback", "sources"}, requester...), "--repository", "owner/repo"),
		append(append([]string{"rollback", "open"}, requester...), "--raw-json", `{}`),
		append(append([]string{"rollback", "open"}, requester...), "--candidate-file", "/tmp/candidate.json"),
	} {
		if err := configCommand(forbidden); err == nil {
			t.Fatalf("forbidden rollback input accepted: %v", forbidden)
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
