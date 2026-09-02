package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestDisabledRuntimeConvergesTypedConfigurationAndClearsFinalRemovalPolicyGuards(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	setFixtureAdmissionEnabled(t, configPath)
	t.Setenv("IFAN_LOOP_LINEAR_TOKEN", "")
	baseline, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	_, initialHeartbeat, err := startManualControllerHeartbeat(context.Background(), baseline.Path, baseline.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer initialHeartbeat.Stop()
	requester := managedDraftRequesterArgs(configPath)
	if _, err := captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) }); err != nil {
		initialHeartbeat.Stop()
		t.Fatal(err)
	}
	disableRepositoryArgs := append([]string{"disable", "owner/repo", "--request-id", "disabled-runtime-removal-fixture"}, requester...)
	if _, err := captureConfigOutput(func() error { return repositoryCommand(disableRepositoryArgs) }); err != nil {
		initialHeartbeat.Stop()
		t.Fatal(err)
	}
	applied := applyAdmissionDisabledDraft(t, requester, baseline.Digest)
	if err := initialHeartbeat.Stop(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil || loaded.Digest != applied.Apply.Generation.Digest || loaded.Automation.LinearTodoAdmission.Enabled {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	lock, err := acquireWorkerProcessLock(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	runtime, err := newAutomaticWorkerRuntime(loaded, "disabled-runtime-fixture")
	if err != nil {
		t.Fatal(err)
	}
	storeOpen := true
	defer func() {
		if storeOpen {
			_ = runtime.store.Close()
		}
	}()
	reporter, err := newWorkerStatusReporter(configPath, "disabled-runtime-fixture", currentBuild.BuildIdentity, loaded.Digest)
	if err != nil {
		t.Fatal(err)
	}
	originalTicker := newWorkerHeartbeatTicker
	ticker := &controllableWorkerHeartbeatTicker{ticks: make(chan time.Time, 1)}
	newWorkerHeartbeatTicker = func(time.Duration) workerHeartbeatTicker {
		return ticker
	}
	defer func() { newWorkerHeartbeatTicker = originalTicker }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result admissionWorkerResult
		err    error
	}, 1)
	go func() {
		result, runErr := runBoundedAdmissionWorkerWithHeartbeatAndMaintenance(ctx, false, time.Minute, automaticWorkerCapacity(loaded.Automation.LinearTodoAdmission, false), runtime.dispatch, runtime.maintenance, reporter, nil)
		done <- struct {
			result admissionWorkerResult
			err    error
		}{result: result, err: runErr}
	}()
	defer cancel()
	first := waitForDisabledWorkerHeartbeat(t, configPath, loaded.Digest, time.Time{})
	ticker.ticks <- time.Now().UTC()
	second := waitForDisabledWorkerHeartbeat(t, configPath, loaded.Digest, first.ObservedAt)
	if second.Cycles != first.Cycles || second.LastCycleOutcome != application.LinearTodoDispatchNoCandidate {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	currentProcessStart, err := processStartIdentity(second.ProcessID)
	if err != nil || currentProcessStart != second.ProcessStartID {
		t.Fatalf("heartbeat process identity=%q current=%q err=%v", second.ProcessStartID, currentProcessStart, err)
	}
	runtimeObservation, err := application.NewConfigurationRuntimeObservationService(workerHeartbeatReader{configPath: configPath, expectedUID: os.Getuid()}, workerProcessIdentityObserver{})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := runtimeObservation.ObserveConfigurationRuntime(context.Background(), time.Now().UTC())
	if err != nil || observed.Liveness != application.RuntimeLivenessFresh {
		t.Fatalf("runtime observation=%+v err=%v", observed, err)
	}
	var statusOutput string
	var status application.ManagedConfigurationStatus
	for attempt := 0; attempt < 4; attempt++ {
		statusOutput, err = captureConfigOutput(func() error { return configCommand(append([]string{"status"}, requester...)) })
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(statusOutput), &status); err != nil {
			t.Fatal(err)
		}
		if status.Convergence.State == application.ConfigurationReady {
			break
		}
		if status.Convergence.State != application.ConfigurationConflict || status.Convergence.Reason != application.ConfigurationReasonRuntimeConflict {
			t.Fatalf("status=%+v output=%s", status, statusOutput)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Convergence.State != application.ConfigurationReady || status.Convergence.DesiredGenerationID != applied.Apply.Generation.GenerationID || status.Convergence.EffectiveGenerationID != applied.Apply.Generation.GenerationID || status.Convergence.LoadedConfigurationDigest != loaded.Digest {
		t.Fatalf("status=%+v output=%s", status, statusOutput)
	}
	openArgs := append([]string{"remove", "open", "owner/repo"}, requester...)
	openOutput, err := captureConfigOutput(func() error { return repositoryCommand(openArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var draft application.RepositoryRemovalDraft
	if err := json.Unmarshal([]byte(openOutput), &draft); err != nil || draft.DraftID == "" {
		t.Fatalf("draft=%+v output=%s err=%v", draft, openOutput, err)
	}
	validateArgs := append([]string{"remove", "validate", "--draft-id", draft.DraftID, "--revision", "1"}, requester...)
	validationOutput, err := captureConfigOutput(func() error { return repositoryCommand(validateArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var validation application.RepositoryRemovalValidation
	if err := json.Unmarshal([]byte(validationOutput), &validation); err != nil || !removalGuardAllowed(validation.Guards, "live_configuration_converged") || !removalGuardAllowed(validation.Guards, "final_repository_admission_disabled") {
		t.Fatalf("validation=%+v output=%s err=%v", validation, validationOutput, err)
	}
	if runs, err := runtime.store.ListNonterminalRuns(context.Background()); err != nil || len(runs) != 0 {
		t.Fatalf("disabled idle runtime created normal admission runs: runs=%+v err=%v", runs, err)
	}
	cancel()
	select {
	case stopped := <-done:
		if stopped.err != nil || stopped.result.Stopped != "canceled" {
			t.Fatalf("stopped=%+v err=%v", stopped.result, stopped.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disabled runtime did not stop within bound")
	}
	if err := closeWorkerStateStore(runtime.store); err != nil {
		t.Fatal(err)
	}
	storeOpen = false
	for _, output := range []string{statusOutput, openOutput, validationOutput} {
		for _, sensitive := range []string{root, databasePath, "secret://", "IFAN_LOOP_LINEAR_TOKEN", "private-key-material"} {
			if strings.Contains(output, sensitive) {
				t.Fatalf("disabled runtime output leaked %q: %s", sensitive, output)
			}
		}
	}
}

func setFixtureAdmissionEnabled(t *testing.T, configPath string) {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	admission := config["automation"].(map[string]any)["linear_todo_admission"].(map[string]any)
	admission["enabled"] = true
	admission["team_id"] = "123e4567-e89b-42d3-a456-426614174100"
	admission["team_key"] = "IFAN"
	admission["todo_state"] = map[string]any{"id": offlineAdmissionTodoState.ID, "name": offlineAdmissionTodoState.Name, "type": offlineAdmissionTodoState.Type}
	admission["in_progress_state"] = map[string]any{"id": offlineAdmissionInProgressState.ID, "name": offlineAdmissionInProgressState.Name, "type": offlineAdmissionInProgressState.Type}
	admission["requester"] = map[string]any{"database_id": 33, "node_id": "MDQ6VXNlcjMz", "login": "ifan0927", "type": "User"}
	admission["notification_mode"] = "local_outbox"
	admission["credential_source_ref"] = "secret://env/IFAN_LOOP_LINEAR_TOKEN"
	rewritten, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
}

func applyAdmissionDisabledDraft(t *testing.T, requester []string, expectedDigest string) application.ConfigurationDraftApplyResult {
	t.Helper()
	openOutput, err := captureConfigOutput(func() error { return configCommand(append([]string{"draft", "open"}, requester...)) })
	if err != nil {
		t.Fatal(err)
	}
	var draft application.ConfigurationDraft
	if err := json.Unmarshal([]byte(openOutput), &draft); err != nil {
		t.Fatal(err)
	}
	setArgs := append([]string{"draft", "set", "--draft-id", draft.DraftID, "--revision", "1", "--automatic-admission-enabled=false"}, requester...)
	setOutput, err := captureConfigOutput(func() error { return configCommand(setArgs) })
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(setOutput), &draft); err != nil || draft.Revision != 2 || draft.Settings.Admission.Enabled {
		t.Fatalf("draft=%+v output=%s err=%v", draft, setOutput, err)
	}
	validateArgs := append([]string{"draft", "validate", "--draft-id", draft.DraftID, "--revision", "2"}, requester...)
	if _, err := captureConfigOutput(func() error { return configCommand(validateArgs) }); err != nil {
		t.Fatal(err)
	}
	previewArgs := append([]string{"draft", "preview", "--draft-id", draft.DraftID, "--revision", "2"}, requester...)
	previewOutput, err := captureConfigOutput(func() error { return configCommand(previewArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var preview application.ConfigurationPreview
	if err := json.Unmarshal([]byte(previewOutput), &preview); err != nil {
		t.Fatal(err)
	}
	applyArgs := append([]string{"draft", "apply", "--draft-id", draft.DraftID, "--revision", "2", "--preview-digest", preview.PreviewDigest, "--expected-generation-id", "1", "--expected-digest", expectedDigest}, requester...)
	applyOutput, err := captureConfigOutput(func() error { return configCommand(applyArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var applied application.ConfigurationDraftApplyResult
	if err := json.Unmarshal([]byte(applyOutput), &applied); err != nil || applied.Convergence.State != application.ConfigurationRestartRequired || applied.Apply.Generation.GenerationID != 2 {
		t.Fatalf("applied=%+v output=%s err=%v", applied, applyOutput, err)
	}
	return applied
}

func waitForDisabledWorkerHeartbeat(t *testing.T, configPath, digest string, after time.Time) workerStatusSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := readWorkerStatusSnapshot(configPath)
		if err == nil && snapshot.ConfigurationDigest == digest && snapshot.Cycles >= 1 && snapshot.LastCycleOutcome == application.LinearTodoDispatchNoCandidate && snapshot.ObservedAt.After(after) {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("disabled worker heartbeat did not become fresh within bound")
	return workerStatusSnapshot{}
}

func removalGuardAllowed(guards []application.RepositoryRemovalGuardResult, name string) bool {
	for _, guard := range guards {
		if guard.Guard == name {
			return guard.Allowed
		}
	}
	return false
}

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

func TestManagedConfigRecoveryCLIIsRetiredBeforeComposition(t *testing.T) {
	root := resolvedTempDir(t)
	configPath := filepath.Join(root, "missing-controller.json")
	err := configCommand([]string{"recover", "restore", "--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "usage: agentctl config") {
		t.Fatalf("retired recovery command err=%v", err)
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("retired command composed configuration: %v", statErr)
	}
}

func TestManagedConfigStatusReportsDriftWithoutRecoveryAuthorityOrMutation(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	if _, err := loadManagedConfiguration(configPath); err != nil {
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
	output, err := captureConfigOutput(func() error {
		return configCommand(append([]string{"status"}, managedDraftRequesterArgs(configPath)...))
	})
	if err != nil {
		t.Fatal(err)
	}
	var status application.ManagedConfigurationStatus
	if err := json.Unmarshal([]byte(output), &status); err != nil || status.Convergence.State != application.ConfigurationConflict || status.Convergence.Reason != application.ConfigurationReasonExternalDrift || status.Convergence.NextAction != application.ConfigurationActionRecoverAuthority {
		t.Fatalf("status=%+v output=%s err=%v", status, output, err)
	}
	if strings.Contains(output, `"recovery"`) || strings.Contains(output, "observed_digest") {
		t.Fatalf("status projected retired recovery authority: %s", output)
	}
	if live, err := os.ReadFile(configPath); err != nil || !bytes.Equal(live, drift) {
		t.Fatalf("drifted live configuration changed: %q err=%v", live, err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var intents, receipts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM configuration_recovery_intents`).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM operation_receipts WHERE operation_type='restore_configuration'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if intents != 0 || receipts != 0 {
		t.Fatalf("retired recovery persistence intents=%d receipts=%d", intents, receipts)
	}
}

func TestManagedConfigDraftCLIRequiresCompleteRequesterForEveryOperation(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"rollback", "sources"}, {"rollback", "open", "--source-generation-id", "1", "--source-digest", strings.Repeat("a", 64), "--expected-generation-id", "2", "--expected-digest", strings.Repeat("b", 64)}, {"draft", "open"}, {"draft", "show", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "set", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1", "--heavy-capacity", "2"}, {"draft", "validate", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "preview", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "apply", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}, {"draft", "discard", "--draft-id", "configuration-draft-00000000000000000000000000000001", "--revision", "1"}} {
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
