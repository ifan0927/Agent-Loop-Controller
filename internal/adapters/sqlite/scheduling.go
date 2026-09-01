package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const heavyCapacityNamespace = "local_heavy_work"

// BindWorkerSupervisor binds this store handle to the supervisor that owns the
// database-directory process lock. Callers must acquire that lock before
// invoking this method and retain it until the store is closed.
func (s *Store) BindWorkerSupervisor(owner string) error {
	if strings.TrimSpace(owner) == "" {
		return errors.New("worker supervisor owner is required")
	}
	s.supervisorMu.Lock()
	defer s.supervisorMu.Unlock()
	if s.supervisorOwner != "" && s.supervisorOwner != owner {
		return errors.New("worker supervisor owner is already bound")
	}
	s.supervisorOwner = owner
	return nil
}

// AuthorizeHeavyPermitAdoption is retained for manual run compositions that
// already use the same process-lock-backed store-handle authority.
func (s *Store) AuthorizeHeavyPermitAdoption(owner string) error {
	return s.BindWorkerSupervisor(owner)
}

func (s *Store) authorizedSupervisorOwner(owner string) bool {
	s.supervisorMu.RLock()
	defer s.supervisorMu.RUnlock()
	return s.supervisorOwner == owner && owner != ""
}

func (s *Store) defaultHeavyPermitOwner(runID string) string {
	s.supervisorMu.RLock()
	defer s.supervisorMu.RUnlock()
	if s.supervisorOwner != "" {
		return s.supervisorOwner
	}
	return "manual:" + runID
}

func (s *Store) HasSchedulingAuthority(ctx context.Context, runID string) (bool, error) {
	if strings.TrimSpace(runID) == "" {
		return false, errors.New("scheduling run ID is required")
	}
	var quarantined int
	err := s.db.QueryRowContext(ctx, `SELECT quarantined FROM run_scheduling WHERE run_id=?`, runID).Scan(&quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		var automatic int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_todo_admission_journal WHERE run_id=?`, runID).Scan(&automatic); err != nil {
			return false, err
		}
		if automatic != 0 {
			return false, errors.New("automatic run is missing scheduling authority")
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if quarantined != 0 {
		return false, errors.New("quarantined run cannot cross the scheduling authority boundary")
	}
	return true, nil
}

func (s *Store) ConfigureHeavyCapacity(ctx context.Context, capacity int, identity string, now time.Time) (application.CapacityProjection, error) {
	if capacity < 1 || capacity > application.MaxHeavyCapacity || strings.TrimSpace(identity) == "" || now.IsZero() {
		return application.CapacityProjection{}, errors.New("heavy capacity configuration is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE heavy_capacity_authority
		SET configured_capacity=?,effective_capacity=?,effective_identity=?,version=version+1,updated_at=?
		WHERE namespace=? AND (configured_capacity<>? OR effective_capacity<>? OR effective_identity<>?)`,
		capacity, capacity, identity, formatTime(now.UTC()), heavyCapacityNamespace, capacity, capacity, identity)
	if err != nil {
		return application.CapacityProjection{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var found int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_capacity_authority WHERE namespace=?`, heavyCapacityNamespace).Scan(&found); err != nil {
			return application.CapacityProjection{}, err
		}
		if found != 1 {
			return application.CapacityProjection{}, errors.New("heavy capacity authority is missing")
		}
	}
	return s.Capacity(ctx, now)
}

func (s *Store) Capacity(ctx context.Context, now time.Time) (application.CapacityProjection, error) {
	if now.IsZero() {
		return application.CapacityProjection{}, errors.New("capacity observation time is required")
	}
	var projection application.CapacityProjection
	if err := s.db.QueryRowContext(ctx, `SELECT configured_capacity,effective_capacity,effective_identity,version FROM heavy_capacity_authority WHERE namespace=?`, heavyCapacityNamespace).
		Scan(&projection.ConfiguredCapacity, &projection.EffectiveCapacity, &projection.EffectiveIdentity, &projection.Version); err != nil {
		return application.CapacityProjection{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_permits`).Scan(&projection.InUse); err != nil {
		return application.CapacityProjection{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_scheduling WHERE supervisor_state='waiting' AND quarantined=0`).Scan(&projection.WaitingRunnable); err != nil {
		return application.CapacityProjection{}, err
	}
	projection.Available = projection.EffectiveCapacity - projection.InUse
	if projection.Available < 0 {
		projection.Available = 0
	}
	projection.Draining = projection.InUse > projection.EffectiveCapacity
	projection.ObservedAt = now.UTC()
	return projection, nil
}

func (s *Store) ListSchedulingRuns(ctx context.Context, scopes application.AuthorizedScopeSet, limit int) ([]application.SchedulingRun, error) {
	if limit < 1 || limit > application.MaxSchedulingQueryItems || scopes.Empty() {
		return nil, errors.New("scheduling run query limit is invalid")
	}
	where, args, err := authorizedRunWhereColumns(scopes, "r.repository", "r.repository_binding_digest", "r.run_id", "", false)
	if err != nil {
		return nil, err
	}
	var missing int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs r LEFT JOIN run_scheduling rs ON rs.run_id=r.run_id WHERE r.current_state NOT IN ('rejected','failed','completed') AND (`+where+`) AND rs.run_id IS NULL`, args...).Scan(&missing); err != nil {
		return nil, err
	}
	if missing != 0 {
		return nil, errors.New("nonterminal run is missing scheduling projection")
	}
	queryArgs := append(append([]any(nil), args...), limit)
	rows, err := s.db.QueryContext(ctx, `SELECT r.run_id,r.repository_binding_digest,r.current_state,rs.runnable_since,rs.supervisor_state,rs.quarantined,
		EXISTS(SELECT 1 FROM heavy_permits hp WHERE hp.run_id=r.run_id)
		FROM run_scheduling rs JOIN runs r ON r.run_id=rs.run_id
		WHERE r.current_state NOT IN ('rejected','failed','completed') AND (`+where+`)
		ORDER BY rs.runnable_since_unix_ns,r.run_id LIMIT ?`, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]application.SchedulingRun, 0, limit)
	for rows.Next() {
		var run application.SchedulingRun
		var state, runnable string
		var quarantined, hasPermit int
		if err := rows.Scan(&run.RunID, &run.RepositoryBindingDigest, &state, &runnable, &run.SupervisorState, &quarantined, &hasPermit); err != nil {
			return nil, err
		}
		run.State = domain.State(state)
		run.RunnableSince = parseTime(runnable)
		run.Quarantined = quarantined != 0
		run.HasHeavyPermit = hasPermit != 0
		run.WaitingForCapacity = run.SupervisorState == "waiting"
		if run.RunID == "" || strings.TrimSpace(run.RepositoryBindingDigest) == "" || run.RunnableSince.IsZero() || !validSupervisorState(run.SupervisorState) {
			return nil, errors.New("scheduling run projection is corrupt")
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func (s *Store) GetSchedulingRun(ctx context.Context, scopes application.AuthorizedScopeSet, runID string) (application.SchedulingRun, error) {
	if strings.TrimSpace(runID) == "" || scopes.Empty() {
		return application.SchedulingRun{}, errors.New("scheduling run lookup is invalid")
	}
	where, args, err := authorizedRunWhereColumns(scopes, "r.repository", "r.repository_binding_digest", "r.run_id", "", false)
	if err != nil {
		return application.SchedulingRun{}, err
	}
	args = append(args, runID)
	var run application.SchedulingRun
	var state, runnable string
	var quarantined, hasPermit int
	err = s.db.QueryRowContext(ctx, `SELECT r.run_id,r.repository_binding_digest,r.current_state,rs.runnable_since,rs.supervisor_state,rs.quarantined,
		EXISTS(SELECT 1 FROM heavy_permits hp WHERE hp.run_id=r.run_id)
		FROM run_scheduling rs JOIN runs r ON r.run_id=rs.run_id
		WHERE (`+where+`) AND r.run_id=?`, args...).
		Scan(&run.RunID, &run.RepositoryBindingDigest, &state, &runnable, &run.SupervisorState, &quarantined, &hasPermit)
	if errors.Is(err, sql.ErrNoRows) {
		return application.SchedulingRun{}, application.ErrRunNotFound
	}
	if err != nil {
		return application.SchedulingRun{}, err
	}
	run.State = domain.State(state)
	run.RunnableSince = parseTime(runnable)
	run.Quarantined = quarantined != 0
	run.HasHeavyPermit = hasPermit != 0
	run.WaitingForCapacity = run.SupervisorState == "waiting"
	if run.RunID == "" || strings.TrimSpace(run.RepositoryBindingDigest) == "" || run.RunnableSince.IsZero() || !validSupervisorState(run.SupervisorState) {
		return application.SchedulingRun{}, errors.New("scheduling run projection is corrupt")
	}
	return run, nil
}

func (s *Store) ListSchedulingDecisions(ctx context.Context, scopes application.AuthorizedScopeSet, limit int) ([]application.SchedulingDecision, error) {
	if limit < 1 || limit > application.MaxSchedulingQueryItems || scopes.Empty() {
		return nil, errors.New("scheduling decision query limit is invalid")
	}
	where, args, err := authorizedRunWhereColumns(scopes, "repository_profile_id", "repository_binding_digest", "run_id", "", true)
	if err != nil {
		return nil, err
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT decision_id,snapshot_digest,observed_at,capacity_identity,issue_uuid,issue_sequence,priority,repository_profile_id,run_id,repository_binding_digest,classification,reason_code,repository_slot_version,heavy_permit_version,admission_lease_version
		FROM scheduling_decisions WHERE `+where+` ORDER BY observed_at DESC,decision_id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]application.SchedulingDecision, 0, limit)
	for rows.Next() {
		var decision application.SchedulingDecision
		var observed string
		if err := rows.Scan(&decision.DecisionID, &decision.SnapshotDigest, &observed, &decision.CapacityIdentity, &decision.IssueUUID, &decision.IssueSequence, &decision.Priority, &decision.RepositoryProfileID, &decision.RunID, &decision.RepositoryBindingDigest, &decision.Classification, &decision.ReasonCode, &decision.RepositorySlotVersion, &decision.HeavyPermitVersion, &decision.AdmissionLeaseVersion); err != nil {
			return nil, err
		}
		decision.ObservedAt = parseTime(observed)
		if err := decision.Validate(); err != nil {
			return nil, errors.New("scheduling decision projection is corrupt")
		}
		result = append(result, decision)
	}
	return result, rows.Err()
}

func validSupervisorState(value string) bool {
	switch value {
	case "waiting", "running", "external_wait", "human_wait", "terminal", "quarantined":
		return true
	default:
		return false
	}
}

func (s *Store) ReconcileSchedulingAuthorities(ctx context.Context, now time.Time) ([]application.SchedulingRun, error) {
	if now.IsZero() {
		return nil, errors.New("scheduling reconciliation time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM heavy_permits WHERE run_id IN (SELECT run_id FROM runs WHERE current_state IN ('rejected','failed','completed'))`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repository_slots WHERE run_id IN (SELECT run_id FROM runs WHERE current_state IN ('rejected','failed','completed'))`); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT run_id,repository_binding_digest,current_state,created_at,lease_expires_unix FROM runs WHERE current_state NOT IN ('rejected','failed','completed') ORDER BY created_at,run_id`)
	if err != nil {
		return nil, err
	}
	type activeRun struct {
		id, binding, state, created string
		leaseExpires                int64
	}
	var active []activeRun
	counts := map[string]int{}
	for rows.Next() {
		var run activeRun
		if err := rows.Scan(&run.id, &run.binding, &run.state, &run.created, &run.leaseExpires); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(run.binding) == "" {
			rows.Close()
			return nil, errors.New("nonterminal run has no repository binding authority")
		}
		active = append(active, run)
		counts[run.binding]++
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	result := make([]application.SchedulingRun, 0, len(active))
	for _, run := range active {
		state := domain.State(run.state)
		if counts[run.binding] > 1 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO run_scheduling(run_id,runnable_since,runnable_since_unix_ns,supervisor_state,quarantined,reason_code,updated_at)
				VALUES(?,?,?,'quarantined',1,'duplicate_nonterminal_repository',?)
				ON CONFLICT(run_id) DO UPDATE SET supervisor_state='quarantined',quarantined=1,reason_code='duplicate_nonterminal_repository',updated_at=excluded.updated_at`,
				run.id, run.created, parseTime(run.created).UnixNano(), formatTime(now.UTC())); err != nil {
				return nil, err
			}
			result = append(result, application.SchedulingRun{RunID: run.id, RepositoryBindingDigest: run.binding, State: state, SupervisorState: "quarantined", Quarantined: true})
			continue
		}

		var slotRun string
		slotErr := tx.QueryRowContext(ctx, `SELECT run_id FROM repository_slots WHERE repository_binding_digest=?`, run.binding).Scan(&slotRun)
		switch {
		case errors.Is(slotErr, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO repository_slots(repository_binding_digest,run_id,version,acquired_at) VALUES(?,?,1,?)`, run.binding, run.id, run.created); err != nil {
				return nil, err
			}
		case slotErr != nil:
			return nil, slotErr
		case slotRun != run.id:
			return nil, errors.New("repository slot authority conflicts with active run")
		}

		var permitCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_permits WHERE run_id=?`, run.id).Scan(&permitCount); err != nil {
			return nil, err
		}
		runnableSince := parseTime(run.created)
		var persistedRunnable string
		if err := tx.QueryRowContext(ctx, `SELECT runnable_since FROM run_scheduling WHERE run_id=?`, run.id).Scan(&persistedRunnable); err == nil {
			runnableSince = parseTime(persistedRunnable)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if runnableSince.IsZero() {
			runnableSince = now.UTC()
		}
		if run.leaseExpires > now.UTC().UnixNano() {
			runnableSince = time.Unix(0, run.leaseExpires).UTC()
		}
		supervisorState := "external_wait"
		waiting := false
		if application.HeavyWorkRequired(state) {
			if permitCount == 0 {
				if runnableSince.After(now.UTC()) {
					supervisorState = "external_wait"
				} else {
					supervisorState, waiting = "waiting", true
				}
			} else {
				supervisorState = "running"
			}
		} else {
			if permitCount != 0 {
				var liveAttempts int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE run_id=? AND status='started' AND process_control_key<>''`, run.id).Scan(&liveAttempts); err != nil {
					return nil, err
				}
				if liveAttempts != 0 {
					return nil, errors.New("heavy permit process ownership is ambiguous")
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM heavy_permits WHERE run_id=?`, run.id); err != nil {
					return nil, err
				}
				permitCount = 0
			}
			if state == domain.StateAwaitingHumanDecision || state == domain.StateManualIntervention {
				expectedEvent := application.OperatorAttentionHumanDecision
				if state == domain.StateManualIntervention {
					expectedEvent = application.OperatorAttentionManualIntervention
				}
				var latestSequence int64
				transitionErr := tx.QueryRowContext(ctx, `SELECT sequence FROM transitions WHERE run_id=? AND to_state=? ORDER BY sequence DESC LIMIT 1`, run.id, state).Scan(&latestSequence)
				if transitionErr != nil && !errors.Is(transitionErr, sql.ErrNoRows) {
					return nil, transitionErr
				}
				attentionCount := 0
				if transitionErr == nil {
					eventKey := fmt.Sprintf("automation:%s:%s:%d", run.id, expectedEvent, latestSequence)
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_attention_outbox WHERE event_key=? AND run_id=? AND event_type=? AND controller_state=?`, eventKey, run.id, expectedEvent, state).Scan(&attentionCount); err != nil {
						return nil, err
					}
				}
				if attentionCount == 0 {
					supervisorState = "external_wait"
				} else {
					supervisorState = "human_wait"
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_scheduling(run_id,runnable_since,runnable_since_unix_ns,supervisor_state,quarantined,reason_code,updated_at)
			VALUES(?,?,?,?,0,'',?) ON CONFLICT(run_id) DO UPDATE SET runnable_since=excluded.runnable_since,runnable_since_unix_ns=excluded.runnable_since_unix_ns,supervisor_state=excluded.supervisor_state,quarantined=0,reason_code='',updated_at=excluded.updated_at`,
			run.id, formatTime(runnableSince), runnableSince.UnixNano(), supervisorState, formatTime(now.UTC())); err != nil {
			return nil, err
		}
		result = append(result, application.SchedulingRun{RunID: run.id, RepositoryBindingDigest: run.binding, State: state, RunnableSince: runnableSince, SupervisorState: supervisorState, WaitingForCapacity: waiting, HasHeavyPermit: permitCount == 1})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) AcquireHeavyPermit(ctx context.Context, runID, owner string, now time.Time) (application.HeavyPermit, bool, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(owner) == "" || now.IsZero() {
		return application.HeavyPermit{}, false, errors.New("heavy permit request is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.HeavyPermit{}, false, err
	}
	defer tx.Rollback()
	var state, binding string
	if err := tx.QueryRowContext(ctx, `SELECT current_state,repository_binding_digest FROM runs WHERE run_id=?`, runID).Scan(&state, &binding); err != nil {
		return application.HeavyPermit{}, false, err
	}
	if !application.HeavyWorkRequired(domain.State(state)) {
		return application.HeavyPermit{}, false, nil
	}
	var slotRun string
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM repository_slots WHERE repository_binding_digest=?`, binding).Scan(&slotRun); err != nil || slotRun != runID {
		return application.HeavyPermit{}, false, errors.New("repository slot authority is unavailable")
	}
	var existing application.HeavyPermit
	var acquiredAt, updatedAt string
	err = tx.QueryRowContext(ctx, `SELECT run_id,owner_nonce,version,acquired_at,updated_at FROM heavy_permits WHERE run_id=?`, runID).
		Scan(&existing.RunID, &existing.OwnerNonce, &existing.Version, &acquiredAt, &updatedAt)
	if err == nil {
		existing.AcquiredAt, existing.UpdatedAt = parseTime(acquiredAt), parseTime(updatedAt)
		if existing.OwnerNonce == owner {
			return existing, true, tx.Commit()
		}
		if !s.authorizedSupervisorOwner(owner) {
			return application.HeavyPermit{}, false, errors.New("heavy permit owner mismatch requires exclusive supervisor fencing")
		}
		var leaseOwner string
		var leaseExpiry int64
		var launched int
		if err := tx.QueryRowContext(ctx, `SELECT lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&leaseOwner, &leaseExpiry); err != nil {
			return application.HeavyPermit{}, false, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE run_id=? AND status='started'`, runID).Scan(&launched); err != nil {
			return application.HeavyPermit{}, false, err
		}
		if leaseExpiry > now.UTC().UnixNano() {
			return application.HeavyPermit{}, false, nil
		}
		if launched != 0 {
			return application.HeavyPermit{}, false, application.ErrHeavyPermitProcessReconciliationRequired
		}
		result, err := tx.ExecContext(ctx, `UPDATE heavy_permits SET owner_nonce=?,version=version+1,updated_at=? WHERE run_id=? AND owner_nonce=? AND version=?`, owner, formatTime(now.UTC()), runID, existing.OwnerNonce, existing.Version)
		if err != nil {
			return application.HeavyPermit{}, false, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return application.HeavyPermit{}, false, errors.New("heavy permit adoption compare-and-swap lost")
		}
		existing.OwnerNonce, existing.Version, existing.UpdatedAt = owner, existing.Version+1, now.UTC()
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return application.HeavyPermit{}, false, err
	}
	var leaseExpiry int64
	var launched int
	if err := tx.QueryRowContext(ctx, `SELECT lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&leaseExpiry); err != nil {
		return application.HeavyPermit{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE run_id=? AND status='started'`, runID).Scan(&launched); err != nil {
		return application.HeavyPermit{}, false, err
	}
	if launched != 0 {
		if leaseExpiry > now.UTC().UnixNano() {
			return application.HeavyPermit{}, false, nil
		}
		if !s.authorizedSupervisorOwner(owner) {
			return application.HeavyPermit{}, false, errors.New("started heavy work requires exclusive supervisor fencing")
		}
		return application.HeavyPermit{}, false, application.ErrHeavyPermitProcessReconciliationRequired
	}
	var capacity, inUse int
	if err := tx.QueryRowContext(ctx, `SELECT effective_capacity FROM heavy_capacity_authority WHERE namespace=?`, heavyCapacityNamespace).Scan(&capacity); err != nil {
		return application.HeavyPermit{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_permits`).Scan(&inUse); err != nil {
		return application.HeavyPermit{}, false, err
	}
	if inUse >= capacity {
		return application.HeavyPermit{}, false, tx.Commit()
	}
	var first string
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM run_scheduling WHERE supervisor_state='waiting' AND quarantined=0 AND runnable_since_unix_ns<=? ORDER BY runnable_since_unix_ns,run_id LIMIT 1`, now.UTC().UnixNano()).Scan(&first)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return application.HeavyPermit{}, false, err
	}
	if err == nil && first != runID {
		return application.HeavyPermit{}, false, tx.Commit()
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM heavy_permits`).Scan(&version); err != nil {
		return application.HeavyPermit{}, false, err
	}
	permit := application.HeavyPermit{RunID: runID, OwnerNonce: owner, Version: version, AcquiredAt: now.UTC(), UpdatedAt: now.UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO heavy_permits(run_id,owner_nonce,version,acquired_at,updated_at) VALUES(?,?,?,?,?)`, permit.RunID, permit.OwnerNonce, permit.Version, formatTime(permit.AcquiredAt), formatTime(permit.UpdatedAt)); err != nil {
		return application.HeavyPermit{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_scheduling SET supervisor_state='running',updated_at=? WHERE run_id=? AND quarantined=0`, formatTime(now.UTC()), runID); err != nil {
		return application.HeavyPermit{}, false, err
	}
	return permit, true, tx.Commit()
}

func (s *Store) ReleaseHeavyPermit(ctx context.Context, permit application.HeavyPermit, reason string, now time.Time) (bool, error) {
	if permit.RunID == "" || permit.OwnerNonce == "" || permit.Version < 1 || now.IsZero() || !validPermitReleaseReason(reason) {
		return false, errors.New("heavy permit release authority is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var liveAttempts int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE run_id=? AND status='started' AND process_control_key<>''`, permit.RunID).Scan(&liveAttempts); err != nil {
		return false, err
	}
	if liveAttempts != 0 {
		return false, errors.New("heavy permit cannot be released while a launched child may be alive")
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM heavy_permits WHERE run_id=? AND owner_nonce=? AND version=?`, permit.RunID, permit.OwnerNonce, permit.Version)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	if count == 1 {
		state := "external_wait"
		if reason == "human_wait" || reason == "manual_intervention" {
			state = "human_wait"
		}
		if reason == "terminal" {
			state = "terminal"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_scheduling SET supervisor_state=?,updated_at=? WHERE run_id=?`, state, formatTime(now.UTC()), permit.RunID); err != nil {
			return false, err
		}
	}
	return count == 1, tx.Commit()
}

func validPermitReleaseReason(reason string) bool {
	switch reason {
	case "external_wait", "human_wait", "manual_intervention", "retry_delay", "terminal", "shutdown_after_process_exit":
		return true
	default:
		return false
	}
}

func (s *Store) DeferSchedulingRun(ctx context.Context, runID string, runnableAt, now time.Time) (bool, error) {
	if strings.TrimSpace(runID) == "" || runnableAt.IsZero() || now.IsZero() || runnableAt.Before(now) {
		return false, errors.New("deferred scheduling authority is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE run_scheduling SET runnable_since=?,runnable_since_unix_ns=?,supervisor_state='external_wait',updated_at=? WHERE run_id=? AND quarantined=0 AND run_id IN (SELECT run_id FROM runs WHERE current_state NOT IN ('rejected','failed','completed'))`, formatTime(runnableAt.UTC()), runnableAt.UTC().UnixNano(), formatTime(now.UTC()), runID)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}

func (s *Store) SaveQueueSnapshot(ctx context.Context, snapshot application.QueueSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot.Candidates)
	if err != nil || len(raw) > 256*1024 {
		return errors.New("queue snapshot candidate projection exceeds its bound")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO queue_snapshot(namespace,digest,observed_at,effective_capacity_identity,candidates_json)
		VALUES('latest_complete',?,?,?,?) ON CONFLICT(namespace) DO UPDATE SET digest=excluded.digest,observed_at=excluded.observed_at,effective_capacity_identity=excluded.effective_capacity_identity,candidates_json=excluded.candidates_json`,
		snapshot.Digest, formatTime(snapshot.ObservedAt), snapshot.EffectiveCapacityIdentity, string(raw))
	return err
}

func (s *Store) LatestQueueSnapshot(ctx context.Context) (application.QueueSnapshot, bool, error) {
	var snapshot application.QueueSnapshot
	var observed, raw string
	err := s.db.QueryRowContext(ctx, `SELECT digest,observed_at,effective_capacity_identity,candidates_json FROM queue_snapshot WHERE namespace='latest_complete'`).
		Scan(&snapshot.Digest, &observed, &snapshot.EffectiveCapacityIdentity, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return application.QueueSnapshot{}, false, nil
	}
	if err != nil {
		return application.QueueSnapshot{}, false, err
	}
	snapshot.ObservedAt = parseTime(observed)
	if err := json.Unmarshal([]byte(raw), &snapshot.Candidates); err != nil || snapshot.Validate() != nil {
		return application.QueueSnapshot{}, false, errors.New("queue snapshot is corrupt")
	}
	return snapshot, true, nil
}

func (s *Store) AppendSchedulingDecision(ctx context.Context, decision application.SchedulingDecision) (bool, error) {
	if err := decision.Validate(); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO scheduling_decisions(decision_id,snapshot_digest,observed_at,capacity_identity,issue_uuid,issue_sequence,priority,repository_profile_id,run_id,repository_binding_digest,classification,reason_code,repository_slot_version,heavy_permit_version,admission_lease_version)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(decision_id) DO NOTHING`, decision.DecisionID, decision.SnapshotDigest, formatTime(decision.ObservedAt), decision.CapacityIdentity, decision.IssueUUID, decision.IssueSequence, decision.Priority, decision.RepositoryProfileID, decision.RunID, decision.RepositoryBindingDigest, decision.Classification, decision.ReasonCode, decision.RepositorySlotVersion, decision.HeavyPermitVersion, decision.AdmissionLeaseVersion)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	if err := appendAdmissionConflictActivityTx(ctx, tx, decision); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return count == 1, nil
}

func reserveSchedulingAuthoritiesTx(ctx context.Context, tx *sql.Tx, run application.Run, reservation application.SchedulingReservation, defaultOwner string, now time.Time) (application.RepositorySlot, application.HeavyPermit, bool, error) {
	if err := reservation.Validate(); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	if strings.TrimSpace(run.RepositoryBindingDigest) == "" {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, errors.New("repository binding digest is required")
	}
	var effective int
	var identity string
	if err := tx.QueryRowContext(ctx, `SELECT effective_capacity,effective_identity FROM heavy_capacity_authority WHERE namespace=?`, heavyCapacityNamespace).Scan(&effective, &identity); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	if reservation.Enabled() && (reservation.Capacity != effective || reservation.CapacityIdentity != identity) {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, errors.New("effective heavy capacity authority changed")
	}
	var quarantinedBinding int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_scheduling rs JOIN runs r ON r.run_id=rs.run_id WHERE rs.quarantined=1 AND r.repository_binding_digest=? AND r.current_state NOT IN ('rejected','failed','completed')`, run.RepositoryBindingDigest).Scan(&quarantinedBinding); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	if quarantinedBinding != 0 {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, nil
	}
	var occupied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_slots WHERE repository_binding_digest=? OR run_id=?`, run.RepositoryBindingDigest, run.ID).Scan(&occupied); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	if occupied != 0 {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, nil
	}
	var inUse, waiting int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_permits`).Scan(&inUse); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_scheduling WHERE supervisor_state='waiting' AND quarantined=0 AND runnable_since_unix_ns<=?`, now.UTC().UnixNano()).Scan(&waiting); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	if inUse >= effective || waiting != 0 {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, nil
	}
	owner := reservation.OwnerNonce
	if owner == "" {
		owner = defaultOwner
	}
	if owner == "" {
		owner = "manual:" + run.ID
	}
	runnableSince := reservation.RunnableSince
	if runnableSince.IsZero() {
		runnableSince = now.UTC()
	}
	var slotVersion, permitVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM repository_slots`).Scan(&slotVersion); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM heavy_permits`).Scan(&permitVersion); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, err
	}
	slot := application.RepositorySlot{RepositoryBindingDigest: run.RepositoryBindingDigest, RunID: run.ID, Version: slotVersion, AcquiredAt: now.UTC()}
	permit := application.HeavyPermit{RunID: run.ID, OwnerNonce: owner, Version: permitVersion, AcquiredAt: now.UTC(), UpdatedAt: now.UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_slots(repository_binding_digest,run_id,version,acquired_at) VALUES(?,?,?,?)`, slot.RepositoryBindingDigest, slot.RunID, slot.Version, formatTime(slot.AcquiredAt)); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, fmt.Errorf("reserve repository slot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO heavy_permits(run_id,owner_nonce,version,acquired_at,updated_at) VALUES(?,?,?,?,?)`, permit.RunID, permit.OwnerNonce, permit.Version, formatTime(permit.AcquiredAt), formatTime(permit.UpdatedAt)); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, fmt.Errorf("reserve heavy permit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_scheduling(run_id,runnable_since,runnable_since_unix_ns,supervisor_state,quarantined,reason_code,updated_at) VALUES(?,?,?,'running',0,'',?)`, run.ID, formatTime(runnableSince), runnableSince.UnixNano(), formatTime(now.UTC())); err != nil {
		return application.RepositorySlot{}, application.HeavyPermit{}, false, fmt.Errorf("reserve run scheduling: %w", err)
	}
	return slot, permit, true, nil
}
