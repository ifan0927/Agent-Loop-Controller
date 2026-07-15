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
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_todo_admission_lease(namespace,owner_nonce,version,acquired_at,renewed_at,expires_at,expires_at_unix_ns) VALUES(?,?,?,?,?,?,?)`, next.Namespace, next.OwnerNonce, next.Version, formatTime(next.AcquiredAt), formatTime(next.RenewedAt), formatTime(next.ExpiresAt), leaseUnixNano(next.ExpiresAt))
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE linear_todo_admission_lease SET owner_nonce=?,version=?,acquired_at=?,renewed_at=?,expires_at=?,expires_at_unix_ns=? WHERE namespace=? AND version=? AND expires_at_unix_ns<=?`, next.OwnerNonce, next.Version, formatTime(next.AcquiredAt), formatTime(next.RenewedAt), formatTime(next.ExpiresAt), leaseUnixNano(next.ExpiresAt), next.Namespace, current.Version, leaseUnixNano(now))
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
	result, err := s.db.ExecContext(ctx, `UPDATE linear_todo_admission_lease SET version=?,renewed_at=?,expires_at=?,expires_at_unix_ns=? WHERE namespace=? AND owner_nonce=? AND version=? AND expires_at_unix_ns>?`, next.Version, formatTime(next.RenewedAt), formatTime(next.ExpiresAt), leaseUnixNano(next.ExpiresAt), lease.Namespace, lease.OwnerNonce, lease.Version, leaseUnixNano(now))
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
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_todo_admission_lease WHERE namespace=? AND owner_nonce=? AND version=? AND expires_at_unix_ns>?`, lease.Namespace, lease.OwnerNonce, lease.Version, leaseUnixNano(now)).Scan(&count)
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

// GetLinearTodoAdmissionJournal returns the immutable reservation evidence for
// one persisted run. It deliberately has no selector for issue IDs or status,
// so automatic recovery can only adopt a known nonterminal run.
func (s *Store) GetLinearTodoAdmissionJournal(ctx context.Context, runID string) (application.LinearTodoAdmissionJournal, bool, error) {
	if strings.TrimSpace(runID) == "" {
		return application.LinearTodoAdmissionJournal{}, false, errors.New("automatic admission journal run ID is required")
	}
	journal, found, err := automaticAdmissionJournal(s.db, ctx, runID)
	if err != nil || !found {
		return journal, found, err
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return application.LinearTodoAdmissionJournal{}, false, err
	}
	if !validAutomaticAdmissionJournal(journal, run) {
		return application.LinearTodoAdmissionJournal{}, false, errors.New("automatic admission journal is corrupt")
	}
	return journal, true, nil
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
	if transition.NextStatus == application.LinearTodoAdmissionJournalManualIntervention && transition.ReasonCode == "" {
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

// AbandonAutomaticAdmission atomically closes the controller-owned admission
// slot and moves the journal to its existing terminal attention projection.
// It performs no Linear, Git, or GitHub operation; local resource cleanup is a
// separate application step after this durable CAS succeeds.
func (s *Store) AbandonAutomaticAdmission(ctx context.Context, request application.AutomaticAdmissionAbandonment) (application.Run, bool, error) {
	if err := request.Validate(); err != nil {
		return application.Run{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Run{}, false, err
	}
	defer tx.Rollback()

	run, err := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, request.RunID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.Run{}, false, application.ErrRunNotFound
	}
	if err != nil {
		return application.Run{}, false, err
	}
	if err := validateAutomaticAdmissionAbandonmentAuthorityTx(request, run, time.Now().UTC()); err != nil {
		return application.Run{}, false, err
	}
	if run.IdempotencyKey != request.IdempotencyKey {
		return application.Run{}, false, errors.New("automatic admission abandonment idempotency authority changed")
	}

	if run.State == domain.StateFailed {
		journal, found, err := automaticAdmissionJournalTx(ctx, tx, run.ID)
		if err != nil {
			return application.Run{}, false, err
		}
		if !found || !validAutomaticAdmissionJournal(journal, run) {
			return application.Run{}, false, errors.New("automatic admission journal is missing or corrupt")
		}
		if journal.Status == "mutation_intent" {
			return application.Run{}, false, errors.New("automatic admission abandonment is blocked by a pending Linear mutation")
		}
		if err := verifyAbandonmentTransitionTx(ctx, tx, request); err != nil {
			return application.Run{}, false, err
		}
		if err := rejectAbandonmentDeliveryEvidenceTx(ctx, tx, run); err != nil {
			return application.Run{}, false, err
		}
		if err := fenceAutomaticAdmissionAbandonmentTx(ctx, tx, request, time.Now().UTC()); err != nil {
			return application.Run{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return application.Run{}, false, err
		}
		return run, true, nil
	}
	if request.ExpectedState == domain.StateFailed || run.State != request.ExpectedState {
		return application.Run{}, false, errors.New("automatic admission abandonment state compare failed")
	}
	if request.ExpectedState != domain.StateReceived && request.ExpectedState != domain.StateAdmitting && request.ExpectedState != domain.StateManualIntervention {
		return application.Run{}, false, errors.New("automatic admission abandonment state is not eligible")
	}

	journal, found, err := automaticAdmissionJournalTx(ctx, tx, run.ID)
	if err != nil {
		return application.Run{}, false, err
	}
	if !found || !validAutomaticAdmissionJournal(journal, run) {
		return application.Run{}, false, errors.New("automatic admission journal is missing or corrupt")
	}
	if journal.Status == "mutation_intent" {
		return application.Run{}, false, errors.New("automatic admission abandonment is blocked by a pending Linear mutation")
	}
	if err := rejectAbandonmentDeliveryEvidenceTx(ctx, tx, run); err != nil {
		return application.Run{}, false, err
	}

	sequence, err := nextTransitionSequenceTx(ctx, tx, run.ID)
	if err != nil {
		return application.Run{}, false, err
	}
	nowTime := time.Now().UTC()
	now := formatTime(nowTime)
	evidence := "operator_abandon:" + request.IdempotencyKey
	result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,last_error=?,updated_at=? WHERE run_id=? AND current_state=? AND idempotency_key=? AND repository=? AND raw_issue_hash=? AND task_hash=? AND profile_digest=? AND lease_owner=? AND lease_expires_unix>?`, domain.StateFailed, application.AutomaticAdmissionAbandonTransition, now, run.ID, request.ExpectedState, request.IdempotencyKey, request.Repository, request.RawIssueHash, request.TaskHash, request.ProfileDigest, request.LeaseOwner, leaseUnixNano(nowTime))
	if err != nil {
		return application.Run{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.Run{}, false, errors.New("automatic admission abandonment state compare update lost")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, run.ID, sequence, request.ExpectedState, domain.StateFailed, application.AutomaticAdmissionAbandonTransition, evidence, run.CandidateHead, now); err != nil {
		return application.Run{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE linear_todo_admission_journal SET status=?,mutation_intent_ref='',reason_code=?,updated_at=? WHERE run_id=? AND status=?`, application.LinearTodoAdmissionJournalManualIntervention, application.AutomaticAdmissionAbandonReason, now, run.ID, journal.Status); err != nil {
		return application.Run{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.Run{}, false, err
	}
	run.State = domain.StateFailed
	run.LastError = application.AutomaticAdmissionAbandonTransition
	run.UpdatedAt = parseTime(now)
	return run, false, nil
}

func validateAutomaticAdmissionAbandonmentAuthorityTx(request application.AutomaticAdmissionAbandonment, run application.Run, now time.Time) error {
	if run.Repository != request.Repository || run.RawIssueHash != request.RawIssueHash || run.TaskHash != request.TaskHash || run.ProfileDigest != request.ProfileDigest || application.AutomaticAdmissionRepositoryConfigDigest(run.RepositoryConfigJSON) != request.RepositoryConfigDigest {
		return errors.New("automatic admission abandonment persisted authority changed")
	}
	if run.LeaseOwner != request.LeaseOwner || run.LeaseExpiresAt.IsZero() || !run.LeaseExpiresAt.After(now.UTC()) {
		return errors.New("automatic admission abandonment run lease was lost")
	}
	if err := application.AuthorizePersistedRequester(run, request.Requester); err != nil {
		return errors.New("automatic admission abandonment requester authority changed")
	}
	return nil
}

func fenceAutomaticAdmissionAbandonmentTx(ctx context.Context, tx *sql.Tx, request application.AutomaticAdmissionAbandonment, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=updated_at WHERE run_id=? AND current_state=? AND idempotency_key=? AND repository=? AND raw_issue_hash=? AND task_hash=? AND profile_digest=? AND lease_owner=? AND lease_expires_unix>?`, request.RunID, domain.StateFailed, request.IdempotencyKey, request.Repository, request.RawIssueHash, request.TaskHash, request.ProfileDigest, request.LeaseOwner, leaseUnixNano(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("automatic admission abandonment run lease compare lost")
	}
	return nil
}

func verifyAbandonmentTransitionTx(ctx context.Context, tx *sql.Tx, request application.AutomaticAdmissionAbandonment) error {
	var from, to, reason, evidence string
	err := tx.QueryRowContext(ctx, `SELECT from_state,to_state,reason,evidence_reference FROM transitions WHERE run_id=? ORDER BY sequence DESC LIMIT 1`, request.RunID).Scan(&from, &to, &reason, &evidence)
	if err != nil {
		return errors.New("automatic admission abandonment idempotency evidence is unavailable")
	}
	if to != string(domain.StateFailed) || reason != application.AutomaticAdmissionAbandonTransition || evidence != "operator_abandon:"+request.IdempotencyKey {
		return errors.New("automatic admission abandonment is not idempotently replayable")
	}
	if request.ExpectedState != domain.StateFailed && from != string(request.ExpectedState) {
		return errors.New("automatic admission abandonment idempotency state does not match")
	}
	return nil
}

func rejectAbandonmentDeliveryEvidenceTx(ctx context.Context, tx *sql.Tx, run application.Run) error {
	var count int
	for _, query := range []string{
		`SELECT COUNT(*) FROM pull_requests WHERE run_id=?`,
		`SELECT COUNT(*) FROM merge_results WHERE run_id=?`,
		`SELECT COUNT(*) FROM human_approvals WHERE run_id=?`,
		`SELECT COUNT(*) FROM human_approval_observations WHERE run_id=?`,
		`SELECT COUNT(*) FROM owned_resources WHERE owning_run=? AND resource_kind IN ('remote_branch','pull_request') AND ownership_status<>'deleted'`,
		`SELECT COUNT(*) FROM side_effects WHERE run_id=? AND NOT (kind='linear_move_to_started' AND status IN ('observed','failed'))`,
	} {
		if err := tx.QueryRowContext(ctx, query, run.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("automatic admission abandonment is blocked by retained external delivery evidence")
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT resource_kind,resource_name,status FROM cleanup_results WHERE run_id=?`, run.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, status string
		if err := rows.Scan(&kind, &name, &status); err != nil {
			return err
		}
		if !isAbandonLocalCleanupEvidence(run, kind, name, status) {
			return errors.New("automatic admission abandonment is blocked by retained external cleanup evidence")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// UpsertAutomaticAdmissionCleanup is the lease-fenced cleanup audit boundary
// used after an abandon CAS. The no-op run update takes the write lock before
// the cleanup upsert, so a replacement lease owner cannot be overwritten by a
// stale retry.
func (s *Store) UpsertAutomaticAdmissionCleanup(ctx context.Context, owner string, record application.CleanupRecord) error {
	if err := validateAutomaticAdmissionCleanupRecord(owner, record); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fenceAutomaticAdmissionCleanupLeaseTx(ctx, tx, owner, record.RunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cleanup_results(run_id,resource_kind,resource_name,status,error_class,last_error,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id,resource_kind,resource_name) DO UPDATE SET status=excluded.status,error_class=excluded.error_class,last_error=excluded.last_error,updated_at=excluded.updated_at`, record.RunID, record.Kind, record.Name, record.Status, record.ErrorClass, record.LastError, nowText()); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkAutomaticAdmissionResourceDeleted updates ownership under the same
// active run lease fence as cleanup audit writes.
func (s *Store) MarkAutomaticAdmissionResourceDeleted(ctx context.Context, owner string, resource application.OwnedResource) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(resource.RunID) == "" || (resource.Kind != "worktree" && resource.Kind != "branch") || strings.TrimSpace(resource.Name) == "" || resource.Status != "deleted" {
		return errors.New("automatic admission cleanup resource update is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fenceAutomaticAdmissionCleanupLeaseTx(ctx, tx, owner, resource.RunID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE owned_resources SET creation_evidence=?,ownership_status=? WHERE resource_kind=? AND resource_name=? AND owning_run=?`, resource.CreationEvidence, resource.Status, resource.Kind, resource.Name, resource.RunID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("automatic admission cleanup resource ownership changed")
	}
	return tx.Commit()
}

func validateAutomaticAdmissionCleanupRecord(owner string, record application.CleanupRecord) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(record.RunID) == "" || strings.TrimSpace(record.Name) == "" {
		return errors.New("automatic admission cleanup audit authority is incomplete")
	}
	switch record.Kind {
	case "artifact_root":
		if record.Status != "retained" {
			return errors.New("automatic admission artifact cleanup status is invalid")
		}
	case "worktree", "branch", "local_branch":
		switch record.Status {
		case "intent", "failed", "deleted":
		default:
			return errors.New("automatic admission local cleanup status is invalid")
		}
	default:
		return errors.New("automatic admission cleanup kind is invalid")
	}
	return nil
}

func fenceAutomaticAdmissionCleanupLeaseTx(ctx context.Context, tx *sql.Tx, owner, runID string) error {
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=updated_at WHERE run_id=? AND current_state=? AND lease_owner=? AND lease_expires_unix>?`, runID, domain.StateFailed, owner, leaseUnixNano(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("automatic admission cleanup run lease was lost")
	}
	return nil
}

func isAbandonLocalCleanupEvidence(run application.Run, kind, name, status string) bool {
	if run.ID == "" {
		return false
	}
	switch kind {
	case "artifact_root":
		return name == run.ArtifactRoot && status == "retained"
	case "worktree":
		return name == run.WorktreePath && abandonLocalCleanupStatus(status)
	case "branch", "local_branch":
		return name == run.WorkingBranch && abandonLocalCleanupStatus(status)
	case "source_checkout":
		return name == "configured_source_checkout" && abandonSourceCheckoutCleanupStatus(status)
	default:
		return false
	}
}

func abandonLocalCleanupStatus(status string) bool {
	switch status {
	case "intent", "failed", "deleted":
		return true
	default:
		return false
	}
}

func abandonSourceCheckoutCleanupStatus(status string) bool {
	switch status {
	case "intent", "failed", "synced", "skipped_attention":
		return true
	default:
		return false
	}
}

func nextTransitionSequenceTx(ctx context.Context, tx *sql.Tx, runID string) (int64, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, runID).Scan(&sequence); err != nil {
		return 0, err
	}
	return sequence, nil
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
	var expiresUnixNS int64
	err := tx.QueryRowContext(ctx, `SELECT namespace,owner_nonce,version,acquired_at,renewed_at,expires_at,expires_at_unix_ns FROM linear_todo_admission_lease WHERE namespace=?`, application.LinearTodoAdmissionLeaseNamespace).Scan(&lease.Namespace, &lease.OwnerNonce, &lease.Version, &acquired, &renewed, &expires, &expiresUnixNS)
	if errors.Is(err, sql.ErrNoRows) {
		return application.LinearTodoAdmissionLease{}, false, nil
	}
	if err != nil {
		return application.LinearTodoAdmissionLease{}, false, err
	}
	lease.AcquiredAt, lease.RenewedAt, lease.ExpiresAt = parseTime(acquired), parseTime(renewed), parseTime(expires)
	if lease.Namespace != application.LinearTodoAdmissionLeaseNamespace || strings.TrimSpace(lease.OwnerNonce) == "" || lease.Version < 1 || lease.AcquiredAt.IsZero() || lease.RenewedAt.IsZero() || lease.ExpiresAt.IsZero() || lease.RenewedAt.Before(lease.AcquiredAt) || expiresUnixNS != leaseUnixNano(lease.ExpiresAt) {
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

func leaseUnixNano(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

// backfillAutomaticAdmissionLeaseExpiryTx preserves valid v16/v17 lease
// rows during the v18 migration while making an unparsable legacy expiry fail
// closed as expired (zero).
func backfillAutomaticAdmissionLeaseExpiryTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT namespace,expires_at FROM linear_todo_admission_lease WHERE expires_at_unix_ns=0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		namespace string
		expiry    int64
	}
	var values []row
	for rows.Next() {
		var namespace, expires string
		if err := rows.Scan(&namespace, &expires); err != nil {
			return err
		}
		parsed := parseTime(expires)
		values = append(values, row{namespace: namespace, expiry: leaseUnixNano(parsed)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, value := range values {
		if _, err := tx.ExecContext(ctx, `UPDATE linear_todo_admission_lease SET expires_at_unix_ns=? WHERE namespace=? AND expires_at_unix_ns=0`, value.expiry, value.namespace); err != nil {
			return err
		}
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
	case application.LinearTodoAdmissionJournalReserved, "mutation_intent", "started", application.LinearTodoAdmissionJournalManualIntervention:
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
	case "authority_conflict", "lease_lost", "persistence_conflict", "source_drift", "mutation_conflict", application.AutomaticAdmissionAbandonReason:
		return true
	default:
		return false
	}
}

func validAutomaticAdmissionJournalTransition(expected, next string) bool {
	return (expected == application.LinearTodoAdmissionJournalReserved && (next == "mutation_intent" || next == application.LinearTodoAdmissionJournalManualIntervention)) ||
		(expected == "mutation_intent" && (next == "started" || next == application.LinearTodoAdmissionJournalManualIntervention)) ||
		(expected == "started" && next == application.LinearTodoAdmissionJournalManualIntervention)
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
	return automaticAdmissionJournal(tx, ctx, runID)
}

type automaticAdmissionJournalReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func automaticAdmissionJournal(db automaticAdmissionJournalReader, ctx context.Context, runID string) (application.LinearTodoAdmissionJournal, bool, error) {
	journal, err := scanAutomaticAdmissionJournal(db.QueryRowContext(ctx, `SELECT issue_uuid,run_id,scan_digest,task_digest,profile_digest,status,mutation_intent_ref,reason_code,created_at,updated_at FROM linear_todo_admission_journal WHERE run_id=?`, runID))
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
	if !validAutomaticAdmissionUUID(journal.IssueUUID) || !isAutomaticAdmissionDigest(journal.ScanDigest) || !isAutomaticAdmissionDigest(journal.TaskDigest) || !isAutomaticAdmissionDigest(journal.ProfileDigest) || !validAutomaticAdmissionJournalStatus(journal.Status) || (journal.MutationIntentRef != "" && !isAutomaticAdmissionDigest(journal.MutationIntentRef)) || !validAutomaticAdmissionReason(journal.ReasonCode) || (journal.Status == application.LinearTodoAdmissionJournalReserved && (journal.MutationIntentRef != "" || journal.ReasonCode != "")) || ((journal.Status == "mutation_intent" || journal.Status == "started") && !isAutomaticAdmissionDigest(journal.MutationIntentRef)) || (journal.Status == application.LinearTodoAdmissionJournalManualIntervention && journal.ReasonCode == "") || journal.CreatedAt.IsZero() || journal.UpdatedAt.IsZero() || journal.UpdatedAt.Before(journal.CreatedAt) {
		return false
	}
	if journal.RunID != run.ID || journal.TaskDigest != run.TaskHash || journal.ProfileDigest != run.ProfileDigest || !isAutomaticAdmissionDigest(run.RawIssueHash) || !isAutomaticAdmissionDigest(run.TaskHash) || digestBytes([]byte(run.RawIssueJSON)) != run.RawIssueHash || digestBytes([]byte(run.NormalizedTaskJSON)) != run.TaskHash {
		return false
	}
	var source application.LinearTaskSource
	var task domain.CodingTask
	return automaticAdmissionJournalStateMatchesRun(journal.Status, run.State) && json.Unmarshal([]byte(run.RawIssueJSON), &source) == nil && json.Unmarshal([]byte(run.NormalizedTaskJSON), &task) == nil && source.Provider == "linear" && source.IssueID == journal.IssueUUID && source.Identifier == run.IssueID && source.SourceRevision == run.SourceRevision && task.Validate() == nil && task.RunID == run.ID && task.IssueID == run.IssueID && task.SourceRevision == run.SourceRevision
}

// Journal status is a narrow pre-delivery witness, not a second state
// machine. In particular, an unstarted reservation may never coexist with an
// executing (or later) run; that would permit recovery to infer a mutation.
func automaticAdmissionJournalStateMatchesRun(status string, state domain.State) bool {
	switch status {
	case application.LinearTodoAdmissionJournalReserved:
		return state == domain.StateReceived
	case "mutation_intent":
		// The Linear start service can durably halt the run before this caller
		// records the final journal manual status.
		return state == domain.StateReceived || state == domain.StateManualIntervention
	case "started":
		// This is the durable proof that the remote start mutation completed.
		// A normal delivery lifecycle can subsequently reach any terminal state
		// without another journal transition, so terminal states remain valid.
		return true
	case application.LinearTodoAdmissionJournalManualIntervention:
		return state == domain.StateManualIntervention || state == domain.StateFailed
	default:
		return false
	}
}

func sameReservedRun(actual, expected application.Run) bool {
	return actual.ID == expected.ID && actual.IssueID == expected.IssueID && actual.IdempotencyKey == expected.IdempotencyKey && actual.SourceRevision == expected.SourceRevision && actual.RawIssueHash == expected.RawIssueHash && actual.TaskHash == expected.TaskHash && actual.Repository == expected.Repository && actual.ProfileDigest == expected.ProfileDigest && actual.RegistryDigest == expected.RegistryDigest && actual.RepositoryBindingDigest == expected.RepositoryBindingDigest && actual.RawIssueJSON == expected.RawIssueJSON && actual.NormalizedTaskJSON == expected.NormalizedTaskJSON && actual.RepositoryConfigJSON == expected.RepositoryConfigJSON && actual.State == domain.StateReceived
}

func sameAutomaticAdmissionJournalEvidence(actual, expected application.LinearTodoAdmissionJournal) bool {
	return actual.IssueUUID == expected.IssueUUID && actual.RunID == expected.RunID && actual.ScanDigest == expected.ScanDigest && actual.TaskDigest == expected.TaskDigest && actual.ProfileDigest == expected.ProfileDigest && actual.Status == expected.Status && actual.MutationIntentRef == expected.MutationIntentRef && actual.ReasonCode == expected.ReasonCode
}
