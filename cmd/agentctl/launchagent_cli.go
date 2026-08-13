package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
)

const (
	launchAgentLabel         = "io.agent-loop-controller.worker"
	legacyLaunchdLabel       = "com.ifan.agent-loop-controller.worker"
	defaultInstalledBinary   = "/usr/local/bin/agentctl"
	defaultLegacyBinary      = "/usr/local/bin/ifan-loop"
	launchdRollbackSuffix    = ".agentctl-rollback"
	launchdDisabledSuffix    = ".agentctl-disabled"
	launchAgentLogDirectory  = "logs"
	launchAgentStdoutLogName = "worker.stdout.log"
	launchAgentStderrLogName = "worker.stderr.log"
)

//go:embed launchagent_worker.plist.tmpl
var launchAgentTemplate string

type launchAgentDoctorOutput struct {
	Ready           bool     `json:"ready"`
	Reasons         []string `json:"reasons"`
	ProcessLifetime string   `json:"process_lifetime"`
	LogPolicy       string   `json:"log_policy"`
}

func controllerLaunchAgent(args []string) error {
	if len(args) == 0 {
		return launchAgentUsage()
	}
	switch args[0] {
	case "build", "render":
		return launchAgentRender(args[1:])
	case "install":
		return launchAgentInstall(args[1:])
	case "doctor":
		return launchAgentDoctor(args[1:], false)
	case "validate":
		return launchAgentDoctor(args[1:], true)
	case "plist-validate":
		return launchAgentPlistValidate(args[1:])
	case "bootstrap":
		return launchAgentBootstrap(args[1:])
	case "kickstart":
		return launchAgentKickstart(args[1:])
	case "status":
		return launchAgentStatus(args[1:])
	case "bootout":
		return launchAgentBootout(args[1:])
	case "migration-status":
		return launchAgentMigrationStatus(args[1:])
	case "migrate":
		return launchAgentMigrate(args[1:])
	case "rollback":
		return launchAgentRollback(args[1:])
	default:
		return launchAgentUsage()
	}
}

func launchAgentUsage() error {
	return errors.New("usage: agentctl controller launchagent <build|render|install|doctor|validate|plist-validate|bootstrap|kickstart|status|bootout|migration-status|migrate|rollback> [options]")
}

type launchAgentOptions struct {
	binary, legacyBinary, config, plist, domain string
	timeout                                     time.Duration
}

func parseLaunchAgentOptions(name string, args []string) (launchAgentOptions, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	binary := flags.String("binary", defaultInstalledBinary, "absolute installed controller binary")
	legacyBinary := flags.String("legacy-binary", defaultLegacyBinary, "absolute installed legacy controller binary used only for migration or rollback")
	config := configPathFlag(flags)
	plist := flags.String("plist", "", "target user LaunchAgent plist path")
	domain := flags.String("domain", "", "GUI launchd domain (default: gui/<current uid>)")
	timeout := flags.Duration("timeout", defaultLaunchAgentControlTimeout, "maximum duration for one launchctl control step")
	if err := flags.Parse(args); err != nil {
		return launchAgentOptions{}, err
	}
	if flags.NArg() != 0 {
		return launchAgentOptions{}, errors.New("launchagent command does not accept positional arguments")
	}
	configPath, err := resolveConfigPath(*config)
	if err != nil {
		return launchAgentOptions{}, err
	}
	plistPath, err := resolveLaunchAgentPath(*plist)
	if err != nil {
		return launchAgentOptions{}, err
	}
	domainValue, err := resolveLaunchAgentDomain(*domain)
	if err != nil {
		return launchAgentOptions{}, err
	}
	if *timeout <= 0 || *timeout > maxLaunchAgentControlTimeout {
		return launchAgentOptions{}, errors.New("--timeout must be greater than zero and no more than 2m")
	}
	return launchAgentOptions{binary: *binary, legacyBinary: *legacyBinary, config: configPath, plist: plistPath, domain: domainValue, timeout: *timeout}, nil
}

func resolveLaunchAgentDomain(override string) (string, error) {
	if strings.TrimSpace(override) == "" {
		return fmt.Sprintf("gui/%d", os.Getuid()), nil
	}
	if !strings.HasPrefix(override, "gui/") || strings.TrimPrefix(override, "gui/") == "" {
		return "", errors.New("--domain must be a gui/<uid> domain")
	}
	for _, char := range strings.TrimPrefix(override, "gui/") {
		if char < '0' || char > '9' {
			return "", errors.New("--domain must be a gui/<uid> domain")
		}
	}
	return override, nil
}

func resolveLaunchAgentPath(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	home, err := userHomeDirectory()
	if err != nil || !filepath.IsAbs(home) || strings.TrimSpace(home) == "" {
		return "", errors.New("default LaunchAgent home is unavailable")
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

func legacyLaunchAgentPath(neutralPlist string) (string, error) {
	if !validLaunchAgentPath(neutralPlist) {
		return "", errors.New("neutral LaunchAgent path is invalid")
	}
	return filepath.Join(filepath.Dir(neutralPlist), legacyLaunchdLabel+".plist"), nil
}

func launchAgentRender(args []string) error {
	options, err := parseLaunchAgentOptions("controller launchagent render", args)
	if err != nil {
		return err
	}
	if !validLaunchAgentPath(options.binary) || !validLaunchAgentPath(options.config) || !validLaunchAgentPath(options.plist) {
		return errors.New("LaunchAgent paths must be absolute and canonical")
	}
	logs := filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory)
	if !validLaunchAgentPath(logs) {
		return errors.New("LaunchAgent log path is invalid")
	}
	_, err = fmt.Fprint(os.Stdout, renderLaunchAgentPlist(options.binary, options.config, filepath.Join(logs, launchAgentStdoutLogName), filepath.Join(logs, launchAgentStderrLogName)))
	return err
}

func renderLaunchAgentPlist(binary, config, stdout, stderr string) string {
	replacer := strings.NewReplacer(
		"{{BINARY_PATH}}", xmlEscape(binary),
		"{{CONFIG_PATH}}", xmlEscape(config),
		"{{STDOUT_PATH}}", xmlEscape(stdout),
		"{{STDERR_PATH}}", xmlEscape(stderr),
	)
	return replacer.Replace(launchAgentTemplate)
}

func xmlEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(value)
}

func launchAgentDoctor(args []string, installValidation bool) error {
	options, err := parseLaunchAgentOptions("controller launchagent doctor", args)
	if err != nil {
		return err
	}
	reasons := launchAgentReasons(options, installValidation)
	return printJSON(launchAgentDoctorOutput{Ready: len(reasons) == 0, Reasons: reasons, ProcessLifetime: workerProcessLifetime, LogPolicy: workerLogPolicy})
}

// launchAgentReasons is intentionally read-only. Its finite reason codes are
// safe to display and never contain a path, credential source, token, or OS
// error text.
func launchAgentReasons(options launchAgentOptions, installValidation bool) []string {
	reasons := launchAgentWorkerAssetReasons(options)
	legacyPlist, _ := legacyLaunchAgentPath(options.plist)
	if launchAgentPathExists(legacyPlist) {
		reasons = append(reasons, "legacy_service_conflict")
	}
	if launchAgentPathExists(launchDaemonPlistPath()) || launchAgentPathExists(legacyLaunchDaemonPlistPath()) {
		reasons = append(reasons, "launchdaemon_conflict")
	}
	if installValidation && launchAgentPathExists(options.plist) {
		reasons = append(reasons, "plist_exists")
	}
	return reasons
}

func launchAgentWorkerAssetReasons(options launchAgentOptions) []string {
	reasons := make([]string, 0, 8)
	if !safeExecutable(options.binary) {
		reasons = append(reasons, "binary_unsafe")
	}
	if !safeControllerConfig(options.config) {
		return append(reasons, "config_unsafe")
	}
	loaded, err := bootstrap.Load(options.config)
	if err != nil {
		return append(reasons, "config_unavailable")
	}
	if !safePrivateDirectory(filepath.Dir(loaded.Controller.DatabasePath)) {
		reasons = append(reasons, "database_parent_unsafe")
	}
	if loaded.Automation.LinearTodoAdmission.Enabled {
		source, sourceErr := linearCredentialSourceForRef(loaded, loaded.Automation.LinearTodoAdmission.CredentialSourceRef)
		checker, ok := source.(credentialChecker)
		if sourceErr != nil || !ok || checker.Check(context.Background()) != nil {
			reasons = append(reasons, "credential_unavailable")
		}
	}
	if !safePrivateDirectory(filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory)) {
		reasons = append(reasons, "log_directory_unsafe")
	} else if !safeLogLeaf(filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory, launchAgentStdoutLogName)) || !safeLogLeaf(filepath.Join(filepath.Dir(options.config), launchAgentLogDirectory, launchAgentStderrLogName)) {
		reasons = append(reasons, "log_file_unsafe")
	}
	return reasons
}

func validLaunchAgentPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\r\n\x00")
}

func safeExecutable(path string) bool {
	if !validLaunchAgentPath(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safeControllerConfig(path string) bool {
	if !validLaunchAgentPath(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm() != 0o600 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safePrivateDirectory(path string) bool {
	if !validLaunchAgentPath(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func safeLogLeaf(path string) bool {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && ownedByCurrentUser(info) && logLinkCount(info) == 1
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func logLinkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func launchAgentPathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
