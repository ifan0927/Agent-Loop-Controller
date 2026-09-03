package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ReadRoutineOverviewSnapshot reads every SQLite-owned Overview component from
// one read transaction. Runtime heartbeat evidence remains outside this port.
func (s *Store) ReadRoutineOverviewSnapshot(ctx context.Context, scopes application.AuthorizedScopeSet, requester domain.GitHubUserIdentity, limit int) (application.RoutinePersistedOverviewSnapshot, error) {
	if !scopes.HasController() || requester.Validate() != nil || limit < 1 || limit > application.RoutineOverviewItemLimit {
		return application.RoutinePersistedOverviewSnapshot{}, errors.New("routine overview query is invalid")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	defer tx.Rollback()

	observedAt := time.Now().UTC()
	result := application.RoutinePersistedOverviewSnapshot{ObservedAt: observedAt}
	if err := readRoutineOverviewCapacity(ctx, tx, observedAt, &result); err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	if err := readRoutineOverviewQueue(ctx, tx, scopes, &result); err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	if err := readRoutineOverviewRuns(ctx, tx, limit, &result); err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	if err := readRoutineOverviewRepositories(ctx, tx, scopes, &result); err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	if err := readRoutineOverviewOnboarding(ctx, tx, scopes, requester, limit, &result); err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	if err := readRoutineOverviewAttention(ctx, tx, limit, &result); err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	if err := readRoutineOverviewConfiguration(ctx, tx, requester, observedAt, &result); err != nil {
		return application.RoutinePersistedOverviewSnapshot{}, err
	}
	return result, tx.Commit()
}

func readRoutineOverviewCapacity(ctx context.Context, tx *sql.Tx, observedAt time.Time, result *application.RoutinePersistedOverviewSnapshot) error {
	capacity := &result.Capacity
	if err := tx.QueryRowContext(ctx, `SELECT configured_capacity,effective_capacity,effective_identity,version FROM heavy_capacity_authority WHERE namespace=?`, heavyCapacityNamespace).Scan(&capacity.ConfiguredCapacity, &capacity.EffectiveCapacity, &capacity.EffectiveIdentity, &capacity.Version); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_permits`).Scan(&capacity.InUse); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_scheduling WHERE supervisor_state='waiting' AND quarantined=0`).Scan(&capacity.WaitingRunnable); err != nil {
		return err
	}
	capacity.Available = max(capacity.EffectiveCapacity-capacity.InUse, 0)
	capacity.Draining = capacity.InUse > capacity.EffectiveCapacity
	capacity.ObservedAt = observedAt
	return nil
}

func readRoutineOverviewQueue(ctx context.Context, tx *sql.Tx, scopes application.AuthorizedScopeSet, result *application.RoutinePersistedOverviewSnapshot) error {
	var snapshot application.QueueSnapshot
	var observedAt, raw string
	err := tx.QueryRowContext(ctx, `SELECT digest,observed_at,effective_capacity_identity,candidates_json FROM queue_snapshot WHERE namespace='latest_complete'`).Scan(&snapshot.Digest, &observedAt, &snapshot.EffectiveCapacityIdentity, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	snapshot.ObservedAt = parseTime(observedAt)
	if json.Unmarshal([]byte(raw), &snapshot.Candidates) != nil || snapshot.Validate() != nil {
		return errors.New("queue snapshot is corrupt")
	}
	result.QueueSnapshot = &snapshot
	rows, err := tx.QueryContext(ctx, `SELECT profile_id,repository_binding_digest,repository FROM repository_lifecycles WHERE retired_at=''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var authority application.RoutineQueueRepositoryAuthority
		if err := rows.Scan(&authority.ProfileID, &authority.BindingDigest, &authority.Repository); err != nil {
			return err
		}
		if scopes.HasController() || scopes.AllowsRepositoryBinding(authority.BindingDigest) {
			result.QueueRepositories = append(result.QueueRepositories, authority)
		}
	}
	return rows.Err()
}

func readRoutineOverviewRuns(ctx context.Context, tx *sql.Tx, limit int, result *application.RoutinePersistedOverviewSnapshot) error {
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN current_state NOT IN ('rejected','failed','completed') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN current_state IN ('rejected','failed','completed') THEN 1 ELSE 0 END),0)
		FROM runs`).Scan(&result.Runs.Active, &result.Runs.Recent); err != nil {
		return err
	}
	read := func(predicate string) ([]application.RoutineRunSummary, bool, error) {
		rows, err := tx.QueryContext(ctx, runSelect+` WHERE `+predicate+` ORDER BY updated_at DESC,run_id DESC LIMIT ?`, limit+1)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		var summaries []application.RoutineRunSummary
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return nil, false, err
			}
			summaries = append(summaries, application.RoutineRunSummary{RunID: run.ID, LinearIdentifier: run.IssueID, Repository: run.Repository, State: run.State, CandidateHead: run.CandidateHead, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt})
		}
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
		truncated := len(summaries) > limit
		if truncated {
			summaries = summaries[:limit]
		}
		return summaries, truncated, nil
	}
	var err error
	result.Runs.ActiveRuns, result.Runs.ActiveTruncated, err = read(`current_state NOT IN ('rejected','failed','completed')`)
	if err != nil {
		return err
	}
	result.Runs.RecentRuns, result.Runs.RecentTruncated, err = read(`current_state IN ('rejected','failed','completed')`)
	return err
}

func readRoutineOverviewRepositories(ctx context.Context, tx *sql.Tx, scopes application.AuthorizedScopeSet, result *application.RoutinePersistedOverviewSnapshot) error {
	rows, err := tx.QueryContext(ctx, `SELECT repository,repository_binding_digest FROM repository_lifecycles WHERE retired_at='' ORDER BY repository`)
	if err != nil {
		return err
	}
	var repositories []string
	for rows.Next() {
		var repository, binding string
		if err := rows.Scan(&repository, &binding); err != nil {
			rows.Close()
			return err
		}
		if scopes.HasController() || scopes.AllowsRepositoryBinding(binding) {
			repositories = append(repositories, repository)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, repository := range repositories {
		projection, err := repositoryProjectionTx(ctx, tx, repository)
		if err != nil {
			return err
		}
		result.Repositories.Total++
		if projection.Lifecycle.Intent == application.RepositoryEnabled {
			result.Repositories.Enabled++
		} else {
			result.Repositories.Disabled++
		}
		switch projection.Readiness.Status {
		case domain.RepositoryReady:
			result.Repositories.Ready++
		case domain.RepositoryUnknown:
			result.Repositories.Unavailable++
		default:
			result.Repositories.NotReady++
		}
	}
	return nil
}

func readRoutineOverviewOnboarding(ctx context.Context, tx *sql.Tx, scopes application.AuthorizedScopeSet, requester domain.GitHubUserIdentity, limit int, result *application.RoutinePersistedOverviewSnapshot) error {
	bindings := scopes.RepositoryBindingDigests()
	where := `(repository_binding_digest='' AND lower(requester_login)=lower(?) AND requester_database_id=? AND requester_node_id=? AND requester_actor_type=?)`
	args := []any{requester.Login, requester.DatabaseID, requester.NodeID, requester.ActorType}
	if scopes.HasController() {
		where += ` OR repository_binding_digest<>''`
	} else if len(bindings) != 0 {
		where += ` OR repository_binding_digest IN (` + strings.TrimSuffix(strings.Repeat("?,", len(bindings)), ",") + `)`
		for _, binding := range bindings {
			args = append(args, binding)
		}
	}
	where = `(` + where + `) AND status IN ('opened','waiting_for_operator','accepted','running')`
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_onboardings WHERE `+where, args...).Scan(&result.Onboarding.Total); err != nil {
		return err
	}
	pageArgs := append(append([]any(nil), args...), limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT onboarding_id FROM repository_onboardings WHERE `+where+` ORDER BY updated_at DESC,onboarding_id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	result.Onboarding.Truncated = len(ids) > limit
	if len(ids) > limit {
		ids = ids[:limit]
	}
	for _, id := range ids {
		onboarding, found, err := onboardingByID(ctx, tx, id)
		if err != nil || !found {
			return errors.New("routine onboarding snapshot conflicts")
		}
		onboarding.CompletedSteps, err = onboardingCompletedSteps(ctx, tx, id)
		if err != nil {
			return err
		}
		result.Onboarding.Active = append(result.Onboarding.Active, projectOverviewOnboarding(onboarding))
	}
	return nil
}

func projectOverviewOnboarding(value application.Onboarding) application.RoutineOnboardingSummary {
	result := application.RoutineOnboardingSummary{OnboardingID: value.OnboardingID, Kind: value.Kind, CanonicalRepository: value.CanonicalRepository, Status: value.Status, CompletedStepCount: len(value.CompletedSteps), ReasonCode: value.ReasonCode, LegalNextActions: domain.OnboardingLegalActions(value.Status, len(value.PreflightDigest) == 64, value.ReasonCode), OperationID: value.OperationID, CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC()}
	if !value.SettledAt.IsZero() {
		settled := value.SettledAt.UTC()
		result.SettledAt = &settled
	}
	return result
}

func readRoutineOverviewAttention(ctx context.Context, tx *sql.Tx, limit int, result *application.RoutinePersistedOverviewSnapshot) error {
	current, err := readCurrentOperatorAttentionFamilies(ctx, tx, currentOperatorAttentionFamilyRead{Limit: 1001, SeverityOrder: true})
	if err != nil {
		return err
	}
	if len(current.Events) > 1000 {
		return errors.New("routine overview attention candidate bound exceeded")
	}
	var queueSnapshot application.QueueSnapshot
	queueSnapshotFound := result.QueueSnapshot != nil
	if queueSnapshotFound {
		queueSnapshot = *result.QueueSnapshot
	}
	for _, event := range current.Events {
		scope, target := application.ScopeController, "controller"
		attentionState := application.RoutineAttentionActive
		if event.RunID != "" {
			scope, target = application.ScopeRun, event.RunID
			state, stateErr := readRoutineOverviewRunAttentionState(ctx, tx, event)
			if stateErr != nil {
				return stateErr
			}
			if state == "" {
				continue
			}
			attentionState = state
		} else {
			if event.RepositoryProfileID != "" && event.RepositoryProfileID != "automation" {
				var repository string
				if err := tx.QueryRowContext(ctx, `SELECT repository FROM repository_lifecycles WHERE profile_id=? AND retired_at='' ORDER BY updated_at DESC LIMIT 1`, event.RepositoryProfileID).Scan(&repository); err == nil {
					scope, target = application.ScopeRepository, repository
				} else if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			if application.ValidateOperatorAttentionEvent(event) != nil && application.ValidatePreviousOperatorAttentionEvent(event) != nil && application.ValidateLegacyOperatorAttentionEvent(event) != nil {
				attentionState = application.RoutineAttentionConflict
			} else {
				attentionState = application.ClassifyRoutineControllerAttentionCurrent(event, queueSnapshot, queueSnapshotFound)
				if attentionState == "" {
					continue
				}
			}
		}
		result.AttentionTotal++
		result.ActionableTotal++
		if len(result.Attention) < limit {
			result.Attention = append(result.Attention, application.RoutineAttentionSummary{EventID: event.EventKey, Scope: scope, TargetID: target, Severity: event.Severity, ReasonCode: event.ReasonCode, State: attentionState, OccurredAt: event.OccurredAt.UTC(), ObservedAt: event.ObservedAt.UTC()})
			result.Actionable = append(result.Actionable, application.RoutineActionableItem{ItemID: event.EventKey, Scope: scope, TargetID: target, Severity: event.Severity, ReasonCode: event.ReasonCode, Navigation: "attention", ObservedAt: event.ObservedAt.UTC()})
		}
		if (event.EventType == application.OperatorAttentionCandidateScan || event.EventType == application.OperatorAttentionSchedulerLease) && (result.QueueAttention == nil || event.OccurredAt.After(result.QueueAttention.OccurredAt)) {
			reason := "candidate_scan_attention"
			if event.EventType == application.OperatorAttentionSchedulerLease {
				reason = "scheduler_attention"
			}
			result.QueueAttention = &application.RoutineQueueAttention{OccurredAt: event.OccurredAt.UTC(), Degraded: event.Severity == "critical" || event.Severity == "error", ReasonCode: reason}
		}
	}
	result.AttentionTruncated = result.AttentionTotal > limit
	result.ActionableTruncated = result.ActionableTotal > limit
	return nil
}

func readRoutineOverviewRunAttentionState(ctx context.Context, tx *sql.Tx, event application.OperatorAttentionEvent) (application.RoutineAttentionState, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT current_state FROM runs WHERE run_id=?`, event.RunID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return application.RoutineAttentionConflict, nil
		}
		return "", err
	}
	var schedules []application.RetrySchedule
	if event.EventType == application.OperatorAttentionRetry {
		rows, err := tx.QueryContext(ctx, `SELECT status,reason_code FROM automatic_retry_schedules WHERE run_id=? ORDER BY phase`, event.RunID)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var status, reason string
			if err := rows.Scan(&status, &reason); err != nil {
				rows.Close()
				return "", err
			}
			schedules = append(schedules, application.RetrySchedule{RunID: event.RunID, Status: application.RetryScheduleStatus(status), ReasonCode: reason})
		}
		if err := rows.Close(); err != nil {
			return "", err
		}
	}
	var cleanup []application.CleanupRecord
	if event.EventType == application.OperatorAttentionCleanupResidue || event.EventType == application.OperatorAttentionSourceCheckoutSkipped {
		rows, err := tx.QueryContext(ctx, `SELECT resource_kind,status FROM cleanup_results WHERE run_id=? ORDER BY cleanup_id`, event.RunID)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var kind, status string
			if err := rows.Scan(&kind, &status); err != nil {
				rows.Close()
				return "", err
			}
			cleanup = append(cleanup, application.CleanupRecord{RunID: event.RunID, Kind: kind, Status: status})
		}
		if err := rows.Close(); err != nil {
			return "", err
		}
	}
	return application.ClassifyRoutineAttentionCurrent(application.Run{ID: event.RunID, State: domain.State(state)}, event, schedules, cleanup), nil
}

func readRoutineOverviewConfiguration(ctx context.Context, tx *sql.Tx, requester domain.GitHubUserIdentity, observedAt time.Time, result *application.RoutinePersistedOverviewSnapshot) error {
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || authority.Desired.ConfiguredOperator != requester {
		return errors.New("routine configuration authority is unavailable")
	}
	result.Configuration = authority
	draft, draftErr := scanConfigurationDraft(tx.QueryRowContext(ctx, configurationDraftSelect+` WHERE lifecycle IN ('open','applying','ambiguous') ORDER BY updated_at DESC,draft_id LIMIT 1`))
	if draftErr == nil {
		result.ActiveDraft = &draft
	} else if !errors.Is(draftErr, sql.ErrNoRows) {
		return draftErr
	}
	return nil
}
