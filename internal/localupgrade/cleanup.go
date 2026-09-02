package localupgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

func (m *Manager) Cleanup(_ context.Context, id string) (result Result, finalErr error) {
	err := m.withActiveLock(id, func() error {
		if err := m.admitHistoricalRecoveryMutation(id, historicalRecoveryCleanup); err != nil {
			return err
		}
		active, err := m.cleanupActivePresent(id)
		if err != nil {
			return err
		}
		bundle, err := m.cleanupBundlePresent(id)
		if err != nil {
			return err
		}
		switch {
		case active && bundle:
			j, bundlePath, err := m.prepareCleanupCommit(id)
			if err != nil {
				return err
			}
			if err := m.commitCleanupPointer(id); err != nil {
				return err
			}
			if err := m.reclaimCleanupBundle(bundlePath); err != nil {
				return err
			}
			result = resultFor(j, "cleaned", "completed_bundle_removed", "none")
			return nil
		case !active && bundle:
			current, err := m.readCurrentInstallation(id)
			if err != nil {
				return err
			}
			bundlePath := m.bundlePath(id)
			if err := m.validateCleanupArtifacts(bundlePath, false); err != nil {
				return err
			}
			if err := syncDirectory(m.upgradeRoot()); err != nil {
				return err
			}
			if err := m.failAt("after_cleanup_active_sync"); err != nil {
				return err
			}
			if err := m.reclaimCleanupBundle(bundlePath); err != nil {
				return err
			}
			result = cleanedResult(current)
			return nil
		case active && !bundle:
			current, err := m.readCurrentInstallation(id)
			if err != nil {
				return err
			}
			if err := m.validateLegacyBundleMissingCleanup(current); err != nil {
				return err
			}
			if err := m.commitCleanupPointer(id); err != nil {
				return err
			}
			result = cleanedResult(current)
			return nil
		default:
			current, err := m.readCurrentInstallation(id)
			if err != nil {
				return err
			}
			if err := syncDirectory(m.upgradeRoot()); err != nil {
				return err
			}
			result = cleanedResult(current)
			return nil
		}
	})
	return result, err
}

func cleanedResult(current currentInstallation) Result {
	return Result{UpgradeID: current.UpgradeID, State: "cleaned", Reason: "completed_bundle_removed", NextAction: "none", UpgradeHealth: "healthy", ControllerReadiness: "ready", Supervisor: current.Supervisor}
}

func (m *Manager) prepareCleanupCommit(id string) (journal, string, error) {
	j, bundle, err := m.loadBundleJournal(id)
	if err != nil {
		return journal{}, "", err
	}
	if j.Phase != "healthy" && j.Phase != "rollback_healthy" && j.Phase != "cleanup_intent" || j.CompletedAt == nil {
		return journal{}, "", errors.New("cleanup requires a verified healthy completion")
	}
	if j.CompletedAt.IsZero() || j.CompletedAt.Before(j.CreatedAt) || j.Phase == "healthy" && j.BootstrapIntentAt == nil || j.Phase == "rollback_healthy" && j.BootstrapIntentAt != nil {
		return journal{}, "", errors.New("cleanup completion authority is contradictory")
	}
	if err := m.validateCompleteCleanupBundle(bundle, j); err != nil {
		return journal{}, "", err
	}
	if err := m.validateExactCleanupLineage(j); err != nil {
		return journal{}, "", err
	}
	if j.Phase != "cleanup_intent" {
		j.Phase, j.UpdatedAt = "cleanup_intent", m.now()
		if err := writeJournal(bundle, j, m.uid); err != nil {
			return journal{}, "", err
		}
		if err := m.failAt("after_cleanup_intent"); err != nil {
			return journal{}, "", err
		}
	}
	expected := currentInstallationFor(j)
	if err := m.publishCurrentInstallation(expected); err != nil {
		return journal{}, "", err
	}
	if err := m.failAt("after_current_installation"); err != nil {
		return journal{}, "", err
	}
	if err := m.validateCompleteCleanupBundle(bundle, j); err != nil {
		return journal{}, "", err
	}
	if err := m.validateExactCleanupLineage(j); err != nil {
		return journal{}, "", err
	}
	if current, err := m.readCurrentInstallation(id); err != nil || current != expected {
		return journal{}, "", errors.New("current installation cleanup evidence changed")
	}
	return j, bundle, nil
}

func currentInstallationFor(j journal) currentInstallation {
	current := currentInstallation{SchemaVersion: 1, UpgradeID: j.UpgradeID, Supervisor: j.Supervisor, VerifiedAt: *j.CompletedAt}
	rollback := j.Phase == "rollback_healthy" || j.Phase == "cleanup_intent" && j.BootstrapIntentAt == nil
	if !rollback {
		current.BinaryDigest, current.BuildIdentity, current.VCSRevision, current.DatabaseSchema = j.Candidate.Digest, j.Candidate.Build.BuildIdentity, j.Revision, j.Candidate.Build.SupportedControllerSchemaVersion
		return current
	}
	current.BinaryDigest, current.DatabaseSchema = j.Previous.Digest, j.Database.SchemaVersion
	if j.Previous.Structured {
		current.BuildIdentity, current.VCSRevision = j.Previous.Build.BuildIdentity, j.Previous.Build.VCSRevision
	} else {
		current.BuildIdentity = j.Previous.LegacyVersion
	}
	return current
}

func (m *Manager) publishCurrentInstallation(expected currentInstallation) error {
	path := filepath.Join(m.controllerRoot(), "current-installation.json")
	if _, err := os.Lstat(path); err == nil {
		var existing currentInstallation
		if err := readPrivateJSON(path, m.uid, &existing); err != nil || validateCurrentInstallation(existing) != nil {
			return errors.New("existing current installation evidence is invalid")
		}
		if existing.UpgradeID == expected.UpgradeID && existing != expected {
			return errors.New("existing current installation evidence conflicts with cleanup")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("current installation evidence is unavailable")
	}
	if err := writePrivateJSON(path, expected, m.uid); err != nil {
		return err
	}
	actual, err := m.readCurrentInstallation(expected.UpgradeID)
	if err != nil || actual != expected {
		return errors.New("current installation evidence could not be verified")
	}
	return nil
}

func (m *Manager) readCurrentInstallation(id string) (currentInstallation, error) {
	var current currentInstallation
	if err := readPrivateJSON(filepath.Join(m.controllerRoot(), "current-installation.json"), m.uid, &current); err != nil || validateCurrentInstallation(current) != nil || current.UpgradeID != id {
		return currentInstallation{}, errors.New("cleanup current installation evidence is unavailable")
	}
	return current, nil
}

func validateCurrentInstallation(current currentInstallation) error {
	if current.SchemaVersion != 1 || !validUpgradeID(current.UpgradeID) || current.Supervisor != "launchagent" && current.Supervisor != "launchdaemon" || !validSHA256(current.BinaryDigest) || current.BuildIdentity == "" || len(current.BuildIdentity) > 512 || current.DatabaseSchema <= 0 || current.VerifiedAt.IsZero() || current.VCSRevision != "" && !validRevision(current.VCSRevision) {
		return errors.New("current installation evidence is invalid")
	}
	if current.VCSRevision != "" && (!strings.HasPrefix(current.BuildIdentity, "sha256:") || !validSHA256(strings.TrimPrefix(current.BuildIdentity, "sha256:"))) || current.VCSRevision == "" && (len(current.BuildIdentity) > 128 || strings.ContainsAny(current.BuildIdentity, "\r\n\x00")) {
		return errors.New("current installation build identity is invalid")
	}
	return nil
}

func (m *Manager) cleanupActivePresent(id string) (bool, error) {
	if _, err := os.Lstat(m.activePath()); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, errors.New("active upgrade pointer is unavailable")
	}
	active, err := m.readActiveUpgrade()
	if err != nil || active.UpgradeID != id {
		return false, errors.New("active upgrade pointer conflicts with cleanup")
	}
	return true, nil
}

func (m *Manager) cleanupBundlePresent(id string) (bool, error) {
	path := m.bundlePath(id)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, errors.New("upgrade bundle is unavailable")
	}
	if err := validatePrivateDirectory(path, m.uid); err != nil {
		return false, errors.New("upgrade bundle is unsafe")
	}
	return true, nil
}

func (m *Manager) commitCleanupPointer(id string) error {
	active, err := m.readActiveUpgrade()
	if err != nil || active.UpgradeID != id {
		return errors.New("exact active upgrade pointer is unavailable")
	}
	info, stat, err := safeRegularFile(m.activePath(), m.uid, false)
	if err != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
		return errors.New("exact active upgrade pointer is unsafe")
	}
	if err := os.Remove(m.activePath()); err != nil {
		return errors.New("active upgrade pointer cleanup failed")
	}
	if err := m.failAt("after_cleanup_active_unlink"); err != nil {
		return err
	}
	if err := syncDirectory(m.upgradeRoot()); err != nil {
		return err
	}
	return m.failAt("after_cleanup_active_sync")
}

func (m *Manager) validateCompleteCleanupBundle(bundle string, j journal) error {
	if err := m.validateCleanupArtifacts(bundle, true); err != nil {
		return err
	}
	var manifest candidateManifest
	if err := readPrivateJSON(filepath.Join(bundle, "candidate-manifest.json"), m.uid, &manifest); err != nil || manifest.SchemaVersion != j.SchemaVersion || manifest.Revision != j.Revision || manifest.Candidate != j.Candidate || manifest.Previous != j.Previous || manifest.Database != j.Database || manifest.ConfigDigest != j.ConfigDigest || manifest.PreparedAt != j.CreatedAt {
		return errors.New("cleanup candidate manifest is inconsistent")
	}
	if !m.cleanupBinaryArtifactMatches(filepath.Join(bundle, "candidate.bin"), j.Candidate) || !m.cleanupBinaryArtifactMatches(filepath.Join(bundle, "previous.bin"), j.Previous) || !validSHA256(j.SnapshotDigest) || !privateArtifactMatches(filepath.Join(bundle, "snapshot.db"), m.uid, j.SnapshotDigest) {
		return errors.New("cleanup bundle artifact evidence is inconsistent")
	}
	return nil
}

func (m *Manager) cleanupBinaryArtifactMatches(path string, evidence binaryEvidence) bool {
	info, _, err := safeRegularFile(path, m.uid, false)
	return err == nil && evidence.Size > 0 && info.Size() == evidence.Size && validSHA256(evidence.Digest) && privateArtifactMatches(path, m.uid, evidence.Digest)
}

func (m *Manager) validateCleanupArtifacts(bundle string, complete bool) error {
	if err := validatePrivateDirectory(bundle, m.uid); err != nil {
		return errors.New("upgrade bundle is unsafe")
	}
	entries, err := os.ReadDir(bundle)
	if err != nil {
		return errors.New("upgrade bundle is unavailable")
	}
	if complete && len(entries) != len(cleanupArtifacts) {
		return errors.New("upgrade bundle contains unowned or missing artifacts")
	}
	for _, entry := range entries {
		path := filepath.Join(bundle, entry.Name())
		info, stat, fileErr := safeRegularFile(path, m.uid, false)
		if !cleanupArtifacts[entry.Name()] || entry.IsDir() || fileErr != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
			return errors.New("upgrade bundle contains unsafe or unowned artifacts")
		}
	}
	return nil
}

func (m *Manager) validateExactCleanupLineage(j journal) error {
	retained, err := m.retainedCleanupJournals(j.UpgradeID)
	if err != nil {
		return err
	}
	var claims []journal
	for _, candidate := range retained {
		if candidate.PredecessorID == j.UpgradeID {
			return errors.New("cleanup lineage has an unexpected successor")
		}
		if candidate.SuccessorID == j.UpgradeID {
			claims = append(claims, candidate)
		}
	}
	if j.PredecessorID == "" {
		if len(claims) != 0 {
			return errors.New("cleanup lineage contains an unbound predecessor")
		}
		return nil
	}
	if len(claims) != 1 || claims[0].UpgradeID != j.PredecessorID || !exactSuccessorRelation(claims[0], j) {
		return errors.New("cleanup predecessor lineage is unavailable or ambiguous")
	}
	return nil
}

func exactSuccessorRelation(predecessor, successor journal) bool {
	return predecessor.Phase == "superseded" && predecessor.SuccessorID == successor.UpgradeID && predecessor.SuccessorRevision == successor.Revision && successor.PredecessorID == predecessor.UpgradeID && predecessor.Supervisor == successor.Supervisor && predecessor.BinaryPath == successor.BinaryPath && predecessor.ConfigPath == successor.ConfigPath && predecessor.DatabasePath == successor.DatabasePath && predecessor.ConfigDigest == successor.ConfigDigest && predecessor.Database == successor.Database && successor.Previous.Digest == predecessor.Candidate.Digest && distinctSuccessorCandidate(predecessor.Revision, predecessor.Candidate, successor.Revision, successor.Candidate)
}

func distinctSuccessorCandidate(predecessorRevision string, predecessor binaryEvidence, successorRevision string, successor binaryEvidence) bool {
	return predecessorRevision != successorRevision && predecessor.Build.VCSRevision == predecessorRevision && successor.Build.VCSRevision == successorRevision && predecessor.Digest != successor.Digest && predecessor.Build.BuildIdentity != successor.Build.BuildIdentity
}

func (m *Manager) retainedCleanupJournals(excludeID string) ([]journal, error) {
	entries, err := os.ReadDir(m.upgradeRoot())
	if err != nil {
		return nil, errors.New("retained cleanup evidence is unavailable")
	}
	retained := make([]journal, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			if name != "active.json" && name != "upgrade.lock" {
				return nil, errors.New("retained cleanup evidence contains an unowned entry")
			}
			continue
		}
		if name == excludeID {
			continue
		}
		if !validUpgradeID(name) {
			return nil, errors.New("retained cleanup evidence contains an invalid bundle")
		}
		j, _, err := m.loadBundleJournal(name)
		if err != nil {
			return nil, errors.New("retained cleanup journal is invalid")
		}
		retained = append(retained, j)
	}
	return retained, nil
}

func (m *Manager) validateLegacyBundleMissingCleanup(current currentInstallation) error {
	retained, err := m.retainedCleanupJournals(current.UpgradeID)
	if err != nil {
		return err
	}
	var claim *journal
	for index := range retained {
		candidate := retained[index]
		if candidate.DatabaseRecovery != nil || candidate.Phase != "superseded" {
			return errors.New("legacy cleanup has recovery-bearing or unresolved retained evidence; replace the complete runtime")
		}
		if candidate.PredecessorID == current.UpgradeID {
			return errors.New("legacy cleanup retained relation is ambiguous; replace the complete runtime")
		}
		if candidate.SuccessorID == current.UpgradeID {
			if claim != nil {
				return errors.New("legacy cleanup has multiple predecessor claims; replace the complete runtime")
			}
			copy := candidate
			claim = &copy
		}
	}
	if claim != nil && (claim.SuccessorRevision != current.VCSRevision || claim.Revision == current.VCSRevision || claim.Candidate.Build.VCSRevision != claim.Revision || claim.Candidate.Digest == current.BinaryDigest || claim.Candidate.Build.BuildIdentity == current.BuildIdentity || claim.Supervisor != current.Supervisor || claim.SupersededAt == nil || current.VerifiedAt.Before(*claim.SupersededAt)) {
		return errors.New("legacy cleanup predecessor relation cannot be proven; replace the complete runtime")
	}
	return nil
}

func (m *Manager) reclaimCleanupBundle(bundle string) error {
	if err := m.validateCleanupArtifacts(bundle, false); err != nil {
		return err
	}
	for _, name := range []string{"candidate-manifest.json", "candidate.bin", "previous.bin", "snapshot.db", "journal.json"} {
		path := filepath.Join(bundle, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return errors.New("owned upgrade artifact cleanup failed")
		}
		info, stat, err := safeRegularFile(path, m.uid, false)
		if err != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
			return errors.New("owned upgrade artifact became unsafe")
		}
		if err := os.Remove(path); err != nil || syncDirectory(bundle) != nil {
			return errors.New("owned upgrade artifact cleanup failed")
		}
		point := "after_cleanup_artifact_" + strings.ReplaceAll(strings.TrimSuffix(name, filepath.Ext(name)), "-", "_")
		if err := m.failAt(point); err != nil {
			return err
		}
	}
	if err := os.Remove(bundle); err != nil && !os.IsNotExist(err) {
		return errors.New("owned upgrade bundle cleanup failed")
	}
	if err := m.failAt("after_cleanup_bundle_removal"); err != nil {
		return err
	}
	return syncDirectory(m.upgradeRoot())
}
