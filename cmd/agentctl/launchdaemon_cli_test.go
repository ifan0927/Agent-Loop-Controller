package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestLaunchDaemonTemplatePinsNonRootIdentityAndSecretFreeEnvironment(t *testing.T) {
	options, _ := launchDaemonTestOptions(t)
	rendered := renderLaunchDaemonPlist(options)
	for _, required := range []string{
		"<key>UserName</key>\n  <string>worker</string>",
		"<key>WorkingDirectory</key>\n  <string>" + options.home + "</string>",
		"<key>HOME</key>\n    <string>" + options.home + "</string>",
		"<string>controller</string>", "<string>worker</string>", "<string>--config</string>",
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("missing exact LaunchDaemon contract %q", required)
		}
	}
	for _, forbidden := range []string{"Authorization", "IFAN_LOOP_LINEAR_TOKEN", "token", "secret://", "<key>GroupName</key>", "<key>RootDirectory</key>"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("rendered LaunchDaemon contains forbidden value %q", forbidden)
		}
	}
}

func TestLaunchDaemonInstallAndValidateExactRootOwnedDocument(t *testing.T) {
	options, args := launchDaemonTestOptions(t)
	output, err := captureConfigOutput(func() error { return launchDaemonInstall(args) })
	if err != nil || !strings.Contains(output, `"outcome": "installed"`) {
		t.Fatalf("install output=%s err=%v", output, err)
	}
	info, err := os.Lstat(options.plist)
	if err != nil || info.Mode().Perm() != 0o600 || !ownedByUID(info, launchDaemonRootUID) {
		t.Fatalf("installed plist info=%v err=%v", info, err)
	}
	output, err = captureConfigOutput(func() error { return launchDaemonPlistValidate(args) })
	if err != nil || !strings.Contains(output, `"outcome": "valid"`) {
		t.Fatalf("validate output=%s err=%v", output, err)
	}
	output, err = captureConfigOutput(func() error { return launchDaemonInstall(args) })
	if err != nil || !strings.Contains(output, `"outcome": "already_installed"`) {
		t.Fatalf("idempotent install output=%s err=%v", output, err)
	}
}

func TestLaunchDaemonInstallRejectsLoadedLegacyService(t *testing.T) {
	options, args := launchDaemonTestOptions(t)
	absent := launchAgentObservation{State: "absent"}
	fake := &scriptedLaunchAgentControl{
		statusFallback: &absent,
		statusByTarget: map[string][]launchAgentObservation{
			"system/" + legacyLaunchdLabel: {{State: "running", ProcessID: os.Getpid()}},
		},
	}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	output, err := captureConfigOutput(func() error { return launchDaemonInstall(args) })
	if err == nil || !strings.Contains(output, `"reason": "legacy_service_conflict"`) || launchAgentPathExists(options.plist) {
		t.Fatalf("output=%s err=%v targets=%v", output, err, fake.targetCalls)
	}
}

func TestLaunchDaemonMutationsRequirePrivilege(t *testing.T) {
	_, args := launchDaemonTestOptions(t)
	launchDaemonRootUID = -1
	output, err := captureConfigOutput(func() error { return launchDaemonInstall(args) })
	if err == nil || !strings.Contains(output, `"reason": "privilege_required"`) {
		t.Fatalf("output=%s err=%v", output, err)
	}
}

func TestLaunchDaemonBootstrapUsesSystemDomainAndRejectsLaunchAgentConflict(t *testing.T) {
	options, args := launchDaemonTestOptions(t)
	if _, err := captureConfigOutput(func() error { return launchDaemonInstall(args) }); err != nil {
		t.Fatal(err)
	}
	fake := &scriptedLaunchAgentControl{statuses: []launchAgentObservation{{State: "absent"}, {State: "absent"}, {State: "running"}}}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	output, err := captureConfigOutput(func() error { return launchDaemonBootstrap(args) })
	if err != nil || !strings.Contains(output, `"outcome": "bootstrapped"`) {
		t.Fatalf("output=%s err=%v calls=%v", output, err, fake.calls)
	}
	if strings.Join(fake.calls, ",") != "status,status,bootstrap,status" {
		t.Fatalf("calls=%v", fake.calls)
	}

	agentDirectory := filepath.Join(options.home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDirectory, launchAgentLabel+".plist"), []byte("reserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	output, err = captureConfigOutput(func() error { return launchDaemonBootstrap(args) })
	if err == nil || !strings.Contains(output, `"reason": "launchagent_conflict"`) || len(fake.calls) != 0 {
		t.Fatalf("conflict output=%s err=%v calls=%v", output, err, fake.calls)
	}
}

func TestLaunchDaemonKickstartAndStatusRejectLoadedLaunchAgent(t *testing.T) {
	_, args := launchDaemonTestOptions(t)
	if _, err := captureConfigOutput(func() error { return launchDaemonInstall(args) }); err != nil {
		t.Fatal(err)
	}
	fake := &scriptedLaunchAgentControl{statuses: []launchAgentObservation{{State: "running"}}}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	output, err := captureConfigOutput(func() error { return launchDaemonKickstart(args) })
	if err == nil || !strings.Contains(output, `"reason": "launchagent_conflict"`) || strings.Contains(strings.Join(fake.calls, ","), "kickstart") {
		t.Fatalf("kickstart output=%s err=%v calls=%v", output, err, fake.calls)
	}
	fake.calls = nil
	fake.statuses = []launchAgentObservation{{State: "running"}, {State: "running"}}
	output, err = captureConfigOutput(func() error { return launchDaemonStatus(args) })
	if err != nil || !strings.Contains(output, `"outcome": "attention_required"`) || !strings.Contains(output, `"reason": "launchagent_conflict"`) {
		t.Fatalf("status output=%s err=%v calls=%v", output, err, fake.calls)
	}
}

func TestLaunchDaemonStatusUsesCanonicalWorkerHeartbeatObservation(t *testing.T) {
	options, args := launchDaemonTestOptions(t)
	reporter, err := newWorkerStatusReporter(options.config, "daemon-worker", version, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Observe(admissionWorkerResult{Status: workerStatusParked, PreviousStatus: workerStatusDriving, Cycles: 3}); err != nil {
		t.Fatal(err)
	}
	originalLoad := loadWorkerRuntimeConfiguration
	loadWorkerRuntimeConfiguration = func(string) (bootstrap.Bootstrap, error) {
		return bootstrap.Bootstrap{Controller: bootstrap.Controller{Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}}}, nil
	}
	t.Cleanup(func() { loadWorkerRuntimeConfiguration = originalLoad })
	absent := launchAgentObservation{State: "absent"}
	fake := &scriptedLaunchAgentControl{statusFallback: &absent, statusByTarget: map[string][]launchAgentObservation{
		"system/" + launchAgentLabel: {{State: "running", ProcessID: os.Getpid()}},
	}}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	output, err := captureConfigOutput(func() error { return launchDaemonStatus(args) })
	if err != nil || !strings.Contains(output, `"worker_liveness": "fresh"`) || !strings.Contains(output, `"worker_status": "parked"`) || !strings.Contains(output, `"worker_identity_verified": true`) || !strings.Contains(output, `"worker_build_identity": "`+version+`"`) || !strings.Contains(output, `"loaded_configuration_digest": "`+strings.Repeat("d", 64)+`"`) {
		t.Fatalf("output=%s err=%v", output, err)
	}
}

func TestLaunchDaemonMutationsRecheckWorkerAssetsAfterInstall(t *testing.T) {
	_, args := launchDaemonTestOptions(t)
	if _, err := captureConfigOutput(func() error { return launchDaemonInstall(args) }); err != nil {
		t.Fatal(err)
	}
	launchDaemonAssetReasons = func(launchDaemonOptions) []string { return []string{"binary_unsafe"} }
	fake := &scriptedLaunchAgentControl{}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	for _, test := range []struct {
		name string
		run  func([]string) error
	}{
		{name: "bootstrap", run: launchDaemonBootstrap},
		{name: "kickstart", run: launchDaemonKickstart},
	} {
		output, err := captureConfigOutput(func() error { return test.run(args) })
		if err == nil || !strings.Contains(output, `"reason": "worker_assets_unsafe"`) || len(fake.calls) != 0 {
			t.Fatalf("%s output=%s err=%v calls=%v", test.name, output, err, fake.calls)
		}
	}
}

func TestLaunchDaemonRejectsRootWorkerAndUserDomainOverride(t *testing.T) {
	_, args := launchDaemonTestOptions(t)
	wrongPlist := append([]string(nil), args...)
	for index := range wrongPlist {
		if wrongPlist[index] == "--plist" {
			wrongPlist[index+1] = filepath.Join(filepath.Dir(launchDaemonDirectory), launchAgentLabel+".plist")
			break
		}
	}
	if _, err := parseLaunchDaemonOptions("test", wrongPlist); err == nil || err.Error() != "--plist must be the canonical system LaunchDaemon path" {
		t.Fatalf("unexpected plist path error: %v", err)
	}
	root := &user.User{Uid: "0", Username: "worker", HomeDir: "/var/root"}
	launchDaemonLookupUser = func(string) (*user.User, error) { return root, nil }
	if _, err := parseLaunchDaemonOptions("test", args); err == nil || err.Error() != "--user must resolve to a non-root account" {
		t.Fatalf("unexpected root user error: %v", err)
	}
	if _, err := parseLaunchDaemonOptions("test", append(args, "--domain", "user/501")); err == nil {
		t.Fatal("user domain override was accepted")
	}
}

func launchDaemonTestOptions(t *testing.T) (launchDaemonOptions, []string) {
	t.Helper()
	root := resolvedTempDir(t)
	home := filepath.Join(root, "home")
	parent := filepath.Join(root, "LaunchDaemons")
	for path, mode := range map[string]os.FileMode{home: 0o700, parent: 0o755} {
		if err := os.MkdirAll(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(root, "agentctl")
	config := filepath.Join(root, "controller.json")
	if err := os.WriteFile(binary, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	account := &user.User{Uid: strconv.Itoa(uid), Username: "worker", HomeDir: home}
	originalCurrent, originalLookup := launchDaemonCurrentUser, launchDaemonLookupUser
	originalEffective, originalRoot := launchDaemonEffectiveUID, launchDaemonRootUID
	originalReasons := launchDaemonAssetReasons
	originalDirectory := launchDaemonDirectory
	originalFactory := launchAgentControlFactory
	t.Cleanup(func() {
		launchDaemonCurrentUser, launchDaemonLookupUser = originalCurrent, originalLookup
		launchDaemonEffectiveUID, launchDaemonRootUID = originalEffective, originalRoot
		launchDaemonAssetReasons = originalReasons
		launchDaemonDirectory = originalDirectory
		launchAgentControlFactory = originalFactory
	})
	launchDaemonCurrentUser = func() (*user.User, error) { return account, nil }
	launchDaemonLookupUser = func(name string) (*user.User, error) {
		if name != account.Username {
			return nil, user.UnknownUserError(name)
		}
		return account, nil
	}
	launchDaemonEffectiveUID = func() int { return uid }
	launchDaemonRootUID = uid
	launchDaemonAssetReasons = func(launchDaemonOptions) []string { return nil }
	launchDaemonDirectory = parent
	absent := launchAgentObservation{State: "absent"}
	launchAgentControlFactory = func(time.Duration) launchAgentControl {
		return &scriptedLaunchAgentControl{statusFallback: &absent}
	}
	plist := filepath.Join(parent, launchAgentLabel+".plist")
	args := []string{"--binary", binary, "--config", config, "--plist", plist, "--user", account.Username, "--working-directory", home, "--timeout", "1s"}
	return launchDaemonOptions{binary: binary, config: config, plist: plist, username: account.Username, uid: uid, home: home, workingDirectory: home, timeout: time.Second}, args
}
