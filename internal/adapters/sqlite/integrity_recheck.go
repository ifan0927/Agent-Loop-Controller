package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type integrityRecheckBinding struct {
	requestKey              string
	requestClaimDigest      string
	requestDigest           string
	requestSchemaVersion    string
	operationID             string
	registryVersion         string
	preAcceptanceGeneration int64
	acceptedGeneration      int64
	scanID                  string
	targetGeneration        int64
	bindingDigest           string
	status                  string
	observationID           string
	observationDigest       string
	readiness               application.IntegrityState
	reasonCode              string
	createdAt               time.Time
	updatedAt               time.Time
	settledAt               time.Time
	receipt                 application.OperationReceipt
}

const integrityRecheckSelect = `SELECT request_key,request_claim_digest,request_digest,request_schema_version,operation_id,registry_version,pre_acceptance_generation,accepted_generation,scan_id,target_generation,binding_digest,status,observation_id,observation_digest,readiness,reason_code,created_at,updated_at,settled_at FROM controller_integrity_rechecks`

func (s *Store) AcceptIntegrityRecheck(ctx context.Context, input application.IntegrityRecheckAcceptance) (application.IntegrityRecheckResult, error) {
	requestID := input.RequestID
	if input.Requester.Validate() != nil || strings.TrimSpace(requestID) == "" || len(requestID) > application.IntegrityRecheckMaximumRequest || strings.ContainsRune(requestID, '\x00') || input.AcceptedAt.IsZero() {
		return application.IntegrityRecheckResult{}, errors.New("integrity recheck acceptance is invalid")
	}
	requestKey := application.IntegrityRecheckRequestKey(input.Requester, requestID)
	requestClaim := application.IntegrityRecheckRequestClaim(requestID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.IntegrityRecheckResult{}, err
	}
	defer tx.Rollback()
	if _, found, err := getIntegrityRecheckByKeyTx(ctx, tx, requestKey); err != nil {
		return application.IntegrityRecheckResult{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return application.IntegrityRecheckResult{}, err
		}
		return s.GetIntegrityRecheck(ctx, requestKey, requestClaim)
	}
	var claimedKey string
	if err := tx.QueryRowContext(ctx, `SELECT request_key FROM controller_integrity_rechecks WHERE request_claim_digest=?`, requestClaim).Scan(&claimedKey); err == nil {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return application.IntegrityRecheckResult{}, err
	}
	var activePointers, activeBindings, guards int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM controller_integrity_active_recheck),(SELECT COUNT(*) FROM controller_integrity_rechecks WHERE status='active'),(SELECT COUNT(*) FROM controller_integrity_finalization_guard)`).Scan(&activePointers, &activeBindings, &guards); err != nil {
		return application.IntegrityRecheckResult{}, err
	}
	if guards != 0 {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	if activePointers != 0 || activeBindings != 0 {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckActive
	}
	if err := validateIntegrityRegistryTx(ctx, tx); err != nil {
		return application.IntegrityRecheckResult{}, err
	}
	var preGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&preGeneration); err != nil {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	receipt := application.NewIntegrityRecheckReceipt(input.Requester, requestID, preGeneration, input.AcceptedAt)
	if err := insertOperationReceiptTx(ctx, tx, receipt, ""); err != nil {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='applied',outcome='pending',applied_at=? WHERE operation_id=? AND phase='accepted' AND outcome='pending'`, formatTime(input.AcceptedAt), receipt.OperationID)
	if err != nil {
		return application.IntegrityRecheckResult{}, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, receipt.OperationID)
	if err != nil || !found || receipt.Phase != application.OperationPhaseApplied || receipt.Outcome != application.OperationOutcomePending {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	var acceptedGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&acceptedGeneration); err != nil || acceptedGeneration <= preGeneration {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE controller_integrity_scans SET status='superseded',reason_code='explicit_recheck_accepted',lease_owner='',lease_expires_at='',updated_at=?,completed_at=? WHERE status='active'`, formatTime(input.AcceptedAt), formatTime(input.AcceptedAt)); err != nil {
		return application.IntegrityRecheckResult{}, err
	}
	scanID := integrityDigest("scan", application.IntegrityRegistryVersion, fmt.Sprint(acceptedGeneration))
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_scans(scan_id,registry_version,target_generation,stable_boundary,status,convergence_attempt,created_at,updated_at) VALUES(?,?,?,?, 'active',0,?,?)`, scanID, application.IntegrityRegistryVersion, acceptedGeneration, integrityDigest("boundary", fmt.Sprint(acceptedGeneration)), formatTime(input.AcceptedAt), formatTime(input.AcceptedAt)); err != nil {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_rechecks(request_key,request_claim_digest,request_digest,request_schema_version,operation_id,registry_version,pre_acceptance_generation,accepted_generation,scan_id,target_generation,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'active',?,?)`, requestKey, requestClaim, receipt.RequestDigest, application.IntegrityRecheckSchemaVersion, receipt.OperationID, application.IntegrityRegistryVersion, preGeneration, acceptedGeneration, scanID, acceptedGeneration, formatTime(input.AcceptedAt), formatTime(input.AcceptedAt)); err != nil {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_active_recheck(singleton,request_key,operation_id,scan_id,target_generation,created_at) VALUES(1,?,?,?,?,?)`, requestKey, receipt.OperationID, scanID, acceptedGeneration, formatTime(input.AcceptedAt)); err != nil {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	if err := tx.Commit(); err != nil {
		return application.IntegrityRecheckResult{}, err
	}
	return s.GetIntegrityRecheck(ctx, requestKey, requestClaim)
}

func (s *Store) GetIntegrityRecheck(ctx context.Context, requestKey, requestClaim string) (application.IntegrityRecheckResult, error) {
	if len(requestKey) != 64 || len(requestClaim) != 64 {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	binding, found, err := getIntegrityRecheckByKeyQuery(ctx, s.db, requestKey)
	if err != nil || !found {
		if err == nil {
			err = application.ErrIntegrityRecheckConflict
		}
		return application.IntegrityRecheckResult{}, err
	}
	if binding.requestClaimDigest != requestClaim {
		return application.IntegrityRecheckResult{}, application.ErrIntegrityRecheckConflict
	}
	return s.projectIntegrityRecheck(ctx, binding)
}

func (s *Store) projectIntegrityRecheck(ctx context.Context, binding integrityRecheckBinding) (application.IntegrityRecheckResult, error) {
	result := application.IntegrityRecheckResult{Receipt: binding.receipt, RegistryVersion: binding.registryVersion, ScanID: binding.scanID, TargetGeneration: binding.targetGeneration, State: application.IntegrityRecheckConflict, ReasonCode: "integrity_recheck_evidence_conflict"}
	if binding.requestSchemaVersion != application.IntegrityRecheckSchemaVersion || binding.registryVersion != application.IntegrityRegistryVersion || binding.acceptedGeneration <= binding.preAcceptanceGeneration || binding.targetGeneration != binding.acceptedGeneration || binding.scanID != integrityDigest("scan", binding.registryVersion, fmt.Sprint(binding.targetGeneration)) || binding.receipt.OperationID != binding.operationID || binding.receipt.OperationType != application.OperationRecheckIntegrity || binding.receipt.Scope != application.ScopeController || binding.receipt.TargetID != application.IntegrityTargetID || binding.receipt.RequestDigest != binding.requestDigest || binding.receipt.ExpectedAuthorityDigest != application.IntegrityRecheckAuthorityDigest(binding.preAcceptanceGeneration) || binding.receipt.OperationAnchorDigest != application.IntegrityRecheckOperationAnchorDigest(binding.requestKey) || binding.receipt.TargetBindingDigest != application.IntegrityRecheckTargetBindingDigest(binding.receipt.Requester) {
		return result, nil
	}
	var scanStatus, scanRegistry string
	var scanTarget int64
	if err := s.db.QueryRowContext(ctx, `SELECT status,registry_version,target_generation FROM controller_integrity_scans WHERE scan_id=?`, binding.scanID).Scan(&scanStatus, &scanRegistry, &scanTarget); err != nil || scanRegistry != binding.registryVersion || scanTarget != binding.targetGeneration {
		return result, nil
	}
	var guardCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_finalization_guard`).Scan(&guardCount); err != nil || guardCount != 0 {
		return result, nil
	}
	var pointerCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_active_recheck WHERE singleton=1 AND request_key=? AND operation_id=? AND scan_id=? AND target_generation=?`, binding.requestKey, binding.operationID, binding.scanID, binding.targetGeneration).Scan(&pointerCount); err != nil {
		return application.IntegrityRecheckResult{}, err
	}
	switch binding.status {
	case "active":
		if binding.receipt.Phase == application.OperationPhaseApplied && binding.receipt.Outcome == application.OperationOutcomePending && scanStatus == "active" && pointerCount == 1 && binding.bindingDigest == "" && binding.observationID == "" && binding.observationDigest == "" && binding.readiness == "" && binding.settledAt.IsZero() {
			result.State, result.ReasonCode = application.IntegrityRecheckPending, "scan_pending"
		}
		return result, nil
	case "observed":
		if pointerCount != 0 || scanStatus != "published" || binding.receipt.Phase != application.OperationPhaseObserved || binding.receipt.Outcome != application.OperationOutcomeSucceeded || binding.settledAt.IsZero() || binding.observationID == "" || binding.observationDigest == "" || !binding.readiness.Valid() {
			return result, nil
		}
		observation, err := s.loadIntegrityObservation(ctx, binding.observationID)
		if err != nil || observation.Digest != binding.observationDigest || observation.TargetGeneration != binding.targetGeneration || observation.EffectiveReadiness != binding.readiness {
			return result, nil
		}
		var observationScan string
		if err := s.db.QueryRowContext(ctx, `SELECT scan_id FROM controller_integrity_observations WHERE observation_id=?`, binding.observationID).Scan(&observationScan); err != nil || observationScan != binding.scanID {
			return result, nil
		}
		expectedBinding := application.IntegrityRecheckBindingDigest(binding.operationID, binding.scanID, binding.targetGeneration, binding.observationID, binding.observationDigest, binding.readiness)
		if binding.bindingDigest != expectedBinding || binding.receipt.ResultingAuthorityDigest != binding.observationDigest || binding.receipt.ResultingState != string(binding.readiness) || binding.receipt.ResultingVersion != binding.targetGeneration || binding.receipt.EvidenceDigest != binding.observationDigest || binding.receipt.ResultDigest != expectedBinding {
			return result, nil
		}
		result.State, result.ReasonCode, result.Observation = application.IntegrityRecheckSucceeded, "observation_published", &observation
		return result, nil
	case "conflict", "ambiguous":
		expectedOutcome := application.OperationOutcomeConflict
		expectedState := application.IntegrityRecheckConflict
		if binding.status == "ambiguous" {
			expectedOutcome, expectedState = application.OperationOutcomeAmbiguous, application.IntegrityRecheckAmbiguous
		}
		validTerminal := pointerCount == 0 && binding.receipt.Phase == application.OperationPhaseObserved && binding.receipt.Outcome == expectedOutcome && binding.observationID == "" && binding.observationDigest == "" && !binding.settledAt.IsZero()
		if binding.status == "conflict" {
			expectedDigest := application.IntegrityRecheckConflictDigest(binding.operationID, binding.scanID, binding.targetGeneration, binding.receipt.ResultingVersion, binding.reasonCode)
			validTerminal = validTerminal && scanStatus == "superseded" && binding.bindingDigest == expectedDigest && binding.receipt.ResultingAuthorityDigest == application.IntegrityRecheckAuthorityDigest(binding.receipt.ResultingVersion) && binding.receipt.ResultingState == string(application.IntegrityConflict) && binding.receipt.EvidenceDigest == expectedDigest && binding.receipt.ResultDigest == expectedDigest
		}
		if validTerminal {
			result.State, result.ReasonCode = expectedState, binding.reasonCode
		}
		return result, nil
	default:
		return result, nil
	}
}

func getIntegrityRecheckByKeyTx(ctx context.Context, tx *sql.Tx, requestKey string) (integrityRecheckBinding, bool, error) {
	return getIntegrityRecheckByKeyQuery(ctx, tx, requestKey)
}

func getIntegrityRecheckByKeyQuery(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, requestKey string) (integrityRecheckBinding, bool, error) {
	var binding integrityRecheckBinding
	var readiness, created, updated, settled string
	err := query.QueryRowContext(ctx, integrityRecheckSelect+` WHERE request_key=?`, requestKey).Scan(&binding.requestKey, &binding.requestClaimDigest, &binding.requestDigest, &binding.requestSchemaVersion, &binding.operationID, &binding.registryVersion, &binding.preAcceptanceGeneration, &binding.acceptedGeneration, &binding.scanID, &binding.targetGeneration, &binding.bindingDigest, &binding.status, &binding.observationID, &binding.observationDigest, &readiness, &binding.reasonCode, &created, &updated, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return integrityRecheckBinding{}, false, nil
	}
	if err != nil {
		return integrityRecheckBinding{}, false, err
	}
	binding.readiness = application.IntegrityState(readiness)
	binding.createdAt, binding.updatedAt, binding.settledAt = parseTime(created), parseTime(updated), parseTime(settled)
	receipt, found, err := getOperationReceiptByIDQuery(ctx, query, binding.operationID)
	if err != nil || !found {
		if err == nil {
			err = application.ErrIntegrityRecheckConflict
		}
		return integrityRecheckBinding{}, false, err
	}
	binding.receipt = receipt
	return binding, true, nil
}

func getIntegrityRecheckByScanTx(ctx context.Context, tx *sql.Tx, scanID string) (integrityRecheckBinding, bool, error) {
	var requestKey string
	if err := tx.QueryRowContext(ctx, `SELECT request_key FROM controller_integrity_rechecks WHERE scan_id=?`, scanID).Scan(&requestKey); errors.Is(err, sql.ErrNoRows) {
		return integrityRecheckBinding{}, false, nil
	} else if err != nil {
		return integrityRecheckBinding{}, false, err
	}
	return getIntegrityRecheckByKeyTx(ctx, tx, requestKey)
}

func advanceIntegrityRecheckReceiptTx(ctx context.Context, tx *sql.Tx, receipt application.OperationReceipt, outcome application.OperationOutcome, resultingAuthority, resultingState string, resultingVersion int64, evidenceDigest, resultDigest string, at time.Time) (application.OperationReceipt, error) {
	if receipt.OperationType != application.OperationRecheckIntegrity || receipt.Phase != application.OperationPhaseApplied || receipt.Outcome != application.OperationOutcomePending || at.Before(receipt.AppliedAt) {
		return application.OperationReceipt{}, application.ErrIntegrityRecheckConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='observed',outcome=?,resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,result_digest=?,settled_at=? WHERE operation_id=? AND operation_type='recheck_integrity' AND phase='applied' AND outcome='pending'`, string(outcome), resultingAuthority, resultingState, resultingVersion, evidenceDigest, resultDigest, formatTime(at), receipt.OperationID)
	if err != nil {
		return application.OperationReceipt{}, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.OperationReceipt{}, application.ErrIntegrityRecheckConflict
	}
	updated, found, err := getOperationReceiptByIDTx(ctx, tx, receipt.OperationID)
	if err != nil || !found {
		return application.OperationReceipt{}, application.ErrIntegrityRecheckConflict
	}
	return updated, nil
}

func settleSupersededIntegrityRecheckTx(ctx context.Context, tx *sql.Tx, binding integrityRecheckBinding, observedGeneration int64, observedAt time.Time, reason string) error {
	digest := application.IntegrityRecheckConflictDigest(binding.operationID, binding.scanID, binding.targetGeneration, observedGeneration, reason)
	receipt, err := advanceIntegrityRecheckReceiptTx(ctx, tx, binding.receipt, application.OperationOutcomeConflict, application.IntegrityRecheckAuthorityDigest(observedGeneration), string(application.IntegrityConflict), observedGeneration, digest, digest, observedAt)
	if err != nil {
		return err
	}
	if err := appendSettledOperationActivityTx(ctx, tx, receipt.OperationID, application.ActivityIngestionCurrent); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE controller_integrity_rechecks SET binding_digest=?,status='conflict',reason_code=?,updated_at=?,settled_at=? WHERE request_key=? AND status='active' AND operation_id=? AND scan_id=? AND target_generation=?`, digest, reason, formatTime(observedAt), formatTime(observedAt), binding.requestKey, binding.operationID, binding.scanID, binding.targetGeneration)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ErrIntegrityRecheckConflict
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM controller_integrity_active_recheck WHERE singleton=1 AND request_key=? AND operation_id=? AND scan_id=? AND target_generation=?`, binding.requestKey, binding.operationID, binding.scanID, binding.targetGeneration)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ErrIntegrityRecheckConflict
	}
	return nil
}

func finalizeIntegrityRecheckObservationTx(ctx context.Context, tx *sql.Tx, binding integrityRecheckBinding, observedAt time.Time) error {
	if binding.status != "active" || binding.receipt.Phase != application.OperationPhaseApplied || binding.receipt.Outcome != application.OperationOutcomePending || binding.targetGeneration != binding.acceptedGeneration {
		return application.ErrIntegrityRecheckConflict
	}
	// Refresh operation_activity once before settlement so convergence-bound
	// placeholder evidence and ordinary family evidence both predict the exact
	// post-bundle result. The required second check after the guarded writes must
	// produce the same immutable observation digest.
	if _, err := tx.ExecContext(ctx, `DELETE FROM controller_integrity_scan_findings WHERE scan_id=? AND family=?`, binding.scanID, application.IntegrityOperationActivity); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM controller_integrity_checked_families WHERE scan_id=? AND family=?`, binding.scanID, application.IntegrityOperationActivity); err != nil {
		return err
	}
	predictedOperation, predictedFindings, err := checkIntegrityFamilyTx(ctx, tx, application.IntegrityOperationActivity)
	if err != nil {
		return err
	}
	if err := persistIntegrityFamilyTx(ctx, tx, binding.scanID, predictedOperation, predictedFindings, observedAt); err != nil {
		return err
	}
	observation, _, err := buildIntegrityObservationTx(ctx, tx, binding.scanID, binding.targetGeneration, observedAt)
	if err != nil {
		return err
	}
	activityIdentity := application.NewActivityEvent(application.ActivityEventInput{SourceKind: "operation_receipt", SourceIdentity: binding.operationID, EventKind: application.ActivityOperationSettled}).EventID
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_integrity_finalization_guard(singleton,operation_id,scan_id,target_generation,activity_event_id,activity_source_identity,activity_link_operation_id,entered_at) VALUES(1,?,?,?,?,?,?,?)`, binding.operationID, binding.scanID, binding.targetGeneration, activityIdentity, binding.operationID, binding.operationID, formatTime(observedAt)); err != nil {
		return application.ErrIntegrityRecheckConflict
	}
	bindingDigest := application.IntegrityRecheckBindingDigest(binding.operationID, binding.scanID, binding.targetGeneration, observation.ObservationID, observation.Digest, observation.EffectiveReadiness)
	receipt, err := advanceIntegrityRecheckReceiptTx(ctx, tx, binding.receipt, application.OperationOutcomeSucceeded, observation.Digest, string(observation.EffectiveReadiness), binding.targetGeneration, observation.Digest, bindingDigest, observedAt)
	if err != nil {
		return err
	}
	if err := appendSettledOperationActivityTx(ctx, tx, receipt.OperationID, application.ActivityIngestionCurrent); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM controller_integrity_scan_findings WHERE scan_id=? AND family=?`, binding.scanID, application.IntegrityOperationActivity); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM controller_integrity_checked_families WHERE scan_id=? AND family=?`, binding.scanID, application.IntegrityOperationActivity); err != nil {
		return err
	}
	operationResult, findings, err := checkIntegrityFamilyTx(ctx, tx, application.IntegrityOperationActivity)
	if err != nil {
		return err
	}
	if err := persistIntegrityFamilyTx(ctx, tx, binding.scanID, operationResult, findings, observedAt); err != nil {
		return err
	}
	rechecked, _, err := buildIntegrityObservationTx(ctx, tx, binding.scanID, binding.targetGeneration, observedAt)
	if err != nil {
		return err
	}
	if rechecked.ObservationID != observation.ObservationID || rechecked.Digest != observation.Digest || rechecked.EffectiveReadiness != observation.EffectiveReadiness {
		return errors.New("integrity finalization operation activity changed observation")
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generation); err != nil || generation != binding.targetGeneration {
		return errors.New("integrity finalization source generation changed")
	}
	var guardCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_finalization_guard WHERE singleton=1 AND operation_id=? AND scan_id=? AND target_generation=? AND activity_event_id=? AND activity_source_identity=? AND activity_link_operation_id=?`, binding.operationID, binding.scanID, binding.targetGeneration, activityIdentity, binding.operationID, binding.operationID).Scan(&guardCount); err != nil || guardCount != 1 {
		return errors.New("integrity finalization guard conflicts")
	}
	activity, err := scanActivityEvent(tx.QueryRowContext(ctx, activityEventSelect+` WHERE event_id=?`, activityIdentity))
	if err != nil || activity.SourceKind != "operation_receipt" || activity.SourceIdentity != binding.operationID || activity.Scope != application.ScopeController || activity.TargetID != application.IntegrityTargetID || activity.EventKind != application.ActivityOperationSettled || activity.ReasonCode != application.ActivityReasonSucceeded || len(activity.OperationIDs) != 1 || activity.OperationIDs[0] != binding.operationID {
		return errors.New("integrity finalization activity conflicts")
	}
	var links int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity_operation_links WHERE operation_id=? AND event_id=?`, binding.operationID, activityIdentity).Scan(&links); err != nil || links != 1 {
		return errors.New("integrity finalization activity link conflicts")
	}
	if receipt.Phase != application.OperationPhaseObserved || receipt.Outcome != application.OperationOutcomeSucceeded || receipt.ResultingAuthorityDigest != rechecked.Digest || receipt.ResultingState != string(rechecked.EffectiveReadiness) || receipt.ResultingVersion != binding.targetGeneration || receipt.EvidenceDigest != rechecked.Digest || receipt.ResultDigest != bindingDigest {
		return errors.New("integrity finalization receipt conflicts")
	}
	if err := persistIntegrityObservationTx(ctx, tx, binding.scanID, rechecked); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE controller_integrity_rechecks SET binding_digest=?,status='observed',observation_id=?,observation_digest=?,readiness=?,reason_code='observation_published',updated_at=?,settled_at=? WHERE request_key=? AND status='active' AND operation_id=? AND scan_id=? AND target_generation=?`, bindingDigest, rechecked.ObservationID, rechecked.Digest, rechecked.EffectiveReadiness, formatTime(observedAt), formatTime(observedAt), binding.requestKey, binding.operationID, binding.scanID, binding.targetGeneration)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ErrIntegrityRecheckConflict
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM controller_integrity_active_recheck WHERE singleton=1 AND request_key=? AND operation_id=? AND scan_id=? AND target_generation=?`, binding.requestKey, binding.operationID, binding.scanID, binding.targetGeneration)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ErrIntegrityRecheckConflict
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM controller_integrity_finalization_guard WHERE singleton=1 AND operation_id=? AND scan_id=? AND target_generation=? AND activity_event_id=?`, binding.operationID, binding.scanID, binding.targetGeneration, activityIdentity)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ErrIntegrityRecheckConflict
	}
	var pointerObservation, pointerDigest string
	var pointerGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT observation_id,observation_digest,published_generation FROM controller_integrity_current WHERE singleton=1`).Scan(&pointerObservation, &pointerDigest, &pointerGeneration); err != nil || pointerObservation != rechecked.ObservationID || pointerDigest != rechecked.Digest || pointerGeneration != binding.targetGeneration {
		return errors.New("integrity finalization current pointer conflicts")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_integrity_finalization_guard`).Scan(&guardCount); err != nil || guardCount != 0 {
		return errors.New("integrity finalization guard was not cleared")
	}
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generation); err != nil || generation != binding.targetGeneration {
		return errors.New("integrity finalization included unrelated mutation")
	}
	return nil
}
