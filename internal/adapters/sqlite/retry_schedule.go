package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const retryScheduleSelect = `SELECT run_id,phase,controller_state,attempt_count,max_attempts,initial_delay_ns,maximum_delay_ns,failure_class,failure_evidence_ref,reason_code,status,next_eligible_at,next_eligible_unix_ns,attention_at,created_at,updated_at FROM automatic_retry_schedules`

func (s *Store) GetRetrySchedule(ctx context.Context, runID, phase string) (application.RetrySchedule, bool, error) {
	if !validRetryScheduleKey(runID) || !validRetryScheduleKey(phase) {
		return application.RetrySchedule{}, false, errors.New("automatic retry schedule key is invalid")
	}
	schedule, found, err := scanRetrySchedule(s.db.QueryRowContext(ctx, retryScheduleSelect+` WHERE run_id=? AND phase=?`, runID, phase))
	if errors.Is(err, sql.ErrNoRows) {
		return application.RetrySchedule{}, false, nil
	}
	return schedule, found, err
}

func (s *Store) ListRetrySchedules(ctx context.Context) ([]application.RetrySchedule, error) {
	rows, err := s.db.QueryContext(ctx, retryScheduleSelect+` ORDER BY run_id,phase`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []application.RetrySchedule
	for rows.Next() {
		schedule, _, scanErr := scanRetrySchedule(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		schedules = append(schedules, schedule)
	}
	return schedules, rows.Err()
}

func (s *Store) listRetrySchedulesForRun(ctx context.Context, runID string) ([]application.RetrySchedule, error) {
	if !validRetryScheduleKey(runID) {
		return nil, errors.New("automatic retry schedule run ID is invalid")
	}
	rows, err := s.db.QueryContext(ctx, retryScheduleSelect+` WHERE run_id=? ORDER BY phase`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []application.RetrySchedule
	for rows.Next() {
		schedule, _, scanErr := scanRetrySchedule(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		schedules = append(schedules, schedule)
	}
	return schedules, rows.Err()
}

func (s *Store) ApplyRetryFailure(ctx context.Context, request application.RetryFailureRequest) (application.RetrySchedule, bool, error) {
	if err := application.ValidateRetryFailureRequest(request); err != nil {
		return application.RetrySchedule{}, false, err
	}
	policy := request.Policy
	if policy.MaxAttempts == 0 && policy.InitialDelay == 0 && policy.MaximumDelay == 0 {
		policy = application.DefaultAutomaticRetryPolicy()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.RetrySchedule{}, false, err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT current_state FROM runs WHERE run_id=?`, request.RunID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return application.RetrySchedule{}, false, application.ErrRunNotFound
		}
		return application.RetrySchedule{}, false, err
	}
	current, found, err := retryScheduleTx(ctx, tx, request.RunID, request.Phase)
	if err != nil {
		return application.RetrySchedule{}, false, err
	}
	if found && current.Status == application.RetryScheduleAttention {
		return current, false, tx.Commit()
	}
	if found && current.AttemptCount != request.ExpectedAttempt {
		return current, false, tx.Commit()
	}
	if !found && request.ExpectedAttempt != 0 {
		return application.RetrySchedule{}, false, errors.New("automatic retry schedule compare authority is invalid")
	}

	stateChanged := state != string(request.ControllerState)
	controllerState := request.ControllerState
	failureClass, reason := request.FailureClass, request.ReasonCode
	if stateChanged {
		controllerState = domain.State(state)
		failureClass, reason = application.RetryFailureAuthority, application.RetryReasonAuthority
		if err := application.ValidateRetryControllerState(controllerState); err != nil {
			return application.RetrySchedule{}, false, err
		}
	}
	if found {
		policy = application.AutomaticRetryPolicy{MaxAttempts: current.MaxAttempts, InitialDelay: current.InitialDelay, MaximumDelay: current.MaximumDelay}
		if err := application.ValidateAutomaticRetryPolicy(policy); err != nil {
			return application.RetrySchedule{}, false, errors.New("automatic retry schedule policy is corrupt")
		}
	}
	now := request.Now.UTC()
	evidenceRef := request.FailureEvidenceRef
	if failureClass == application.RetryFailureProcessStart {
		var after time.Time
		if found {
			after = current.UpdatedAt
		}
		if err := validateProcessStartEvidenceRefTx(ctx, tx, request.RunID, evidenceRef, after, now); err != nil {
			return application.RetrySchedule{}, false, err
		}
	} else {
		evidenceRef = ""
	}
	attempt := request.ExpectedAttempt + 1
	status := application.RetryScheduleAttention
	var nextEligible, attentionAt time.Time
	if !stateChanged && application.RetryFailureIsRetryable(failureClass) && attempt <= policy.MaxAttempts {
		status = application.RetryScheduleScheduled
		nextEligible = now.Add(application.AutomaticRetryDelay(policy, attempt))
	} else {
		attentionAt = now
		if !stateChanged && application.RetryFailureIsRetryable(failureClass) && attempt > policy.MaxAttempts {
			reason = application.RetryReasonBudgetExhausted
		}
	}
	schedule := application.RetrySchedule{
		RunID: request.RunID, Phase: request.Phase, ControllerState: string(controllerState), AttemptCount: attempt,
		MaxAttempts: policy.MaxAttempts, InitialDelay: policy.InitialDelay, MaximumDelay: policy.MaximumDelay, FailureClass: failureClass, FailureEvidenceRef: evidenceRef, ReasonCode: reason, Status: status,
		NextEligibleAt: nextEligible, AttentionAt: attentionAt, CreatedAt: now, UpdatedAt: now,
	}
	if found {
		schedule.CreatedAt = current.CreatedAt
		result, updateErr := tx.ExecContext(ctx, `UPDATE automatic_retry_schedules SET controller_state=?,attempt_count=?,failure_class=?,failure_evidence_ref=?,reason_code=?,status=?,next_eligible_at=?,next_eligible_unix_ns=?,attention_at=?,updated_at=? WHERE run_id=? AND phase=? AND status='scheduled' AND attempt_count=? AND (SELECT current_state FROM runs WHERE run_id=?)=?`, schedule.ControllerState, schedule.AttemptCount, schedule.FailureClass, schedule.FailureEvidenceRef, schedule.ReasonCode, schedule.Status, formatTime(schedule.NextEligibleAt), retryUnixNano(schedule.NextEligibleAt), formatTime(schedule.AttentionAt), formatTime(schedule.UpdatedAt), schedule.RunID, schedule.Phase, request.ExpectedAttempt, request.RunID, state)
		if updateErr != nil {
			return application.RetrySchedule{}, false, updateErr
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return application.RetrySchedule{}, false, rowsErr
		}
		if changed != 1 {
			latest, latestFound, latestErr := retryScheduleTx(ctx, tx, request.RunID, request.Phase)
			if latestErr != nil {
				return application.RetrySchedule{}, false, latestErr
			}
			if !latestFound {
				return application.RetrySchedule{}, false, errors.New("automatic retry schedule compare authority disappeared")
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return application.RetrySchedule{}, false, commitErr
			}
			return latest, false, nil
		}
	} else {
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO automatic_retry_schedules(run_id,phase,controller_state,attempt_count,max_attempts,initial_delay_ns,maximum_delay_ns,failure_class,failure_evidence_ref,reason_code,status,next_eligible_at,next_eligible_unix_ns,attention_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, schedule.RunID, schedule.Phase, schedule.ControllerState, schedule.AttemptCount, schedule.MaxAttempts, schedule.InitialDelay, schedule.MaximumDelay, schedule.FailureClass, schedule.FailureEvidenceRef, schedule.ReasonCode, schedule.Status, formatTime(schedule.NextEligibleAt), retryUnixNano(schedule.NextEligibleAt), formatTime(schedule.AttentionAt), formatTime(schedule.CreatedAt), formatTime(schedule.UpdatedAt))
		if insertErr != nil {
			latest, latestFound, latestErr := retryScheduleTx(ctx, tx, request.RunID, request.Phase)
			if latestErr != nil {
				return application.RetrySchedule{}, false, latestErr
			}
			if latestFound {
				if commitErr := tx.Commit(); commitErr != nil {
					return application.RetrySchedule{}, false, commitErr
				}
				return latest, false, nil
			}
			return application.RetrySchedule{}, false, insertErr
		}
	}
	if err := tx.Commit(); err != nil {
		return application.RetrySchedule{}, false, err
	}
	return schedule, true, nil
}

func (s *Store) ClearRetrySchedule(ctx context.Context, runID, phase string, expectedAttempt int) (bool, error) {
	if !validRetryScheduleKey(runID) || !validRetryScheduleKey(phase) || expectedAttempt < 1 {
		return false, errors.New("automatic retry schedule clear authority is invalid")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM automatic_retry_schedules WHERE run_id=? AND phase=? AND status='scheduled' AND attempt_count=?`, runID, phase, expectedAttempt)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 1 {
		return true, nil
	}
	var status string
	err = s.db.QueryRowContext(ctx, `SELECT status FROM automatic_retry_schedules WHERE run_id=? AND phase=?`, runID, phase).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return status != string(application.RetryScheduleAttention), nil
}

func retryScheduleTx(ctx context.Context, tx *sql.Tx, runID, phase string) (application.RetrySchedule, bool, error) {
	schedule, found, err := scanRetrySchedule(tx.QueryRowContext(ctx, retryScheduleSelect+` WHERE run_id=? AND phase=?`, runID, phase))
	if errors.Is(err, sql.ErrNoRows) {
		return application.RetrySchedule{}, false, nil
	}
	return schedule, found, err
}

func scanRetrySchedule(row rowScanner) (application.RetrySchedule, bool, error) {
	var schedule application.RetrySchedule
	var status, nextEligible, attention, created, updated string
	var nextEligibleUnix int64
	if err := row.Scan(&schedule.RunID, &schedule.Phase, &schedule.ControllerState, &schedule.AttemptCount, &schedule.MaxAttempts, &schedule.InitialDelay, &schedule.MaximumDelay, &schedule.FailureClass, &schedule.FailureEvidenceRef, &schedule.ReasonCode, &status, &nextEligible, &nextEligibleUnix, &attention, &created, &updated); err != nil {
		return application.RetrySchedule{}, false, err
	}
	schedule.Status = application.RetryScheduleStatus(status)
	schedule.NextEligibleAt, schedule.AttentionAt = parseTime(nextEligible), parseTime(attention)
	schedule.CreatedAt, schedule.UpdatedAt = parseTime(created), parseTime(updated)
	if schedule.Status == application.RetryScheduleScheduled && nextEligibleUnix != retryUnixNano(schedule.NextEligibleAt) {
		return application.RetrySchedule{}, false, errors.New("automatic retry schedule time authority is corrupt")
	}
	if schedule.Status == application.RetryScheduleAttention && (nextEligibleUnix != 0 || !schedule.NextEligibleAt.IsZero()) {
		return application.RetrySchedule{}, false, errors.New("automatic retry attention time authority is corrupt")
	}
	if err := application.ValidateRetrySchedule(schedule); err != nil {
		return application.RetrySchedule{}, false, errors.New("automatic retry schedule is corrupt")
	}
	return schedule, true, nil
}

func validateProcessStartEvidenceRefTx(ctx context.Context, tx *sql.Tx, runID, ref string, after, at time.Time) error {
	kind, rawID, ok := strings.Cut(ref, ":")
	if !ok {
		return errors.New("process-start retry evidence reference is invalid")
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id < 1 {
		return errors.New("process-start retry evidence reference is invalid")
	}
	var persistedAt string
	switch kind {
	case "attempt":
		err = tx.QueryRowContext(ctx, `SELECT finished_at FROM attempts WHERE run_id=? AND attempt_id=? AND status='failed' AND error_category='process_start' AND finished_at<>''`, runID, id).Scan(&persistedAt)
	case "verification":
		err = tx.QueryRowContext(ctx, `SELECT created_at FROM verifications WHERE run_id=? AND verification_id=? AND process_outcome='not_started' AND failure_category='process_start'`, runID, id).Scan(&persistedAt)
	default:
		return errors.New("process-start retry evidence reference is invalid")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("process-start retry lacks exact persisted failure evidence")
	}
	if err != nil {
		return err
	}
	evidenceAt := parseTime(persistedAt)
	if evidenceAt.IsZero() || evidenceAt.After(at) || !after.IsZero() && !evidenceAt.After(after) {
		return errors.New("process-start retry evidence time is invalid")
	}
	return nil
}

func retryUnixNano(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func validRetryScheduleKey(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "\x00\r\n") && len(value) <= 128 && !strings.Contains(value, "/")
}
