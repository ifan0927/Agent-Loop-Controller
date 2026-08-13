package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	"golang.org/x/sys/unix"
)

type launchdMigrationSpec struct {
	supervisor      string
	domain          string
	oppositeDomain  string
	neutralPlist    string
	legacyPlist     string
	neutralDisabled string
	legacyBackup    string
	oppositeNeutral string
	oppositeLegacy  string
	binary          string
	legacyBinary    string
	config          string
	plistUID        int
	workerUID       int
	timeout         time.Duration
	privileged      bool
	renderNeutral   func() []byte
	assetsSafe      func() bool
	legacyAssetSafe func() bool
	control         launchAgentControl
}

type launchdMigrationObservation struct {
	neutral         launchAgentObservation
	legacy          launchAgentObservation
	oppositeNeutral launchAgentObservation
	oppositeLegacy  launchAgentObservation
}

type launchdMigrationResult struct {
	Supervisor               string `json:"supervisor"`
	Phase                    string `json:"phase"`
	Outcome                  string `json:"outcome"`
	NextSafeAction           string `json:"next_safe_action"`
	Reason                   string `json:"reason,omitempty"`
	NeutralLabel             string `json:"neutral_label"`
	LegacyLabel              string `json:"legacy_label"`
	NeutralInstalled         bool   `json:"neutral_installed"`
	LegacyInstalled          bool   `json:"legacy_installed"`
	RollbackAvailable        bool   `json:"rollback_available"`
	NeutralDisabled          bool   `json:"neutral_disabled"`
	NeutralState             string `json:"neutral_state"`
	LegacyState              string `json:"legacy_state"`
	OppositeNeutralState     string `json:"opposite_neutral_state"`
	OppositeLegacyState      string `json:"opposite_legacy_state"`
	OppositeNeutralInstalled bool   `json:"opposite_neutral_installed"`
	OppositeLegacyInstalled  bool   `json:"opposite_legacy_installed"`
}

func launchAgentMigrationSpec(options launchAgentOptions) (launchdMigrationSpec, error) {
	legacyPlist, err := legacyLaunchAgentPath(options.plist)
	if err != nil {
		return launchdMigrationSpec{}, err
	}
	return launchdMigrationSpec{
		supervisor:      "launchagent",
		domain:          options.domain,
		oppositeDomain:  "system",
		neutralPlist:    options.plist,
		legacyPlist:     legacyPlist,
		neutralDisabled: options.plist + launchdDisabledSuffix,
		legacyBackup:    legacyPlist + launchdRollbackSuffix,
		oppositeNeutral: launchDaemonPlistPath(),
		oppositeLegacy:  legacyLaunchDaemonPlistPath(),
		binary:          options.binary,
		legacyBinary:    options.legacyBinary,
		config:          options.config,
		plistUID:        os.Getuid(),
		workerUID:       os.Getuid(),
		timeout:         options.timeout,
		renderNeutral: func() []byte {
			logs := filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory)
			return []byte(renderLaunchAgentPlist(options.binary, options.config, filepath.Join(logs, launchAgentStdoutLogName), filepath.Join(logs, launchAgentStderrLogName)))
		},
		assetsSafe: func() bool {
			return len(launchAgentWorkerAssetReasons(options)) == 0
		},
		legacyAssetSafe: func() bool { return safeExecutable(options.legacyBinary) },
		control:         launchAgentControlFactory(options.timeout),
	}, nil
}

func launchDaemonMigrationSpec(options launchDaemonOptions) launchdMigrationSpec {
	return launchdMigrationSpec{
		supervisor:      "launchdaemon",
		domain:          "system",
		oppositeDomain:  fmt.Sprintf("gui/%d", options.uid),
		neutralPlist:    options.plist,
		legacyPlist:     legacyLaunchDaemonPlistPath(),
		neutralDisabled: options.plist + launchdDisabledSuffix,
		legacyBackup:    legacyLaunchDaemonPlistPath() + launchdRollbackSuffix,
		oppositeNeutral: filepath.Join(options.home, "Library", "LaunchAgents", launchAgentLabel+".plist"),
		oppositeLegacy:  filepath.Join(options.home, "Library", "LaunchAgents", legacyLaunchdLabel+".plist"),
		binary:          options.binary,
		legacyBinary:    options.legacyBinary,
		config:          options.config,
		plistUID:        launchDaemonRootUID,
		workerUID:       options.uid,
		timeout:         options.timeout,
		privileged:      true,
		renderNeutral:   func() []byte { return []byte(renderLaunchDaemonPlist(options)) },
		assetsSafe:      func() bool { return len(launchDaemonAssetReasons(options)) == 0 },
		legacyAssetSafe: func() bool { return safeExecutableForUID(options.legacyBinary, options.uid) },
		control:         launchAgentControlFactory(options.timeout),
	}
}

func launchAgentMigrationStatus(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent migration-status", args)
	if err != nil {
		return err
	}
	spec, err := launchAgentMigrationSpec(options)
	if err != nil {
		return err
	}
	return printLaunchdMigrationStatus(spec)
}

func launchAgentMigrate(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent migrate", args)
	if err != nil {
		return err
	}
	spec, err := launchAgentMigrationSpec(options)
	if err != nil {
		return err
	}
	return executeLaunchdMigration(spec, false)
}

func launchAgentRollback(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent rollback", args)
	if err != nil {
		return err
	}
	spec, err := launchAgentMigrationSpec(options)
	if err != nil {
		return err
	}
	return executeLaunchdMigration(spec, true)
}

func launchDaemonMigrationStatus(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon migration-status", args)
	if err != nil {
		return err
	}
	return printLaunchdMigrationStatus(launchDaemonMigrationSpec(options))
}

func launchDaemonMigrate(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon migrate", args)
	if err != nil {
		return err
	}
	return executeLaunchdMigration(launchDaemonMigrationSpec(options), false)
}

func launchDaemonRollback(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon rollback", args)
	if err != nil {
		return err
	}
	return executeLaunchdMigration(launchDaemonMigrationSpec(options), true)
}

func printLaunchdMigrationStatus(spec launchdMigrationSpec) error {
	ctx, cancel := localContext(spec.timeout)
	defer cancel()
	result, err := inspectLaunchdMigration(ctx, spec)
	if err != nil {
		result.Outcome = "attention_required"
		result.NextSafeAction = "migration-status"
		result.Reason = "supervisor_state_unverified"
		if printErr := printJSON(result); printErr != nil {
			return printErr
		}
		return &launchAgentControlError{Code: result.Reason}
	}
	return printJSON(result)
}

func executeLaunchdMigration(spec launchdMigrationSpec, rollback bool) error {
	if spec.privileged && launchDaemonEffectiveUID() != launchDaemonRootUID {
		return writeLaunchdMigrationFailure(spec, "privilege_required")
	}
	ctx, cancel := localContext(spec.timeout)
	defer cancel()
	if rollback {
		return rollbackLaunchdMigration(ctx, spec)
	}
	return migrateLaunchdIdentity(ctx, spec)
}

func migrateLaunchdIdentity(ctx context.Context, spec launchdMigrationSpec) error {
	result, err := inspectLaunchdMigration(ctx, spec)
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "supervisor_state_unverified")
	}
	if launchdOppositeConflict(result) {
		return writeLaunchdMigrationFailure(spec, "opposite_supervisor_conflict")
	}
	if result.Phase == "both_configured" {
		return writeLaunchdMigrationFailure(spec, "dual_configuration")
	}
	if !spec.assetsSafe() || !spec.legacyAssetSafe() {
		return writeLaunchdMigrationFailure(spec, "worker_assets_unsafe")
	}
	legacySource := spec.legacyPlist
	if !result.LegacyInstalled {
		legacySource = spec.legacyBackup
	}
	if !launchAgentPathExists(legacySource) {
		return writeLaunchdMigrationFailure(spec, "legacy_plist_unavailable")
	}
	if err := validateMigrationPlist(ctx, legacySource, legacyLaunchdLabel, spec.legacyBinary, spec.config, spec.plistUID, nil); err != nil {
		return writeLaunchdMigrationFailure(spec, "legacy_plist_invalid")
	}
	if result.Phase == "neutral_running" && result.RollbackAvailable && !result.LegacyInstalled {
		if !result.NeutralInstalled {
			return writeLaunchdMigrationFailure(spec, "neutral_plist_unavailable")
		}
		if err := validateMigrationPlist(ctx, spec.neutralPlist, launchAgentLabel, spec.binary, spec.config, spec.plistUID, spec.renderNeutral()); err != nil {
			return writeLaunchdMigrationFailure(spec, "neutral_plist_invalid")
		}
		result.Outcome, result.NextSafeAction = "already_migrated", "migration-status"
		return printJSON(result)
	}
	if result.NeutralState != "absent" && result.LegacyInstalled {
		return writeLaunchdMigrationFailure(spec, "dual_service_state")
	}
	neutralAlreadyLoaded := result.NeutralState != "absent"
	if result.LegacyState != "absent" {
		if err := spec.control.Bootout(ctx, spec.domain+"/"+legacyLaunchdLabel); err != nil {
			return writeLaunchdMigrationFailure(spec, "legacy_bootout_failed")
		}
		if _, err := observeLaunchAgentAbsence(ctx, spec.control, spec.domain+"/"+legacyLaunchdLabel); err != nil {
			return writeLaunchdMigrationFailure(spec, "legacy_absence_unverified")
		}
	}
	lock, err := claimMigrationWorkerLock(spec.config, spec.workerUID)
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "worker_absence_unverified")
	}
	locked := true
	defer func() {
		if locked {
			_ = releaseMigrationWorkerLock(lock)
		}
	}()
	if launchAgentPathExists(spec.legacyPlist) {
		if err := moveMigrationPlist(spec.legacyPlist, spec.legacyBackup, spec.plistUID); err != nil {
			return writeLaunchdMigrationFailure(spec, "legacy_backup_failed")
		}
	}
	if launchAgentPathExists(spec.neutralDisabled) {
		if err := validateMigrationPlist(ctx, spec.neutralDisabled, launchAgentLabel, spec.binary, spec.config, spec.plistUID, spec.renderNeutral()); err != nil {
			return writeLaunchdMigrationFailure(spec, "neutral_plist_invalid")
		}
		if err := moveMigrationPlist(spec.neutralDisabled, spec.neutralPlist, spec.plistUID); err != nil {
			return writeLaunchdMigrationFailure(spec, "neutral_restore_failed")
		}
	}
	if !launchAgentPathExists(spec.neutralPlist) {
		if err := writeMigrationPlist(spec.neutralPlist, spec.renderNeutral(), spec.plistUID); err != nil {
			return writeLaunchdMigrationFailure(spec, "neutral_install_failed")
		}
	}
	if err := validateMigrationPlist(ctx, spec.neutralPlist, launchAgentLabel, spec.binary, spec.config, spec.plistUID, spec.renderNeutral()); err != nil {
		return writeLaunchdMigrationFailure(spec, "neutral_plist_invalid")
	}
	if err := releaseMigrationWorkerLock(lock); err != nil {
		return writeLaunchdMigrationFailure(spec, "worker_fence_release_failed")
	}
	locked = false
	if neutralAlreadyLoaded {
		err = spec.control.Kickstart(ctx, spec.domain+"/"+launchAgentLabel)
	} else {
		err = spec.control.Bootstrap(ctx, spec.domain, spec.neutralPlist)
	}
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "neutral_bootstrap_failed")
	}
	if _, err := observeLaunchdMigrationRunning(ctx, spec.control, spec.domain+"/"+launchAgentLabel); err != nil {
		return writeLaunchdMigrationFailure(spec, "neutral_bootstrap_unverified")
	}
	result, err = inspectLaunchdMigration(ctx, spec)
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "supervisor_state_unverified")
	}
	result.Outcome, result.NextSafeAction = "migrated", "migration-status"
	return printJSON(result)
}

func rollbackLaunchdMigration(ctx context.Context, spec launchdMigrationSpec) error {
	result, err := inspectLaunchdMigration(ctx, spec)
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "supervisor_state_unverified")
	}
	if launchdOppositeConflict(result) {
		return writeLaunchdMigrationFailure(spec, "opposite_supervisor_conflict")
	}
	if result.Phase == "both_configured" {
		return writeLaunchdMigrationFailure(spec, "dual_configuration")
	}
	if !result.RollbackAvailable && !result.NeutralDisabled {
		return writeLaunchdMigrationFailure(spec, "rollback_unavailable")
	}
	if !spec.assetsSafe() || !spec.legacyAssetSafe() {
		return writeLaunchdMigrationFailure(spec, "worker_assets_unsafe")
	}
	if result.LegacyState == "running" && result.NeutralState == "absent" && result.LegacyInstalled {
		if err := validateMigrationPlist(ctx, spec.legacyPlist, legacyLaunchdLabel, spec.legacyBinary, spec.config, spec.plistUID, nil); err != nil {
			return writeLaunchdMigrationFailure(spec, "legacy_plist_invalid")
		}
		result.Outcome, result.NextSafeAction = "already_rolled_back", "migration-status"
		return printJSON(result)
	}
	if result.LegacyState != "absent" && result.NeutralState != "absent" {
		return writeLaunchdMigrationFailure(spec, "dual_service_state")
	}
	legacyAlreadyLoaded := result.LegacyState != "absent"
	if result.NeutralState != "absent" {
		if err := spec.control.Bootout(ctx, spec.domain+"/"+launchAgentLabel); err != nil {
			return writeLaunchdMigrationFailure(spec, "neutral_bootout_failed")
		}
		if _, err := observeLaunchAgentAbsence(ctx, spec.control, spec.domain+"/"+launchAgentLabel); err != nil {
			return writeLaunchdMigrationFailure(spec, "neutral_absence_unverified")
		}
	}
	lock, err := claimMigrationWorkerLock(spec.config, spec.workerUID)
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "worker_absence_unverified")
	}
	locked := true
	defer func() {
		if locked {
			_ = releaseMigrationWorkerLock(lock)
		}
	}()
	if launchAgentPathExists(spec.neutralPlist) {
		if err := moveMigrationPlist(spec.neutralPlist, spec.neutralDisabled, spec.plistUID); err != nil {
			return writeLaunchdMigrationFailure(spec, "neutral_disable_failed")
		}
	}
	if launchAgentPathExists(spec.legacyBackup) {
		if err := moveMigrationPlist(spec.legacyBackup, spec.legacyPlist, spec.plistUID); err != nil {
			return writeLaunchdMigrationFailure(spec, "legacy_restore_failed")
		}
	}
	if err := validateMigrationPlist(ctx, spec.legacyPlist, legacyLaunchdLabel, spec.legacyBinary, spec.config, spec.plistUID, nil); err != nil {
		return writeLaunchdMigrationFailure(spec, "legacy_plist_invalid")
	}
	if err := releaseMigrationWorkerLock(lock); err != nil {
		return writeLaunchdMigrationFailure(spec, "worker_fence_release_failed")
	}
	locked = false
	if legacyAlreadyLoaded {
		err = spec.control.Kickstart(ctx, spec.domain+"/"+legacyLaunchdLabel)
	} else {
		err = spec.control.Bootstrap(ctx, spec.domain, spec.legacyPlist)
	}
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "legacy_bootstrap_failed")
	}
	if _, err := observeLaunchdMigrationRunning(ctx, spec.control, spec.domain+"/"+legacyLaunchdLabel); err != nil {
		return writeLaunchdMigrationFailure(spec, "legacy_bootstrap_unverified")
	}
	result, err = inspectLaunchdMigration(ctx, spec)
	if err != nil {
		return writeLaunchdMigrationFailure(spec, "supervisor_state_unverified")
	}
	result.Outcome, result.NextSafeAction = "rolled_back", "migration-status"
	return printJSON(result)
}

func inspectLaunchdMigration(ctx context.Context, spec launchdMigrationSpec) (launchdMigrationResult, error) {
	observation, err := observeLaunchdMigration(ctx, spec)
	result := launchdMigrationResult{
		Supervisor:               spec.supervisor,
		Outcome:                  "observed",
		NextSafeAction:           "migration-status",
		NeutralLabel:             launchAgentLabel,
		LegacyLabel:              legacyLaunchdLabel,
		NeutralInstalled:         launchAgentPathExists(spec.neutralPlist),
		LegacyInstalled:          launchAgentPathExists(spec.legacyPlist),
		RollbackAvailable:        launchAgentPathExists(spec.legacyBackup),
		NeutralDisabled:          launchAgentPathExists(spec.neutralDisabled),
		NeutralState:             observation.neutral.State,
		LegacyState:              observation.legacy.State,
		OppositeNeutralState:     observation.oppositeNeutral.State,
		OppositeLegacyState:      observation.oppositeLegacy.State,
		OppositeNeutralInstalled: launchAgentPathExists(spec.oppositeNeutral),
		OppositeLegacyInstalled:  launchAgentPathExists(spec.oppositeLegacy),
	}
	if err != nil {
		result.Phase = "unknown"
		return result, err
	}
	result.Phase, result.NextSafeAction = classifyLaunchdMigration(result)
	return result, nil
}

func observeLaunchdMigration(ctx context.Context, spec launchdMigrationSpec) (launchdMigrationObservation, error) {
	var result launchdMigrationObservation
	checks := []struct {
		target string
		out    *launchAgentObservation
	}{
		{spec.domain + "/" + launchAgentLabel, &result.neutral},
		{spec.domain + "/" + legacyLaunchdLabel, &result.legacy},
		{spec.oppositeDomain + "/" + launchAgentLabel, &result.oppositeNeutral},
		{spec.oppositeDomain + "/" + legacyLaunchdLabel, &result.oppositeLegacy},
	}
	for _, check := range checks {
		observed, err := spec.control.Status(ctx, check.target)
		*check.out = observed
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func observeLaunchdMigrationRunning(ctx context.Context, control launchAgentControl, target string) (launchAgentObservation, error) {
	last := launchAgentObservation{State: "unknown"}
	for {
		observed, err := control.Status(ctx, target)
		if err != nil {
			return observed, err
		}
		last = observed
		if observed.State == "running" {
			return observed, nil
		}
		timer := time.NewTimer(launchAgentObservationInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, &launchAgentControlError{Code: "control_timeout"}
		case <-timer.C:
		}
	}
}

func classifyLaunchdMigration(result launchdMigrationResult) (string, string) {
	if launchdOppositeConflict(result) {
		return "opposite_supervisor_conflict", "operator_attention"
	}
	legacyConfigured := result.LegacyInstalled || result.LegacyState != "absent"
	neutralConfigured := result.NeutralInstalled || result.NeutralState != "absent"
	if legacyConfigured && neutralConfigured {
		return "both_configured", "operator_attention"
	}
	if result.NeutralDisabled && legacyConfigured {
		if result.LegacyState == "running" {
			return "rolled_back", "migration-status"
		}
		return "rollback_interrupted", "rollback"
	}
	if result.NeutralDisabled && result.RollbackAvailable {
		return "rollback_interrupted", "rollback"
	}
	if result.RollbackAvailable && neutralConfigured {
		if result.NeutralState == "running" {
			return "neutral_running", "migration-status"
		}
		return "interrupted_migration", "migrate"
	}
	if result.RollbackAvailable {
		return "interrupted_migration", "migrate"
	}
	if legacyConfigured {
		if result.LegacyState == "running" {
			return "legacy_running", "migrate"
		}
		return "legacy_installed_stopped", "migrate"
	}
	if neutralConfigured {
		if result.NeutralState == "running" {
			return "neutral_running", "migration-status"
		}
		return "neutral_only", "status"
	}
	if result.NeutralDisabled {
		return "stale_neutral_artifact", "operator_attention"
	}
	return "neither_installed", "install"
}

func launchdOppositeConflict(result launchdMigrationResult) bool {
	return result.OppositeNeutralInstalled || result.OppositeLegacyInstalled || result.OppositeNeutralState != "absent" || result.OppositeLegacyState != "absent"
}

func verifyNoLaunchdCompatibilityConflict(ctx context.Context, control launchAgentControl, domain, oppositeDomain, legacyPlist, oppositeNeutralPlist, oppositeLegacyPlist string) error {
	if launchAgentPathExists(legacyPlist) {
		return &launchAgentControlError{Code: "legacy_service_conflict"}
	}
	if launchAgentPathExists(oppositeNeutralPlist) || launchAgentPathExists(oppositeLegacyPlist) {
		return &launchAgentControlError{Code: "opposite_supervisor_conflict"}
	}
	opposite, err := control.Status(ctx, oppositeDomain+"/"+launchAgentLabel)
	if err != nil {
		return &launchAgentControlError{Code: "opposite_supervisor_state_unverified"}
	}
	if opposite.State != "absent" {
		return &launchAgentControlError{Code: "opposite_supervisor_conflict"}
	}
	legacy, err := control.Status(ctx, domain+"/"+legacyLaunchdLabel)
	if err != nil {
		return &launchAgentControlError{Code: "legacy_state_unverified"}
	}
	if legacy.State != "absent" {
		return &launchAgentControlError{Code: "legacy_service_conflict"}
	}
	legacyOpposite, err := control.Status(ctx, oppositeDomain+"/"+legacyLaunchdLabel)
	if err != nil {
		return &launchAgentControlError{Code: "opposite_supervisor_state_unverified"}
	}
	if legacyOpposite.State != "absent" {
		return &launchAgentControlError{Code: "opposite_supervisor_conflict"}
	}
	return nil
}

func validateMigrationPlist(ctx context.Context, path, label, binary, config string, uid int, exact []byte) error {
	data, err := readMigrationPlist(ctx, path, uid)
	if err != nil {
		return err
	}
	if exact != nil && !bytes.Equal(data, exact) {
		return errors.New("plist_mismatch")
	}
	inspection, err := inspectLaunchAgentPlistData(data)
	if err != nil || inspection.Label != label || !inspection.RunAtLoadObserved || !inspection.RunAtLoad || !sameStrings(inspection.ProgramArguments, []string{binary, "controller", "worker", "--config", config}) {
		return errors.New("plist_mismatch")
	}
	return nil
}

func readMigrationPlist(ctx context.Context, path string, uid int) ([]byte, error) {
	if !safeMigrationPlist(path, uid) {
		return nil, errors.New("plist_unsafe")
	}
	parent, err := openLaunchAgentParent(path)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	name := filepath.Base(path)
	file, err := openLaunchAgentFileAt(parent, name)
	if err != nil {
		return nil, err
	}
	identity, err := launchAgentFileIdentityFor(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	data, err := readLaunchAgentOpenedFile(ctx, file)
	if err != nil {
		return nil, err
	}
	if !launchAgentInstallTargetStillBound(path, parent, name, identity) || !safeMigrationPlist(path, uid) {
		return nil, errors.New("plist identity changed during inspection")
	}
	return data, nil
}

func safeMigrationPlist(path string, uid int) bool {
	if !validLaunchAgentPath(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByUID(info, uid) {
		return false
	}
	links := logLinkCount(info)
	if links < 1 || links > 2 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func moveMigrationPlist(source, destination string, uid int) error {
	if !validLaunchAgentPath(source) || !validLaunchAgentPath(destination) || filepath.Dir(source) != filepath.Dir(destination) {
		return errors.New("plist move paths are invalid")
	}
	directory, err := openLaunchAgentParent(source)
	if err != nil {
		return err
	}
	defer directory.Close()
	sourceName, destinationName := filepath.Base(source), filepath.Base(destination)
	sourceIdentity, _, err := migrationPlistIdentityAt(directory, sourceName, uid)
	if err != nil {
		return errors.New("source plist is unsafe")
	}
	destinationIdentity, _, destinationErr := migrationPlistIdentityAt(directory, destinationName, uid)
	if destinationErr == nil {
		if sourceIdentity != destinationIdentity {
			return errors.New("destination plist already exists")
		}
	} else if !errors.Is(destinationErr, unix.ENOENT) {
		return destinationErr
	} else if err := unix.Linkat(int(directory.Fd()), sourceName, int(directory.Fd()), destinationName, 0); err != nil {
		return err
	}
	destinationIdentity, _, err = migrationPlistIdentityAt(directory, destinationName, uid)
	if err != nil || sourceIdentity != destinationIdentity {
		return errors.New("plist move identity is unverified")
	}
	if err := unix.Unlinkat(int(directory.Fd()), sourceName, 0); err != nil {
		return err
	}
	return directory.Sync()
}

func writeMigrationPlist(path string, data []byte, uid int) error {
	if !validLaunchAgentPath(path) || len(data) == 0 || len(data) > maxLaunchAgentPlistBytes {
		return errors.New("plist write is invalid")
	}
	parent, err := openLaunchAgentParent(path)
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(path)
	file, err := createLaunchAgentFileAt(parent, name)
	if err != nil {
		return err
	}
	cleanup := true
	var identity launchAgentFileIdentity
	defer func() {
		_ = file.Close()
		if cleanup {
			removeLaunchAgentFileIfSame(parent, name, identity)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	identity, err = launchAgentFileIdentityFor(file)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByUID(info, uid) || logLinkCount(info) != 1 {
		return errors.New("plist write identity is unsafe")
	}
	if written, err := file.Write(data); err != nil || written != len(data) || file.Sync() != nil || file.Close() != nil {
		return errors.New("plist write is incomplete")
	}
	if !launchAgentInstallTargetStillBound(path, parent, name, identity) || !safeMigrationPlist(path, uid) || parent.Sync() != nil {
		return errors.New("plist write is unverified")
	}
	cleanup = false
	return nil
}

func migrationPlistIdentityAt(directory *os.File, name string, uid int) (launchAgentFileIdentity, uint64, error) {
	if directory == nil || filepath.Base(name) != name || name == "" {
		return launchAgentFileIdentity{}, 0, errors.New("plist entry is invalid")
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return launchAgentFileIdentity{}, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || int(stat.Uid) != uid || stat.Nlink < 1 || stat.Nlink > 2 {
		return launchAgentFileIdentity{}, 0, errors.New("plist entry is unsafe")
	}
	return launchAgentFileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino), uid: stat.Uid, mode: uint32(stat.Mode)}, uint64(stat.Nlink), nil
}

func claimMigrationWorkerLock(configPath string, workerUID int) (*os.File, error) {
	loaded, err := bootstrap.Load(configPath)
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(loaded.Controller.DatabasePath)
	if !safePrivateDirectoryForUID(directory, workerUID) {
		return nil, errors.New("worker lock directory is unsafe")
	}
	path := filepath.Join(directory, "worker.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	created := err == nil
	if os.IsExist(err) {
		file, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			opened, openedErr := file.Stat()
			_ = file.Close()
			if created {
				if current, currentErr := os.Lstat(path); openedErr == nil && currentErr == nil && os.SameFile(opened, current) {
					_ = os.Remove(path)
				}
			}
		}
	}()
	if created && os.Geteuid() != workerUID {
		if err := file.Chown(workerUID, -1); err != nil {
			return nil, err
		}
	}
	if created {
		if err := file.Chmod(0o600); err != nil {
			return nil, err
		}
	}
	opened, err := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || !ownedByUID(opened, workerUID) || logLinkCount(opened) != 1 || !os.SameFile(opened, current) {
		return nil, errors.New("worker lock is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, err
	}
	cleanup = false
	return file, nil
}

func releaseMigrationWorkerLock(file *os.File) error {
	if file == nil {
		return errors.New("worker lock is unavailable")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeLaunchdMigrationFailure(spec launchdMigrationSpec, reason string) error {
	result := launchdMigrationResult{
		Supervisor:               spec.supervisor,
		Phase:                    "attention_required",
		Outcome:                  "attention_required",
		NextSafeAction:           "migration-status",
		Reason:                   reason,
		NeutralLabel:             launchAgentLabel,
		LegacyLabel:              legacyLaunchdLabel,
		NeutralInstalled:         launchAgentPathExists(spec.neutralPlist),
		LegacyInstalled:          launchAgentPathExists(spec.legacyPlist),
		RollbackAvailable:        launchAgentPathExists(spec.legacyBackup),
		NeutralDisabled:          launchAgentPathExists(spec.neutralDisabled),
		NeutralState:             "unknown",
		LegacyState:              "unknown",
		OppositeNeutralState:     "unknown",
		OppositeLegacyState:      "unknown",
		OppositeNeutralInstalled: launchAgentPathExists(spec.oppositeNeutral),
		OppositeLegacyInstalled:  launchAgentPathExists(spec.oppositeLegacy),
	}
	if err := printJSON(result); err != nil {
		return err
	}
	return &launchAgentControlError{Code: reason}
}
