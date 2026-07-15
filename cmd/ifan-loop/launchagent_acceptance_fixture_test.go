package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOfflineAcceptanceLaunchAgentControlReportsOneSanitizedServiceOutcome(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	binary := filepath.Join(root, "ifan-loop")
	plist := filepath.Join(root, "worker.plist")
	writeLaunchAgentFixture(t, binary, config, plist, true)

	oldFactory := launchAgentControlFactory
	defer func() { launchAgentControlFactory = oldFactory }()
	fake := &scriptedLaunchAgentControl{statuses: []launchAgentObservation{{State: "absent"}, {State: "unknown"}}}
	launchAgentControlFactory = func(time.Duration) launchAgentControl { return fake }
	output, err := captureConfigOutput(func() error {
		return launchAgentBootstrap([]string{"--binary", binary, "--config", config, "--plist", plist, "--domain", "gui/501", "--timeout", "1s"})
	})
	if err == nil {
		t.Fatalf("bootstrap unexpectedly succeeded: %s", output)
	}
	var result launchAgentControlResult
	if unmarshalErr := json.Unmarshal([]byte(output), &result); unmarshalErr != nil {
		t.Fatalf("output=%q err=%v unmarshal=%v", output, err, unmarshalErr)
	}
	if result.Outcome != "attention_required" || result.Reason != "bootstrap_not_observed" || result.ObservedState != "unknown" {
		t.Fatalf("result=%+v output=%s err=%v", result, output, err)
	}
	if strings.Count(output, `"outcome"`) != 1 || strings.Join(fake.calls, ",") != "status,bootstrap,status" {
		t.Fatalf("output=%s calls=%v", output, fake.calls)
	}
	for _, forbidden := range []string{root, binary, config, plist, "secret://", "Authorization", "stderr"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("sanitized service output leaked %q: %s", forbidden, output)
		}
	}
}
