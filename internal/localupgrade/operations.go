package localupgrade

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func (m *Manager) Status(ctx context.Context, id string) (result Result, finalErr error) {
	err := m.withActiveLock(id, func() error {
		j, bundle, err := m.loadBundleJournal(id)
		if err != nil {
			return err
		}
		if j.Phase == "superseded" && !exists(m.activePath()) && !exists(m.bundlePath(j.SuccessorID)) {
			result = resultFor(j, "superseded", "verified_successor_linked", "none")
			return nil
		}
		active, err := m.readActiveUpgrade()
		if err != nil {
			return err
		}
		if j.Phase == "superseded" {
			next := "none"
			if active.UpgradeID == j.UpgradeID {
				next = "successor-prepare"
			} else if active.UpgradeID == j.SuccessorID {
				next = "status_successor"
			}
			result = resultFor(j, "superseded", "verified_successor_linked", next)
			return nil
		}
		if active.UpgradeID != id {
			return errors.New("upgrade is not the active managed bundle")
		}
		if j.Phase == "successor_prepare_intent" {
			result = resultFor(j, "successor_preparation_interrupted", "successor_prepare_intent_durable", "successor-prepare")
			return nil
		}
		result = m.reconcileStatus(ctx, j, bundle)
		return nil
	})
	return result, err
}

func (m *Manager) reconcileStatus(ctx context.Context, j journal, bundle string) Result {
	targetInfo, targetStat, err := safeRegularFile(j.BinaryPath, m.uid, true)
	if err != nil || targetStat.Nlink != 1 || uint32(targetInfo.Mode().Perm()) != j.Previous.Mode {
		return resultFor(j, "attention", "installed_binary_topology_failed", "preserve_bundle")
	}
	targetDigest, err := digestFile(j.BinaryPath)
	if err != nil {
		return resultFor(j, "attention", "installed_binary_unavailable", "preserve_bundle")
	}
	if !databaseStillMatches(j.DatabasePath, m.uid, j.Database) && j.Phase != "bootstrap_intent" && j.Phase != "healthy" && j.Phase != "attention" && j.Phase != "cleanup_intent" {
		return resultFor(j, "attention", "database_identity_or_schema_changed", "preserve_bundle")
	}
	topology := m.observeSupervisorTopology(ctx, j.Supervisor)
	if topology.Reason != "" {
		return resultFor(j, "attention", topology.Reason, "status")
	}
	switch j.Phase {
	case "prepared":
		if targetDigest != j.Previous.Digest {
			return resultFor(j, "attention", "installed_binary_drift", "preserve_bundle")
		}
		if topology.Selected.State == "absent" {
			return resultFor(j, "prepared", "candidate_verified_and_supervisor_absent", "replace")
		}
		return resultFor(j, "prepared", "candidate_verified", "bootout_selected_supervisor")
	case "replacement_intent":
		next := "replace"
		if topology.Selected.State != "absent" {
			next = "bootout_selected_supervisor"
		}
		if targetDigest == j.Candidate.Digest {
			return resultFor(j, "replacement_interrupted", "candidate_installed_after_replacement_intent", next)
		}
		if targetDigest == j.Previous.Digest {
			return resultFor(j, "replacement_interrupted", "previous_binary_intact", next)
		}
		return resultFor(j, "attention", "replacement_identity_ambiguous", "preserve_bundle")
	case "replacement_committed":
		if targetDigest != j.Candidate.Digest {
			return resultFor(j, "attention", "installed_candidate_drift", "preserve_bundle")
		}
		if topology.Selected.State != "absent" {
			return resultFor(j, "attention", "supervisor_started_without_bootstrap_intent", "preserve_bundle")
		}
		return resultFor(j, "replaced", "candidate_installed", "authorize-bootstrap")
	case "rollback_intent":
		next := "rollback"
		if topology.Selected.State != "absent" {
			next = "bootout_selected_supervisor"
		}
		if targetDigest == j.Previous.Digest {
			return resultFor(j, "rollback_interrupted", "previous_binary_restored_after_rollback_intent", next)
		}
		if targetDigest == j.Candidate.Digest {
			return resultFor(j, "rollback_interrupted", "candidate_binary_intact", next)
		}
		return resultFor(j, "attention", "rollback_identity_ambiguous", "preserve_bundle")
	case "bootstrap_intent":
		return m.postIntentStatus(ctx, j)
	case "healthy":
		observed := m.postIntentStatus(ctx, j)
		if observed.State == "observed_healthy" {
			return resultFor(j, "healthy", "upgrade_and_controller_ready", "cleanup")
		}
		return observed
	case "attention":
		observed := m.postIntentStatus(ctx, j)
		if observed.State == "observed_healthy" {
			return resultFor(j, "attention", "attention_recheck_available", "observe")
		}
		if observed.State == "attention" {
			return observed
		}
		return resultFor(j, "attention", j.FailureReason, "preserve_bundle")
	case "rolled_back":
		if topology.Selected.State == "running" {
			return resultFor(j, "rolled_back", "previous_binary_restored_and_supervisor_running", "observe")
		}
		return resultFor(j, "rolled_back", "previous_binary_restored", "bootstrap_selected_supervisor")
	case "rollback_healthy":
		return resultFor(j, "rollback_healthy", "restored_worker_verified", "cleanup")
	case "cleanup_intent":
		return resultFor(j, "cleanup_interrupted", "cleanup_intent_durable", "cleanup")
	default:
		return resultFor(j, "attention", "journal_phase_invalid", "preserve_bundle")
	}
}

func (m *Manager) postIntentStatus(ctx context.Context, j journal) Result {
	inspected, err := inspectBinary(ctx, m.runner, j.BinaryPath, m.uid)
	if err != nil || inspected.Digest != j.Candidate.Digest || !inspected.Structured || inspected.Build.BuildIdentity != j.Candidate.Build.BuildIdentity || inspected.Build.VCSRevision != j.Revision || inspected.Build.VCSModified {
		return resultFor(j, "attention", "installed_build_identity_failed", "preserve_bundle")
	}
	topology := m.observeSupervisorTopology(ctx, j.Supervisor)
	if topology.Reason != "" {
		return resultFor(j, "attention", topology.Reason, "preserve_bundle")
	}
	if topology.Selected.State != "running" || topology.Selected.PID < 1 {
		return resultFor(j, "attention", "selected_supervisor_not_running", "preserve_bundle")
	}
	heartbeat, reason := readHeartbeat(j.ConfigPath, m.uid)
	if reason != "" {
		return resultFor(j, "attention", reason, "preserve_bundle")
	}
	if heartbeat.ProcessID != topology.Selected.PID || heartbeat.BuildIdentity != j.Candidate.Build.BuildIdentity || heartbeat.ConfigurationDigest != j.ConfigDigest {
		return resultFor(j, "attention", "heartbeat_identity_failed", "preserve_bundle")
	}
	started, err := processStartIdentity(heartbeat.ProcessID)
	if err != nil || started != heartbeat.ProcessStartID {
		return resultFor(j, "attention", "process_start_identity_failed", "preserve_bundle")
	}
	age := m.now().Sub(heartbeat.ObservedAt)
	if age < -5*time.Second || age > 45*time.Second {
		return resultFor(j, "attention", "heartbeat_freshness_failed", "preserve_bundle")
	}
	readiness, reason := configurationAndIntegrityReadiness(ctx, j.DatabasePath, j.ConfigDigest, j.Candidate.Build.SupportedControllerSchemaVersion)
	switch readiness {
	case "ready":
		return resultFor(j, "observed_healthy", reason, "observe")
	case "pending":
		return resultFor(j, "pending", reason, "status")
	default:
		return resultFor(j, "attention", reason, "preserve_bundle")
	}
}

func (m *Manager) Replace(ctx context.Context, id string, fullBackupConfirmed bool) (result Result, finalErr error) {
	if !fullBackupConfirmed {
		return Result{}, errors.New("replacement requires a newly confirmed encrypted full backup")
	}
	err := m.withActiveLock(id, func() error {
		j, bundle, err := m.loadJournal(id)
		if err != nil {
			return err
		}
		if j.BootstrapIntentAt != nil || j.Phase != "prepared" && j.Phase != "replacement_intent" {
			return errors.New("replacement is not authorized in the current phase")
		}
		lock, err := acquireWorkerLock(j.DatabasePath, m.uid)
		if err != nil {
			return err
		}
		defer lock.Close()
		topology := m.observeSupervisorTopology(ctx, j.Supervisor)
		if topology.Reason != "" || topology.Selected.State != "absent" {
			return errors.New("selected supervisor is not absent and fenced")
		}
		if !databaseStillMatches(j.DatabasePath, m.uid, j.Database) || !configDigestMatches(j.ConfigPath, j.ConfigDigest) {
			return errors.New("journal-bound database or configuration evidence changed")
		}
		candidatePath, previousPath := filepath.Join(bundle, "candidate.bin"), filepath.Join(bundle, "previous.bin")
		if !privateArtifactMatches(candidatePath, m.uid, j.Candidate.Digest) || !privateArtifactMatches(previousPath, m.uid, j.Previous.Digest) {
			return errors.New("upgrade binary artifacts do not match the journal")
		}
		targetInfo, targetStat, targetErr := safeRegularFile(j.BinaryPath, m.uid, true)
		if targetErr != nil || targetStat.Nlink != 1 || uint32(targetInfo.Mode().Perm()) != j.Previous.Mode {
			return errors.New("installed target topology is unsafe")
		}
		targetDigest, digestErr := digestFile(j.BinaryPath)
		if digestErr != nil {
			return errors.New("installed target is unavailable")
		}
		if j.Phase == "replacement_intent" && targetDigest == j.Candidate.Digest {
			j.Phase, j.UpdatedAt = "replacement_committed", m.now()
			if err := writeJournal(bundle, j, m.uid); err != nil {
				return err
			}
			result = resultFor(j, "replaced", "candidate_installed", "authorize-bootstrap")
			return nil
		}
		if targetDigest != j.Previous.Digest {
			return errors.New("installed binary drift blocks replacement")
		}
		snapshotPath := filepath.Join(bundle, "snapshot.db")
		if j.SnapshotDigest == "" {
			digest, err := createConsistentSnapshot(ctx, j.DatabasePath, snapshotPath, m.uid, j.Database)
			if err != nil {
				return err
			}
			j.SnapshotDigest, j.UpdatedAt = digest, m.now()
			if err := writeJournal(bundle, j, m.uid); err != nil {
				return err
			}
			if err := m.failAt("after_snapshot_journal"); err != nil {
				return err
			}
		} else if !privateArtifactMatches(snapshotPath, m.uid, j.SnapshotDigest) {
			return errors.New("SQLite snapshot evidence is inconsistent")
		}
		j.Phase, j.UpdatedAt = "replacement_intent", m.now()
		if err := writeJournal(bundle, j, m.uid); err != nil {
			return err
		}
		if err := m.failAt("after_replacement_intent"); err != nil {
			return err
		}
		if err := atomicallyInstall(candidatePath, j.BinaryPath, os.FileMode(j.Previous.Mode), m.uid, j.Previous.Digest, j.Candidate.Digest, j.UpgradeID); err != nil {
			return err
		}
		if err := m.failAt("after_binary_replacement"); err != nil {
			return err
		}
		j.Phase, j.UpdatedAt = "replacement_committed", m.now()
		if err := writeJournal(bundle, j, m.uid); err != nil {
			return err
		}
		result = resultFor(j, "replaced", "candidate_installed", "authorize-bootstrap")
		return nil
	})
	return result, err
}

func (m *Manager) Rollback(ctx context.Context, id string) (result Result, finalErr error) {
	err := m.withActiveLock(id, func() error {
		j, bundle, err := m.loadJournal(id)
		if err != nil {
			return err
		}
		if j.BootstrapIntentAt != nil {
			return errors.New("rollback is permanently forbidden after bootstrap intent")
		}
		if j.Phase != "replacement_committed" && j.Phase != "rollback_intent" {
			return errors.New("rollback is unavailable in the current phase")
		}
		lock, err := acquireWorkerLock(j.DatabasePath, m.uid)
		if err != nil {
			return err
		}
		defer lock.Close()
		topology := m.observeSupervisorTopology(ctx, j.Supervisor)
		if topology.Reason != "" || topology.Selected.State != "absent" || !databaseStillMatches(j.DatabasePath, m.uid, j.Database) {
			return errors.New("rollback database or supervisor authority is unavailable")
		}
		previousPath := filepath.Join(bundle, "previous.bin")
		if !privateArtifactMatches(previousPath, m.uid, j.Previous.Digest) {
			return errors.New("previous binary evidence is unavailable")
		}
		targetInfo, targetStat, targetErr := safeRegularFile(j.BinaryPath, m.uid, true)
		if targetErr != nil || targetStat.Nlink != 1 || uint32(targetInfo.Mode().Perm()) != j.Previous.Mode {
			return errors.New("installed binary topology changed")
		}
		current, digestErr := digestFile(j.BinaryPath)
		if digestErr != nil || current != j.Candidate.Digest && current != j.Previous.Digest {
			return errors.New("installed candidate identity changed")
		}
		if current == j.Candidate.Digest {
			j.Phase, j.UpdatedAt = "rollback_intent", m.now()
			if err := writeJournal(bundle, j, m.uid); err != nil {
				return err
			}
			if err := m.failAt("after_rollback_intent"); err != nil {
				return err
			}
			if err := atomicallyInstall(previousPath, j.BinaryPath, os.FileMode(j.Previous.Mode), m.uid, j.Candidate.Digest, j.Previous.Digest, j.UpgradeID); err != nil {
				return err
			}
			if err := m.failAt("after_binary_rollback"); err != nil {
				return err
			}
		}
		j.Phase, j.UpdatedAt = "rolled_back", m.now()
		if err := writeJournal(bundle, j, m.uid); err != nil {
			return err
		}
		result = resultFor(j, "rolled_back", "previous_binary_restored", "bootstrap_selected_supervisor")
		return nil
	})
	return result, err
}

func (m *Manager) AuthorizeBootstrap(ctx context.Context, id string) (result Result, finalErr error) {
	err := m.withActiveLock(id, func() error {
		j, bundle, err := m.loadJournal(id)
		if err != nil {
			return err
		}
		if j.Phase != "replacement_committed" || j.BootstrapIntentAt != nil {
			return errors.New("bootstrap authorization is unavailable in the current phase")
		}
		lock, err := acquireWorkerLock(j.DatabasePath, m.uid)
		if err != nil {
			return err
		}
		defer lock.Close()
		topology := m.observeSupervisorTopology(ctx, j.Supervisor)
		if topology.Reason != "" || topology.Selected.State != "absent" || !databaseStillMatches(j.DatabasePath, m.uid, j.Database) || !configDigestMatches(j.ConfigPath, j.ConfigDigest) {
			return errors.New("bootstrap authority could not be revalidated")
		}
		installed, err := inspectBinary(ctx, m.runner, j.BinaryPath, m.uid)
		if err != nil || installed.Digest != j.Candidate.Digest || !installed.Structured || installed.Build.BuildIdentity != j.Candidate.Build.BuildIdentity || installed.Build.VCSRevision != j.Revision || installed.Build.VCSModified {
			return errors.New("installed candidate identity changed")
		}
		now := m.now()
		j.Phase, j.BootstrapIntentAt, j.UpdatedAt = "bootstrap_intent", &now, now
		if err := writeJournal(bundle, j, m.uid); err != nil {
			return err
		}
		if err := m.failAt("after_bootstrap_intent"); err != nil {
			return err
		}
		instruction := []string{j.BinaryPath, "controller", j.Supervisor, "bootstrap", "--binary", j.BinaryPath, "--config", j.ConfigPath}
		result = resultFor(j, "bootstrap_authorized", "bootstrap_intent_durable", "bootstrap_selected_supervisor")
		if j.Supervisor == "launchdaemon" {
			instruction = append(instruction, "--user", m.user)
			result.RequiresSudo = true
		}
		result.BootstrapInstruction = instruction
		return nil
	})
	return result, err
}

func atomicallyInstall(source, target string, mode os.FileMode, uid int, expectedCurrentDigest, expectedDigest, id string) error {
	parent := filepath.Dir(target)
	temporary := filepath.Join(parent, ".agentctl-"+id+".tmp")
	if exists(temporary) {
		return errors.New("replacement temporary target already exists")
	}
	sourceFile, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("replacement source is unavailable")
	}
	defer sourceFile.Close()
	targetFile, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("replacement temporary target could not be created")
	}
	remove := true
	defer func() {
		_ = targetFile.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := io.Copy(targetFile, io.LimitReader(sourceFile, 1<<30)); err != nil || targetFile.Chmod(mode.Perm()) != nil || targetFile.Sync() != nil || targetFile.Close() != nil {
		return errors.New("replacement temporary target could not be synchronized")
	}
	info, stat, err := safeRegularFile(temporary, uid, true)
	if err != nil || stat.Nlink != 1 || info.Mode().Perm() != mode.Perm() {
		return errors.New("replacement temporary target is unsafe")
	}
	if digest, err := digestFile(temporary); err != nil || digest != expectedDigest {
		return errors.New("replacement temporary target digest mismatch")
	}
	currentInfo, currentStat, currentErr := safeRegularFile(target, uid, true)
	if currentErr != nil || currentStat.Nlink != 1 || currentInfo.Mode().Perm() != mode.Perm() {
		return errors.New("installed target changed before atomic replacement")
	}
	if digest, err := digestFile(target); err != nil || digest != expectedCurrentDigest {
		return errors.New("installed target drifted before atomic replacement")
	}
	if err := os.Rename(temporary, target); err != nil || syncDirectory(parent) != nil {
		return errors.New("atomic binary replacement failed")
	}
	remove = false
	info, stat, err = safeRegularFile(target, uid, true)
	if err != nil || stat.Nlink != 1 || info.Mode().Perm() != mode.Perm() {
		return errors.New("installed binary verification failed")
	}
	if digest, err := digestFile(target); err != nil || digest != expectedDigest {
		return errors.New("installed binary identity verification failed")
	}
	return nil
}

func privateArtifactMatches(path string, uid int, digest string) bool {
	info, stat, err := safeRegularFile(path, uid, false)
	if err != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
		return false
	}
	actual, err := digestFile(path)
	return err == nil && actual == digest
}

func configDigestMatches(path, expected string) bool {
	actual, err := digestFile(path)
	return err == nil && actual == expected
}
