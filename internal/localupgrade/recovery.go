package localupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const databaseRecoveryEvidenceVersion = 1

type recoveryPreviewEvidence struct {
	UpgradeID         string                          `json:"upgrade_id"`
	Revision          string                          `json:"revision"`
	Supervisor        string                          `json:"supervisor"`
	FailureReason     string                          `json:"failure_reason"`
	ConfigDigest      string                          `json:"configuration_digest"`
	Installed         binaryEvidence                  `json:"installed_predecessor"`
	OldDatabase       databaseEvidence                `json:"old_database"`
	Replacement       databaseEvidence                `json:"replacement_database"`
	Verification      replacementDatabaseVerification `json:"replacement_verification"`
	LocatorVersion    int                             `json:"locator_version"`
	LocatorConfigPath string                          `json:"locator_config_path"`
	LocatorDBPath     string                          `json:"locator_database_path"`
	SupervisorsAbsent bool                            `json:"supervisors_absent"`
}

func (m *Manager) PreviewSuccessorRecovery(ctx context.Context, request SuccessorRecoveryPreviewRequest) (preview SuccessorRecoveryPreview, finalErr error) {
	if !validUpgradeID(request.PredecessorUpgradeID) || !validRevision(request.Revision) {
		return SuccessorRecoveryPreview{}, errors.New("successor-recovery-preview requires a predecessor upgrade identifier and exact revision")
	}
	err := m.withActiveLock(request.PredecessorUpgradeID, func() error {
		predecessor, _, err := m.loadBundleJournal(request.PredecessorUpgradeID)
		if err != nil {
			return err
		}
		active, err := m.readActiveUpgrade()
		if err != nil || active.UpgradeID != predecessor.UpgradeID || predecessor.Phase != "attention" {
			return errors.New("successor database recovery preview is unavailable in the current phase")
		}
		if err := proveWorkerAbsentReadOnly(predecessor.DatabasePath, m.uid); err != nil {
			return err
		}
		evidence, digest, err := m.collectRecoveryPreview(ctx, predecessor, request.Revision, predecessor.Database, false)
		if err != nil {
			return err
		}
		_ = evidence
		preview = SuccessorRecoveryPreview{
			UpgradeID: predecessor.UpgradeID, State: "eligible", Reason: "authorized_database_relocation_verified",
			SuccessorRevision: request.Revision, PreviewDigest: digest,
			RequiredConfirmations: []string{"database_relocation_confirmed", "full_backup_confirmed"},
		}
		return nil
	})
	return preview, err
}

func (m *Manager) RecoverPrepareSuccessor(ctx context.Context, request SuccessorRecoverPrepareRequest) (result Result, finalErr error) {
	if !validUpgradeID(request.PredecessorUpgradeID) || !validRevision(request.Revision) || !validSHA256(request.PreviewDigest) {
		return Result{}, errors.New("successor-recover-prepare requires a predecessor upgrade identifier, exact revision, and preview digest")
	}
	if !request.DatabaseRelocationConfirmed || !request.FullBackupConfirmed {
		return Result{}, errors.New("successor-recover-prepare requires database relocation and encrypted full backup confirmations")
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
			if predecessor.DatabaseRecovery == nil || predecessor.DatabaseRecovery.PreviewDigest != request.PreviewDigest {
				return errors.New("successor database recovery replay conflicts")
			}
			if active.UpgradeID == predecessor.UpgradeID {
				workerLock, err := acquireExistingWorkerLock(predecessor.DatabasePath, m.uid)
				if err != nil {
					return err
				}
				defer workerLock.Close()
				if _, err := m.validatePublishedRecovery(ctx, predecessor, request.Revision); err != nil {
					return errors.New("successor database recovery evidence changed before resumed active pointer transfer")
				}
			}
			return m.resumeSuccessorActivation(predecessor, request.Revision, active, &result)
		}
		if active.UpgradeID != predecessor.UpgradeID {
			return errors.New("predecessor is not the active managed upgrade")
		}

		workerLock, err := acquireExistingWorkerLock(predecessor.DatabasePath, m.uid)
		if err != nil {
			return err
		}
		defer workerLock.Close()

		var prepared preparedCandidate
		preparedAvailable := false
		defer func() {
			if preparedAvailable {
				prepared.Cleanup()
			}
		}()
		prepareCandidate := func(schema int) error {
			if preparedAvailable || predecessor.SuccessorID != "" && exists(m.bundlePath(predecessor.SuccessorID)) {
				return nil
			}
			candidate, prepareErr := m.prepareVerifiedCandidate(ctx, request.Revision, schema)
			if prepareErr != nil {
				return prepareErr
			}
			prepared, preparedAvailable = candidate, true
			return nil
		}

		switch predecessor.Phase {
		case "attention":
			if request.Revision == predecessor.Revision {
				return errors.New("successor revision must differ from the predecessor revision")
			}
			evidence, digest, err := m.collectRecoveryPreview(ctx, predecessor, request.Revision, predecessor.Database, false)
			if err != nil || digest != request.PreviewDigest {
				return errors.New("successor database recovery preview changed")
			}
			if err := prepareCandidate(evidence.Replacement.SchemaVersion); err != nil {
				return err
			}
			revalidated, revalidatedDigest, err := m.collectRecoveryPreview(ctx, predecessor, request.Revision, predecessor.Database, false)
			if err != nil || revalidatedDigest != request.PreviewDigest || revalidated != evidence {
				return errors.New("successor database recovery evidence changed before intent")
			}
			now := m.now()
			predecessor.SchemaVersion = journalSchemaVersion
			predecessor.Phase = "successor_recovery_intent"
			predecessor.SuccessorID = newUpgradeID()
			predecessor.SuccessorRevision = request.Revision
			predecessor.DatabaseRecovery = &databaseRecoveryEvidence{
				Version: databaseRecoveryEvidenceVersion, PreviewDigest: request.PreviewDigest,
				OldDatabase: evidence.OldDatabase, ReplacementDatabase: evidence.Replacement, Verification: evidence.Verification,
				SuccessorRevision: request.Revision, DatabaseRelocationConfirmed: true, FullBackupConfirmed: true, IntentAt: now,
			}
			predecessor.UpdatedAt = now
			if err := writeJournal(predecessorBundle, predecessor, m.uid); err != nil {
				return err
			}
			if err := m.failAt("after_successor_recovery_intent"); err != nil {
				return err
			}
		case "successor_recovery_intent", "successor_prepare_intent":
			if err := validateRecoveryReplay(predecessor, request); err != nil {
				return err
			}
		default:
			return errors.New("successor database recovery is unavailable in the current phase")
		}

		if predecessor.Phase == "successor_recovery_intent" {
			if err := prepareCandidate(predecessor.DatabaseRecovery.ReplacementDatabase.SchemaVersion); err != nil {
				return err
			}
			if err := m.publishRecoveredLocator(ctx, &predecessor, predecessorBundle, request.Revision); err != nil {
				return err
			}
			if err := m.failAt("after_successor_recovery_journal"); err != nil {
				return err
			}
		}

		if _, err := m.validatePublishedRecovery(ctx, predecessor, request.Revision); err != nil {
			return err
		}
		successorBundle := m.bundlePath(predecessor.SuccessorID)
		if exists(successorBundle) {
			if _, err := m.validatePreparedSuccessor(predecessor, successorBundle); err != nil {
				return err
			}
		} else {
			if err := prepareCandidate(predecessor.Database.SchemaVersion); err != nil {
				return err
			}
			installed, err := m.validatePublishedRecovery(ctx, predecessor, request.Revision)
			if err != nil {
				return err
			}
			if err := m.stageSuccessorBundle(predecessor, predecessor.Database, installed, prepared); err != nil {
				return err
			}
		}
		if err := m.failAt("after_successor_bundle"); err != nil {
			return err
		}
		if _, err := m.validatePublishedRecovery(ctx, predecessor, request.Revision); err != nil {
			return errors.New("successor database recovery evidence changed before activation")
		}
		now := m.now()
		predecessor.Phase, predecessor.SupersededAt, predecessor.UpdatedAt = "superseded", &now, now
		if err := writeJournal(predecessorBundle, predecessor, m.uid); err != nil {
			return err
		}
		if err := m.failAt("after_predecessor_superseded"); err != nil {
			return err
		}
		if _, err := m.validatePublishedRecovery(ctx, predecessor, request.Revision); err != nil {
			return errors.New("successor database recovery evidence changed before active pointer transfer")
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
		result = resultFor(successor, "prepared", "verified_recovered_successor_activated", "replace")
		return nil
	})
	return result, err
}

func validateRecoveryReplay(predecessor journal, request SuccessorRecoverPrepareRequest) error {
	recovery := predecessor.DatabaseRecovery
	if recovery == nil || predecessor.SuccessorRevision != request.Revision || recovery.SuccessorRevision != request.Revision || recovery.PreviewDigest != request.PreviewDigest || !recovery.DatabaseRelocationConfirmed || !recovery.FullBackupConfirmed {
		return errors.New("successor database recovery replay conflicts")
	}
	return nil
}

func (m *Manager) collectRecoveryPreview(ctx context.Context, predecessor journal, revision string, oldDatabase databaseEvidence, locatorMayBeRecovered bool) (recoveryPreviewEvidence, string, error) {
	if predecessor.BootstrapIntentAt == nil || !eligibleSuccessorReason(predecessor.FailureReason) || revision == predecessor.Revision {
		return recoveryPreviewEvidence{}, "", errors.New("active attention is not eligible for successor database recovery")
	}
	if !configDigestMatches(predecessor.ConfigPath, predecessor.ConfigDigest) {
		return recoveryPreviewEvidence{}, "", errors.New("successor recovery configuration evidence changed")
	}
	installed, err := inspectBinary(ctx, m.runner, predecessor.BinaryPath, m.uid)
	if err != nil || installed.Digest != predecessor.Candidate.Digest || !installed.Structured || installed.Build.BuildIdentity != predecessor.Candidate.Build.BuildIdentity || installed.Build.VCSRevision != predecessor.Revision || installed.Build.VCSModified {
		return recoveryPreviewEvidence{}, "", errors.New("successor recovery installed predecessor identity is unavailable")
	}
	topology := m.observeSupervisorTopology(ctx, predecessor.Supervisor)
	if topology.Reason != "" || topology.Selected.State != "absent" {
		return recoveryPreviewEvidence{}, "", errors.New("successor recovery requires every supervisor to be absent")
	}
	if _, err := resolveCandidateSource(ctx, m.runner, revision); err != nil {
		return recoveryPreviewEvidence{}, "", err
	}
	locator, found, err := configurationadapter.ReadLocator(predecessor.ConfigPath)
	if err != nil || !found || locator.ConfigPath != predecessor.ConfigPath || locator.DatabasePath != predecessor.DatabasePath {
		return recoveryPreviewEvidence{}, "", errors.New("successor recovery locator authority is unavailable")
	}
	oldIdentity := databaseIdentity(oldDatabase)
	currentLocatorIdentity := locator.DatabaseIdentity
	replacement, verification, err := verifyReplacementDatabase(ctx, predecessor.DatabasePath, predecessor.ConfigPath, predecessor.ConfigDigest, predecessor.FailureReason, predecessor.Candidate.Build.SupportedControllerSchemaVersion, m.uid)
	if err != nil || replacement == oldDatabase {
		return recoveryPreviewEvidence{}, "", errors.New("successor recovery replacement database is unavailable")
	}
	replacementIdentity := databaseIdentity(replacement)
	if currentLocatorIdentity != oldIdentity && (!locatorMayBeRecovered || currentLocatorIdentity != replacementIdentity) {
		return recoveryPreviewEvidence{}, "", errors.New("successor recovery locator has an unexpected database identity")
	}
	evidence := recoveryPreviewEvidence{
		UpgradeID: predecessor.UpgradeID, Revision: revision, Supervisor: predecessor.Supervisor, FailureReason: predecessor.FailureReason,
		ConfigDigest: predecessor.ConfigDigest, Installed: installed, OldDatabase: oldDatabase, Replacement: replacement, Verification: verification,
		LocatorVersion: locator.Version, LocatorConfigPath: locator.ConfigPath, LocatorDBPath: locator.DatabasePath, SupervisorsAbsent: true,
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return recoveryPreviewEvidence{}, "", errors.New("successor recovery preview evidence is unavailable")
	}
	return evidence, sha256Hex(raw), nil
}

func (m *Manager) publishRecoveredLocator(ctx context.Context, predecessor *journal, bundle, revision string) error {
	recovery := predecessor.DatabaseRecovery
	if recovery == nil {
		return errors.New("successor database recovery intent is unavailable")
	}
	evidence, digest, err := m.collectRecoveryPreview(ctx, *predecessor, revision, recovery.OldDatabase, true)
	if err != nil {
		return err
	}
	if digest != recovery.PreviewDigest || evidence.Replacement != recovery.ReplacementDatabase || evidence.Verification != recovery.Verification {
		return errors.New("successor database recovery evidence changed before locator publication")
	}
	files, err := configurationadapter.NewFiles(predecessor.ConfigPath)
	if err != nil {
		return errors.New("successor database recovery filesystem authority is unavailable")
	}
	lock, acquired, err := files.AcquireMutation()
	if err != nil || !acquired {
		return errors.New("successor database recovery filesystem lock is unavailable")
	}
	defer lock.Release()
	evidence, digest, err = m.collectRecoveryPreview(ctx, *predecessor, revision, recovery.OldDatabase, true)
	if err != nil {
		return err
	}
	if digest != recovery.PreviewDigest || evidence.Replacement != recovery.ReplacementDatabase || evidence.Verification != recovery.Verification {
		return errors.New("successor database recovery evidence changed under filesystem lock")
	}
	locator, found, err := configurationadapter.ReadLocator(predecessor.ConfigPath)
	if err != nil || !found {
		return errors.New("successor database recovery locator authority is unavailable")
	}
	oldIdentity, replacementIdentity := databaseIdentity(recovery.OldDatabase), databaseIdentity(recovery.ReplacementDatabase)
	switch locator.DatabaseIdentity {
	case oldIdentity:
		if err := files.RebindLocatorDatabaseIdentity(locator, replacementIdentity); err != nil {
			return err
		}
		if err := m.failAt("after_successor_recovery_locator"); err != nil {
			return err
		}
	case replacementIdentity:
	default:
		return errors.New("successor database recovery encountered a third locator identity")
	}
	evidence, digest, err = m.collectRecoveryPreview(ctx, *predecessor, revision, recovery.OldDatabase, true)
	if err != nil {
		return err
	}
	if digest != recovery.PreviewDigest || evidence.Replacement != recovery.ReplacementDatabase || evidence.Verification != recovery.Verification {
		return errors.New("successor database recovery evidence changed after locator publication")
	}
	now := m.now()
	recovery.LocatorPublishedAt = &now
	predecessor.Database = recovery.ReplacementDatabase
	predecessor.Phase = "successor_prepare_intent"
	predecessor.UpdatedAt = now
	return writeJournal(bundle, *predecessor, m.uid)
}

func (m *Manager) validatePublishedRecovery(ctx context.Context, predecessor journal, revision string) (binaryEvidence, error) {
	recovery := predecessor.DatabaseRecovery
	if recovery == nil || recovery.LocatorPublishedAt == nil || predecessor.Database != recovery.ReplacementDatabase {
		return binaryEvidence{}, errors.New("published successor database recovery evidence is unavailable")
	}
	evidence, digest, err := m.collectRecoveryPreview(ctx, predecessor, revision, recovery.OldDatabase, true)
	if err != nil || digest != recovery.PreviewDigest || evidence.Replacement != recovery.ReplacementDatabase || evidence.Verification != recovery.Verification {
		return binaryEvidence{}, errors.New("published successor database recovery evidence changed")
	}
	locator, found, err := configurationadapter.ReadLocator(predecessor.ConfigPath)
	if err != nil || !found || locator.DatabaseIdentity != databaseIdentity(recovery.ReplacementDatabase) {
		return binaryEvidence{}, errors.New("published successor database recovery locator conflicts")
	}
	return evidence.Installed, nil
}

func databaseIdentity(evidence databaseEvidence) application.DatabaseFileIdentity {
	return application.DatabaseFileIdentity{Device: evidence.Device, Inode: evidence.Inode}
}

func proveWorkerAbsentReadOnly(databasePath string, uid int) error {
	path := filepath.Join(filepath.Dir(databasePath), "worker.lock")
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("worker lock is unavailable")
	}
	defer file.Close()
	if !safePrivateFileHandle(file, uid, 0) {
		return errors.New("worker lock is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("worker remains active")
	}
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
