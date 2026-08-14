package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	"golang.org/x/sys/unix"
)

const defaultLaunchDaemonDirectory = "/Library/LaunchDaemons"

//go:embed launchdaemon_worker.plist.tmpl
var launchDaemonTemplate string

var (
	launchDaemonCurrentUser  = user.Current
	launchDaemonLookupUser   = user.Lookup
	launchDaemonEffectiveUID = os.Geteuid
	launchDaemonRootUID      = 0
	launchDaemonAssetReasons = launchDaemonStaticAssetReasons
	launchDaemonDirectory    = defaultLaunchDaemonDirectory
)

type launchDaemonOptions struct {
	binary, legacyBinary, config, plist, username, home, workingDirectory string
	uid                                                                   int
	timeout                                                               time.Duration
}

func controllerLaunchDaemon(args []string) error {
	if len(args) == 0 {
		return launchDaemonUsage()
	}
	switch args[0] {
	case "build", "render":
		return launchDaemonRender(args[1:])
	case "install":
		return launchDaemonInstall(args[1:])
	case "doctor":
		return launchDaemonDoctor(args[1:], false)
	case "validate":
		return launchDaemonDoctor(args[1:], true)
	case "plist-validate":
		return launchDaemonPlistValidate(args[1:])
	case "bootstrap":
		return launchDaemonBootstrap(args[1:])
	case "kickstart":
		return launchDaemonKickstart(args[1:])
	case "status":
		return launchDaemonStatus(args[1:])
	case "bootout":
		return launchDaemonBootout(args[1:])
	case "migration-status":
		return launchDaemonMigrationStatus(args[1:])
	case "migrate":
		return launchDaemonMigrate(args[1:])
	case "rollback":
		return launchDaemonRollback(args[1:])
	default:
		return launchDaemonUsage()
	}
}

func launchDaemonUsage() error {
	return errors.New("usage: agentctl controller launchdaemon <build|render|install|doctor|validate|plist-validate|bootstrap|kickstart|status|bootout|migration-status|migrate|rollback> [options]")
}

func parseLaunchDaemonOptions(name string, args []string) (launchDaemonOptions, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	binary := flags.String("binary", defaultInstalledBinary, "absolute installed controller binary")
	legacyBinary := flags.String("legacy-binary", defaultLegacyBinary, "absolute installed legacy controller binary used only for migration or rollback")
	config := flags.String("config", "", "absolute controller configuration")
	plist := flags.String("plist", launchDaemonPlistPath(), "absolute system LaunchDaemon plist")
	username := flags.String("user", "", "non-root worker account")
	workingDirectory := flags.String("working-directory", "", "absolute worker working directory (default: worker home)")
	timeout := flags.Duration("timeout", defaultLaunchAgentControlTimeout, "maximum duration for one launchctl control step")
	if err := flags.Parse(args); err != nil {
		return launchDaemonOptions{}, err
	}
	if flags.NArg() != 0 {
		return launchDaemonOptions{}, errors.New("launchdaemon command does not accept positional arguments")
	}
	if *timeout <= 0 || *timeout > maxLaunchAgentControlTimeout {
		return launchDaemonOptions{}, errors.New("--timeout must be greater than zero and no more than 2m")
	}
	account, err := resolveLaunchDaemonUser(*username)
	if err != nil {
		return launchDaemonOptions{}, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return launchDaemonOptions{}, errors.New("--user must resolve to a non-root account")
	}
	home := filepath.Clean(account.HomeDir)
	if !validLaunchAgentPath(home) {
		return launchDaemonOptions{}, errors.New("worker home must be absolute and canonical")
	}
	workdir := strings.TrimSpace(*workingDirectory)
	if workdir == "" {
		workdir = home
	}
	configPath := strings.TrimSpace(*config)
	if configPath == "" {
		configPath = filepath.Join(home, "Library", "Application Support", defaultConfigDirectoryName, defaultConfigFileName)
	}
	options := launchDaemonOptions{binary: *binary, legacyBinary: *legacyBinary, config: configPath, plist: *plist, username: account.Username, uid: uid, home: home, workingDirectory: workdir, timeout: *timeout}
	if !validLaunchAgentPath(options.binary) || !validLaunchAgentPath(options.config) || !validLaunchAgentPath(options.plist) || !validLaunchAgentPath(options.workingDirectory) {
		return launchDaemonOptions{}, errors.New("LaunchDaemon paths must be absolute and canonical")
	}
	if options.plist != launchDaemonPlistPath() {
		return launchDaemonOptions{}, errors.New("--plist must be the canonical system LaunchDaemon path")
	}
	return options, nil
}

func launchDaemonPlistPath() string {
	return filepath.Join(launchDaemonDirectory, launchAgentLabel+".plist")
}

func legacyLaunchDaemonPlistPath() string {
	return filepath.Join(launchDaemonDirectory, legacyLaunchdLabel+".plist")
}

func resolveLaunchDaemonUser(override string) (*user.User, error) {
	if strings.TrimSpace(override) == "" {
		if launchDaemonEffectiveUID() == launchDaemonRootUID {
			return nil, errors.New("--user is required for privileged LaunchDaemon commands")
		}
		account, err := launchDaemonCurrentUser()
		if err != nil {
			return nil, errors.New("worker account is unavailable")
		}
		return account, nil
	}
	if strings.TrimSpace(override) != override || strings.ContainsAny(override, "\r\n\x00") {
		return nil, errors.New("--user is invalid")
	}
	account, err := launchDaemonLookupUser(override)
	if err != nil || account == nil || account.Username != override {
		return nil, errors.New("--user is unavailable")
	}
	return account, nil
}

func renderLaunchDaemonPlist(options launchDaemonOptions) string {
	logs := filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory)
	replacer := strings.NewReplacer(
		"{{BINARY_PATH}}", xmlEscape(options.binary),
		"{{CONFIG_PATH}}", xmlEscape(options.config),
		"{{USER_NAME}}", xmlEscape(options.username),
		"{{HOME_PATH}}", xmlEscape(options.home),
		"{{WORKING_DIRECTORY}}", xmlEscape(options.workingDirectory),
		"{{STDOUT_PATH}}", xmlEscape(filepath.Join(logs, launchAgentStdoutLogName)),
		"{{STDERR_PATH}}", xmlEscape(filepath.Join(logs, launchAgentStderrLogName)),
	)
	return replacer.Replace(launchDaemonTemplate)
}

func launchDaemonRender(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon render", args)
	if err != nil {
		return err
	}
	if launchDaemonEffectiveUID() != options.uid {
		return errors.New("LaunchDaemon render must run as the worker user")
	}
	_, err = fmt.Fprint(os.Stdout, renderLaunchDaemonPlist(options))
	return err
}

func launchDaemonDoctor(args []string, installValidation bool) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon doctor", args)
	if err != nil {
		return err
	}
	reasons := launchDaemonReasons(options, installValidation)
	return printJSON(launchAgentDoctorOutput{Ready: len(reasons) == 0, Reasons: reasons, ProcessLifetime: workerProcessLifetime, LogPolicy: workerLogPolicy})
}

func launchDaemonReasons(options launchDaemonOptions, installValidation bool) []string {
	if launchDaemonEffectiveUID() != options.uid {
		return []string{"worker_identity_mismatch"}
	}
	reasons := launchDaemonAssetReasons(options)
	loaded, err := bootstrap.Load(options.config)
	if err == nil && loaded.Automation.LinearTodoAdmission.Enabled && loaded.Automation.LinearTodoAdmission.CredentialSourceRef == linearadapter.FileCredentialSourceRef {
		source, sourceErr := linearCredentialSourceForRef(loaded, loaded.Automation.LinearTodoAdmission.CredentialSourceRef)
		checker, ok := source.(credentialChecker)
		if sourceErr != nil || !ok || checker.Check(context.Background()) != nil {
			reasons = append(reasons, "linear_credential_unavailable")
		}
	}
	if launchAgentPathExists(filepath.Join(options.home, "Library", "LaunchAgents", launchAgentLabel+".plist")) || launchAgentPathExists(filepath.Join(options.home, "Library", "LaunchAgents", legacyLaunchdLabel+".plist")) {
		reasons = append(reasons, "launchagent_conflict")
	}
	if launchAgentPathExists(legacyLaunchDaemonPlistPath()) {
		reasons = append(reasons, "legacy_service_conflict")
	}
	if installValidation && launchAgentPathExists(options.plist) {
		reasons = append(reasons, "plist_exists")
	}
	return reasons
}

func launchDaemonStaticAssetReasons(options launchDaemonOptions) []string {
	reasons := make([]string, 0, 10)
	if !safeExecutableForUID(options.binary, options.uid) {
		reasons = append(reasons, "binary_unsafe")
	}
	if !safePrivateFileForUID(options.config, options.uid) {
		return append(reasons, "config_unsafe")
	}
	loaded, err := bootstrap.Load(options.config)
	if err != nil {
		return append(reasons, "config_unavailable")
	}
	if !safePrivateDirectoryForUID(filepath.Dir(loaded.Controller.DatabasePath), options.uid) {
		reasons = append(reasons, "database_parent_unsafe")
	}
	if !safeOptionalPrivateFileForUID(loaded.Controller.DatabasePath, options.uid) || !safeOptionalPrivateFileForUID(loaded.Controller.DatabasePath+"-wal", options.uid) || !safeOptionalPrivateFileForUID(loaded.Controller.DatabasePath+"-shm", options.uid) {
		reasons = append(reasons, "database_file_unsafe")
	}
	if !safeDirectoryForUID(options.workingDirectory, options.uid) {
		reasons = append(reasons, "working_directory_unsafe")
	}
	logs := filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory)
	if !safePrivateDirectoryForUID(logs, options.uid) {
		reasons = append(reasons, "log_directory_unsafe")
	} else if !safeLogLeafForUID(filepath.Join(logs, launchAgentStdoutLogName), options.uid) || !safeLogLeafForUID(filepath.Join(logs, launchAgentStderrLogName), options.uid) {
		reasons = append(reasons, "log_file_unsafe")
	}
	if !safePrivateFileForUID(filepath.Join(options.home, ".codex", "auth.json"), options.uid) {
		reasons = append(reasons, "codex_auth_unavailable")
	}
	for _, profile := range loaded.GitHubProfiles {
		if !safePrivateFileForUID(profile.Config.PrivateKeyFile, options.uid) {
			reasons = append(reasons, "github_credential_unavailable")
			break
		}
	}
	for _, binding := range loaded.Registry.Bindings() {
		if !safeDirectoryForUID(binding.SourcePath, options.uid) || !safePrivateDirectoryForUID(binding.RunRoot, options.uid) || !safePrivateDirectoryForUID(binding.WorktreeRoot, options.uid) {
			reasons = append(reasons, "repository_path_unsafe")
			break
		}
	}
	if loaded.Automation.LinearTodoAdmission.Enabled {
		if loaded.Automation.LinearTodoAdmission.CredentialSourceRef != linearadapter.FileCredentialSourceRef || !safePrivateFileForUID(filepath.Join(loaded.CredentialDirectory, "linear-token"), options.uid) {
			reasons = append(reasons, "linear_credential_unavailable")
		}
	}
	return reasons
}

func safeExecutableForUID(path string, uid int) bool {
	info, err := os.Lstat(path)
	if err != nil || !validLaunchAgentPath(path) || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	owner, ok := fileOwnerUID(info)
	if !ok || (owner != uid && owner != launchDaemonRootUID) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safePrivateFileForUID(path string, uid int) bool {
	info, err := os.Lstat(path)
	if err != nil || !validLaunchAgentPath(path) || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByUID(info, uid) || logLinkCount(info) != 1 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safeOptionalPrivateFileForUID(path string, uid int) bool {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return true
	}
	return safePrivateFileForUID(path, uid)
}

func safePrivateDirectoryForUID(path string, uid int) bool {
	info, err := os.Lstat(path)
	if err != nil || !validLaunchAgentPath(path) || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByUID(info, uid) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safeDirectoryForUID(path string, uid int) bool {
	info, err := os.Lstat(path)
	if err != nil || !validLaunchAgentPath(path) || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !ownedByUID(info, uid) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safeLogLeafForUID(path string, uid int) bool {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && ownedByUID(info, uid) && logLinkCount(info) == 1
}

func fileOwnerUID(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), ok
}

func ownedByUID(info os.FileInfo, uid int) bool {
	owner, ok := fileOwnerUID(info)
	return ok && owner == uid
}

func launchDaemonInstall(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon install", args)
	if err != nil {
		return err
	}
	result := launchDaemonResult("install", "not_observed", "attention_required", "operator_attention", "", false)
	if launchDaemonEffectiveUID() != launchDaemonRootUID {
		result.Reason = "privilege_required"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	if launchAgentPathExists(filepath.Join(options.home, "Library", "LaunchAgents", launchAgentLabel+".plist")) || launchAgentPathExists(filepath.Join(options.home, "Library", "LaunchAgents", legacyLaunchdLabel+".plist")) {
		result.Reason = "launchagent_conflict"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	if launchAgentPathExists(legacyLaunchDaemonPlistPath()) {
		result.Reason = "legacy_service_conflict"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	if len(launchDaemonAssetReasons(options)) != 0 {
		result.Reason = "worker_assets_unsafe"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	if err := verifyNoLaunchAgentConflict(ctx, options); err != nil {
		result.Reason, _ = launchAgentControlErrorCode(err)
		return writeLaunchAgentControlResult(result, err)
	}
	desired := []byte(renderLaunchDaemonPlist(options))
	parent, err := openLaunchAgentParent(options.plist)
	if err != nil {
		result.Reason = launchAgentInstallReason(err)
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	defer parent.Close()
	name := filepath.Base(options.plist)
	existing, openErr := openLaunchAgentFileAt(parent, name)
	if openErr == nil {
		current, readErr := readLaunchAgentOpenedFile(ctx, existing)
		if readErr == nil && bytes.Equal(current, desired) && safeLaunchDaemonPlist(options.plist) {
			result.Outcome, result.NextSafeAction = "already_installed", "bootstrap"
			return writeLaunchAgentControlResult(result, nil)
		}
		result.Reason = "plist_exists"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	if !errors.Is(openErr, unix.ENOENT) && !errors.Is(openErr, os.ErrNotExist) {
		result.Reason = launchAgentInstallReason(openErr)
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	file, createErr := createLaunchAgentFileAt(parent, name)
	if createErr != nil {
		result.Reason = launchAgentInstallReason(createErr)
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	identity, identityErr := launchAgentFileIdentityFor(file)
	if identityErr != nil {
		_ = file.Close()
		result.Reason = "plist_unavailable"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	cleanup := func() { _ = file.Close(); removeLaunchAgentFileIfSame(parent, name, identity) }
	written, writeErr := file.Write(desired)
	if writeErr != nil || written != len(desired) || file.Sync() != nil || file.Close() != nil || !launchAgentInstallTargetStillBound(options.plist, parent, name, identity) || !safeLaunchDaemonPlist(options.plist) {
		cleanup()
		result.Reason = "install_unverified"
		return writeLaunchAgentControlResult(result, &launchAgentControlError{Code: result.Reason})
	}
	result.Outcome, result.NextSafeAction = "installed", "plist_validate"
	return writeLaunchAgentControlResult(result, nil)
}

func safeLaunchDaemonPlist(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !validLaunchAgentPath(path) || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByUID(info, launchDaemonRootUID) || logLinkCount(info) != 1 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func validateLaunchDaemonPlist(ctx context.Context, options launchDaemonOptions) error {
	if !safeLaunchDaemonPlist(options.plist) {
		return errors.New("plist_unsafe")
	}
	data, err := readLaunchAgentFile(ctx, options.plist)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, []byte(renderLaunchDaemonPlist(options))) {
		return errors.New("plist_mismatch")
	}
	return nil
}

func launchDaemonPlistValidate(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon plist-validate", args)
	if err != nil {
		return err
	}
	if err := requireLaunchDaemonPrivilege(); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("plist_validate", err), err)
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	if err := validateLaunchDaemonPlist(ctx, options); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("plist_validate", err), err)
	}
	if len(launchDaemonAssetReasons(options)) != 0 {
		err := &launchAgentControlError{Code: "worker_assets_unsafe"}
		return writeLaunchAgentControlResult(launchDaemonErrorResult("plist_validate", err), err)
	}
	return writeLaunchAgentControlResult(launchDaemonResult("plist_validate", "not_observed", "valid", "bootstrap", "", true), nil)
}

func launchDaemonBootstrap(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon bootstrap", args)
	if err != nil {
		return err
	}
	if err := requireLaunchDaemonPrivilege(); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("bootstrap", err), err)
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	if err := validateLaunchDaemonPlist(ctx, options); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("bootstrap", err), err)
	}
	if len(launchDaemonAssetReasons(options)) != 0 {
		err := &launchAgentControlError{Code: "worker_assets_unsafe"}
		return writeLaunchAgentControlResult(launchDaemonErrorResult("bootstrap", err), err)
	}
	if err := verifyNoLaunchAgentConflict(ctx, options); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("bootstrap", err), err)
	}
	return controlLaunchDaemonBootstrap(ctx, options)
}

func verifyNoLaunchAgentConflict(ctx context.Context, options launchDaemonOptions) error {
	control := launchAgentControlFactory(options.timeout)
	err := verifyNoLaunchdCompatibilityConflict(ctx, control, "system", fmt.Sprintf("gui/%d", options.uid), legacyLaunchDaemonPlistPath(), filepath.Join(options.home, "Library", "LaunchAgents", launchAgentLabel+".plist"), filepath.Join(options.home, "Library", "LaunchAgents", legacyLaunchdLabel+".plist"))
	var controlErr *launchAgentControlError
	if errors.As(err, &controlErr) {
		switch controlErr.Code {
		case "opposite_supervisor_conflict":
			return &launchAgentControlError{Code: "launchagent_conflict"}
		case "opposite_supervisor_state_unverified":
			return &launchAgentControlError{Code: "launchagent_state_unverified"}
		}
	}
	return err
}

func controlLaunchDaemonBootstrap(ctx context.Context, options launchDaemonOptions) error {
	control := launchAgentControlFactory(options.timeout)
	target := "system/" + launchAgentLabel
	observed, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("bootstrap", observed.State, err), err)
	}
	if observed.State != "absent" {
		return writeLaunchAgentControlResult(launchDaemonResult("bootstrap", observed.State, "reused", "status", "service_already_loaded", true), nil)
	}
	if err := control.Bootstrap(ctx, "system", options.plist); err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("bootstrap", "unknown", err), err)
	}
	after, err := control.Status(ctx, target)
	if err != nil || after.State == "absent" || after.State == "unknown" {
		if err == nil {
			err = &launchAgentControlError{Code: "bootstrap_not_observed"}
		}
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("bootstrap", after.State, err), err)
	}
	return writeLaunchAgentControlResult(launchDaemonResult("bootstrap", after.State, "bootstrapped", "status", "", true), nil)
}

func launchDaemonKickstart(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon kickstart", args)
	if err != nil {
		return err
	}
	if err := requireLaunchDaemonPrivilege(); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("kickstart", err), err)
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	if err := validateLaunchDaemonPlist(ctx, options); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("kickstart", err), err)
	}
	if len(launchDaemonAssetReasons(options)) != 0 {
		err := &launchAgentControlError{Code: "worker_assets_unsafe"}
		return writeLaunchAgentControlResult(launchDaemonErrorResult("kickstart", err), err)
	}
	if err := verifyNoLaunchAgentConflict(ctx, options); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("kickstart", err), err)
	}
	control := launchAgentControlFactory(options.timeout)
	target := "system/" + launchAgentLabel
	observed, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("kickstart", observed.State, err), err)
	}
	if observed.State == "absent" {
		return writeLaunchAgentControlResult(launchDaemonResult("kickstart", "absent", "not_loaded", "bootstrap", "service_absent", true), nil)
	}
	if observed.State == "running" {
		return writeLaunchAgentControlResult(launchDaemonResult("kickstart", "running", "already_running", "status", "", true), nil)
	}
	if err := control.Kickstart(ctx, target); err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("kickstart", "unknown", err), err)
	}
	after, err := control.Status(ctx, target)
	if err != nil || after.State != "running" {
		if err == nil {
			err = &launchAgentControlError{Code: "kickstart_not_observed"}
		}
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("kickstart", after.State, err), err)
	}
	return writeLaunchAgentControlResult(launchDaemonResult("kickstart", "running", "kickstarted", "status", "", true), nil)
}

func launchDaemonStatus(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon status", args)
	if err != nil {
		return err
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	control := launchAgentControlFactory(options.timeout)
	observed, err := control.Status(ctx, "system/"+launchAgentLabel)
	if err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("status", observed.State, err), err)
	}
	next, outcome := "status", "observed"
	if observed.State == "absent" {
		next, outcome = "install", "absent"
		if launchAgentPathExists(options.plist) {
			next = "bootstrap"
		}
	}
	result := launchDaemonResult("status", observed.State, outcome, next, "", launchAgentPathExists(options.plist))
	legacy, legacyErr := control.Status(ctx, "system/"+legacyLaunchdLabel)
	result.LegacyInstalled = launchAgentPathExists(legacyLaunchDaemonPlistPath())
	result.LegacyObservedState = legacy.State
	if legacyErr != nil {
		result.Outcome, result.NextSafeAction, result.Reason = "attention_required", "migration-status", "legacy_state_unverified"
	} else if result.LegacyInstalled || legacy.State != "absent" {
		result.Outcome, result.NextSafeAction, result.Reason = "attention_required", "migrate", "legacy_service_conflict"
	}
	agent, agentErr := control.Status(ctx, fmt.Sprintf("gui/%d/%s", options.uid, launchAgentLabel))
	legacyAgent, legacyAgentErr := control.Status(ctx, fmt.Sprintf("gui/%d/%s", options.uid, legacyLaunchdLabel))
	if agentErr != nil {
		result.Outcome = "attention_required"
		result.NextSafeAction = "status"
		result.Reason = "launchagent_state_unverified"
	} else if legacyAgentErr != nil {
		result.Outcome, result.NextSafeAction, result.Reason = "attention_required", "status", "launchagent_state_unverified"
	} else if launchAgentPathExists(filepath.Join(options.home, "Library", "LaunchAgents", launchAgentLabel+".plist")) || launchAgentPathExists(filepath.Join(options.home, "Library", "LaunchAgents", legacyLaunchdLabel+".plist")) || agent.State != "absent" || legacyAgent.State != "absent" {
		result.Outcome = "attention_required"
		result.NextSafeAction = "bootout_launchagent"
		result.Reason = "launchagent_conflict"
	}
	if observed.State == "running" {
		observation, observationErr := observeConfiguredWorkerRuntime(ctx, options.config, options.uid, observed.ProcessID, time.Now().UTC())
		applyRuntimeObservation(&result, observation, observationErr)
	}
	return writeLaunchAgentControlResult(result, nil)
}

func launchDaemonBootout(args []string) error {
	options, err := parseLaunchDaemonOptions("controller launchdaemon bootout", args)
	if err != nil {
		return err
	}
	if err := requireLaunchDaemonPrivilege(); err != nil {
		return writeLaunchAgentControlResult(launchDaemonErrorResult("bootout", err), err)
	}
	ctx, cancel := localContext(options.timeout)
	defer cancel()
	control := launchAgentControlFactory(options.timeout)
	target := "system/" + launchAgentLabel
	observed, err := control.Status(ctx, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("bootout", observed.State, err), err)
	}
	if observed.State == "absent" {
		return writeLaunchAgentControlResult(launchDaemonResult("bootout", "absent", "already_stopped", "status", "service_absent", false), nil)
	}
	if err := control.Bootout(ctx, target); err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("bootout", "unknown", err), err)
	}
	after, err := observeLaunchAgentAbsence(ctx, control, target)
	if err != nil {
		return writeLaunchAgentControlResult(launchDaemonControlErrorResult("bootout", after.State, err), err)
	}
	return writeLaunchAgentControlResult(launchDaemonResult("bootout", "absent", "stopped", "status", "", false), nil)
}

func requireLaunchDaemonPrivilege() error {
	if launchDaemonEffectiveUID() != launchDaemonRootUID {
		return &launchAgentControlError{Code: "privilege_required"}
	}
	return nil
}

func launchDaemonResult(step, state, outcome, next, reason string, runAtLoad bool) launchAgentControlResult {
	return launchAgentControlResult{Step: step, Label: launchAgentLabel, ObservedState: state, RunAtLoad: runAtLoad, Outcome: outcome, NextSafeAction: next, Reason: reason, ProcessLifetime: workerProcessLifetime, LogPolicy: workerLogPolicy}
}

func launchDaemonErrorResult(step string, err error) launchAgentControlResult {
	reason := "control_failed"
	if err != nil {
		reason = err.Error()
	}
	return launchDaemonResult(step, "unknown", "attention_required", "operator_attention", reason, false)
}

func launchDaemonControlErrorResult(step, state string, err error) launchAgentControlResult {
	code, timedOut := launchAgentControlErrorCode(err)
	result := launchDaemonResult(step, state, "attention_required", "status", code, false)
	result.TimedOut = timedOut
	return result
}
