package localupgrade

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
)

const (
	historicalRecoveryReason     = "database_relocation_recovery_retired"
	historicalRecoveryNextAction = "preserve_bundle"
)

type historicalRecoveryState string

const (
	historicalRecoveryUnresolved       historicalRecoveryState = "unresolved"
	historicalRecoveryActiveSuccessor  historicalRecoveryState = "active_successor"
	historicalRecoveryCleanupCommitted historicalRecoveryState = "cleanup_committed"
	historicalRecoveryCompleted        historicalRecoveryState = "completed"
)

type historicalRecoveryOperation int

const (
	historicalRecoveryNewUpgrade historicalRecoveryOperation = iota
	historicalRecoveryLifecycle
	historicalRecoveryCleanup
)

type historicalRecoveryRecord struct {
	predecessor journal
	successor   *journal
	state       historicalRecoveryState
}

func (m *Manager) historicalRecoveryRecords() ([]historicalRecoveryRecord, map[string]journal, error) {
	entries, err := os.ReadDir(m.upgradeRoot())
	if err != nil {
		return nil, nil, errors.New("historical managed-upgrade evidence is unavailable")
	}
	journals := make(map[string]journal)
	for _, entry := range entries {
		if !validUpgradeID(entry.Name()) {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, nil, errors.New("historical managed-upgrade evidence is invalid")
		}
		value, _, err := m.loadBundleJournal(entry.Name())
		if err != nil {
			return nil, nil, errors.New("historical managed-upgrade evidence is invalid")
		}
		journals[value.UpgradeID] = value
	}
	ids := make([]string, 0, len(journals))
	for id := range journals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	records := make([]historicalRecoveryRecord, 0)
	for _, id := range ids {
		predecessor := journals[id]
		if predecessor.DatabaseRecovery == nil {
			continue
		}
		records = append(records, m.classifyHistoricalRecovery(predecessor, journals))
	}
	return records, journals, nil
}

func (m *Manager) classifyHistoricalRecovery(predecessor journal, journals map[string]journal) historicalRecoveryRecord {
	record := historicalRecoveryRecord{predecessor: predecessor, state: historicalRecoveryUnresolved}
	if predecessor.Phase != "superseded" || predecessor.SupersededAt == nil {
		return record
	}
	successor, found := journals[predecessor.SuccessorID]
	if found {
		record.successor = &successor
		if !exactSuccessorRelation(predecessor, successor) || m.validateHistoricalSuccessorBundle(predecessor, successor) != nil {
			return record
		}
	}
	active, activePresent, activeErr := m.optionalActiveUpgrade()
	if activeErr != nil {
		return record
	}
	if found && activePresent {
		if active.UpgradeID != successor.UpgradeID {
			return record
		}
		record.state = historicalRecoveryActiveSuccessor
		return record
	}
	if found {
		if activePresent {
			return record
		}
		if successor.Phase != "cleanup_intent" || successor.CompletedAt == nil {
			return record
		}
		expected := currentInstallationFor(successor)
		current, err := m.readCurrentInstallation(successor.UpgradeID)
		if err != nil || current != expected {
			return record
		}
		record.state = historicalRecoveryCleanupCommitted
		return record
	}
	if activePresent {
		return record
	}
	if m.exactCompletedHistoricalRecoveryMatches(predecessor) {
		record.state = historicalRecoveryCompleted
	}
	return record
}

func (m *Manager) optionalActiveUpgrade() (activeUpgrade, bool, error) {
	if _, err := os.Lstat(m.activePath()); errors.Is(err, os.ErrNotExist) {
		return activeUpgrade{}, false, nil
	} else if err != nil {
		return activeUpgrade{}, false, errors.New("active upgrade pointer is unavailable")
	}
	active, err := m.readActiveUpgrade()
	return active, err == nil, err
}

func (m *Manager) validateHistoricalSuccessorBundle(predecessor, successor journal) error {
	bundle := m.bundlePath(successor.UpgradeID)
	if err := m.validateCleanupArtifacts(bundle, false); err != nil {
		return err
	}
	if !privateArtifactMatches(filepath.Join(bundle, "candidate.bin"), m.uid, successor.Candidate.Digest) || !privateArtifactMatches(filepath.Join(bundle, "previous.bin"), m.uid, successor.Previous.Digest) {
		return errors.New("historical successor binary evidence is inconsistent")
	}
	var manifest candidateManifest
	if err := readPrivateJSON(filepath.Join(bundle, "candidate-manifest.json"), m.uid, &manifest); err != nil || manifest.SchemaVersion != successor.SchemaVersion || manifest.Revision != successor.Revision || manifest.Candidate != successor.Candidate || manifest.Previous != successor.Previous || manifest.Database != successor.Database || manifest.ConfigDigest != successor.ConfigDigest {
		return errors.New("historical successor manifest is inconsistent")
	}
	if !exactSuccessorRelation(predecessor, successor) {
		return errors.New("historical successor relation is inconsistent")
	}
	return nil
}

func (m *Manager) exactCompletedHistoricalRecoveryMatches(predecessor journal) bool {
	var current currentInstallation
	err := readPrivateJSON(filepath.Join(m.controllerRoot(), "current-installation.json"), m.uid, &current)
	if err != nil || validateCurrentInstallation(current) != nil || predecessor.SupersededAt == nil {
		return false
	}
	return current.UpgradeID == predecessor.SuccessorID &&
		current.VerifiedAt.Compare(*predecessor.SupersededAt) >= 0 &&
		current.Supervisor == predecessor.Supervisor &&
		current.VCSRevision == predecessor.SuccessorRevision &&
		current.DatabaseSchema == predecessor.Database.SchemaVersion &&
		current.BinaryDigest != predecessor.Candidate.Digest &&
		current.BuildIdentity != predecessor.Candidate.Build.BuildIdentity
}

func (m *Manager) admitHistoricalRecoveryMutation(targetID string, operation historicalRecoveryOperation) error {
	if operation == historicalRecoveryCleanup {
		_, activePresent, err := m.optionalActiveUpgrade()
		if err != nil {
			return err
		}
		if !activePresent {
			return nil
		}
	}
	records, _, err := m.historicalRecoveryRecords()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	if operation == historicalRecoveryNewUpgrade {
		return errors.New("historical database relocation recovery is retained; replace the complete disposable runtime before another managed upgrade")
	}
	for _, record := range records {
		if record.state != historicalRecoveryActiveSuccessor {
			return errors.New("historical database relocation recovery is unresolved; replace the complete disposable runtime")
		}
	}
	for _, record := range records {
		if targetID == record.predecessor.UpgradeID {
			return errors.New("historical database relocation recovery is read-only; replace the complete disposable runtime")
		}
		if record.successor == nil || targetID != record.successor.UpgradeID {
			return errors.New("historical database relocation recovery only authorizes the exact transferred successor lifecycle")
		}
	}
	return nil
}

func (m *Manager) historicalRecoveryStatus(id string) (Result, bool) {
	records, journals, err := m.historicalRecoveryRecords()
	if err != nil {
		if value, _, loadErr := m.loadBundleJournal(id); loadErr == nil {
			return resultFor(value, "attention", historicalRecoveryReason, historicalRecoveryNextAction), true
		}
		return Result{}, false
	}
	for _, record := range records {
		if record.state == historicalRecoveryUnresolved {
			value, found := journals[id]
			if !found {
				value = record.predecessor
			}
			return resultFor(value, "attention", historicalRecoveryReason, historicalRecoveryNextAction), true
		}
	}
	for _, record := range records {
		if id == record.predecessor.UpgradeID {
			next := "none"
			if record.state == historicalRecoveryActiveSuccessor || record.state == historicalRecoveryCleanupCommitted {
				next = "status_successor"
			}
			return resultFor(record.predecessor, "superseded", "verified_successor_linked", next), true
		}
		if record.successor != nil && id == record.successor.UpgradeID && record.state == historicalRecoveryCleanupCommitted {
			return resultFor(*record.successor, "cleanup_interrupted", "cleanup_intent_durable", "cleanup"), true
		}
	}
	return Result{}, false
}
