package localupgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type supervisorObservation struct {
	State string
	PID   int
}

type supervisorTopology struct {
	Selected supervisorObservation
	Reason   string
}

func (m *Manager) observeSupervisorTopology(ctx context.Context, supervisor string) supervisorTopology {
	selectedDomain, oppositeDomain := fmt.Sprintf("gui/%d", m.uid), "system"
	if supervisor == "launchdaemon" {
		selectedDomain, oppositeDomain = "system", fmt.Sprintf("gui/%d", m.uid)
	}
	targets := []string{
		selectedDomain + "/" + neutralLaunchdLabel,
		selectedDomain + "/" + legacyLaunchdLabel,
		oppositeDomain + "/" + neutralLaunchdLabel,
		oppositeDomain + "/" + legacyLaunchdLabel,
	}
	observations := make([]supervisorObservation, len(targets))
	for index, target := range targets {
		observation, err := observeLaunchctl(ctx, m.runner, target)
		if err != nil {
			return supervisorTopology{Reason: "supervisor_state_unverified"}
		}
		observations[index] = observation
	}
	if observations[1].State != "absent" {
		return supervisorTopology{Selected: observations[0], Reason: "legacy_supervisor_conflict"}
	}
	agentDirectory := filepath.Join(m.home, "Library", "LaunchAgents")
	daemonDirectory := m.launchDaemonDirectory
	selectedLegacyPlist, oppositeNeutralPlist, oppositeLegacyPlist := filepath.Join(agentDirectory, legacyLaunchdLabel+".plist"), filepath.Join(daemonDirectory, neutralLaunchdLabel+".plist"), filepath.Join(daemonDirectory, legacyLaunchdLabel+".plist")
	if supervisor == "launchdaemon" {
		selectedLegacyPlist, oppositeNeutralPlist, oppositeLegacyPlist = filepath.Join(daemonDirectory, legacyLaunchdLabel+".plist"), filepath.Join(agentDirectory, neutralLaunchdLabel+".plist"), filepath.Join(agentDirectory, legacyLaunchdLabel+".plist")
	}
	if present, known := pathPresence(selectedLegacyPlist); !known {
		return supervisorTopology{Selected: observations[0], Reason: "supervisor_state_unverified"}
	} else if present {
		return supervisorTopology{Selected: observations[0], Reason: "legacy_supervisor_conflict"}
	}
	if observations[2].State != "absent" || observations[3].State != "absent" {
		return supervisorTopology{Selected: observations[0], Reason: "opposite_supervisor_conflict"}
	}
	for _, path := range []string{oppositeNeutralPlist, oppositeLegacyPlist} {
		if present, known := pathPresence(path); !known {
			return supervisorTopology{Selected: observations[0], Reason: "supervisor_state_unverified"}
		} else if present {
			return supervisorTopology{Selected: observations[0], Reason: "opposite_supervisor_conflict"}
		}
	}
	return supervisorTopology{Selected: observations[0]}
}

func pathPresence(path string) (present, known bool) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, true
	}
	if os.IsNotExist(err) {
		return false, true
	}
	return false, false
}

func observeLaunchctl(ctx context.Context, runner commandRunner, target string) (supervisorObservation, error) {
	stepCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := runner.Run(stepCtx, "", "launchctl", "print", target)
	if err != nil {
		return supervisorObservation{}, err
	}
	if result.ExitCode != 0 {
		combined := strings.ToLower(string(append(append([]byte(nil), result.Stdout...), result.Stderr...)))
		for _, marker := range []string{"could not find service", "service not found", "no such process", "unknown service", "domain does not support specified action"} {
			if strings.Contains(combined, marker) || result.ExitCode == 113 {
				return supervisorObservation{State: "absent"}, nil
			}
		}
		return supervisorObservation{}, errors.New("launchctl observation failed")
	}
	observation := supervisorObservation{State: "loaded"}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if value, found := strings.CutPrefix(line, "state = "); found {
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "running":
				observation.State = "running"
			case "waiting", "stopped", "exited", "not running":
				observation.State = "stopped"
			}
		}
		if value, found := strings.CutPrefix(line, "pid = "); found {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr == nil && pid > 0 {
				observation.PID = pid
			}
		}
	}
	return observation, nil
}
