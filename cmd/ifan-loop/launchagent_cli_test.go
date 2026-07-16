package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchAgentTemplateRendersOnlyExactWorkerArguments(t *testing.T) {
	binary := "/usr/local/bin/ifan-loop"
	config := "/Users/operator/Library/Application Support/agent-loop-controller/controller.json"
	stdout := "/Users/operator/Library/Application Support/agent-loop-controller/logs/worker.stdout.log"
	stderr := "/Users/operator/Library/Application Support/agent-loop-controller/logs/worker.stderr.log"
	rendered := renderLaunchAgentPlist(binary, config, stdout, stderr)
	for _, required := range []string{`<string>com.ifan.agent-loop-controller.worker</string>`, `<string>/usr/local/bin/ifan-loop</string>`, `<string>controller</string>`, `<string>worker</string>`, `<string>--config</string>`, `<key>SuccessfulExit</key>`, `<false/>`, `<integer>30</integer>`, `<integer>63</integer>`, stdout, stderr} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("template missing %q: %s", required, rendered)
		}
	}
	for _, forbidden := range []string{"/project/", "go run", "EnvironmentVariables", "IFAN_LOOP_LINEAR_TOKEN", "secret://", "github", "Authorization", "--once", "--max-runtime"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("template contains forbidden %q: %s", forbidden, rendered)
		}
	}
	if !strings.HasPrefix(rendered, `<?xml version="1.0"`) || !strings.Contains(rendered, `<plist version="1.0">`) || !strings.HasSuffix(rendered, "</plist>\n") {
		t.Fatalf("template is not a plist document: %s", rendered)
	}
}

func TestLaunchAgentDoctorUsesOnlyReasonCodesAndDoesNotOverwriteExistingPlist(t *testing.T) {
	root := resolvedTempDir(t)
	config, _ := writeControllerStatusConfig(t, root)
	binary := filepath.Join(root, "bin", "ifan-loop")
	if err := os.Mkdir(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(root, launchAgentLogDirectory)
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(root, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("unrelated existing plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	output, err := captureConfigOutput(func() error {
		return controllerLaunchAgent([]string{"validate", "--binary", binary, "--config", config, "--plist", plist})
	})
	if err != nil || !strings.Contains(output, `"ready": false`) || !strings.Contains(output, `"plist_exists"`) {
		t.Fatalf("output=%s err=%v", output, err)
	}
	if !strings.Contains(output, `"process_lifetime": "indefinite"`) || !strings.Contains(output, `"log_policy": "startup_truncate_8_mib"`) {
		t.Fatalf("doctor omitted worker lifetime/log contract: %s", output)
	}
	for _, forbidden := range []string{root, binary, config, plist, "unrelated existing plist", "secret://"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("doctor leaked %q in %s", forbidden, output)
		}
	}
	after, err := os.ReadFile(plist)
	if err != nil || string(after) != string(before) {
		t.Fatalf("validation changed existing plist after=%q err=%v", after, err)
	}
}

func TestLaunchAgentDoctorRejectsUnsafeLogLeafAndRenderRejectsRelativePath(t *testing.T) {
	root := resolvedTempDir(t)
	logs := filepath.Join(root, launchAgentLogDirectory)
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(logs, launchAgentStdoutLogName)
	if err := os.WriteFile(unsafe, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if safeLogLeaf(unsafe) {
		t.Fatal("group-readable log leaf was accepted")
	}
	if _, err := captureConfigOutput(func() error {
		return controllerLaunchAgent([]string{"render", "--binary", "relative/ifan-loop", "--config", filepath.Join(root, "controller.json")})
	}); err == nil || !strings.Contains(err.Error(), "absolute and canonical") {
		t.Fatalf("relative render error=%v", err)
	}
}

func TestLaunchAgentDoctorRejectsGroupWritableBinaryAndConfig(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, binary, config string)
		reason string
	}{
		{name: "binary", reason: "binary_unsafe", mutate: func(t *testing.T, binary, _ string) {
			t.Helper()
			if err := os.Chmod(binary, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "config", reason: "config_unsafe", mutate: func(t *testing.T, _, config string) {
			t.Helper()
			if err := os.Chmod(config, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := resolvedTempDir(t)
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
			test.mutate(t, binary, config)
			output, err := captureConfigOutput(func() error {
				return controllerLaunchAgent([]string{"doctor", "--binary", binary, "--config", config})
			})
			if err != nil || !strings.Contains(output, `"ready": false`) || !strings.Contains(output, `"`+test.reason+`"`) {
				t.Fatalf("output=%s err=%v", output, err)
			}
			for _, forbidden := range []string{root, binary, config} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("doctor leaked %q in %s", forbidden, output)
				}
			}
		})
	}
}
