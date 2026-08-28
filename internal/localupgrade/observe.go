package localupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type heartbeatEvidence struct {
	SchemaVersion       int       `json:"schema_version"`
	WorkerInstanceID    string    `json:"worker_instance_id"`
	ProcessID           int       `json:"process_id"`
	ProcessStartID      string    `json:"process_start_id"`
	BuildIdentity       string    `json:"build_identity,omitempty"`
	ConfigurationDigest string    `json:"loaded_configuration_digest,omitempty"`
	Status              string    `json:"status"`
	PreviousStatus      string    `json:"previous_status,omitempty"`
	Cycles              int       `json:"cycles"`
	ObservedAt          time.Time `json:"observed_at"`
	LastCycleOutcome    string    `json:"last_cycle_outcome,omitempty"`
	LastQueueReason     string    `json:"last_queue_decision_reason,omitempty"`
	LastCycleCompleted  time.Time `json:"last_cycle_completed_at,omitempty"`
	NextAdmission       time.Time `json:"next_admission_evaluation_at,omitempty"`
}

func (m *Manager) Observe(ctx context.Context, id string) (result Result, finalErr error) {
	err := m.withActiveLock(id, func() error {
		j, bundle, err := m.loadJournal(id)
		if err != nil {
			return err
		}
		rollback := j.Phase == "rolled_back"
		if !rollback && (j.BootstrapIntentAt == nil || j.Phase != "bootstrap_intent" && j.Phase != "attention") {
			return errors.New("observation is unavailable before durable bootstrap intent")
		}
		expectedDigest, expectedBuild, expectedSchema := j.Candidate.Digest, j.Candidate.Build.BuildIdentity, j.Candidate.Build.SupportedControllerSchemaVersion
		if rollback {
			expectedDigest = j.Previous.Digest
			expectedSchema = j.Database.SchemaVersion
			if j.Previous.Structured {
				expectedBuild = j.Previous.Build.BuildIdentity
			} else {
				expectedBuild = j.Previous.LegacyVersion
			}
		}
		inspected, err := inspectBinary(ctx, m.runner, j.BinaryPath, m.uid)
		if err != nil || inspected.Digest != expectedDigest || !rollback && (!inspected.Structured || inspected.Build.BuildIdentity != expectedBuild || inspected.Build.VCSRevision != j.Revision || inspected.Build.VCSModified || inspected.Build.SupportedControllerSchemaVersion != expectedSchema) {
			return m.observationAttention(bundle, &j, "installed_build_identity_failed", rollback, &result)
		}
		topology := m.observeSupervisorTopology(ctx, j.Supervisor)
		if topology.Reason != "" {
			return m.observationAttention(bundle, &j, topology.Reason, rollback, &result)
		}
		if topology.Selected.State != "running" || topology.Selected.PID < 1 {
			if rollback {
				result = resultFor(j, "pending", "restored_worker_not_running", "bootstrap_selected_supervisor")
				return nil
			}
			return m.setAttention(bundle, &j, "selected_supervisor_not_running", &result)
		}
		heartbeat, reason := readHeartbeat(j.ConfigPath, m.uid)
		if reason != "" {
			if rollback {
				result = resultFor(j, "pending", reason, "observe")
				return nil
			}
			return m.setAttention(bundle, &j, reason, &result)
		}
		now := m.now()
		if heartbeat.ProcessID != topology.Selected.PID || heartbeat.BuildIdentity != expectedBuild || heartbeat.ConfigurationDigest != j.ConfigDigest {
			return m.observationAttention(bundle, &j, "heartbeat_identity_failed", rollback, &result)
		}
		started, err := processStartIdentity(heartbeat.ProcessID)
		if err != nil || started != heartbeat.ProcessStartID {
			return m.observationAttention(bundle, &j, "process_start_identity_failed", rollback, &result)
		}
		age := now.Sub(heartbeat.ObservedAt)
		if age < -5*time.Second {
			return m.observationAttention(bundle, &j, "heartbeat_clock_conflict", rollback, &result)
		}
		if age > 45*time.Second {
			if rollback {
				result = resultFor(j, "pending", "heartbeat_stale", "observe")
				return nil
			}
			return m.setAttention(bundle, &j, "heartbeat_stale", &result)
		}
		readiness, reason := configurationAndIntegrityReadiness(ctx, j.DatabasePath, j.ConfigDigest, expectedSchema)
		switch readiness {
		case "pending":
			result = resultFor(j, "pending", reason, "observe")
			return nil
		case "not_ready", "conflict":
			return m.observationAttention(bundle, &j, reason, rollback, &result)
		case "ready":
			now := m.now()
			j.CompletedAt, j.UpdatedAt = &now, now
			if rollback {
				j.Phase = "rollback_healthy"
				if err := writeJournal(bundle, j, m.uid); err != nil {
					return err
				}
				result = resultFor(j, "rollback_healthy", "restored_worker_verified", "cleanup")
				return nil
			}
			j.Phase = "healthy"
			if err := writeJournal(bundle, j, m.uid); err != nil {
				return err
			}
			result = resultFor(j, "healthy", "upgrade_and_controller_ready", "cleanup")
			return nil
		default:
			return m.observationAttention(bundle, &j, "controller_readiness_invalid", rollback, &result)
		}
	})
	return result, err
}

func (m *Manager) observationAttention(bundle string, j *journal, reason string, rollback bool, result *Result) error {
	if rollback {
		*result = resultFor(*j, "attention", reason, "preserve_bundle")
		return nil
	}
	return m.setAttention(bundle, j, reason, result)
}

func (m *Manager) setAttention(bundle string, j *journal, reason string, result *Result) error {
	if j.BootstrapIntentAt == nil {
		return errors.New(reason)
	}
	j.Phase, j.FailureReason, j.UpdatedAt = "attention", reason, m.now()
	if err := writeJournal(bundle, *j, m.uid); err != nil {
		return err
	}
	*result = resultFor(*j, "attention", reason, "preserve_bundle")
	return nil
}

func readHeartbeat(configPath string, uid int) (heartbeatEvidence, string) {
	path := configPath + ".worker-status.json"
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return heartbeatEvidence{}, "heartbeat_absent"
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByUID(info, uid) || info.Size() < 2 || info.Size() > 4<<10 {
		return heartbeatEvidence{}, "heartbeat_unavailable"
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return heartbeatEvidence{}, "heartbeat_unavailable"
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return heartbeatEvidence{}, "heartbeat_unavailable"
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4<<10+1))
	if err != nil || len(raw) > 4<<10 {
		return heartbeatEvidence{}, "heartbeat_unavailable"
	}
	var evidence heartbeatEvidence
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil || decoder.Decode(&struct{}{}) != io.EOF || evidence.SchemaVersion < 2 || evidence.SchemaVersion > 3 || evidence.ProcessID < 1 || evidence.ProcessStartID == "" || evidence.BuildIdentity == "" || evidence.ConfigurationDigest == "" || evidence.ObservedAt.IsZero() {
		return heartbeatEvidence{}, "heartbeat_invalid"
	}
	return evidence, ""
}

func (m *Manager) Cleanup(ctx context.Context, id string) (result Result, finalErr error) {
	err := m.withActiveLock(id, func() error {
		j, bundle, err := m.loadJournal(id)
		if err != nil {
			if recovered, recoverErr := m.finishInterruptedCleanup(id); recoverErr == nil && recovered {
				result = Result{UpgradeID: id, State: "cleaned", Reason: "completed_bundle_removed", NextAction: "none", UpgradeHealth: "healthy", ControllerReadiness: "ready"}
				return nil
			}
			return err
		}
		if j.Phase != "healthy" && j.Phase != "rollback_healthy" && j.Phase != "cleanup_intent" || j.CompletedAt == nil {
			return errors.New("cleanup requires a verified healthy completion")
		}
		if err := validateCleanupBundle(bundle, m.uid, j.Phase != "cleanup_intent"); err != nil {
			return err
		}
		rollbackCompleted := j.Phase == "rollback_healthy" || j.Phase == "cleanup_intent" && j.BootstrapIntentAt == nil
		if j.Phase != "cleanup_intent" {
			j.Phase, j.UpdatedAt = "cleanup_intent", m.now()
			if err := writeJournal(bundle, j, m.uid); err != nil {
				return err
			}
		}
		current := currentInstallation{SchemaVersion: 1, UpgradeID: id, Supervisor: j.Supervisor, VerifiedAt: *j.CompletedAt}
		if !rollbackCompleted {
			current.BinaryDigest, current.BuildIdentity, current.VCSRevision, current.DatabaseSchema = j.Candidate.Digest, j.Candidate.Build.BuildIdentity, j.Revision, j.Candidate.Build.SupportedControllerSchemaVersion
		} else {
			current.BinaryDigest, current.DatabaseSchema = j.Previous.Digest, j.Database.SchemaVersion
			if j.Previous.Structured {
				current.BuildIdentity, current.VCSRevision = j.Previous.Build.BuildIdentity, j.Previous.Build.VCSRevision
			} else {
				current.BuildIdentity = j.Previous.LegacyVersion
			}
		}
		if err := writePrivateJSON(filepath.Join(m.controllerRoot(), "current-installation.json"), current, m.uid); err != nil {
			return err
		}
		if err := m.failAt("after_current_installation"); err != nil {
			return err
		}
		if err := m.removeCompletedBundle(bundle); err != nil {
			return err
		}
		result = resultFor(j, "cleaned", "completed_bundle_removed", "none")
		return nil
	})
	return result, err
}

type currentInstallation struct {
	SchemaVersion  int       `json:"schema_version"`
	UpgradeID      string    `json:"upgrade_id"`
	Supervisor     string    `json:"supervisor"`
	BinaryDigest   string    `json:"binary_digest"`
	BuildIdentity  string    `json:"build_identity"`
	VCSRevision    string    `json:"vcs_revision,omitempty"`
	DatabaseSchema int       `json:"database_schema_version"`
	VerifiedAt     time.Time `json:"verified_at"`
}

var cleanupArtifacts = map[string]bool{"journal.json": true, "candidate-manifest.json": true, "candidate.bin": true, "previous.bin": true, "snapshot.db": true}

func validateCleanupBundle(bundle string, uid int, requireComplete bool) error {
	entries, err := os.ReadDir(bundle)
	if err != nil {
		return errors.New("upgrade bundle is unavailable")
	}
	if requireComplete && len(entries) != len(cleanupArtifacts) {
		return errors.New("upgrade bundle contains unowned or missing artifacts")
	}
	for _, entry := range entries {
		if !cleanupArtifacts[entry.Name()] || entry.IsDir() {
			return errors.New("upgrade bundle contains unowned artifacts")
		}
		path := filepath.Join(bundle, entry.Name())
		info, stat, fileErr := safeRegularFile(path, uid, false)
		if fileErr != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
			return errors.New("upgrade bundle artifact is unsafe")
		}
	}
	return nil
}

func (m *Manager) removeCompletedBundle(bundle string) error {
	if err := validateCleanupBundle(bundle, m.uid, false); err != nil {
		return err
	}
	for _, name := range []string{"candidate-manifest.json", "candidate.bin", "previous.bin", "snapshot.db", "journal.json"} {
		if err := os.Remove(filepath.Join(bundle, name)); err != nil && !os.IsNotExist(err) {
			return errors.New("owned upgrade artifact cleanup failed")
		}
	}
	if err := os.Remove(bundle); err != nil && !os.IsNotExist(err) {
		return errors.New("owned upgrade bundle cleanup failed")
	}
	if err := os.Remove(m.activePath()); err != nil && !os.IsNotExist(err) {
		return errors.New("active upgrade pointer cleanup failed")
	}
	return syncDirectory(m.upgradeRoot())
}

func (m *Manager) finishInterruptedCleanup(id string) (bool, error) {
	var current currentInstallation
	if err := readPrivateJSON(filepath.Join(m.controllerRoot(), "current-installation.json"), m.uid, &current); err != nil || current.SchemaVersion != 1 || current.UpgradeID != id {
		return false, errors.New("cleanup recovery evidence is unavailable")
	}
	var active struct {
		UpgradeID string `json:"upgrade_id"`
	}
	if err := readPrivateJSON(m.activePath(), m.uid, &active); err != nil || active.UpgradeID != id {
		return false, errors.New("cleanup recovery active pointer is unavailable")
	}
	bundle := m.bundlePath(id)
	if exists(bundle) {
		if err := m.removeCompletedBundle(bundle); err != nil {
			return false, err
		}
	} else {
		if err := os.Remove(m.activePath()); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		if err := syncDirectory(m.upgradeRoot()); err != nil {
			return false, err
		}
	}
	return true, nil
}
