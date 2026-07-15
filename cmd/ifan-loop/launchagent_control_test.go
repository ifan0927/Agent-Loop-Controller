package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type scriptedLaunchAgentControl struct {
	statuses []launchAgentObservation
	calls    []string
}

func (c *scriptedLaunchAgentControl) Status(context.Context, string) (launchAgentObservation, error) {
	c.calls = append(c.calls, "status")
	if len(c.statuses) == 0 {
		return launchAgentObservation{State: "unknown"}, errors.New("missing scripted status")
	}
	status := c.statuses[0]
	c.statuses = c.statuses[1:]
	return status, nil
}

func (c *scriptedLaunchAgentControl) Bootstrap(context.Context, string, string) error {
	c.calls = append(c.calls, "bootstrap")
	return nil
}

func (c *scriptedLaunchAgentControl) Kickstart(context.Context, string) error {
	c.calls = append(c.calls, "kickstart")
	return nil
}

func (c *scriptedLaunchAgentControl) Bootout(context.Context, string) error {
	c.calls = append(c.calls, "bootout")
	return nil
}

type recordingLaunchAgentRunner struct {
	calls  [][]string
	result launchAgentCommandResult
}

func (r *recordingLaunchAgentRunner) Run(_ context.Context, args []string) (launchAgentCommandResult, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.result, nil
}

type blockingLaunchAgentRunner struct{}

func (blockingLaunchAgentRunner) Run(ctx context.Context, _ []string) (launchAgentCommandResult, error) {
	<-ctx.Done()
	return launchAgentCommandResult{}, ctx.Err()
}

func TestLaunchctlControlUsesExplicitArgvAndNormalizesState(t *testing.T) {
	runner := &recordingLaunchAgentRunner{result: launchAgentCommandResult{ExitCode: 0, Stdout: []byte("state = running\nsecret-token\n")}}
	control := launchctlControl{runner: runner, timeout: time.Second}
	status, err := control.Status(context.Background(), "gui/501/"+launchAgentLabel)
	if err != nil || status.State != "running" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if err := control.Bootstrap(context.Background(), "gui/501", "/tmp/worker.plist"); err != nil {
		t.Fatal(err)
	}
	if err := control.Kickstart(context.Background(), "gui/501/"+launchAgentLabel); err != nil {
		t.Fatal(err)
	}
	if err := control.Bootout(context.Background(), "gui/501/"+launchAgentLabel); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"print", "gui/501/" + launchAgentLabel}, {"bootstrap", "gui/501", "/tmp/worker.plist"}, {"kickstart", "gui/501/" + launchAgentLabel}, {"bootout", "gui/501/" + launchAgentLabel}}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls=%q want=%q", runner.calls, want)
	}
	for index := range want {
		if strings.Join(runner.calls[index], "\x00") != strings.Join(want[index], "\x00") {
			t.Fatalf("call[%d]=%q want=%q", index, runner.calls[index], want[index])
		}
	}
}

func TestLaunchctlControlTimeoutIsFiniteAndSanitized(t *testing.T) {
	control := launchctlControl{runner: blockingLaunchAgentRunner{}, timeout: 20 * time.Millisecond}
	_, err := control.Status(context.Background(), "gui/501/"+launchAgentLabel)
	var controlErr *launchAgentControlError
	if !errors.As(err, &controlErr) || controlErr.Code != "control_timeout" {
		t.Fatalf("err=%v", err)
	}
}

func TestLaunchAgentBootstrapReusesLoadedServiceAndVerifiesAfterBootstrap(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	binary := filepath.Join(root, "ifan-loop")
	plist := filepath.Join(root, "worker.plist")
	writeLaunchAgentFixture(t, binary, config, plist, true)
	oldFactory := launchAgentControlFactory
	defer func() { launchAgentControlFactory = oldFactory }()
	fake := &scriptedLaunchAgentControl{statuses: []launchAgentObservation{{State: "absent"}, {State: "running"}}}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	output, err := captureConfigOutput(func() error {
		return launchAgentBootstrap([]string{"--binary", binary, "--config", config, "--plist", plist, "--domain", "gui/501", "--timeout", "1s"})
	})
	if err != nil || !strings.Contains(output, `"outcome": "bootstrapped"`) || !strings.Contains(output, `"observed_state": "running"`) {
		t.Fatalf("output=%s err=%v calls=%v", output, err, fake.calls)
	}
	if strings.Join(fake.calls, ",") != "status,bootstrap,status" {
		t.Fatalf("calls=%v", fake.calls)
	}
}

func TestLaunchAgentKickstartRestartsStoppedRunAtLoadService(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	binary := filepath.Join(root, "ifan-loop")
	plist := filepath.Join(root, "worker.plist")
	writeLaunchAgentFixture(t, binary, config, plist, true)
	oldFactory := launchAgentControlFactory
	defer func() { launchAgentControlFactory = oldFactory }()
	fake := &scriptedLaunchAgentControl{statuses: []launchAgentObservation{{State: "stopped"}, {State: "running"}}}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	output, err := captureConfigOutput(func() error {
		return launchAgentKickstart([]string{"--binary", binary, "--config", config, "--plist", plist, "--domain", "gui/501", "--timeout", "1s"})
	})
	if err != nil || !strings.Contains(output, `"outcome": "kickstarted"`) {
		t.Fatalf("output=%s err=%v calls=%v", output, err, fake.calls)
	}
	if strings.Join(fake.calls, ",") != "status,kickstart,status" {
		t.Fatalf("calls=%v", fake.calls)
	}
}

func TestLaunchAgentPlistParserRejectsInvalidStructureAndDuplicateKeys(t *testing.T) {
	root := resolvedTempDir(t)
	binary := filepath.Join(root, "ifan-loop")
	config := filepath.Join(root, "controller.json")
	plist := filepath.Join(root, "worker.plist")
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := renderLaunchAgentPlist(binary, config, filepath.Join(root, "logs", launchAgentStdoutLogName), filepath.Join(root, "logs", launchAgentStderrLogName))
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "wrong root", content: strings.Replace(valid, `<?xml version="1.0" encoding="UTF-8"?>`, `<?xml version="1.0" encoding="UTF-8"?><wrapper>`, 1) + "</wrapper>"},
		{name: "duplicate label", content: strings.Replace(valid, "</dict>\n</plist>", "  <key>Label</key>\n  <string>duplicate</string>\n</dict>\n</plist>", 1)},
		{name: "wrong program arguments type", content: strings.Replace(valid, "<key>ProgramArguments</key>\n  <array>", "<key>ProgramArguments</key>\n  <dict>", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(plist, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := validateLaunchAgentPlist(context.Background(), launchAgentOptions{binary: binary, config: config, plist: plist})
			if err == nil || err.Error() != "plist_invalid" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLaunchAgentInstallIsIdempotentAndDoesNotOverwrite(t *testing.T) {
	root := resolvedTempDir(t)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config, _ := writeControllerStatusConfig(t, root)
	binary := filepath.Join(root, "bin", "ifan-loop")
	if err := os.Mkdir(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, launchAgentLogDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(root, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"install", "--binary", binary, "--config", config, "--plist", plist, "--domain", "gui/501", "--timeout", "1s"}
	first, err := captureConfigOutput(func() error { return controllerLaunchAgent(args) })
	if err != nil || !strings.Contains(first, `"outcome": "installed"`) {
		t.Fatalf("first=%s err=%v", first, err)
	}
	before, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	second, err := captureConfigOutput(func() error { return controllerLaunchAgent(args) })
	if err != nil || !strings.Contains(second, `"outcome": "already_installed"`) {
		t.Fatalf("second=%s err=%v", second, err)
	}
	after, err := os.ReadFile(plist)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("install changed existing plist err=%v", err)
	}
}

func writeLaunchAgentFixture(t *testing.T, binary, config, plist string, runAtLoad bool) {
	t.Helper()
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	rendered := renderLaunchAgentPlist(binary, config, filepath.Join(filepath.Dir(config), launchAgentLogDirectory, launchAgentStdoutLogName), filepath.Join(filepath.Dir(config), launchAgentLogDirectory, launchAgentStderrLogName))
	if !runAtLoad {
		rendered = strings.Replace(rendered, "<key>RunAtLoad</key>\n  <true/>", "<key>RunAtLoad</key>\n  <false/>", 1)
	}
	if err := os.WriteFile(plist, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
}
