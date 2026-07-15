package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func launchAgentInstall(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent install", args)
	if err != nil {
		return err
	}
	result := launchAgentControlResultFor(options, "install", "not_observed", "attention_required", "operator_attention", "", false, false)
	if !validLaunchAgentPath(options.binary) || !validLaunchAgentPath(options.config) || !validLaunchAgentPath(options.plist) {
		result.Reason = "path_invalid"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	reasons := launchAgentReasons(options, false)
	if len(reasons) != 0 {
		result.Reason = reasons[0]
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: reasons[0]})
	}
	desired := []byte(renderLaunchAgentPlist(options.binary, options.config, filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory, launchAgentStdoutLogName), filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory, launchAgentStderrLogName)))
	if launchAgentPathExists(options.plist) {
		if !safeLaunchAgentPlist(options.plist) {
			result.Reason = "plist_unsafe"
			return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
		}
		current, readErr := readLaunchAgentFile(context.Background(), options.plist)
		if readErr == nil && bytes.Equal(current, desired) {
			result.ObservedState = "not_observed"
			result.Outcome = "already_installed"
			result.NextSafeAction = "bootstrap"
			return writeLaunchAgentControlResult(result, nil)
		}
		result.Reason = "plist_exists"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	file, createErr := os.OpenFile(options.plist, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if createErr != nil {
		result.Reason = "plist_unavailable"
		if errors.Is(createErr, os.ErrExist) {
			result.Reason = "plist_exists"
		}
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	createdInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		result.Reason = "plist_unavailable"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	if _, writeErr := file.Write(desired); writeErr != nil {
		_ = file.Close()
		removeLaunchAgentFileIfSame(options.plist, createdInfo)
		result.Reason = "plist_unavailable"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	if closeErr := file.Close(); closeErr != nil {
		removeLaunchAgentFileIfSame(options.plist, createdInfo)
		result.Reason = "plist_unavailable"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	result.ObservedState = "not_observed"
	result.Outcome = "installed"
	result.NextSafeAction = "plist_validate"
	return writeLaunchAgentControlResult(result, nil)
}

func removeLaunchAgentFileIfSame(path string, created os.FileInfo) {
	if created == nil {
		return
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(created, current) {
		return
	}
	_ = os.Remove(path)
}

func launchAgentPlistValidate(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent plist-validate", args)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	inspection, err := validateLaunchAgentPlist(ctx, options)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentPlistErrorResult(options, "plist_validate", err), err)
	}
	result := launchAgentControlResultFor(options, "plist_validate", "not_observed", "valid", "bootstrap", "", inspection.RunAtLoad, false)
	return writeLaunchAgentControlResult(result, nil)
}

func launchAgentBootstrap(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent bootstrap", args)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	inspection, err := validateLaunchAgentPlist(ctx, options)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentPlistErrorResult(options, "bootstrap", err), err)
	}
	control := launchAgentControlFactory(options.timeout)
	target := launchAgentTarget(options)
	observed, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootstrap", observed.State, inspection.RunAtLoad, err, "status"), err)
	}
	if observed.State != "absent" {
		return writeLaunchAgentControlResult(launchAgentControlResultFor(options, "bootstrap", observed.State, "reused", "status", "service_already_loaded", inspection.RunAtLoad, false), nil)
	}
	if err := control.Bootstrap(ctx, options.domain, options.plist); err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootstrap", "unknown", inspection.RunAtLoad, err, "status"), err)
	}
	after, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootstrap", after.State, inspection.RunAtLoad, err, "status"), err)
	}
	if after.State == "absent" || after.State == "unknown" {
		err := &launchAgentControlError{Code: "bootstrap_not_observed"}
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootstrap", after.State, inspection.RunAtLoad, err, "status"), err)
	}
	return writeLaunchAgentControlResult(launchAgentControlResultFor(options, "bootstrap", after.State, "bootstrapped", "status", "", inspection.RunAtLoad, false), nil)
}

func launchAgentKickstart(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent kickstart", args)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	if !safeLaunchAgentPlist(options.plist) {
		err := errors.New("plist_unsafe")
		return writeLaunchAgentControlResult(launchAgentPlistErrorResult(options, "kickstart", err), err)
	}
	inspection, err := inspectLaunchAgentControlPlist(ctx, options)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentPlistErrorResult(options, "kickstart", err), err)
	}
	control := launchAgentControlFactory(options.timeout)
	target := launchAgentTarget(options)
	observed, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "kickstart", observed.State, inspection.RunAtLoad, err, "status"), err)
	}
	if observed.State == "absent" {
		result := launchAgentControlResultFor(options, "kickstart", observed.State, "not_loaded", "bootstrap", "service_absent", inspection.RunAtLoad, false)
		return writeLaunchAgentControlResult(result, nil)
	}
	if observed.State == "running" {
		result := launchAgentControlResultFor(options, "kickstart", observed.State, "already_running", "status", "", inspection.RunAtLoad, false)
		return writeLaunchAgentControlResult(result, nil)
	}
	if inspection.RunAtLoad {
		result := launchAgentControlResultFor(options, "kickstart", observed.State, "awaiting_run_at_load", "status", "run_at_load", inspection.RunAtLoad, false)
		return writeLaunchAgentControlResult(result, nil)
	}
	if err := control.Kickstart(ctx, target); err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "kickstart", "unknown", inspection.RunAtLoad, err, "status"), err)
	}
	after, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "kickstart", after.State, inspection.RunAtLoad, err, "status"), err)
	}
	if after.State != "running" {
		err := &launchAgentControlError{Code: "kickstart_not_observed"}
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "kickstart", after.State, inspection.RunAtLoad, err, "status"), err)
	}
	return writeLaunchAgentControlResult(launchAgentControlResultFor(options, "kickstart", after.State, "kickstarted", "status", "", inspection.RunAtLoad, false), nil)
}

func launchAgentStatus(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent status", args)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	runAtLoad := false
	if launchAgentPathExists(options.plist) {
		inspection, inspectErr := inspectLaunchAgentControlPlist(ctx, options)
		if inspectErr != nil {
			return writeLaunchAgentControlResult(launchAgentPlistErrorResult(options, "status", inspectErr), inspectErr)
		}
		runAtLoad = inspection.RunAtLoad
	}
	control := launchAgentControlFactory(options.timeout)
	observed, err := control.Status(ctx, launchAgentTarget(options))
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "status", observed.State, runAtLoad, err, "status"), err)
	}
	next := "status"
	outcome := "observed"
	if observed.State == "absent" {
		outcome = "absent"
		next = "install"
		if launchAgentPathExists(options.plist) {
			next = "bootstrap"
		}
	}
	return writeLaunchAgentControlResult(launchAgentControlResultFor(options, "status", observed.State, outcome, next, "", runAtLoad, false), nil)
}

func launchAgentBootout(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent bootout", args)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	control := launchAgentControlFactory(options.timeout)
	target := launchAgentTarget(options)
	observed, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootout", observed.State, false, err, "status"), err)
	}
	if observed.State == "absent" {
		return writeLaunchAgentControlResult(launchAgentControlResultFor(options, "bootout", observed.State, "already_stopped", "status", "service_absent", false, false), nil)
	}
	if err := control.Bootout(ctx, target); err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootout", "unknown", false, err, "status"), err)
	}
	after, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootout", after.State, false, err, "status"), err)
	}
	if after.State != "absent" {
		err := &launchAgentControlError{Code: "bootout_not_observed"}
		return writeLaunchAgentControlResult(launchAgentControlErrorResult(options, "bootout", after.State, false, err, "status"), err)
	}
	return writeLaunchAgentControlResult(launchAgentControlResultFor(options, "bootout", after.State, "stopped", "status", "", false, false), nil)
}

func safeLaunchAgentPlist(path string) bool {
	if !validLaunchAgentPath(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByCurrentUser(info) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func launchAgentPlistErrorResult(options launchAgentOptions, step string, err error) launchAgentControlResult {
	reason := "plist_invalid"
	if err != nil {
		switch err.Error() {
		case "plist_unavailable":
			reason = "plist_unavailable"
		case "plist_mismatch":
			reason = "plist_mismatch"
		case "plist_unsafe":
			reason = "plist_unsafe"
		case "path_invalid":
			reason = "path_invalid"
		case "control_timeout":
			reason = "control_timeout"
		}
	}
	return launchAgentControlResultFor(options, step, "unknown", "attention_required", "operator_attention", reason, false, reason == "control_timeout")
}

func launchAgentControlErrorResult(options launchAgentOptions, step, state string, runAtLoad bool, err error, next string) launchAgentControlResult {
	reason, timedOut := launchAgentControlErrorCode(err)
	return launchAgentControlResultFor(options, step, state, "attention_required", next, reason, runAtLoad, timedOut)
}
