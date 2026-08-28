package localupgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

func (m *Manager) PrepareSuccessor(ctx context.Context, request SuccessorPrepareRequest) (result Result, finalErr error) {
	if !validUpgradeID(request.PredecessorUpgradeID) || !validRevision(request.Revision) {
		return Result{}, errors.New("successor-prepare requires a predecessor upgrade identifier and exact revision")
	}
	err := m.withActiveLock(request.PredecessorUpgradeID, func() error {
		predecessor, predecessorBundle, err := m.loadBundleJournal(request.PredecessorUpgradeID)
		if err != nil {
			return err
		}
		active, err := m.readActiveUpgrade()
		if err != nil {
			return err
		}
		if predecessor.Phase == "superseded" {
			return m.resumeSuccessorActivation(predecessor, request.Revision, active, &result)
		}
		if active.UpgradeID != predecessor.UpgradeID {
			return errors.New("predecessor is not the active managed upgrade")
		}
		var database databaseEvidence
		var previous binaryEvidence
		switch predecessor.Phase {
		case "attention":
			if request.Revision == predecessor.Revision {
				return errors.New("successor revision must differ from the predecessor revision")
			}
			database, previous, err = m.validateSuccessorEligibility(ctx, predecessor)
			if err != nil {
				return err
			}
			if _, err := resolveCandidateSource(ctx, m.runner, request.Revision); err != nil {
				return err
			}
			predecessor.SchemaVersion = journalSchemaVersion
			predecessor.Phase = "successor_prepare_intent"
			predecessor.SuccessorID = newUpgradeID()
			predecessor.SuccessorRevision = request.Revision
			predecessor.UpdatedAt = m.now()
			if err := writeJournal(predecessorBundle, predecessor, m.uid); err != nil {
				return err
			}
			if err := m.failAt("after_successor_prepare_intent"); err != nil {
				return err
			}
		case "successor_prepare_intent":
			if predecessor.SuccessorRevision != request.Revision {
				return errors.New("successor preparation is already bound to another revision")
			}
			database, previous, err = m.validateSuccessorEligibility(ctx, predecessor)
			if err != nil {
				return err
			}
		default:
			return errors.New("successor preparation is unavailable in the current phase")
		}

		successorBundle := m.bundlePath(predecessor.SuccessorID)
		if exists(successorBundle) {
			if _, err := m.validatePreparedSuccessor(predecessor, successorBundle); err != nil {
				return err
			}
		} else {
			prepared, err := m.prepareVerifiedCandidate(ctx, request.Revision, database.SchemaVersion)
			if err != nil {
				return err
			}
			defer prepared.Cleanup()
			if err := m.stageSuccessorBundle(predecessor, database, previous, prepared); err != nil {
				return err
			}
		}
		if err := m.failAt("after_successor_bundle"); err != nil {
			return err
		}
		revalidatedDatabase, revalidatedPrevious, err := m.validateSuccessorEligibility(ctx, predecessor)
		if err != nil || revalidatedDatabase != database || revalidatedPrevious.Digest != previous.Digest {
			return errors.New("successor eligibility changed before activation")
		}

		now := m.now()
		predecessor.Phase, predecessor.SupersededAt, predecessor.UpdatedAt = "superseded", &now, now
		if err := writeJournal(predecessorBundle, predecessor, m.uid); err != nil {
			return err
		}
		if err := m.failAt("after_predecessor_superseded"); err != nil {
			return err
		}
		if err := m.writeActiveUpgrade(predecessor.SuccessorID); err != nil {
			return err
		}
		if err := m.failAt("after_successor_activation"); err != nil {
			return err
		}
		successor, _, err := m.loadBundleJournal(predecessor.SuccessorID)
		if err != nil {
			return err
		}
		result = resultFor(successor, "prepared", "verified_successor_activated", "bootout_selected_supervisor")
		return nil
	})
	return result, err
}

func newUpgradeID() string {
	return "upgrade-" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func (m *Manager) validateSuccessorEligibility(ctx context.Context, predecessor journal) (databaseEvidence, binaryEvidence, error) {
	if predecessor.BootstrapIntentAt == nil || !eligibleSuccessorReason(predecessor.FailureReason) {
		return databaseEvidence{}, binaryEvidence{}, errors.New("active attention is not eligible for a managed successor")
	}
	observed := m.postIntentStatus(ctx, predecessor)
	if observed.State != "attention" || observed.UpgradeHealth != "healthy" || observed.ControllerReadiness != "not_ready" || observed.Reason != predecessor.FailureReason {
		return databaseEvidence{}, binaryEvidence{}, errors.New("successor eligibility could not be revalidated")
	}
	if !configDigestMatches(predecessor.ConfigPath, predecessor.ConfigDigest) {
		return databaseEvidence{}, binaryEvidence{}, errors.New("successor configuration evidence changed")
	}
	installed, err := inspectBinary(ctx, m.runner, predecessor.BinaryPath, m.uid)
	if err != nil || installed.Digest != predecessor.Candidate.Digest || !installed.Structured || installed.Build.BuildIdentity != predecessor.Candidate.Build.BuildIdentity || installed.Build.VCSRevision != predecessor.Revision || installed.Build.VCSModified {
		return databaseEvidence{}, binaryEvidence{}, errors.New("successor installed candidate identity is unavailable")
	}
	database, err := inspectDatabaseReadOnly(predecessor.DatabasePath, m.uid)
	if err != nil || database.Device != predecessor.Database.Device || database.Inode != predecessor.Database.Inode || database.SchemaVersion != predecessor.Candidate.Build.SupportedControllerSchemaVersion {
		return databaseEvidence{}, binaryEvidence{}, errors.New("successor database topology is unavailable")
	}
	return database, installed, nil
}

func (m *Manager) stageSuccessorBundle(predecessor journal, database databaseEvidence, previous binaryEvidence, prepared preparedCandidate) error {
	if !candidateCompatible(prepared.Evidence, predecessor.SuccessorRevision, database.SchemaVersion) {
		return errors.New("successor candidate build identity is unverifiable or incompatible")
	}
	finalBundle := m.bundlePath(predecessor.SuccessorID)
	stagingBundle := filepath.Join(m.upgradeRoot(), "."+predecessor.SuccessorID+".prepare")
	if exists(stagingBundle) {
		if err := removePartialSuccessorStaging(stagingBundle, m.uid); err != nil {
			return err
		}
	}
	if err := os.Mkdir(stagingBundle, 0o700); err != nil {
		return errors.New("successor bundle staging could not be created")
	}
	remove := true
	defer func() {
		if remove {
			_ = removePartialSuccessorStaging(stagingBundle, m.uid)
		}
	}()
	if !sameFilesystem(stagingBundle, predecessor.BinaryPath) {
		return errors.New("successor candidate cannot be staged on the installed target filesystem")
	}
	if err := copyPrivateArtifact(prepared.Path, filepath.Join(stagingBundle, "candidate.bin"), m.uid); err != nil {
		return err
	}
	if err := copyPrivateArtifact(predecessor.BinaryPath, filepath.Join(stagingBundle, "previous.bin"), m.uid); err != nil {
		return err
	}
	now := m.now()
	manifest := candidateManifest{SchemaVersion: journalSchemaVersion, Revision: predecessor.SuccessorRevision, Candidate: prepared.Evidence, Previous: previous, Database: database, ConfigDigest: predecessor.ConfigDigest, PreparedAt: now}
	if err := writePrivateJSON(filepath.Join(stagingBundle, "candidate-manifest.json"), manifest, m.uid); err != nil {
		return err
	}
	successor := journal{SchemaVersion: journalSchemaVersion, UpgradeID: predecessor.SuccessorID, Phase: "prepared", Supervisor: predecessor.Supervisor, Revision: predecessor.SuccessorRevision, BinaryPath: predecessor.BinaryPath, ConfigPath: predecessor.ConfigPath, DatabasePath: predecessor.DatabasePath, ConfigDigest: predecessor.ConfigDigest, Candidate: prepared.Evidence, Previous: previous, Database: database, PredecessorID: predecessor.UpgradeID, CreatedAt: now, UpdatedAt: now}
	if err := writeJournal(stagingBundle, successor, m.uid); err != nil {
		return err
	}
	if _, err := m.validatePreparedSuccessor(predecessor, stagingBundle); err != nil {
		return err
	}
	if exists(finalBundle) || os.Rename(stagingBundle, finalBundle) != nil || syncDirectory(m.upgradeRoot()) != nil {
		return errors.New("successor bundle could not be published")
	}
	remove = false
	return nil
}

func (m *Manager) validatePreparedSuccessor(predecessor journal, bundle string) (journal, error) {
	entries, err := os.ReadDir(bundle)
	if err != nil || len(entries) != 4 {
		return journal{}, errors.New("successor bundle is incomplete")
	}
	allowed := map[string]bool{"candidate.bin": true, "previous.bin": true, "candidate-manifest.json": true, "journal.json": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] || entry.IsDir() {
			return journal{}, errors.New("successor bundle contains unowned artifacts")
		}
	}
	var successor journal
	if err := readPrivateJSON(filepath.Join(bundle, "journal.json"), m.uid, &successor); err != nil || validateJournal(successor, predecessor.SuccessorID) != nil {
		return journal{}, errors.New("successor journal is unavailable")
	}
	if successor.Phase != "prepared" || successor.PredecessorID != predecessor.UpgradeID || successor.Revision != predecessor.SuccessorRevision || successor.Supervisor != predecessor.Supervisor || successor.BinaryPath != predecessor.BinaryPath || successor.ConfigPath != predecessor.ConfigPath || successor.DatabasePath != predecessor.DatabasePath || successor.ConfigDigest != predecessor.ConfigDigest || successor.Previous.Digest != predecessor.Candidate.Digest {
		return journal{}, errors.New("successor linkage is inconsistent")
	}
	if !privateArtifactMatches(filepath.Join(bundle, "candidate.bin"), m.uid, successor.Candidate.Digest) || !privateArtifactMatches(filepath.Join(bundle, "previous.bin"), m.uid, successor.Previous.Digest) {
		return journal{}, errors.New("successor binary evidence is inconsistent")
	}
	var manifest candidateManifest
	if err := readPrivateJSON(filepath.Join(bundle, "candidate-manifest.json"), m.uid, &manifest); err != nil || manifest.SchemaVersion != journalSchemaVersion || manifest.Revision != successor.Revision || manifest.Candidate != successor.Candidate || manifest.Previous != successor.Previous || manifest.Database != successor.Database || manifest.ConfigDigest != successor.ConfigDigest {
		return journal{}, errors.New("successor candidate manifest is inconsistent")
	}
	return successor, nil
}

func removePartialSuccessorStaging(path string, uid int) error {
	if err := validatePrivateDirectory(path, uid); err != nil {
		return errors.New("partial successor staging is unsafe")
	}
	allowed := map[string]bool{"candidate.bin": true, "previous.bin": true, "candidate-manifest.json": true, "journal.json": true, ".candidate-manifest.json.tmp": true, ".journal.json.tmp": true}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errors.New("partial successor staging is unavailable")
	}
	for _, entry := range entries {
		info, stat, fileErr := safeRegularFile(filepath.Join(path, entry.Name()), uid, false)
		if !allowed[entry.Name()] || entry.IsDir() || fileErr != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
			return errors.New("partial successor staging contains unsafe artifacts")
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(path, entry.Name())); err != nil {
			return errors.New("partial successor staging artifact could not be removed")
		}
	}
	if err := os.Remove(path); err != nil {
		return errors.New("partial successor staging could not be removed")
	}
	return syncDirectory(filepath.Dir(path))
}

func (m *Manager) resumeSuccessorActivation(predecessor journal, revision string, active activeUpgrade, result *Result) error {
	if predecessor.SuccessorRevision != revision {
		return errors.New("successor preparation is already bound to another revision")
	}
	successor, err := m.validatePreparedSuccessor(predecessor, m.bundlePath(predecessor.SuccessorID))
	if err != nil {
		return err
	}
	switch active.UpgradeID {
	case predecessor.UpgradeID:
		if err := m.writeActiveUpgrade(successor.UpgradeID); err != nil {
			return err
		}
	case successor.UpgradeID:
	default:
		return errors.New("active upgrade pointer conflicts with successor linkage")
	}
	*result = resultFor(successor, "prepared", "verified_successor_activated", "bootout_selected_supervisor")
	return nil
}
