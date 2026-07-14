package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func (s *Store) AcquireLinearTodoAdmissionLease(ctx context.Context, owner string, ttl time.Duration, now time.Time) (application.LinearTodoAdmissionLease, bool, error) {
	if err := validateAutomaticAdmissionLeaseRequest(owner, ttl, now); err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	defer tx.Rollback()
	current, found, err := automaticAdmissionLeaseTx(ctx, tx)
	if err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	if found && current.ExpiresAt.After(now.UTC()) {
		// A failed acquisition must not disclose a usable owner/version pair.
		// Treat a scheduler lease as a capability, not status telemetry.
		return application.LinearTodoAdmissionLease{}, false, nil
	}
	next := automaticAdmissionLease(owner, ttl, now, current.Version+1)
	if !found {
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_todo_admission_lease(namespace,owner_nonce,version,acquired_at,renewed_at,expires_at) VALUES(?,?,?,?,?,?)`, next.Namespace, next.OwnerNonce, next.Version, formatTime(next.AcquiredAt), formatTime(next.RenewedAt), formatTime(next.ExpiresAt))
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE linear_todo_admission_lease SET owner_nonce=?,version=?,acquired_at=?,renewed_at=?,expires_at=? WHERE namespace=? AND version=? AND expires_at<=?`, next.OwnerNonce, next.Version, formatTime(next.AcquiredAt), formatTime(next.RenewedAt), formatTime(next.ExpiresAt), next.Namespace, current.Version, formatTime(now))
		err = updateErr
		if err == nil {
			count, _ := result.RowsAffected()
			if count != 1 {
				return application.LinearTodoAdmissionLease{}, false, errors.New("automatic admission lease compare-and-swap lost")
			}
		}
	}
	if err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	return next, true, nil
}

func (s *Store) RenewLinearTodoAdmissionLease(ctx context.Context, lease application.LinearTodoAdmissionLease, ttl time.Duration, now time.Time) (application.LinearTodoAdmissionLease, bool, error) {
	if err := validateAutomaticAdmissionLeaseRequest(lease.OwnerNonce, ttl, now); err != nil || lease.Namespace != application.LinearTodoAdmissionLeaseNamespace || lease.Version < 1 {
		return application.LinearTodoAdmissionLease{}, false, errors.New("automatic admission lease renewal is invalid")
	}
	next := automaticAdmissionLease(lease.OwnerNonce, ttl, now, lease.Version+1)
	result, err := s.db.ExecContext(ctx, `UPDATE linear_todo_admission_lease SET version=?,renewed_at=?,expires_at=? WHERE namespace=? AND owner_nonce=? AND version=? AND expires_at>?`, next.Version, formatTime(next.RenewedAt), formatTime(next.ExpiresAt), lease.Namespace, lease.OwnerNonce, lease.Version, formatTime(now))
	if err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return application.LinearTodoAdmissionLease{}, false, nil
	}
	next.AcquiredAt = lease.AcquiredAt
	return next, true, nil
}

func (s *Store) ReleaseLinearTodoAdmissionLease(ctx context.Context, lease application.LinearTodoAdmissionLease) (bool, error) {
	if lease.Namespace != application.LinearTodoAdmissionLeaseNamespace || strings.TrimSpace(lease.OwnerNonce) == "" || lease.Version < 1 {
		return false, errors.New("automatic admission lease release is invalid")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM linear_todo_admission_lease WHERE namespace=? AND owner_nonce=? AND version=?`, lease.Namespace, lease.OwnerNonce, lease.Version)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}

func (s *Store) LinearTodoAdmissionLeaseHeld(ctx context.Context, lease application.LinearTodoAdmissionLease, now time.Time) (bool, error) {
	if lease.Namespace != application.LinearTodoAdmissionLeaseNamespace || strings.TrimSpace(lease.OwnerNonce) == "" || lease.Version < 1 || now.IsZero() {
		return false, errors.New("automatic admission lease check is invalid")
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_todo_admission_lease WHERE namespace=? AND owner_nonce=? AND version=? AND expires_at>?`, lease.Namespace, lease.OwnerNonce, lease.Version, formatTime(now)).Scan(&count)
	return count == 1, err
}

func (s *Store) ListNonterminalRuns(ctx context.Context) ([]application.Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE current_state NOT IN ('rejected','failed','completed') ORDER BY created_at,run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []application.Run
	for rows.Next() {
		run, scanErr := scanRun(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) ReserveLinearTodoAdmission(ctx context.Context, reservation application.LinearTodoAdmissionReservation) (application.Run, application.LinearTodoAdmissionJournal, bool, error) {
	run, journal, err := validateAutomaticAdmissionReservation(reservation)
	if err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if err := requireAutomaticAdmissionLeaseTx(ctx, tx, reservation.Lease, now); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	if err := requireNoAdmissionJournalCorruption(ctx, tx); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE current_state NOT IN ('rejected','failed','completed')`).Scan(&active); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	if active != 0 {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, nil
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_todo_admission_journal WHERE issue_uuid=? OR run_id=?`, journal.IssueUUID, journal.RunID).Scan(&existing); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	if existing != 0 {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, errors.New("automatic admission journal already reserves this issue or run")
	}
	if err := insertReservedRun(ctx, tx, run, now); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, fmt.Errorf("reserve automatic admission run: %w", err)
	}
	journal.CreatedAt, journal.UpdatedAt = now, now
	if _, err := tx.ExecContext(ctx, `INSERT INTO linear_todo_admission_journal(issue_uuid,run_id,scan_digest,task_digest,profile_digest,status,mutation_intent_ref,reason_code,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, journal.IssueUUID, journal.RunID, journal.ScanDigest, journal.TaskDigest, journal.ProfileDigest, journal.Status, journal.MutationIntentRef, journal.ReasonCode, formatTime(journal.CreatedAt), formatTime(journal.UpdatedAt)); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	created, err := s.GetRun(ctx, run.ID)
	if err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	return created, journal, true, nil
}

// AdoptLinearTodoAdmissionReservation is a proof operation for restart. It
// does not extend or reclaim a lease, modify a journal, or infer missing data.
func (s *Store) AdoptLinearTodoAdmissionReservation(ctx context.Context, reservation application.LinearTodoAdmissionReservation) (application.Run, application.LinearTodoAdmissionJournal, bool, error) {
	run, expected, err := validateAutomaticAdmissionReservation(reservation)
	if err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	defer tx.Rollback()
	if err := requireAutomaticAdmissionLeaseTx(ctx, tx, reservation.Lease, time.Now().UTC()); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	if err := requireNoAdmissionJournalCorruption(ctx, tx); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	actual, found, err := automaticAdmissionJournalTx(ctx, tx, expected.RunID)
	if err != nil || !found {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	if !sameAutomaticAdmissionJournalEvidence(actual, expected) {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, errors.New("automatic admission journal evidence conflicts")
	}
	persisted, err := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, run.ID))
	if err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, errors.New("automatic admission journal is orphaned")
	}
	if !sameReservedRun(persisted, run) {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, errors.New("automatic admission run evidence conflicts")
	}
	if err := tx.Commit(); err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, false, err
	}
	return persisted, actual, true, nil
}

// AdvanceLinearTodoAdmissionJournal records only the four controller-owned
// journal states. Mutation references are SHA-256 identifiers, never request
// bodies or variables; reason codes are fixed tokens rather than raw errors.
func (s *Store) AdvanceLinearTodoAdmissionJournal(ctx context.Context, transition application.LinearTodoAdmissionJournalTransition) (bool, error) {
	if strings.TrimSpace(transition.RunID) == "" || !validAutomaticAdmissionJournalStatus(transition.ExpectedStatus) || !validAutomaticAdmissionJournalStatus(transition.NextStatus) || !validAutomaticAdmissionReason(transition.ReasonCode) || (transition.MutationIntentRef != "" && !isAutomaticAdmissionDigest(transition.MutationIntentRef)) {
		return false, errors.New("automatic admission journal transition is invalid")
	}
	if (transition.NextStatus == "mutation_intent" || transition.NextStatus == "started") && !isAutomaticAdmissionDigest(transition.MutationIntentRef) {
		return false, errors.New("automatic admission journal mutation reference is required")
	}
	if transition.NextStatus == application.LinearTodoAdmissionJournalReserved && (transition.MutationIntentRef != "" || transition.ReasonCode != "") {
		return false, errors.New("automatic admission reservation journal cannot carry outcome data")
	}
	if transition.NextStatus == "manual_intervention" && transition.ReasonCode == "" {
		return false, errors.New("automatic admission journal reason is required")
	}
	if !validAutomaticAdmissionJournalTransition(transition.ExpectedStatus, transition.NextStatus) {
		return false, errors.New("automatic admission journal transition is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := requireAutomaticAdmissionLeaseTx(ctx, tx, transition.Lease, time.Now().UTC()); err != nil {
		return false, err
	}
	if err := requireNoAdmissionJournalCorruption(ctx, tx); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE linear_todo_admission_journal SET status=?,mutation_intent_ref=?,reason_code=?,updated_at=? WHERE run_id=? AND status=?`, transition.NextStatus, transition.MutationIntentRef, transition.ReasonCode, nowText(), transition.RunID, transition.ExpectedStatus)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func validateAutomaticAdmissionLeaseRequest(owner string, ttl time.Duration, now time.Time) error {
	if strings.TrimSpace(owner) == "" || ttl < 30*time.Second || ttl > application.MaxLinearTodoAdmissionLeaseTTL || now.IsZero() {
		return errors.New("automatic admission lease request is invalid")
	}
	return nil
}

func automaticAdmissionLease(owner string, ttl time.Duration, now time.Time, version int64) application.LinearTodoAdmissionLease {
	now = now.UTC()
	return application.LinearTodoAdmissionLease{Namespace: application.LinearTodoAdmissionLeaseNamespace, OwnerNonce: owner, Version: version, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl)}
}

func automaticAdmissionLeaseTx(ctx context.Context, tx *sql.Tx) (application.LinearTodoAdmissionLease, bool, error) {
	var lease application.LinearTodoAdmissionLease
	var acquired, renewed, expires string
	err := tx.QueryRowContext(ctx, `SELECT namespace,owner_nonce,version,acquired_at,renewed_at,expires_at FROM linear_todo_admission_lease WHERE namespace=?`, application.LinearTodoAdmissionLeaseNamespace).Scan(&lease.Namespace, &lease.OwnerNonce, &lease.Version, &acquired, &renewed, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return application.LinearTodoAdmissionLease{}, false, nil
	}
	if err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	lease.AcquiredAt, lease.RenewedAt, lease.ExpiresAt = parseTime(acquired), parseTime(renewed), parseTime(expires)
	if lease.Namespace != application.LinearTodoAdmissionLeaseNamespace || strings.TrimSpace(lease.OwnerNonce) == "" || lease.Version < 1 || lease.AcquiredAt.IsZero() || lease.RenewedAt.IsZero() || lease.ExpiresAt.IsZero() || lease.RenewedAt.Before(lease.AcquiredAt) {
		return application.LinearTodoAdmissionLease{}, false, errors.New("automatic admission lease is corrupt")
	}
	return lease, true, nil
}

func requireAutomaticAdmissionLeaseTx(ctx context.Context, tx *sql.Tx, lease application.LinearTodoAdmissionLease, now time.Time) error {
	if lease.Namespace != application.LinearTodoAdmissionLeaseNamespace || strings.TrimSpace(lease.OwnerNonce) == "" || lease.Version < 1 || now.IsZero() {
		return errors.New("automatic admission lease authority is invalid")
	}
	current, found, err := automaticAdmissionLeaseTx(ctx, tx)
	if err != nil {
		return err
	}
	if !found || current.OwnerNonce != lease.OwnerNonce || current.Version != lease.Version || !current.ExpiresAt.After(now.UTC()) {
		return errors.New("automatic admission lease was lost")
	}
	return nil
}

func validateAutomaticAdmissionReservation(reservation application.LinearTodoAdmissionReservation) (application.Run, application.LinearTodoAdmissionJournal, error) {
	if reservation.Lease.Namespace != application.LinearTodoAdmissionLeaseNamespace || strings.TrimSpace(reservation.Lease.OwnerNonce) == "" || reservation.Lease.Version < 1 || !validAutomaticAdmissionUUID(reservation.IssueUUID) || !isAutomaticAdmissionDigest(reservation.ScanDigest) {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, errors.New("automatic admission reservation is invalid")
	}
	run, err := application.ReservedRunFromAdmissionSnapshot(reservation.Input)
	if err != nil {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, err
	}
	if !isAutomaticAdmissionDigest(run.RawIssueHash) || !isAutomaticAdmissionDigest(run.TaskHash) || !isAutomaticAdmissionDigest(run.ProfileDigest) || !isAutomaticAdmissionDigest(run.RegistryDigest) || !isAutomaticAdmissionDigest(run.RepositoryBindingDigest) || strings.TrimSpace(run.IdempotencyKey) == "" {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, errors.New("automatic admission snapshot digests are invalid")
	}
	if digestBytes([]byte(run.RawIssueJSON)) != run.RawIssueHash || digestBytes([]byte(run.NormalizedTaskJSON)) != run.TaskHash {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, errors.New("automatic admission snapshot digest mismatch")
	}
	var source application.LinearTaskSource
	if json.Unmarshal([]byte(run.RawIssueJSON), &source) != nil || source.Provider != "linear" || source.IssueID != reservation.IssueUUID || source.Identifier != run.IssueID || source.SourceRevision != run.SourceRevision {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, errors.New("automatic admission source snapshot is invalid")
	}
	var task domain.CodingTask
	if json.Unmarshal([]byte(run.NormalizedTaskJSON), &task) != nil || !reflect.DeepEqual(task, reservation.Input.Task) || task.IssueID != run.IssueID || task.RunID != run.ID {
		return application.Run{}, application.LinearTodoAdmissionJournal{}, errors.New("automatic admission normalized task is invalid")
	}
	return run, application.LinearTodoAdmissionJournal{IssueUUID: reservation.IssueUUID, RunID: run.ID, ScanDigest: reservation.ScanDigest, TaskDigest: run.TaskHash, ProfileDigest: run.ProfileDigest, Status: application.LinearTodoAdmissionJournalReserved}, nil
}

func validAutomaticAdmissionUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value && parsed.Variant() == uuid.RFC4122
}

func isAutomaticAdmissionDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func validAutomaticAdmissionJournalStatus(value string) bool {
	switch value {
	case application.LinearTodoAdmissionJournalReserved, "mutation_intent", "started", "manual_intervention":
		return true
	default:
		return false
	}
}

func validAutomaticAdmissionReason(value string) bool {
	if value == "" {
		return true
	}
	switch value {
	case "authority_conflict", "lease_lost", "persistence_conflict", "source_drift", "mutation_conflict":
		return true
	default:
		return false
	}
}

func validAutomaticAdmissionJournalTransition(expected, next string) bool {
	return (expected == application.LinearTodoAdmissionJournalReserved && (next == "mutation_intent" || next == "manual_intervention")) ||
		(expected == "mutation_intent" && (next == "started" || next == "manual_intervention"))
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func insertReservedRun(ctx context.Context, tx *sql.Tx, run application.Run, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO runs(run_id,issue_id,idempotency_key,source_revision,raw_issue_json,raw_issue_hash,
		normalized_task_json,task_hash,repository,repository_config_json,profile_id,profile_snapshot_version,profile_digest,profile_snapshot_json,registry_version,registry_digest,repository_binding_digest,base_branch,working_branch,worktree_path,artifact_root,current_state,implementation_model,review_model,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.IssueID, run.IdempotencyKey, run.SourceRevision, run.RawIssueJSON, run.RawIssueHash, run.NormalizedTaskJSON, run.TaskHash, run.Repository, run.RepositoryConfigJSON, run.ProfileID, run.ProfileSnapshotVersion, run.ProfileDigest, run.ProfileSnapshotJSON, run.RegistryVersion, run.RegistryDigest, run.RepositoryBindingDigest, run.BaseBranch, run.WorkingBranch, run.WorktreePath, run.ArtifactRoot, domain.StateReceived, run.ImplementationModel, run.ReviewModel, formatTime(now), formatTime(now))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,1,'',?,'run created','task snapshot','',?)`, run.ID, domain.StateReceived, formatTime(now))
	return err
}

func requireNoAdmissionJournalCorruption(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT issue_uuid,run_id,scan_digest,task_digest,profile_digest,status,mutation_intent_ref,reason_code,created_at,updated_at FROM linear_todo_admission_journal ORDER BY run_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var journals []application.LinearTodoAdmissionJournal
	for rows.Next() {
		journal, scanErr := scanAutomaticAdmissionJournal(rows)
		if scanErr != nil {
			return scanErr
		}
		journals = append(journals, journal)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, journal := range journals {
		run, runErr := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, journal.RunID))
		if runErr != nil || !validAutomaticAdmissionJournal(journal, run) {
			return errors.New("automatic admission journal is corrupt")
		}
	}
	return nil
}

func automaticAdmissionJournalTx(ctx context.Context, tx *sql.Tx, runID string) (application.LinearTodoAdmissionJournal, bool, error) {
	journal, err := scanAutomaticAdmissionJournal(tx.QueryRowContext(ctx, `SELECT issue_uuid,run_id,scan_digest,task_digest,profile_digest,status,mutation_intent_ref,reason_code,created_at,updated_at FROM linear_todo_admission_journal WHERE run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.LinearTodoAdmissionJournal{}, false, nil
	}
	if err != nil {
		return application.LinearTodoAdmissionJournal{}, false, err
	}
	return journal, true, nil
}

func scanAutomaticAdmissionJournal(row rowScanner) (application.LinearTodoAdmissionJournal, error) {
	var journal application.LinearTodoAdmissionJournal
	var created, updated string
	err := row.Scan(&journal.IssueUUID, &journal.RunID, &journal.ScanDigest, &journal.TaskDigest, &journal.ProfileDigest, &journal.Status, &journal.MutationIntentRef, &journal.ReasonCode, &created, &updated)
	if err != nil {
		return application.LinearTodoAdmissionJournal{}, err
	}
	journal.CreatedAt, journal.UpdatedAt = parseTime(created), parseTime(updated)
	return journal, nil
}

func validAutomaticAdmissionJournal(journal application.LinearTodoAdmissionJournal, run application.Run) bool {
	if !validAutomaticAdmissionUUID(journal.IssueUUID) || !isAutomaticAdmissionDigest(journal.ScanDigest) || !isAutomaticAdmissionDigest(journal.TaskDigest) || !isAutomaticAdmissionDigest(journal.ProfileDigest) || !validAutomaticAdmissionJournalStatus(journal.Status) || (journal.MutationIntentRef != "" && !isAutomaticAdmissionDigest(journal.MutationIntentRef)) || !validAutomaticAdmissionReason(journal.ReasonCode) || (journal.Status == application.LinearTodoAdmissionJournalReserved && (journal.MutationIntentRef != "" || journal.ReasonCode != "")) || ((journal.Status == "mutation_intent" || journal.Status == "started") && !isAutomaticAdmissionDigest(journal.MutationIntentRef)) || (journal.Status == "manual_intervention" && journal.ReasonCode == "") || journal.CreatedAt.IsZero() || journal.UpdatedAt.IsZero() || journal.UpdatedAt.Before(journal.CreatedAt) {
		return false
	}
	if journal.RunID != run.ID || journal.TaskDigest != run.TaskHash || journal.ProfileDigest != run.ProfileDigest || !isAutomaticAdmissionDigest(run.RawIssueHash) || !isAutomaticAdmissionDigest(run.TaskHash) || digestBytes([]byte(run.RawIssueJSON)) != run.RawIssueHash || digestBytes([]byte(run.NormalizedTaskJSON)) != run.TaskHash {
		return false
	}
	var source application.LinearTaskSource
	var task domain.CodingTask
	return json.Unmarshal([]byte(run.RawIssueJSON), &source) == nil && json.Unmarshal([]byte(run.NormalizedTaskJSON), &task) == nil && source.Provider == "linear" && source.IssueID == journal.IssueUUID && source.Identifier == run.IssueID && source.SourceRevision == run.SourceRevision && task.Validate() == nil && task.RunID == run.ID && task.IssueID == run.IssueID && task.SourceRevision == run.SourceRevision
}

func sameReservedRun(actual, expected application.Run) bool {
	return actual.ID == expected.ID && actual.IssueID == expected.IssueID && actual.IdempotencyKey == expected.IdempotencyKey && actual.SourceRevision == expected.SourceRevision && actual.RawIssueHash == expected.RawIssueHash && actual.TaskHash == expected.TaskHash && actual.Repository == expected.Repository && actual.ProfileDigest == expected.ProfileDigest && actual.RegistryDigest == expected.RegistryDigest && actual.RepositoryBindingDigest == expected.RepositoryBindingDigest && actual.RawIssueJSON == expected.RawIssueJSON && actual.NormalizedTaskJSON == expected.NormalizedTaskJSON && actual.RepositoryConfigJSON == expected.RepositoryConfigJSON && actual.State == domain.StateReceived
}

func sameAutomaticAdmissionJournalEvidence(actual, expected application.LinearTodoAdmissionJournal) bool {
	return actual.IssueUUID == expected.IssueUUID && actual.RunID == expected.RunID && actual.ScanDigest == expected.ScanDigest && actual.TaskDigest == expected.TaskDigest && actual.ProfileDigest == expected.ProfileDigest && actual.Status == expected.Status && actual.MutationIntentRef == expected.MutationIntentRef && actual.ReasonCode == expected.ReasonCode
}
