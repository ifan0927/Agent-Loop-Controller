package localupgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

var legacyIntegrityFamilies = [...]string{
	"storage_schema",
	"run_delivery",
	"operation_activity",
	"configuration",
	"repository_onboarding",
	"scheduling_admission",
	"owned_resource_cleanup",
}

const legacyIntegrityConvergenceSchemaVersion = 42

type legacyIntegrityFamilyEvidence struct {
	family             string
	state              string
	reasonCode         string
	checkedRevision    int64
	affectedScopeCount int64
	countComplete      int
	coverageComplete   int
}

// legacyIntegrityConvergenceExhausted recognizes only the immutable v1
// observation shape published by the pre-v43 convergence bound.
func legacyIntegrityConvergenceExhausted(ctx context.Context, db *sql.DB) bool {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false
	}
	defer tx.Rollback()

	var currentGeneration int64
	var pointerID, pointerDigest string
	var pointerGeneration int64
	var observationID, observationDigest, schema, registry, scanID, readiness, reasonCode, observedAt string
	var targetGeneration, publishedGeneration, affectedScopeCount int64
	var countComplete, coverageComplete int
	var linkedScanID, scanRegistry, stableBoundary, scanStatus, scanReason string
	var scanTarget int64
	var scanCursor, convergenceAttempt int
	err = tx.QueryRowContext(ctx, `SELECT
		g.generation,
		c.observation_id,c.observation_digest,c.published_generation,
		o.observation_id,o.observation_digest,o.schema_version,o.registry_version,o.target_generation,o.published_generation,
		o.scan_id,o.effective_readiness,o.reason_code,o.affected_scope_count,o.count_complete,o.coverage_complete,o.observed_at,
		s.scan_id,s.registry_version,s.target_generation,s.stable_boundary,s.family_cursor,s.status,s.convergence_attempt,s.reason_code
		FROM controller_integrity_generation g
		JOIN controller_integrity_current c ON c.singleton=1
		JOIN controller_integrity_observations o ON o.observation_id=c.observation_id
		JOIN controller_integrity_scans s ON s.scan_id=o.scan_id
		WHERE g.singleton=1`).Scan(
		&currentGeneration,
		&pointerID, &pointerDigest, &pointerGeneration,
		&observationID, &observationDigest, &schema, &registry, &targetGeneration, &publishedGeneration,
		&scanID, &readiness, &reasonCode, &affectedScopeCount, &countComplete, &coverageComplete, &observedAt,
		&linkedScanID, &scanRegistry, &scanTarget, &stableBoundary, &scanCursor, &scanStatus, &convergenceAttempt, &scanReason,
	)
	if err != nil || currentGeneration < publishedGeneration ||
		pointerID != observationID || pointerDigest != observationDigest || pointerGeneration != publishedGeneration ||
		!validSHA256(observationDigest) || schema != "v1" || registry != "v1" ||
		targetGeneration != publishedGeneration || readiness != "unknown" || reasonCode != "family_unknown" ||
		affectedScopeCount != 0 || countComplete != 0 || coverageComplete != 0 ||
		scanID != linkedScanID || observationID != localUpgradeIntegrityDigest("observation", scanID) ||
		scanID != localUpgradeIntegrityDigest("scan", "v1", fmt.Sprint(publishedGeneration)) ||
		scanRegistry != "v1" || scanTarget != publishedGeneration || stableBoundary != localUpgradeIntegrityDigest("boundary", fmt.Sprint(publishedGeneration)) || scanCursor != len(legacyIntegrityFamilies) ||
		scanStatus != "published" || convergenceAttempt != 8 || scanReason != "family_unknown" {
		return false
	}
	if parsed, parseErr := time.Parse(time.RFC3339Nano, observedAt); parseErr != nil || parsed.IsZero() || parsed.UTC().Format(time.RFC3339Nano) != observedAt {
		return false
	}

	registryRows, err := tx.QueryContext(ctx, `SELECT family,family_order,reason_version FROM integrity_registry_families WHERE registry_version='v1' ORDER BY family_order`)
	if err != nil {
		return false
	}
	registryIndex := 0
	for registryRows.Next() {
		var family, version string
		var order int
		if registryRows.Scan(&family, &order, &version) != nil || registryIndex >= len(legacyIntegrityFamilies) || family != legacyIntegrityFamilies[registryIndex] || order != registryIndex || version != "v1" {
			registryRows.Close()
			return false
		}
		registryIndex++
	}
	if registryRows.Err() != nil || registryRows.Close() != nil || registryIndex != len(legacyIntegrityFamilies) {
		return false
	}

	rows, err := tx.QueryContext(ctx, `SELECT family,state,reason_code,checked_revision,affected_scope_count,count_complete,coverage_complete
		FROM controller_integrity_observation_families WHERE observation_id=?
		ORDER BY CASE family WHEN 'storage_schema' THEN 0 WHEN 'run_delivery' THEN 1 WHEN 'operation_activity' THEN 2 WHEN 'configuration' THEN 3 WHEN 'repository_onboarding' THEN 4 WHEN 'scheduling_admission' THEN 5 WHEN 'owned_resource_cleanup' THEN 6 ELSE 7 END`, observationID)
	if err != nil {
		return false
	}
	families := make([]legacyIntegrityFamilyEvidence, 0, len(legacyIntegrityFamilies))
	for rows.Next() {
		var family legacyIntegrityFamilyEvidence
		if rows.Scan(&family.family, &family.state, &family.reasonCode, &family.checkedRevision, &family.affectedScopeCount, &family.countComplete, &family.coverageComplete) != nil ||
			len(families) >= len(legacyIntegrityFamilies) || family.family != legacyIntegrityFamilies[len(families)] ||
			family.state != "unknown" || family.reasonCode != "convergence_bound_exhausted" || family.checkedRevision < 0 ||
			family.affectedScopeCount != 0 || family.countComplete != 0 || family.coverageComplete != 0 {
			rows.Close()
			return false
		}
		families = append(families, family)
	}
	if rows.Err() != nil || rows.Close() != nil || len(families) != len(legacyIntegrityFamilies) {
		return false
	}
	checkedRows, err := tx.QueryContext(ctx, `SELECT family,state,reason_code,checked_revision,affected_scope_count,count_complete,coverage_complete,findings_digest
		FROM controller_integrity_checked_families WHERE scan_id=?
		ORDER BY CASE family WHEN 'storage_schema' THEN 0 WHEN 'run_delivery' THEN 1 WHEN 'operation_activity' THEN 2 WHEN 'configuration' THEN 3 WHEN 'repository_onboarding' THEN 4 WHEN 'scheduling_admission' THEN 5 WHEN 'owned_resource_cleanup' THEN 6 ELSE 7 END`, scanID)
	if err != nil {
		return false
	}
	checkedIndex := 0
	for checkedRows.Next() {
		var checked legacyIntegrityFamilyEvidence
		var findingsDigest string
		if checkedRows.Scan(&checked.family, &checked.state, &checked.reasonCode, &checked.checkedRevision, &checked.affectedScopeCount, &checked.countComplete, &checked.coverageComplete, &findingsDigest) != nil ||
			findingsDigest != localUpgradeIntegrityDigest("findings") ||
			checkedIndex >= len(families) || checked != families[checkedIndex] {
			checkedRows.Close()
			return false
		}
		checkedIndex++
	}
	if checkedRows.Err() != nil || checkedRows.Close() != nil || checkedIndex != len(families) {
		return false
	}
	var observationFindings, scanFindings int
	if tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_observation_findings WHERE observation_id=?`, observationID).Scan(&observationFindings) != nil || observationFindings != 0 ||
		tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_scan_findings WHERE scan_id=?`, scanID).Scan(&scanFindings) != nil || scanFindings != 0 {
		return false
	}

	parts := []string{
		"observation", schema, registry, observationID,
		fmt.Sprint(targetGeneration), fmt.Sprint(publishedGeneration), observedAt,
		readiness, reasonCode, fmt.Sprint(affectedScopeCount), "false", "false",
	}
	for _, family := range families {
		parts = append(parts, family.family, family.state, family.reasonCode, fmt.Sprint(family.checkedRevision), fmt.Sprint(family.affectedScopeCount), "false", "false")
	}
	if localUpgradeIntegrityDigest(parts...) != observationDigest {
		return false
	}
	return tx.Commit() == nil
}

func localUpgradeIntegrityDigest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte{0})
		hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
