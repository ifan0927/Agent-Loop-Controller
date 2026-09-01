package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const integrityLeaseDuration = 30 * time.Second

type integrityPrivateFinding struct {
	reason         string
	privateKind    string
	privateID      string
	publicScope    application.AuthorityScopeKind
	publicTarget   string
	classification map[string]string
}

func (s *Store) RunIntegrityMaintenance(ctx context.Context, owner string, observedAt time.Time) (application.IntegrityMaintenanceResult, error) {
	if strings.TrimSpace(owner) == "" || strings.ContainsRune(owner, '\x00') || observedAt.IsZero() {
		return application.IntegrityMaintenanceResult{}, errors.New("integrity maintenance request is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.IntegrityMaintenanceResult{}, err
	}
	defer tx.Rollback()
	if err := validateIntegrityRegistryTx(ctx, tx); err != nil {
		return application.IntegrityMaintenanceResult{}, err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generation); err != nil {
		return application.IntegrityMaintenanceResult{}, errors.New("integrity generation authority conflicts")
	}

	var scanID, leaseOwner, leaseExpiry string
	var target int64
	var cursor, leaseVersion, convergenceAttempt int
	err = tx.QueryRowContext(ctx, `SELECT scan_id,target_generation,family_cursor,lease_owner,lease_version,lease_expires_at,convergence_attempt FROM controller_integrity_scans WHERE status='active'`).Scan(&scanID, &target, &cursor, &leaseOwner, &leaseVersion, &leaseExpiry, &convergenceAttempt)
	var recheck integrityRecheckBinding
	explicitRecheck := false
	var recheckSchema int
	if schemaErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='controller_integrity_rechecks'`).Scan(&recheckSchema); schemaErr != nil {
		return application.IntegrityMaintenanceResult{}, schemaErr
	}
	if err == nil && recheckSchema == 1 {
		recheck, explicitRecheck, err = getIntegrityRecheckByScanTx(ctx, tx, scanID)
		if err != nil {
			return application.IntegrityMaintenanceResult{}, err
		}
		var activeRechecks int
		if countErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_rechecks WHERE status='active'`).Scan(&activeRechecks); countErr != nil {
			return application.IntegrityMaintenanceResult{}, countErr
		}
		if explicitRecheck {
			var exactPointer int
			if pointerErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_active_recheck WHERE singleton=1 AND request_key=? AND operation_id=? AND scan_id=? AND target_generation=?`, recheck.requestKey, recheck.operationID, recheck.scanID, recheck.targetGeneration).Scan(&exactPointer); pointerErr != nil || exactPointer != 1 || activeRechecks != 1 || recheck.status != "active" || recheck.receipt.Phase != application.OperationPhaseApplied || recheck.receipt.Outcome != application.OperationOutcomePending {
				return application.IntegrityMaintenanceResult{}, application.ErrIntegrityRecheckConflict
			}
		} else if activeRechecks != 0 {
			return application.IntegrityMaintenanceResult{}, application.ErrIntegrityRecheckConflict
		}
	} else if errors.Is(err, sql.ErrNoRows) && recheckSchema == 1 {
		var activeRechecks int
		if countErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_rechecks WHERE status='active'`).Scan(&activeRechecks); countErr != nil {
			return application.IntegrityMaintenanceResult{}, countErr
		}
		if activeRechecks != 0 {
			return application.IntegrityMaintenanceResult{}, application.ErrIntegrityRecheckConflict
		}
	}
	superseded := false
	if err == nil && target != generation {
		if _, err := tx.ExecContext(ctx, `UPDATE controller_integrity_scans SET status='superseded',reason_code='source_generation_advanced',lease_owner='',lease_expires_at='',updated_at=?,completed_at=? WHERE scan_id=? AND status='active'`, formatTime(observedAt), formatTime(observedAt), scanID); err != nil {
			return application.IntegrityMaintenanceResult{}, err
		}
		if explicitRecheck {
			if err := settleSupersededIntegrityRecheckTx(ctx, tx, recheck, generation, observedAt, "source_generation_advanced"); err != nil {
				return application.IntegrityMaintenanceResult{}, err
			}
			if err := tx.Commit(); err != nil {
				return application.IntegrityMaintenanceResult{}, err
			}
			return application.IntegrityMaintenanceResult{ScanID: scanID, TargetGeneration: target, Superseded: true}, nil
		}
		convergenceAttempt++
		err, superseded = sql.ErrNoRows, true
	}
	if errors.Is(err, sql.ErrNoRows) {
		var currentGeneration int64
		currentErr := tx.QueryRowContext(ctx, `SELECT published_generation FROM controller_integrity_current WHERE singleton=1`).Scan(&currentGeneration)
		if currentErr == nil && currentGeneration == generation {
			if err := tx.Commit(); err != nil {
				return application.IntegrityMaintenanceResult{}, err
			}
			return application.IntegrityMaintenanceResult{TargetGeneration: generation, Superseded: superseded}, nil
		}
		if currentErr != nil && !errors.Is(currentErr, sql.ErrNoRows) {
			return application.IntegrityMaintenanceResult{}, errors.New("integrity current pointer conflicts")
		}
		scanID = integrityDigest("scan", application.IntegrityRegistryVersion, fmt.Sprint(generation))
		target, cursor, leaseVersion, leaseOwner, leaseExpiry = generation, 0, 0, "", ""
		if convergenceAttempt > 8 {
			convergenceAttempt = 8
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO controller_integrity_scans(scan_id,registry_version,target_generation,stable_boundary,status,convergence_attempt,created_at,updated_at) VALUES(?,?,?,?, 'active',?,?,?)`, scanID, application.IntegrityRegistryVersion, generation, integrityDigest("boundary", fmt.Sprint(generation)), convergenceAttempt, formatTime(observedAt), formatTime(observedAt))
	}
	if err != nil {
		return application.IntegrityMaintenanceResult{}, errors.New("integrity scan control conflicts")
	}
	if leaseOwner != "" && leaseOwner != owner && parseTime(leaseExpiry).After(observedAt) {
		if err := tx.Commit(); err != nil {
			return application.IntegrityMaintenanceResult{}, err
		}
		return application.IntegrityMaintenanceResult{ScanID: scanID, TargetGeneration: target, Superseded: superseded}, nil
	}
	leaseVersion++
	result, err := tx.ExecContext(ctx, `UPDATE controller_integrity_scans SET lease_owner=?,lease_version=?,lease_expires_at=?,updated_at=? WHERE scan_id=? AND status='active' AND lease_version=?`, owner, leaseVersion, formatTime(observedAt.Add(integrityLeaseDuration)), formatTime(observedAt), scanID, leaseVersion-1)
	if err != nil {
		return application.IntegrityMaintenanceResult{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.IntegrityMaintenanceResult{}, errors.New("integrity scan lease conflicts")
	}

	families := application.IntegrityFamilies()
	maintenance := application.IntegrityMaintenanceResult{ScanID: scanID, TargetGeneration: target, Superseded: superseded}
	if convergenceAttempt == 8 && cursor == 0 {
		for _, family := range families {
			familyResult, findings, checkErr := checkIntegrityFamilyTx(ctx, tx, family)
			if checkErr != nil {
				familyResult = application.IntegrityFamilyResult{Family: family, State: application.IntegrityUnknown, ReasonCode: "bounded_check_failed", CountComplete: false, CoverageComplete: false}
				findings = nil
			}
			if err := persistIntegrityFamilyTx(ctx, tx, scanID, familyResult, findings, observedAt); err != nil {
				return application.IntegrityMaintenanceResult{}, err
			}
		}
		cursor = len(families)
		if _, err := tx.ExecContext(ctx, `UPDATE controller_integrity_scans SET family_cursor=?,lease_owner='',lease_expires_at='',updated_at=? WHERE scan_id=? AND status='active' AND lease_version=?`, cursor, formatTime(observedAt), scanID, leaseVersion); err != nil {
			return application.IntegrityMaintenanceResult{}, err
		}
	}
	if cursor < len(families) {
		family := families[cursor]
		maintenance.Family = family
		familyResult, findings, err := checkIntegrityFamilyTx(ctx, tx, family)
		if err != nil {
			familyResult = application.IntegrityFamilyResult{Family: family, State: application.IntegrityUnknown, ReasonCode: "bounded_check_failed", CountComplete: false, CoverageComplete: false}
		}
		if err := persistIntegrityFamilyTx(ctx, tx, scanID, familyResult, findings, observedAt); err != nil {
			return application.IntegrityMaintenanceResult{}, err
		}
		cursor++
		if _, err := tx.ExecContext(ctx, `UPDATE controller_integrity_scans SET family_cursor=?,lease_owner='',lease_expires_at='',updated_at=? WHERE scan_id=? AND status='active' AND lease_version=?`, cursor, formatTime(observedAt), scanID, leaseVersion); err != nil {
			return application.IntegrityMaintenanceResult{}, err
		}
	}
	if cursor == len(families) {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&current); err != nil {
			return application.IntegrityMaintenanceResult{}, errors.New("integrity publication generation conflicts")
		}
		if current != target {
			_, err = tx.ExecContext(ctx, `UPDATE controller_integrity_scans SET status='superseded',reason_code='publication_generation_advanced',lease_owner='',lease_expires_at='',updated_at=?,completed_at=? WHERE scan_id=? AND status='active'`, formatTime(observedAt), formatTime(observedAt), scanID)
			maintenance.Superseded = true
			if err == nil && explicitRecheck {
				err = settleSupersededIntegrityRecheckTx(ctx, tx, recheck, current, observedAt, "publication_generation_advanced")
			}
		} else {
			if explicitRecheck {
				err = finalizeIntegrityRecheckObservationTx(ctx, tx, recheck, observedAt)
			} else {
				err = publishIntegrityObservationTx(ctx, tx, scanID, target, observedAt)
			}
			maintenance.Published = err == nil
		}
		if err != nil {
			return application.IntegrityMaintenanceResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return application.IntegrityMaintenanceResult{}, err
	}
	return maintenance, nil
}

func validateIntegrityRegistryTx(ctx context.Context, tx *sql.Tx) error {
	families := application.IntegrityFamilies()
	rows, err := tx.QueryContext(ctx, `SELECT family,family_order,reason_version FROM integrity_registry_families WHERE registry_version=? ORDER BY family_order`, application.IntegrityRegistryVersion)
	if err != nil {
		return errors.New("integrity registry is unavailable")
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var family string
		var order int
		var reasonVersion string
		if rows.Scan(&family, &order, &reasonVersion) != nil || index >= len(families) || family != string(families[index]) || order != index || reasonVersion != "v1" {
			return errors.New("integrity registry conflicts")
		}
		index++
	}
	if rows.Err() != nil || index != len(families) {
		return errors.New("integrity registry conflicts")
	}
	return nil
}

func checkIntegrityFamilyTx(ctx context.Context, tx *sql.Tx, family application.IntegrityFamily) (application.IntegrityFamilyResult, []integrityPrivateFinding, error) {
	result := application.IntegrityFamilyResult{Family: family, State: application.IntegrityReady, ReasonCode: "complete", CountComplete: true, CoverageComplete: true}
	if err := tx.QueryRowContext(ctx, `SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family=? AND scope_kind='controller' AND scope_id='local-controller'`, family).Scan(&result.CheckedRevision); err != nil {
		result.State, result.ReasonCode, result.CountComplete, result.CoverageComplete = application.IntegrityConflict, "scope_revision_conflict", false, false
		return result, nil, nil
	}
	var findings []integrityPrivateFinding
	addController := func(reason, privateKind, privateID string) {
		findings = append(findings, integrityPrivateFinding{reason: reason, privateKind: privateKind, privateID: privateID, publicScope: application.ScopeController, publicTarget: "local-controller", classification: map[string]string{"class": "persistence_invariant"}})
	}
	queryIDs := func(query, reason, privateKind string, publicScope application.AuthorityScopeKind) error {
		rows, err := tx.QueryContext(ctx, query, application.IntegrityMaximumLimit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			target := "local-controller"
			scope := publicScope
			if strings.TrimSpace(id) == "" || strings.ContainsRune(id, '\x00') {
				scope = application.ScopeController
			} else if scope != application.ScopeController {
				target = id
			}
			findings = append(findings, integrityPrivateFinding{reason: reason, privateKind: privateKind, privateID: id, publicScope: scope, publicTarget: target, classification: map[string]string{"class": "binding_consistency"}})
		}
		return rows.Err()
	}

	switch family {
	case application.IntegrityStorageSchema:
		var count, minimum, maximum int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(version),0),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &minimum, &maximum); err != nil || count != schemaVersion || minimum != 1 || maximum != schemaVersion {
			addController("migration_continuity_violation", "schema", "migrations")
		}
		for sourceFamily, tables := range integritySourceFamilies {
			for _, table := range tables {
				var objects, triggers int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&objects); err != nil || objects != 1 {
					addController("required_object_missing", "registry_source", string(sourceFamily)+":"+table)
					continue
				}
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN (?,?,?)`, "integrity_track_"+table+"_insert", "integrity_track_"+table+"_update", "integrity_track_"+table+"_delete").Scan(&triggers); err != nil || triggers != 3 {
					addController("mutation_coverage_conflict", "registry_source", string(sourceFamily)+":"+table)
				}
			}
		}
		var recheckObjects, residualGuards int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('controller_integrity_rechecks','controller_integrity_active_recheck','controller_integrity_finalization_guard')`).Scan(&recheckObjects); err != nil || recheckObjects != 3 {
			addController("required_object_missing", "integrity_recheck", "schema-v41")
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_finalization_guard`).Scan(&residualGuards); err != nil || residualGuards != 0 {
			addController("finalization_guard_conflict", "integrity_recheck", "finalization-guard")
			result.State, result.ReasonCode = application.IntegrityConflict, "finalization_guard_conflict"
		}
		var quick string
		if err := tx.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&quick); err != nil || quick != "ok" {
			addController("sqlite_consistency_violation", "storage", "quick-check")
		}
		var foreignTable string
		if err := tx.QueryRowContext(ctx, `SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&foreignTable); err == nil {
			addController("foreign_key_consistency_violation", "storage", foreignTable)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return result, nil, err
		}
	case application.IntegrityRunDelivery:
		if err := queryIDs(`SELECT r.run_id FROM runs r WHERE NOT EXISTS(SELECT 1 FROM transitions t WHERE t.run_id=r.run_id) OR r.current_state<>COALESCE((SELECT t.to_state FROM transitions t WHERE t.run_id=r.run_id ORDER BY t.sequence DESC LIMIT 1),'') OR (SELECT COUNT(*) FROM transitions t WHERE t.run_id=r.run_id)<>(SELECT COALESCE(MAX(t.sequence),0) FROM transitions t WHERE t.run_id=r.run_id) ORDER BY r.run_id LIMIT ?`, "run_transition_continuity_violation", "run", application.ScopeRun); err != nil {
			return result, nil, err
		}
	case application.IntegrityOperationActivity:
		var conflicts int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity_backfill_progress WHERE status='conflict'`).Scan(&conflicts); err != nil {
			return result, nil, err
		}
		if conflicts != 0 {
			addController("activity_source_conflict", "activity", "backfill")
			result.State, result.ReasonCode = application.IntegrityConflict, "activity_source_conflict"
		}
		var incomplete int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity_backfill_progress WHERE status<>'complete'`).Scan(&incomplete); err != nil {
			return result, nil, err
		}
		if conflicts == 0 && incomplete != 0 {
			result.State, result.ReasonCode, result.CountComplete, result.CoverageComplete = application.IntegrityUnknown, "activity_backfill_incomplete", false, false
		}
		if incomplete == 0 {
			if err := queryIDs(`SELECT operation_id FROM operation_receipts r WHERE r.outcome<>'pending' AND (SELECT COUNT(*) FROM activity_operation_links l WHERE l.operation_id=r.operation_id)<>1 ORDER BY operation_id LIMIT ?`, "operation_activity_link_violation", "operation", application.ScopeController); err != nil {
				return result, nil, err
			}
		}
		var recheckSchema int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='controller_integrity_rechecks'`).Scan(&recheckSchema); err != nil {
			return result, nil, err
		}
		if recheckSchema == 1 {
			if err := queryIDs(`SELECT r.request_key FROM controller_integrity_rechecks r LEFT JOIN operation_receipts o ON o.operation_id=r.operation_id LEFT JOIN controller_integrity_scans s ON s.scan_id=r.scan_id WHERE (r.status='active' AND (o.phase<>'applied' OR o.outcome<>'pending' OR s.status<>'active' OR (SELECT COUNT(*) FROM controller_integrity_active_recheck a WHERE a.request_key=r.request_key AND a.operation_id=r.operation_id AND a.scan_id=r.scan_id AND a.target_generation=r.target_generation)<>1) AND NOT EXISTS(SELECT 1 FROM controller_integrity_finalization_guard g WHERE g.operation_id=r.operation_id AND g.scan_id=r.scan_id AND g.target_generation=r.target_generation AND o.phase='observed' AND o.outcome='succeeded')) OR (r.status='observed' AND (o.phase<>'observed' OR o.outcome<>'succeeded' OR s.status<>'published' OR r.observation_id='' OR NOT EXISTS(SELECT 1 FROM controller_integrity_observations i WHERE i.observation_id=r.observation_id AND i.scan_id=r.scan_id AND i.observation_digest=r.observation_digest AND i.target_generation=r.target_generation))) OR (r.status IN ('conflict','ambiguous') AND (o.phase<>'observed' OR o.outcome<>r.status OR r.observation_id<>'')) ORDER BY r.request_key LIMIT ?`, "integrity_recheck_binding_violation", "integrity_recheck", application.ScopeController); err != nil {
				return result, nil, err
			}
		}
	case application.IntegrityConfiguration:
		if err := queryIDs(`SELECT CAST(g.generation_id AS TEXT) FROM configuration_generations g WHERE g.parent_generation_id>=g.generation_id ORDER BY g.generation_id LIMIT ?`, "configuration_ancestry_violation", "generation", application.ScopeController); err != nil {
			return result, nil, err
		}
		var authorityConflict int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM configuration_authority a LEFT JOIN configuration_generations d ON d.generation_id=a.desired_generation_id LEFT JOIN configuration_generations e ON e.generation_id=a.effective_generation_id WHERE d.generation_id IS NULL OR (a.effective_generation_id IS NOT NULL AND e.generation_id IS NULL)`).Scan(&authorityConflict); err != nil {
			return result, nil, err
		}
		if authorityConflict != 0 {
			addController("configuration_authority_binding_violation", "configuration", "authority")
		}
	case application.IntegrityRepositoryOnboarding:
		if err := queryIDs(`SELECT s.repository FROM repository_readiness_snapshots s WHERE (SELECT COUNT(*) FROM repository_readiness_dimensions d WHERE d.snapshot_id=s.snapshot_id)<>8 ORDER BY s.repository LIMIT ?`, "readiness_dimension_completeness_violation", "readiness_snapshot", application.ScopeRepository); err != nil {
			return result, nil, err
		}
		if err := queryIDs(`SELECT l.repository FROM repository_lifecycles l WHERE l.current_snapshot_id<>'' AND NOT EXISTS(SELECT 1 FROM repository_readiness_snapshots s WHERE s.snapshot_id=l.current_snapshot_id AND s.incarnation_id=l.incarnation_id) ORDER BY l.repository LIMIT ?`, "readiness_pointer_violation", "repository", application.ScopeRepository); err != nil {
			return result, nil, err
		}
		if err := queryIDs(`SELECT c.onboarding_id FROM repository_onboarding_step_claims c JOIN repository_onboarding_steps s ON s.onboarding_id=c.onboarding_id AND s.step_name=c.step_name JOIN repository_onboardings o ON o.onboarding_id=c.onboarding_id WHERE c.intent_digest<>s.intent_digest OR c.attempt_number>s.attempt_number OR (c.status='active' AND (c.attempt_number<>s.attempt_number OR s.status<>'intended' OR o.step_index+1<>s.step_order)) OR (c.status='settled' AND c.attempt_number=s.attempt_number AND s.status<>'observed') OR (c.status='superseded' AND NOT EXISTS(SELECT 1 FROM repository_onboarding_step_claims n WHERE n.onboarding_id=c.onboarding_id AND n.step_name=c.step_name AND n.attempt_number=c.attempt_number AND n.claim_version=c.claim_version+1)) OR c.claim_version<>(SELECT COUNT(*) FROM repository_onboarding_step_claims p WHERE p.onboarding_id=c.onboarding_id AND p.step_name=c.step_name AND p.attempt_number=c.attempt_number AND p.claim_version<=c.claim_version) ORDER BY c.onboarding_id,c.step_name,c.attempt_number,c.claim_version LIMIT ?`, "onboarding_claim_binding_violation", "onboarding_claim", application.ScopeOnboarding); err != nil {
			return result, nil, err
		}
		claimRows, err := tx.QueryContext(ctx, `SELECT onboarding_id,step_name,attempt_number,intent_digest,supervisor_id,execution_nonce,claim_version,claim_digest FROM repository_onboarding_step_claims ORDER BY onboarding_id,step_name,attempt_number,claim_version LIMIT ?`, application.IntegrityMaximumLimit+1)
		if err != nil {
			return result, nil, err
		}
		claimCount := 0
		for claimRows.Next() {
			claimCount++
			var onboardingID, stepName, intentDigest, supervisorID, executionNonce, claimDigest string
			var attempt, version int64
			if err := claimRows.Scan(&onboardingID, &stepName, &attempt, &intentDigest, &supervisorID, &executionNonce, &version, &claimDigest); err != nil {
				claimRows.Close()
				return result, nil, err
			}
			if claimDigest != onboardingStepClaimDigest(onboardingID, domain.OnboardingStep(stepName), attempt, intentDigest, supervisorID, executionNonce, version) {
				addController("onboarding_claim_digest_violation", "onboarding_claim", fmt.Sprintf("%s:%s:%d:%d", onboardingID, stepName, attempt, version))
			}
		}
		if err := claimRows.Err(); err != nil {
			claimRows.Close()
			return result, nil, err
		}
		if err := claimRows.Close(); err != nil {
			return result, nil, err
		}
		if claimCount > application.IntegrityMaximumLimit {
			result.CountComplete, result.CoverageComplete = false, false
			if result.State != application.IntegrityConflict {
				result.State, result.ReasonCode = application.IntegrityUnknown, "claim_scan_bound_exhausted"
			}
		}
	case application.IntegritySchedulingAdmission:
		if err := queryIDs(`SELECT s.run_id FROM repository_slots s JOIN runs r ON r.run_id=s.run_id WHERE r.repository_binding_digest<>s.repository_binding_digest ORDER BY s.run_id LIMIT ?`, "repository_slot_binding_violation", "run", application.ScopeRun); err != nil {
			return result, nil, err
		}
		if err := queryIDs(`SELECT p.run_id FROM heavy_permits p LEFT JOIN run_scheduling s ON s.run_id=p.run_id WHERE s.run_id IS NULL OR s.supervisor_state IN ('terminal','human_wait','external_wait') ORDER BY p.run_id LIMIT ?`, "heavy_permit_lifecycle_violation", "run", application.ScopeRun); err != nil {
			return result, nil, err
		}
	case application.IntegrityOwnedResourceCleanup:
		if err := queryIDs(`SELECT o.owning_run FROM owned_resources o JOIN runs r ON r.run_id=o.owning_run WHERE o.ownership_status='released' AND NOT EXISTS(SELECT 1 FROM cleanup_results c WHERE c.run_id=o.owning_run AND c.resource_kind=o.resource_kind AND c.resource_name=o.resource_name AND c.status='succeeded') ORDER BY o.owning_run LIMIT ?`, "release_evidence_violation", "run", application.ScopeRun); err != nil {
			return result, nil, err
		}
	default:
		return result, nil, errors.New("integrity family is unsupported")
	}

	if len(findings) > application.IntegrityMaximumLimit {
		findings = findings[:application.IntegrityMaximumLimit]
		result.CountComplete, result.CoverageComplete = false, false
		if result.State != application.IntegrityConflict {
			result.State, result.ReasonCode = application.IntegrityUnknown, "finding_bound_exhausted"
		}
	}
	result.AffectedScopeCount = distinctIntegrityScopes(findings)
	if len(findings) != 0 && result.State == application.IntegrityReady {
		result.State, result.ReasonCode = application.IntegrityNotReady, "deterministic_findings"
	}
	return result, findings, nil
}

func distinctIntegrityScopes(findings []integrityPrivateFinding) int {
	seen := map[string]struct{}{}
	for _, finding := range findings {
		seen[string(finding.publicScope)+"\x00"+finding.publicTarget] = struct{}{}
	}
	return len(seen)
}

func persistIntegrityFamilyTx(ctx context.Context, tx *sql.Tx, scanID string, result application.IntegrityFamilyResult, findings []integrityPrivateFinding, observedAt time.Time) error {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		id := integrityDigest("finding", scanID, string(result.Family), finding.reason, finding.privateKind, finding.privateID)
		classification, _ := json.Marshal(finding.classification)
		if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_scan_findings(finding_id,scan_id,family,reason_code,private_scope_kind,private_scope_id,public_scope_kind,public_target_id,classification_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, scanID, result.Family, finding.reason, finding.privateKind, finding.privateID, finding.publicScope, finding.publicTarget, string(classification), formatTime(observedAt)); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	_, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_checked_families(scan_id,family,checked_revision,state,reason_code,affected_scope_count,count_complete,coverage_complete,findings_digest,checked_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, scanID, result.Family, result.CheckedRevision, result.State, result.ReasonCode, result.AffectedScopeCount, boolInt(result.CountComplete), boolInt(result.CoverageComplete), integrityDigest(append([]string{"findings"}, ids...)...), formatTime(observedAt))
	return err
}

func buildIntegrityObservationTx(ctx context.Context, tx *sql.Tx, scanID string, generation int64, observedAt time.Time) (application.IntegrityObservation, []string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT family,checked_revision,state,reason_code,affected_scope_count,count_complete,coverage_complete FROM controller_integrity_checked_families WHERE scan_id=? ORDER BY CASE family WHEN 'storage_schema' THEN 0 WHEN 'run_delivery' THEN 1 WHEN 'operation_activity' THEN 2 WHEN 'configuration' THEN 3 WHEN 'repository_onboarding' THEN 4 WHEN 'scheduling_admission' THEN 5 WHEN 'owned_resource_cleanup' THEN 6 ELSE 7 END`, scanID)
	if err != nil {
		return application.IntegrityObservation{}, nil, err
	}
	var results []application.IntegrityFamilyResult
	for rows.Next() {
		var result application.IntegrityFamilyResult
		var complete, coverage int
		if err := rows.Scan(&result.Family, &result.CheckedRevision, &result.State, &result.ReasonCode, &result.AffectedScopeCount, &complete, &coverage); err != nil {
			rows.Close()
			return application.IntegrityObservation{}, nil, err
		}
		result.CountComplete, result.CoverageComplete = complete != 0, coverage != 0
		results = append(results, result)
	}
	if err := rows.Close(); err != nil || len(results) != len(application.IntegrityFamilies()) {
		return application.IntegrityObservation{}, nil, errors.New("integrity publication family evidence conflicts")
	}
	states := make([]application.IntegrityState, 0, len(results))
	countComplete, coverageComplete := true, true
	for index, result := range results {
		if result.Family != application.IntegrityFamilies()[index] || result.Validate() != nil {
			return application.IntegrityObservation{}, nil, errors.New("integrity publication family evidence conflicts")
		}
		states = append(states, result.State)
		countComplete = countComplete && result.CountComplete
		coverageComplete = coverageComplete && result.CoverageComplete
	}
	readiness := application.AggregateIntegrity(states...)
	reason := "complete"
	if readiness != application.IntegrityReady {
		reason = "family_" + string(readiness)
	}
	var affected int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT DISTINCT public_scope_kind,public_target_id FROM controller_integrity_scan_findings WHERE scan_id=?)`, scanID).Scan(&affected); err != nil {
		return application.IntegrityObservation{}, nil, err
	}
	observationID := integrityDigest("observation", scanID)
	findingRows, err := tx.QueryContext(ctx, `SELECT finding_id FROM controller_integrity_scan_findings WHERE scan_id=? ORDER BY finding_id`, scanID)
	if err != nil {
		return application.IntegrityObservation{}, nil, err
	}
	var findingIDs []string
	for findingRows.Next() {
		var findingID string
		if err := findingRows.Scan(&findingID); err != nil {
			findingRows.Close()
			return application.IntegrityObservation{}, nil, err
		}
		findingIDs = append(findingIDs, findingID)
	}
	if err := findingRows.Close(); err != nil {
		return application.IntegrityObservation{}, nil, err
	}
	observation := application.IntegrityObservation{SchemaVersion: application.IntegritySchemaVersion, RegistryVersion: application.IntegrityRegistryVersion, ObservationID: observationID, TargetGeneration: generation, PublishedGeneration: generation, ObservedAt: observedAt.UTC(), EffectiveReadiness: readiness, ReasonCode: reason, Results: results, AffectedScopeCount: affected, CountComplete: countComplete, CoverageComplete: coverageComplete}
	observation.Digest = integrityObservationDigest(observation, findingIDs)
	return observation, findingIDs, nil
}

func persistIntegrityObservationTx(ctx context.Context, tx *sql.Tx, scanID string, observation application.IntegrityObservation) error {
	if observation.Validate() != nil || observation.ObservationID != integrityDigest("observation", scanID) {
		return errors.New("integrity observation publication is invalid")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_observations(observation_id,schema_version,registry_version,observation_digest,target_generation,published_generation,scan_id,effective_readiness,reason_code,affected_scope_count,count_complete,coverage_complete,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, observation.ObservationID, observation.SchemaVersion, observation.RegistryVersion, observation.Digest, observation.TargetGeneration, observation.PublishedGeneration, scanID, observation.EffectiveReadiness, observation.ReasonCode, observation.AffectedScopeCount, boolInt(observation.CountComplete), boolInt(observation.CoverageComplete), formatTime(observation.ObservedAt)); err != nil {
		return err
	}
	for _, result := range observation.Results {
		if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_observation_families(observation_id,family,state,reason_code,checked_revision,affected_scope_count,count_complete,coverage_complete) VALUES(?,?,?,?,?,?,?,?)`, observation.ObservationID, result.Family, result.State, result.ReasonCode, result.CheckedRevision, result.AffectedScopeCount, boolInt(result.CountComplete), boolInt(result.CoverageComplete)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_observation_findings(observation_id,finding_id,family,reason_code,public_scope_kind,public_target_id,classification_json) SELECT ?,finding_id,family,reason_code,public_scope_kind,public_target_id,classification_json FROM controller_integrity_scan_findings WHERE scan_id=?`, observation.ObservationID, scanID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_current(singleton,observation_id,observation_digest,published_generation) VALUES(1,?,?,?) ON CONFLICT(singleton) DO UPDATE SET observation_id=excluded.observation_id,observation_digest=excluded.observation_digest,published_generation=excluded.published_generation`, observation.ObservationID, observation.Digest, observation.PublishedGeneration); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE controller_integrity_scans SET status='published',reason_code=?,lease_owner='',lease_expires_at='',updated_at=?,completed_at=? WHERE scan_id=? AND status='active'`, observation.ReasonCode, formatTime(observation.ObservedAt), formatTime(observation.ObservedAt), scanID)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return errors.New("integrity scan publication conflicts")
	}
	return err
}

func publishIntegrityObservationTx(ctx context.Context, tx *sql.Tx, scanID string, generation int64, observedAt time.Time) error {
	observation, _, err := buildIntegrityObservationTx(ctx, tx, scanID, generation, observedAt)
	if err != nil {
		return err
	}
	return persistIntegrityObservationTx(ctx, tx, scanID, observation)
}

func (s *Store) IntegritySummary(ctx context.Context, scopes application.AuthorizedScopeSet) (application.IntegritySummary, error) {
	if scopes.Empty() || !scopes.HasController() {
		return application.IntegritySummary{}, errors.New("integrity summary is not found")
	}
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generation); err != nil {
		return application.IntegritySummary{Readiness: application.IntegrityConflict, ReasonCode: "scan_control_conflict"}, nil
	}
	var observationID, digest string
	var published int64
	err := s.db.QueryRowContext(ctx, `SELECT observation_id,observation_digest,published_generation FROM controller_integrity_current WHERE singleton=1`).Scan(&observationID, &digest, &published)
	if errors.Is(err, sql.ErrNoRows) {
		return application.IntegritySummary{CurrentGeneration: generation, Readiness: application.IntegrityUnknown, ReasonCode: "initial_scan_required"}, nil
	}
	if err != nil {
		return application.IntegritySummary{CurrentGeneration: generation, Readiness: application.IntegrityConflict, ReasonCode: "current_pointer_conflict"}, nil
	}
	observation, err := s.loadIntegrityObservation(ctx, observationID)
	if err != nil || observation.Digest != digest || observation.PublishedGeneration != published {
		return application.IntegritySummary{CurrentGeneration: generation, Readiness: application.IntegrityConflict, ReasonCode: "observation_conflict"}, nil
	}
	current := observation.RegistryVersion == application.IntegrityRegistryVersion && observation.PublishedGeneration == generation
	summary := application.IntegritySummary{CurrentGeneration: generation, Current: current, Readiness: observation.EffectiveReadiness, ReasonCode: observation.ReasonCode, Observation: observation}
	if !current {
		summary.Readiness, summary.ReasonCode = application.IntegrityUnknown, "source_generation_advanced"
	}
	return summary, nil
}

func (s *Store) loadIntegrityObservation(ctx context.Context, observationID string) (application.IntegrityObservation, error) {
	var observation application.IntegrityObservation
	var observed string
	var countComplete, coverageComplete int
	err := s.db.QueryRowContext(ctx, `SELECT schema_version,registry_version,observation_id,observation_digest,target_generation,published_generation,effective_readiness,reason_code,affected_scope_count,count_complete,coverage_complete,observed_at FROM controller_integrity_observations WHERE observation_id=?`, observationID).Scan(&observation.SchemaVersion, &observation.RegistryVersion, &observation.ObservationID, &observation.Digest, &observation.TargetGeneration, &observation.PublishedGeneration, &observation.EffectiveReadiness, &observation.ReasonCode, &observation.AffectedScopeCount, &countComplete, &coverageComplete, &observed)
	if err != nil {
		return observation, err
	}
	observation.CountComplete, observation.CoverageComplete, observation.ObservedAt = countComplete != 0, coverageComplete != 0, parseTime(observed)
	rows, err := s.db.QueryContext(ctx, `SELECT family,state,reason_code,checked_revision,affected_scope_count,count_complete,coverage_complete FROM controller_integrity_observation_families WHERE observation_id=? ORDER BY CASE family WHEN 'storage_schema' THEN 0 WHEN 'run_delivery' THEN 1 WHEN 'operation_activity' THEN 2 WHEN 'configuration' THEN 3 WHEN 'repository_onboarding' THEN 4 WHEN 'scheduling_admission' THEN 5 WHEN 'owned_resource_cleanup' THEN 6 ELSE 7 END`, observationID)
	if err != nil {
		return observation, err
	}
	for rows.Next() {
		var result application.IntegrityFamilyResult
		var complete, coverage int
		if err := rows.Scan(&result.Family, &result.State, &result.ReasonCode, &result.CheckedRevision, &result.AffectedScopeCount, &complete, &coverage); err != nil {
			return observation, err
		}
		result.CountComplete, result.CoverageComplete = complete != 0, coverage != 0
		observation.Results = append(observation.Results, result)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return observation, err
	}
	if err := rows.Close(); err != nil {
		return observation, err
	}
	findingRows, err := s.db.QueryContext(ctx, `SELECT finding_id FROM controller_integrity_observation_findings WHERE observation_id=? ORDER BY finding_id`, observationID)
	if err != nil {
		return observation, err
	}
	var findingIDs []string
	for findingRows.Next() {
		var findingID string
		if err := findingRows.Scan(&findingID); err != nil {
			findingRows.Close()
			return observation, err
		}
		findingIDs = append(findingIDs, findingID)
	}
	if err := findingRows.Close(); err != nil {
		return observation, err
	}
	if observation.Validate() != nil || integrityObservationDigest(observation, findingIDs) != observation.Digest {
		return observation, errors.New("integrity observation digest conflicts")
	}
	return observation, nil
}

type integrityCursor struct {
	Schema            string `json:"schema"`
	ObservationDigest string `json:"observation_digest"`
	ScopeDigest       string `json:"scope_digest"`
	FilterDigest      string `json:"filter_digest"`
	LastID            string `json:"last_id"`
	Seal              string `json:"seal"`
}

func (s *Store) ListIntegrityFindings(ctx context.Context, scopes application.AuthorizedScopeSet, query application.IntegrityFindingQuery) (application.IntegrityFindingPage, error) {
	if scopes.Empty() || !scopes.HasController() {
		return application.IntegrityFindingPage{}, errors.New("integrity findings are not found")
	}
	filterDigest := integrityDigest("filter", string(query.Family), string(query.Scope), query.TargetID)
	var observationID, observationDigest string
	lastID := ""
	if query.Cursor != "" {
		cursor, err := decodeIntegrityCursor(query.Cursor)
		if err != nil || cursor.Schema != application.IntegritySchemaVersion || cursor.ScopeDigest != scopes.Digest() || cursor.FilterDigest != filterDigest {
			return application.IntegrityFindingPage{}, errors.New("integrity finding cursor conflicts")
		}
		observationDigest, lastID = cursor.ObservationDigest, cursor.LastID
		if err := s.db.QueryRowContext(ctx, `SELECT observation_id FROM controller_integrity_observations WHERE observation_digest=?`, observationDigest).Scan(&observationID); err != nil {
			return application.IntegrityFindingPage{}, errors.New("integrity finding cursor conflicts")
		}
	} else if err := s.db.QueryRowContext(ctx, `SELECT observation_id,observation_digest FROM controller_integrity_current WHERE singleton=1`).Scan(&observationID, &observationDigest); errors.Is(err, sql.ErrNoRows) {
		return application.IntegrityFindingPage{Findings: []application.IntegrityFinding{}, CountComplete: false}, nil
	} else if err != nil {
		return application.IntegrityFindingPage{}, errors.New("integrity finding evidence conflicts")
	}
	filters := []string{"observation_id=?"}
	filterArgs := []any{observationID}
	if query.Family != "" {
		filters, filterArgs = append(filters, "family=?"), append(filterArgs, query.Family)
	}
	if query.Scope != "" {
		filters, filterArgs = append(filters, "public_scope_kind=?"), append(filterArgs, query.Scope)
	}
	if query.TargetID != "" {
		filters, filterArgs = append(filters, "public_target_id=?"), append(filterArgs, query.TargetID)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_observation_findings WHERE `+strings.Join(filters, " AND "), filterArgs...).Scan(&total); err != nil {
		return application.IntegrityFindingPage{}, err
	}
	observation, err := s.loadIntegrityObservation(ctx, observationID)
	if err != nil {
		return application.IntegrityFindingPage{}, err
	}
	where := append(append([]string(nil), filters...), "finding_id>?")
	args := append(append([]any(nil), filterArgs...), lastID, query.Limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT finding_id,family,reason_code,public_scope_kind,public_target_id,classification_json FROM controller_integrity_observation_findings WHERE `+strings.Join(where, " AND ")+` ORDER BY finding_id LIMIT ?`, args...)
	if err != nil {
		return application.IntegrityFindingPage{}, err
	}
	defer rows.Close()
	page := application.IntegrityFindingPage{ObservationID: observationID, ObservationDigest: observationDigest, Count: total, CountComplete: true, Findings: []application.IntegrityFinding{}}
	page.CountComplete = observation.CountComplete
	for rows.Next() {
		var finding application.IntegrityFinding
		var classification string
		if err := rows.Scan(&finding.FindingID, &finding.Family, &finding.ReasonCode, &finding.Scope, &finding.TargetID, &classification); err != nil {
			return application.IntegrityFindingPage{}, err
		}
		finding.ObservationAt = observation.ObservedAt
		if err := json.Unmarshal([]byte(classification), &finding.Classification); err != nil || len(finding.Classification) > 8 {
			return application.IntegrityFindingPage{}, errors.New("integrity finding classification conflicts")
		}
		page.Findings = append(page.Findings, finding)
	}
	if err := rows.Err(); err != nil {
		return application.IntegrityFindingPage{}, err
	}
	if len(page.Findings) > query.Limit {
		last := page.Findings[query.Limit-1].FindingID
		page.Findings = page.Findings[:query.Limit]
		page.NextCursor = encodeIntegrityCursor(integrityCursor{Schema: application.IntegritySchemaVersion, ObservationDigest: observationDigest, ScopeDigest: scopes.Digest(), FilterDigest: filterDigest, LastID: last})
	}
	return page, nil
}

func encodeIntegrityCursor(cursor integrityCursor) string {
	cursor.Seal = integrityDigest("cursor", cursor.Schema, cursor.ObservationDigest, cursor.ScopeDigest, cursor.FilterDigest, cursor.LastID)
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeIntegrityCursor(value string) (integrityCursor, error) {
	var cursor integrityCursor
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &cursor) != nil || cursor.Seal != integrityDigest("cursor", cursor.Schema, cursor.ObservationDigest, cursor.ScopeDigest, cursor.FilterDigest, cursor.LastID) {
		return integrityCursor{}, errors.New("integrity cursor is invalid")
	}
	return cursor, nil
}

func integrityDigest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte{0})
		hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func integrityObservationDigest(observation application.IntegrityObservation, findingIDs []string) string {
	parts := []string{"observation", observation.SchemaVersion, observation.RegistryVersion, observation.ObservationID, fmt.Sprint(observation.TargetGeneration), fmt.Sprint(observation.PublishedGeneration), formatTime(observation.ObservedAt), string(observation.EffectiveReadiness), observation.ReasonCode, fmt.Sprint(observation.AffectedScopeCount), fmt.Sprint(observation.CountComplete), fmt.Sprint(observation.CoverageComplete)}
	for _, result := range observation.Results {
		parts = append(parts, string(result.Family), string(result.State), result.ReasonCode, fmt.Sprint(result.CheckedRevision), fmt.Sprint(result.AffectedScopeCount), fmt.Sprint(result.CountComplete), fmt.Sprint(result.CoverageComplete))
	}
	parts = append(parts, findingIDs...)
	return integrityDigest(parts...)
}
