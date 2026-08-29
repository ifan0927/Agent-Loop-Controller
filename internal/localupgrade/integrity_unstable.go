package localupgrade

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const installedIntegrityPublicationSchemaVersion = 43

type stableReadyObservation struct {
	observationID       string
	observationDigest   string
	scanID              string
	targetGeneration    int64
	publishedGeneration int64
	observedAt          string
}

// integrityPublicationNotStable recognizes only repeated, complete v43
// convergence publications followed by durable post-publication scan churn.
// It exists solely to let an already-installed affected binary enter the
// managed-successor path; ordinary stale readiness remains pending.
func integrityPublicationNotStable(ctx context.Context, db *sql.DB) bool {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false
	}
	defer tx.Rollback()

	var currentGeneration, pointerGeneration int64
	var pointerID, pointerDigest string
	if err := tx.QueryRowContext(ctx, `SELECT g.generation,c.observation_id,c.observation_digest,c.published_generation
		FROM controller_integrity_generation g JOIN controller_integrity_current c ON c.singleton=1 WHERE g.singleton=1`).Scan(&currentGeneration, &pointerID, &pointerDigest, &pointerGeneration); err != nil || currentGeneration <= pointerGeneration {
		return false
	}
	latest, ok := loadStableReadyObservation(ctx, tx, pointerID)
	if !ok || latest.observationDigest != pointerDigest || latest.publishedGeneration != pointerGeneration {
		return false
	}
	var newerObservations int
	if tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_observations WHERE published_generation>?`, latest.publishedGeneration).Scan(&newerObservations) != nil || newerObservations != 0 {
		return false
	}
	var previousID string
	if tx.QueryRowContext(ctx, `SELECT observation_id FROM controller_integrity_observations WHERE published_generation<? ORDER BY published_generation DESC LIMIT 1`, latest.publishedGeneration).Scan(&previousID) != nil {
		return false
	}
	previous, ok := loadStableReadyObservation(ctx, tx, previousID)
	if !ok || previous.publishedGeneration >= latest.publishedGeneration {
		return false
	}
	previousTime, previousErr := time.Parse(time.RFC3339Nano, previous.observedAt)
	latestTime, latestErr := time.Parse(time.RFC3339Nano, latest.observedAt)
	if previousErr != nil || latestErr != nil || !previousTime.Before(latestTime) {
		return false
	}

	var priorScanID, priorRegistry, priorBoundary string
	var priorTarget int64
	if tx.QueryRowContext(ctx, `SELECT scan_id,registry_version,target_generation,stable_boundary FROM controller_integrity_scans
		WHERE target_generation>? AND target_generation<? AND status='superseded' AND reason_code='source_generation_advanced' ORDER BY target_generation LIMIT 1`, previous.publishedGeneration, latest.publishedGeneration).Scan(&priorScanID, &priorRegistry, &priorTarget, &priorBoundary) != nil ||
		!validIntegrityScanIdentity(priorScanID, priorRegistry, priorTarget, priorBoundary) {
		return false
	}
	var postScanID, postStatus, postReason, postRegistry, postBoundary string
	var postTarget int64
	if tx.QueryRowContext(ctx, `SELECT scan_id,registry_version,target_generation,stable_boundary,status,reason_code
		FROM controller_integrity_scans WHERE target_generation>? ORDER BY target_generation LIMIT 1`, latest.publishedGeneration).Scan(&postScanID, &postRegistry, &postTarget, &postBoundary, &postStatus, &postReason) != nil ||
		!validIntegrityScanIdentity(postScanID, postRegistry, postTarget, postBoundary) || postTarget >= currentGeneration {
		return false
	}
	switch postStatus {
	case "active":
		if postReason != "" {
			return false
		}
	case "superseded":
		if postReason != "source_generation_advanced" {
			return false
		}
	default:
		return false
	}
	return tx.Commit() == nil
}

func validIntegrityScanIdentity(scanID, registry string, target int64, boundary string) bool {
	return target >= 0 && registry == "v1" &&
		scanID == localUpgradeIntegrityDigest("scan", registry, fmt.Sprint(target)) &&
		boundary == localUpgradeIntegrityDigest("boundary", fmt.Sprint(target))
}

func loadStableReadyObservation(ctx context.Context, tx *sql.Tx, observationID string) (stableReadyObservation, bool) {
	var observation stableReadyObservation
	var schema, registry, readiness, reason string
	var affected int64
	var countComplete, coverageComplete int
	var scanRegistry, stableBoundary, scanStatus, scanReason string
	var scanTarget int64
	var scanCursor, convergenceAttempt int
	err := tx.QueryRowContext(ctx, `SELECT
		o.observation_id,o.observation_digest,o.schema_version,o.registry_version,o.target_generation,o.published_generation,
		o.scan_id,o.effective_readiness,o.reason_code,o.affected_scope_count,o.count_complete,o.coverage_complete,o.observed_at,
		s.registry_version,s.target_generation,s.stable_boundary,s.family_cursor,s.status,s.convergence_attempt,s.reason_code
		FROM controller_integrity_observations o JOIN controller_integrity_scans s ON s.scan_id=o.scan_id
		WHERE o.observation_id=?`, observationID).Scan(
		&observation.observationID, &observation.observationDigest, &schema, &registry, &observation.targetGeneration, &observation.publishedGeneration,
		&observation.scanID, &readiness, &reason, &affected, &countComplete, &coverageComplete, &observation.observedAt,
		&scanRegistry, &scanTarget, &stableBoundary, &scanCursor, &scanStatus, &convergenceAttempt, &scanReason,
	)
	if err != nil || observation.observationID != localUpgradeIntegrityDigest("observation", observation.scanID) ||
		!validSHA256(observation.observationDigest) || schema != "v1" || registry != "v1" ||
		observation.targetGeneration != observation.publishedGeneration || readiness != "ready" || reason != "complete" ||
		affected != 0 || countComplete != 1 || coverageComplete != 1 ||
		scanRegistry != "v1" || scanTarget != observation.publishedGeneration ||
		observation.scanID != localUpgradeIntegrityDigest("scan", "v1", fmt.Sprint(scanTarget)) ||
		stableBoundary != localUpgradeIntegrityDigest("boundary", fmt.Sprint(scanTarget)) || scanCursor != len(legacyIntegrityFamilies) ||
		scanStatus != "published" || convergenceAttempt != 8 || scanReason != "complete" {
		return stableReadyObservation{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, observation.observedAt); err != nil || parsed.IsZero() || parsed.UTC().Format(time.RFC3339Nano) != observation.observedAt {
		return stableReadyObservation{}, false
	}
	if !validIntegrityRegistry(ctx, tx) {
		return stableReadyObservation{}, false
	}

	type familyEvidence struct {
		family             string
		state              string
		reason             string
		checkedRevision    int64
		affectedScopeCount int64
		countComplete      int
		coverageComplete   int
	}
	rows, err := tx.QueryContext(ctx, `SELECT family,state,reason_code,checked_revision,affected_scope_count,count_complete,coverage_complete
		FROM controller_integrity_observation_families WHERE observation_id=?
		ORDER BY CASE family WHEN 'storage_schema' THEN 0 WHEN 'run_delivery' THEN 1 WHEN 'operation_activity' THEN 2 WHEN 'configuration' THEN 3 WHEN 'repository_onboarding' THEN 4 WHEN 'scheduling_admission' THEN 5 WHEN 'owned_resource_cleanup' THEN 6 ELSE 7 END`, observation.observationID)
	if err != nil {
		return stableReadyObservation{}, false
	}
	var families []familyEvidence
	for rows.Next() {
		var family familyEvidence
		if rows.Scan(&family.family, &family.state, &family.reason, &family.checkedRevision, &family.affectedScopeCount, &family.countComplete, &family.coverageComplete) != nil ||
			len(families) >= len(legacyIntegrityFamilies) || family.family != legacyIntegrityFamilies[len(families)] || family.state != "ready" || family.reason != "complete" ||
			family.checkedRevision < 0 || family.affectedScopeCount != 0 || family.countComplete != 1 || family.coverageComplete != 1 {
			rows.Close()
			return stableReadyObservation{}, false
		}
		families = append(families, family)
	}
	if rows.Err() != nil || rows.Close() != nil || len(families) != len(legacyIntegrityFamilies) {
		return stableReadyObservation{}, false
	}
	checkedRows, err := tx.QueryContext(ctx, `SELECT family,state,reason_code,checked_revision,affected_scope_count,count_complete,coverage_complete,findings_digest
		FROM controller_integrity_checked_families WHERE scan_id=?
		ORDER BY CASE family WHEN 'storage_schema' THEN 0 WHEN 'run_delivery' THEN 1 WHEN 'operation_activity' THEN 2 WHEN 'configuration' THEN 3 WHEN 'repository_onboarding' THEN 4 WHEN 'scheduling_admission' THEN 5 WHEN 'owned_resource_cleanup' THEN 6 ELSE 7 END`, observation.scanID)
	if err != nil {
		return stableReadyObservation{}, false
	}
	checkedIndex := 0
	for checkedRows.Next() {
		var checked familyEvidence
		var findingsDigest string
		if checkedRows.Scan(&checked.family, &checked.state, &checked.reason, &checked.checkedRevision, &checked.affectedScopeCount, &checked.countComplete, &checked.coverageComplete, &findingsDigest) != nil ||
			checkedIndex >= len(families) || checked != families[checkedIndex] || findingsDigest != localUpgradeIntegrityDigest("findings") {
			checkedRows.Close()
			return stableReadyObservation{}, false
		}
		checkedIndex++
	}
	if checkedRows.Err() != nil || checkedRows.Close() != nil || checkedIndex != len(families) {
		return stableReadyObservation{}, false
	}
	var observationFindings, scanFindings int
	if tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_observation_findings WHERE observation_id=?`, observation.observationID).Scan(&observationFindings) != nil || observationFindings != 0 ||
		tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_scan_findings WHERE scan_id=?`, observation.scanID).Scan(&scanFindings) != nil || scanFindings != 0 {
		return stableReadyObservation{}, false
	}
	parts := []string{"observation", schema, registry, observation.observationID, fmt.Sprint(observation.targetGeneration), fmt.Sprint(observation.publishedGeneration), observation.observedAt, readiness, reason, "0", "true", "true"}
	for _, family := range families {
		parts = append(parts, family.family, family.state, family.reason, fmt.Sprint(family.checkedRevision), "0", "true", "true")
	}
	if localUpgradeIntegrityDigest(parts...) != observation.observationDigest {
		return stableReadyObservation{}, false
	}
	return observation, true
}

func validIntegrityRegistry(ctx context.Context, tx *sql.Tx) bool {
	rows, err := tx.QueryContext(ctx, `SELECT family,family_order,reason_version FROM integrity_registry_families WHERE registry_version='v1' ORDER BY family_order`)
	if err != nil {
		return false
	}
	index := 0
	for rows.Next() {
		var family, version string
		var order int
		if rows.Scan(&family, &order, &version) != nil || index >= len(legacyIntegrityFamilies) || family != legacyIntegrityFamilies[index] || order != index || version != "v1" {
			rows.Close()
			return false
		}
		index++
	}
	return rows.Err() == nil && rows.Close() == nil && index == len(legacyIntegrityFamilies)
}
