package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
)

type migrationLaunchdControl struct {
	states  map[string]launchAgentObservation
	actions []string
}

func TestLaunchdMigrationClassifiesEveryOperatorTopology(t *testing.T) {
	for _, test := range []struct {
		name   string
		result launchdMigrationResult
		phase  string
		next   string
	}{
		{name: "neither", result: launchdMigrationResult{NeutralState: "absent", LegacyState: "absent", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "neither_installed", next: "install"},
		{name: "legacy stopped", result: launchdMigrationResult{LegacyInstalled: true, NeutralState: "absent", LegacyState: "absent", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "legacy_installed_stopped", next: "migrate"},
		{name: "legacy running", result: launchdMigrationResult{LegacyInstalled: true, NeutralState: "absent", LegacyState: "running", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "legacy_running", next: "migrate"},
		{name: "neutral stopped", result: launchdMigrationResult{NeutralInstalled: true, NeutralState: "stopped", LegacyState: "absent", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "neutral_only", next: "status"},
		{name: "neutral running", result: launchdMigrationResult{NeutralInstalled: true, NeutralState: "running", LegacyState: "absent", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "neutral_running", next: "migration-status"},
		{name: "both configured", result: launchdMigrationResult{NeutralInstalled: true, LegacyInstalled: true, NeutralState: "absent", LegacyState: "absent", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "both_configured", next: "operator_attention"},
		{name: "interrupted", result: launchdMigrationResult{RollbackAvailable: true, NeutralState: "absent", LegacyState: "absent", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "interrupted_migration", next: "migrate"},
		{name: "rolled back", result: launchdMigrationResult{LegacyInstalled: true, NeutralDisabled: true, NeutralState: "absent", LegacyState: "running", OppositeNeutralState: "absent", OppositeLegacyState: "absent"}, phase: "rolled_back", next: "migration-status"},
		{name: "opposite", result: launchdMigrationResult{NeutralState: "absent", LegacyState: "absent", OppositeNeutralState: "running", OppositeLegacyState: "absent"}, phase: "opposite_supervisor_conflict", next: "operator_attention"},
	} {
		t.Run(test.name, func(t *testing.T) {
			phase, next := classifyLaunchdMigration(test.result)
			if phase != test.phase || next != test.next {
				t.Fatalf("phase=%q next=%q want=%q/%q", phase, next, test.phase, test.next)
			}
		})
	}
}

func (c *migrationLaunchdControl) Status(_ context.Context, target string) (launchAgentObservation, error) {
	c.actions = append(c.actions, "status "+target)
	if observed, ok := c.states[target]; ok {
		return observed, nil
	}
	return launchAgentObservation{State: "absent"}, nil
}

func (c *migrationLaunchdControl) Bootstrap(ctx context.Context, domain, plist string) error {
	inspection, err := inspectLaunchAgentPlist(ctx, plist)
	if err != nil {
		return err
	}
	c.actions = append(c.actions, "bootstrap "+domain+"/"+inspection.Label)
	c.states[domain+"/"+inspection.Label] = launchAgentObservation{State: "running", ProcessID: os.Getpid()}
	return nil
}

func (c *migrationLaunchdControl) Kickstart(_ context.Context, target string) error {
	c.actions = append(c.actions, "kickstart "+target)
	c.states[target] = launchAgentObservation{State: "running", ProcessID: os.Getpid()}
	return nil
}

func (c *migrationLaunchdControl) Bootout(_ context.Context, target string) error {
	c.actions = append(c.actions, "bootout "+target)
	c.states[target] = launchAgentObservation{State: "absent"}
	return nil
}

func TestLaunchAgentIdentityMigrationAndRollbackAreRestartSafe(t *testing.T) {
	spec, control, workflowBefore := launchAgentIdentityMigrationFixture(t)
	output, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) })
	if err != nil || !strings.Contains(output, `"outcome": "migrated"`) {
		t.Fatalf("migration output=%s err=%v actions=%v", output, err, control.actions)
	}
	assertMigrationFiles(t, spec, false, true, true, false)
	assertWorkflowStateUnchanged(t, spec.config, workflowBefore)
	if control.states[spec.domain+"/"+legacyLaunchdLabel].State != "absent" || control.states[spec.domain+"/"+launchAgentLabel].State != "running" {
		t.Fatalf("states=%+v", control.states)
	}

	control.actions = nil
	output, err = captureConfigOutput(func() error { return rollbackLaunchdMigration(context.Background(), spec) })
	if err != nil || !strings.Contains(output, `"outcome": "rolled_back"`) {
		t.Fatalf("rollback output=%s err=%v actions=%v", output, err, control.actions)
	}
	assertMigrationFiles(t, spec, true, false, false, true)
	assertWorkflowStateUnchanged(t, spec.config, workflowBefore)
	joined := strings.Join(control.actions, "\n")
	if strings.Index(joined, "bootout "+spec.domain+"/"+launchAgentLabel) > strings.Index(joined, "bootstrap "+spec.domain+"/"+legacyLaunchdLabel) {
		t.Fatalf("legacy bootstrap preceded neutral absence proof actions=%v", control.actions)
	}
	output, err = captureConfigOutput(func() error { return rollbackLaunchdMigration(context.Background(), spec) })
	if err != nil || !strings.Contains(output, `"outcome": "already_rolled_back"`) {
		t.Fatalf("idempotent rollback output=%s err=%v", output, err)
	}
}

func TestLaunchdMigrationResumesInterruptedArtifactState(t *testing.T) {
	spec, control, workflowBefore := launchAgentIdentityMigrationFixture(t)
	control.states[spec.domain+"/"+legacyLaunchdLabel] = launchAgentObservation{State: "absent"}
	if err := moveMigrationPlist(spec.legacyPlist, spec.legacyBackup, spec.plistUID); err != nil {
		t.Fatal(err)
	}
	status, err := inspectLaunchdMigration(context.Background(), spec)
	if err != nil || status.Phase != "interrupted_migration" || status.NextSafeAction != "migrate" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	output, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) })
	if err != nil || !strings.Contains(output, `"outcome": "migrated"`) {
		t.Fatalf("output=%s err=%v actions=%v", output, err, control.actions)
	}
	assertMigrationFiles(t, spec, false, true, true, false)
	assertWorkflowStateUnchanged(t, spec.config, workflowBefore)
}

func TestLaunchdMigrationRecoversInterruptedExclusiveFileMoves(t *testing.T) {
	spec, control, _ := launchAgentIdentityMigrationFixture(t)
	control.states[spec.domain+"/"+legacyLaunchdLabel] = launchAgentObservation{State: "absent"}
	if err := os.Link(spec.legacyPlist, spec.legacyBackup); err != nil {
		t.Fatal(err)
	}
	status, err := inspectLaunchdMigration(context.Background(), spec)
	if err != nil || status.Phase != "interrupted_migration" {
		t.Fatalf("legacy link interruption status=%+v err=%v", status, err)
	}
	if _, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) }); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(spec.neutralPlist, spec.neutralDisabled); err != nil {
		t.Fatal(err)
	}
	status, err = inspectLaunchdMigration(context.Background(), spec)
	if err != nil || status.Phase != "rollback_interrupted" || status.NextSafeAction != "rollback" {
		t.Fatalf("neutral link interruption status=%+v err=%v", status, err)
	}
	if _, err := captureConfigOutput(func() error { return rollbackLaunchdMigration(context.Background(), spec) }); err != nil {
		t.Fatal(err)
	}
	assertMigrationFiles(t, spec, true, false, false, true)
}

func TestLaunchdMigrationFailsClosedForDualAndOppositeSupervisors(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(launchdMigrationSpec, *migrationLaunchdControl)
		reason string
	}{
		{
			name: "both configured",
			mutate: func(spec launchdMigrationSpec, _ *migrationLaunchdControl) {
				if err := writeMigrationPlist(spec.neutralPlist, spec.renderNeutral(), spec.plistUID); err != nil {
					t.Fatal(err)
				}
			},
			reason: "dual_configuration",
		},
		{
			name: "opposite loaded",
			mutate: func(spec launchdMigrationSpec, control *migrationLaunchdControl) {
				control.states[spec.oppositeDomain+"/"+launchAgentLabel] = launchAgentObservation{State: "running"}
			},
			reason: "opposite_supervisor_conflict",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, control, _ := launchAgentIdentityMigrationFixture(t)
			test.mutate(spec, control)
			output, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) })
			if err == nil || !strings.Contains(output, `"reason": "`+test.reason+`"`) {
				t.Fatalf("output=%s err=%v", output, err)
			}
			for _, action := range control.actions {
				if strings.HasPrefix(action, "bootout ") || strings.HasPrefix(action, "bootstrap ") {
					t.Fatalf("unsafe mutation action=%q all=%v", action, control.actions)
				}
			}
		})
	}
}

func TestLaunchdMigrationRejectsUnsafeLegacyOwnershipAndActiveWorker(t *testing.T) {
	t.Run("neutral worker assets", func(t *testing.T) {
		spec, control, _ := launchAgentIdentityMigrationFixture(t)
		if err := os.RemoveAll(filepath.Join(filepath.Dir(spec.config), launchAgentLogDirectory)); err != nil {
			t.Fatal(err)
		}
		output, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) })
		if err == nil || !strings.Contains(output, `"reason": "worker_assets_unsafe"`) {
			t.Fatalf("output=%s err=%v", output, err)
		}
		for _, action := range control.actions {
			if strings.HasPrefix(action, "bootout ") {
				t.Fatalf("legacy service was stopped before neutral asset validation actions=%v", control.actions)
			}
		}
	})

	t.Run("plist permission", func(t *testing.T) {
		spec, _, _ := launchAgentIdentityMigrationFixture(t)
		if err := os.Chmod(spec.legacyPlist, 0o644); err != nil {
			t.Fatal(err)
		}
		output, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) })
		if err == nil || !strings.Contains(output, `"reason": "legacy_plist_invalid"`) {
			t.Fatalf("output=%s err=%v", output, err)
		}
	})

	t.Run("worker lock", func(t *testing.T) {
		spec, control, _ := launchAgentIdentityMigrationFixture(t)
		control.states[spec.domain+"/"+legacyLaunchdLabel] = launchAgentObservation{State: "absent"}
		lock, err := claimMigrationWorkerLock(spec.config, spec.workerUID)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseMigrationWorkerLock(lock)
		output, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) })
		if err == nil || !strings.Contains(output, `"reason": "worker_absence_unverified"`) {
			t.Fatalf("output=%s err=%v", output, err)
		}
		if launchAgentPathExists(spec.neutralPlist) || launchAgentPathExists(spec.legacyBackup) {
			t.Fatal("migration mutated plists without authenticated worker absence")
		}
	})
}

func TestLaunchdRollbackRequiresMigrationEvidence(t *testing.T) {
	spec, _, _ := launchAgentIdentityMigrationFixture(t)
	output, err := captureConfigOutput(func() error { return rollbackLaunchdMigration(context.Background(), spec) })
	if err == nil || !strings.Contains(output, `"reason": "rollback_unavailable"`) {
		t.Fatalf("output=%s err=%v", output, err)
	}
}

func TestLaunchdRollbackRejectsDualConfiguredTopology(t *testing.T) {
	spec, control, _ := launchAgentIdentityMigrationFixture(t)
	if err := os.Link(spec.legacyPlist, spec.legacyBackup); err != nil {
		t.Fatal(err)
	}
	if err := writeMigrationPlist(spec.neutralPlist, spec.renderNeutral(), spec.plistUID); err != nil {
		t.Fatal(err)
	}
	output, err := captureConfigOutput(func() error { return rollbackLaunchdMigration(context.Background(), spec) })
	if err == nil || !strings.Contains(output, `"reason": "dual_configuration"`) {
		t.Fatalf("output=%s err=%v", output, err)
	}
	for _, action := range control.actions {
		if strings.HasPrefix(action, "bootout ") || strings.HasPrefix(action, "bootstrap ") || strings.HasPrefix(action, "kickstart ") {
			t.Fatalf("rollback mutated a dual-configured supervisor actions=%v", control.actions)
		}
	}
	assertMigrationFiles(t, spec, true, true, true, false)
}

func TestNeutralLaunchAgentInstallAndBootstrapRejectLegacySupervisor(t *testing.T) {
	spec, _, _ := launchAgentIdentityMigrationFixture(t)
	args := []string{"--binary", spec.binary, "--legacy-binary", spec.legacyBinary, "--config", spec.config, "--plist", spec.neutralPlist, "--domain", spec.domain, "--timeout", "1s"}
	output, err := captureConfigOutput(func() error { return launchAgentInstall(args) })
	if err == nil || !strings.Contains(output, `"reason": "legacy_service_conflict"`) {
		t.Fatalf("install output=%s err=%v", output, err)
	}

	if err := os.Remove(spec.legacyPlist); err != nil {
		t.Fatal(err)
	}
	control := &scriptedLaunchAgentControl{statuses: []launchAgentObservation{{State: "absent"}}, statusByTarget: map[string][]launchAgentObservation{
		spec.domain + "/" + legacyLaunchdLabel: {{State: "running", ProcessID: os.Getpid()}},
	}}
	originalFactory := launchAgentControlFactory
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return control }
	t.Cleanup(func() { launchAgentControlFactory = originalFactory })
	output, err = captureConfigOutput(func() error { return launchAgentInstall(args) })
	if err == nil || !strings.Contains(output, `"reason": "legacy_service_conflict"`) || launchAgentPathExists(spec.neutralPlist) {
		t.Fatalf("loaded-only install output=%s err=%v targets=%v", output, err, control.targetCalls)
	}

	if err := writeMigrationPlist(spec.neutralPlist, spec.renderNeutral(), spec.plistUID); err != nil {
		t.Fatal(err)
	}
	control.statuses = []launchAgentObservation{{State: "absent"}}
	control.statusByTarget[spec.domain+"/"+legacyLaunchdLabel] = []launchAgentObservation{{State: "running", ProcessID: os.Getpid()}}
	output, err = captureConfigOutput(func() error { return launchAgentBootstrap(args) })
	if err == nil || !strings.Contains(output, `"reason": "legacy_service_conflict"`) {
		t.Fatalf("bootstrap output=%s err=%v targets=%v", output, err, control.targetCalls)
	}
	if strings.Contains(strings.Join(control.calls, ","), "bootstrap") {
		t.Fatalf("neutral bootstrap crossed loaded legacy worker calls=%v", control.calls)
	}
}

func TestLaunchDaemonIdentityMigrationUsesNeutralRootOwnedPlist(t *testing.T) {
	options, _ := launchDaemonTestOptions(t)
	root := filepath.Dir(options.config)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config, db := writeControllerStatusConfig(t, root)
	options.config = config
	options.legacyBinary = filepath.Join(root, "ifan-loop")
	if err := os.WriteFile(options.legacyBinary, []byte("legacy"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyOptions := options
	legacyOptions.binary = options.legacyBinary
	legacy := strings.Replace(renderLaunchDaemonPlist(legacyOptions), launchAgentLabel, legacyLaunchdLabel, 1)
	if err := os.WriteFile(legacyLaunchDaemonPlistPath(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("durable-controller-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := launchDaemonMigrationSpec(options)
	control := &migrationLaunchdControl{states: map[string]launchAgentObservation{spec.domain + "/" + legacyLaunchdLabel: {State: "running", ProcessID: os.Getpid()}}}
	spec.control = control
	output, err := captureConfigOutput(func() error { return migrateLaunchdIdentity(context.Background(), spec) })
	if err != nil || !strings.Contains(output, `"supervisor": "launchdaemon"`) || !strings.Contains(output, `"outcome": "migrated"`) {
		t.Fatalf("output=%s err=%v actions=%v", output, err, control.actions)
	}
	info, err := os.Lstat(spec.neutralPlist)
	if err != nil || !ownedByUID(info, launchDaemonRootUID) || info.Mode().Perm() != 0o600 {
		t.Fatalf("neutral plist info=%v err=%v", info, err)
	}
	assertWorkflowStateUnchanged(t, spec.config, []byte("durable-controller-state"))
}

func launchAgentIdentityMigrationFixture(t *testing.T) (launchdMigrationSpec, *migrationLaunchdControl, []byte) {
	t.Helper()
	root := resolvedTempDir(t)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	isolateLaunchDaemonDirectory(t, root)
	if err := os.MkdirAll(launchDaemonDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	config, db := writeControllerStatusConfig(t, root)
	workflow := []byte("durable-controller-state")
	if err := os.WriteFile(db, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	neutralBinary := filepath.Join(root, "agentctl")
	legacyBinary := filepath.Join(root, "ifan-loop")
	for _, binary := range []string{neutralBinary, legacyBinary} {
		if err := os.WriteFile(binary, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	plistDirectory := filepath.Join(root, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	neutralPlist := filepath.Join(plistDirectory, launchAgentLabel+".plist")
	legacyPlist := filepath.Join(plistDirectory, legacyLaunchdLabel+".plist")
	logs := filepath.Join(root, launchAgentLogDirectory)
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := renderLaunchAgentPlist(legacyBinary, config, filepath.Join(logs, launchAgentStdoutLogName), filepath.Join(logs, launchAgentStderrLogName))
	legacy = strings.Replace(legacy, launchAgentLabel, legacyLaunchdLabel, 1)
	if err := os.WriteFile(legacyPlist, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	options := launchAgentOptions{binary: neutralBinary, legacyBinary: legacyBinary, config: config, plist: neutralPlist, domain: "gui/501", timeout: time.Second}
	spec, err := launchAgentMigrationSpec(options)
	if err != nil {
		t.Fatal(err)
	}
	control := &migrationLaunchdControl{states: map[string]launchAgentObservation{spec.domain + "/" + legacyLaunchdLabel: {State: "running", ProcessID: os.Getpid()}}}
	spec.control = control
	return spec, control, workflow
}

func assertMigrationFiles(t *testing.T, spec launchdMigrationSpec, legacy, neutral, backup, disabled bool) {
	t.Helper()
	for path, want := range map[string]bool{spec.legacyPlist: legacy, spec.neutralPlist: neutral, spec.legacyBackup: backup, spec.neutralDisabled: disabled} {
		if got := launchAgentPathExists(path); got != want {
			t.Fatalf("path=%s exists=%t want=%t", filepath.Base(path), got, want)
		}
	}
}

func assertWorkflowStateUnchanged(t *testing.T, config string, before []byte) {
	t.Helper()
	loaded, err := bootstrap.Load(config)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(loaded.Controller.DatabasePath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("workflow state changed after=%q err=%v", after, err)
	}
}
