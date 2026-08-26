package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func configurationIncompleteRecovery(ctx context.Context, query queryRower) (application.ConfigurationRecoveryIntent, bool, error) {
	return configurationRecoveryIntentQuery(ctx, query, `WHERE status IN ('accepted','ambiguous') ORDER BY accepted_at DESC,operation_id DESC LIMIT 1`)
}

func (s *Store) ConfigurationRecoveryIntent(ctx context.Context, operationID string) (application.ConfigurationRecoveryIntent, bool, error) {
	if strings.TrimSpace(operationID) == "" {
		return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery operation is required")
	}
	return configurationRecoveryIntentQuery(ctx, s.db, `WHERE operation_id=?`, operationID)
}

func (s *Store) ConfigurationRecoveryIsLatest(ctx context.Context, operationID string, generationID int64) (bool, error) {
	if strings.TrimSpace(operationID) == "" || generationID <= 0 {
		return false, errors.New("configuration recovery replay authority is invalid")
	}
	var eventType, currentOperation string
	err := s.db.QueryRowContext(ctx, `SELECT event_type,operation_id FROM configuration_convergence_events WHERE generation_id=? AND event_type IN ('drift_entered','drift_cleared') ORDER BY event_id DESC LIMIT 1`, generationID).Scan(&eventType, &currentOperation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && eventType == "drift_cleared" && currentOperation == operationID, err
}

func configurationRecoveryIntentQuery(ctx context.Context, query queryRower, clause string, args ...any) (application.ConfigurationRecoveryIntent, bool, error) {
	var intent application.ConfigurationRecoveryIntent
	var state, accepted, settled, reason string
	err := query.QueryRowContext(ctx, `SELECT desired_generation_id,desired_digest,authority_version,observed_digest,operation_id,requester_login,requester_database_id,requester_node_id,requester_actor_type,status,accepted_at,settled_at,reason_code,evidence_digest FROM configuration_recovery_intents `+clause, args...).Scan(
		&intent.DesiredGenerationID, &intent.DesiredDigest, &intent.AuthorityVersion, &intent.ObservedDigest, &intent.OperationID,
		&intent.Requester.Login, &intent.Requester.DatabaseID, &intent.Requester.NodeID, &intent.Requester.ActorType,
		&state, &accepted, &settled, &reason, &intent.EvidenceDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return application.ConfigurationRecoveryIntent{}, false, nil
	}
	if err != nil {
		return application.ConfigurationRecoveryIntent{}, false, err
	}
	intent.State = application.ConfigurationRecoveryState(state)
	intent.AcceptedAt, intent.SettledAt = parseTime(accepted), parseTime(settled)
	intent.Reason = application.ConfigurationReason(reason)
	if intent.DesiredGenerationID <= 0 || intent.AuthorityVersion <= 0 || !validConfigurationDigest(intent.DesiredDigest) || !validConfigurationDigest(intent.ObservedDigest) || intent.DesiredDigest == intent.ObservedDigest || intent.Requester.Validate() != nil || intent.AcceptedAt.IsZero() || intent.State != application.ConfigurationRecoveryAccepted && intent.State != application.ConfigurationRecoveryCommitted && intent.State != application.ConfigurationRecoveryAmbiguous {
		return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
	}
	switch intent.State {
	case application.ConfigurationRecoveryAccepted:
		if !intent.SettledAt.IsZero() || intent.Reason != "" || intent.EvidenceDigest != "" {
			return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
		}
	case application.ConfigurationRecoveryCommitted:
		if intent.SettledAt.IsZero() || intent.Reason != application.ConfigurationReasonReady || !validConfigurationDigest(intent.EvidenceDigest) {
			return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
		}
	case application.ConfigurationRecoveryAmbiguous:
		if intent.SettledAt.IsZero() || intent.Reason != application.ConfigurationReasonRecoveryAmbiguous || !validConfigurationDigest(intent.EvidenceDigest) {
			return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
		}
	}
	return intent, true, nil
}

func (s *Store) BeginConfigurationRecovery(ctx context.Context, input application.ConfigurationRecoveryAcceptance) (application.ConfigurationRecoveryIntent, application.OperationReceipt, bool, error) {
	if input.DesiredGenerationID <= 0 || input.AuthorityVersion <= 0 || !validConfigurationDigest(input.DesiredDigest) || !validConfigurationDigest(input.ObservedDigest) || input.DesiredDigest == input.ObservedDigest || input.Requester.Validate() != nil || input.AcceptedAt.IsZero() || application.ValidateOperationReceipt(input.Receipt) != nil || input.Receipt.OperationType != application.OperationRestoreConfiguration || input.Receipt.Scope != application.ScopeController || input.Receipt.TargetID != application.ConfigurationTargetID || !input.Receipt.Requester.Equal(input.Requester) || !input.Receipt.AcceptedAt.Equal(input.AcceptedAt) {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("configuration recovery acceptance is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	if existing, found, err := getOperationReceiptByIDTx(ctx, tx, input.Receipt.OperationID); err != nil {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	} else if found {
		if !sameAcceptedOperationReceipt(existing, input.Receipt) {
			return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
		}
		intent, intentFound, intentErr := configurationRecoveryIntentQuery(ctx, tx, `WHERE operation_id=?`, input.Receipt.OperationID)
		if intentErr != nil || !intentFound || !sameRecoveryAcceptance(intent, input) {
			return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
		}
		return intent, existing, false, tx.Commit()
	}
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || authority.Incomplete != nil || authority.IncompleteRecovery != nil || authority.Desired.GenerationID != input.DesiredGenerationID || authority.Desired.Digest != input.DesiredDigest || authority.Version != input.AuthorityVersion || !authority.Desired.RawRetained {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	var eventType, observedDigest string
	if err := tx.QueryRowContext(ctx, `SELECT event_type,digest FROM configuration_convergence_events WHERE generation_id=? AND event_type IN ('drift_entered','drift_cleared') ORDER BY event_id DESC LIMIT 1`, input.DesiredGenerationID).Scan(&eventType, &observedDigest); err != nil || eventType != "drift_entered" || observedDigest != input.ObservedDigest {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if err := insertOperationReceiptTx(ctx, tx, input.Receipt, ""); err != nil {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_recovery_intents(operation_id,desired_generation_id,desired_digest,authority_version,observed_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,status,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,'accepted',?)`, input.Receipt.OperationID, input.DesiredGenerationID, input.DesiredDigest, input.AuthorityVersion, input.ObservedDigest, input.Requester.Login, input.Requester.DatabaseID, input.Requester.NodeID, input.Requester.ActorType, formatTime(input.AcceptedAt)); err != nil {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationRecoveryInProgress
	}
	intent, intentFound, err := configurationRecoveryIntentQuery(ctx, tx, `WHERE operation_id=?`, input.Receipt.OperationID)
	if err != nil || !intentFound {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	return intent, input.Receipt, true, nil
}

func (s *Store) SettleConfigurationRecovery(ctx context.Context, settlement application.ConfigurationRecoverySettlement) (application.ConfigurationAuthority, application.ConfigurationRecoveryIntent, application.OperationReceipt, bool, error) {
	if strings.TrimSpace(settlement.OperationID) == "" || settlement.SettledAt.IsZero() || !validConfigurationDigest(settlement.EvidenceDigest) || settlement.Outcome != application.ConfigurationRecoveryCommitted && settlement.Outcome != application.ConfigurationRecoveryAmbiguous || settlement.Outcome == application.ConfigurationRecoveryCommitted && settlement.Reason != application.ConfigurationReasonReady || settlement.Outcome == application.ConfigurationRecoveryAmbiguous && settlement.Reason != application.ConfigurationReasonRecoveryAmbiguous {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("configuration recovery settlement is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	intent, found, err := configurationRecoveryIntentQuery(ctx, tx, `WHERE operation_id=?`, settlement.OperationID)
	if err != nil || !found {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	receipt, receiptFound, err := getOperationReceiptByIDTx(ctx, tx, settlement.OperationID)
	if err != nil || !receiptFound || receipt.OperationType != application.OperationRestoreConfiguration {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if intent.State != application.ConfigurationRecoveryAccepted {
		if intent.State == settlement.Outcome {
			authority, _, authorityErr := configurationAuthorityQuery(ctx, tx)
			return authority, intent, receipt, false, authorityErr
		}
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	authority, authorityFound, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !authorityFound || authority.Desired.GenerationID != intent.DesiredGenerationID || authority.Desired.Digest != intent.DesiredDigest || authority.Version != intent.AuthorityVersion || authority.Incomplete != nil || authority.IncompleteRecovery == nil || authority.IncompleteRecovery.OperationID != intent.OperationID {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if err := configurationOne(tx.ExecContext(ctx, `UPDATE configuration_recovery_intents SET status=?,settled_at=?,reason_code=?,evidence_digest=? WHERE operation_id=? AND status='accepted'`, string(settlement.Outcome), formatTime(settlement.SettledAt), string(settlement.Reason), settlement.EvidenceDigest, settlement.OperationID)); err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	outcome := application.OperationOutcomeAmbiguous
	if settlement.Outcome == application.ConfigurationRecoveryCommitted {
		outcome = application.OperationOutcomeSucceeded
		var lastType, lastDigest string
		if err := tx.QueryRowContext(ctx, `SELECT event_type,digest FROM configuration_convergence_events WHERE generation_id=? AND event_type IN ('drift_entered','drift_cleared') ORDER BY event_id DESC LIMIT 1`, intent.DesiredGenerationID).Scan(&lastType, &lastDigest); err != nil || lastType != "drift_entered" || lastDigest != intent.ObservedDigest {
			return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_convergence_events(event_type,generation_id,operation_id,digest,reason_code,evidence_digest,observed_at) VALUES('drift_cleared',?,?,?,?,?,?)`, intent.DesiredGenerationID, intent.OperationID, intent.DesiredDigest, string(application.ConfigurationReasonReady), settlement.EvidenceDigest, formatTime(settlement.SettledAt)); err != nil {
			return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
		}
		if err := appendConfigurationActivityTx(ctx, tx, "drift_cleared", intent.DesiredGenerationID, intent.OperationID, intent.DesiredDigest, settlement.EvidenceDigest, settlement.SettledAt); err != nil {
			return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
		}
		if err := configurationOne(tx.ExecContext(ctx, `UPDATE configuration_authority SET authority_version=authority_version+1,updated_at=? WHERE authority_id=1 AND desired_generation_id=? AND authority_version=?`, formatTime(settlement.SettledAt), intent.DesiredGenerationID, intent.AuthorityVersion)); err != nil {
			return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
		}
	}
	resultDigest := configurationEvidence("configuration-recovery-result", intent.DesiredGenerationID, intent.DesiredDigest, intent.ObservedDigest, string(settlement.Outcome), settlement.EvidenceDigest)
	if err := configurationOne(tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='applied',outcome='pending',resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,applied_at=? WHERE operation_id=? AND phase='accepted'`, intent.DesiredDigest, string(authority.Desired.State), intent.DesiredGenerationID, settlement.EvidenceDigest, formatTime(settlement.SettledAt), settlement.OperationID)); err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if err := configurationOne(tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='observed',outcome=?,result_digest=?,settled_at=? WHERE operation_id=? AND phase='applied'`, string(outcome), resultDigest, formatTime(settlement.SettledAt), settlement.OperationID)); err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	if err := appendSettledOperationActivityTx(ctx, tx, settlement.OperationID, application.ActivityIngestionCurrent); err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	authority, _, err = configurationAuthorityQuery(ctx, tx)
	if err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	intent, _, err = configurationRecoveryIntentQuery(ctx, tx, `WHERE operation_id=?`, settlement.OperationID)
	if err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	receipt, _, err = getOperationReceiptByIDTx(ctx, tx, settlement.OperationID)
	if err != nil || application.ValidateOperationReceipt(receipt) != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, errors.New("configuration recovery receipt is corrupt")
	}
	if err := tx.Commit(); err != nil {
		return application.ConfigurationAuthority{}, application.ConfigurationRecoveryIntent{}, application.OperationReceipt{}, false, err
	}
	return authority, intent, receipt, true, nil
}

func sameRecoveryAcceptance(intent application.ConfigurationRecoveryIntent, input application.ConfigurationRecoveryAcceptance) bool {
	return intent.OperationID == input.Receipt.OperationID && intent.DesiredGenerationID == input.DesiredGenerationID && intent.DesiredDigest == input.DesiredDigest && intent.AuthorityVersion == input.AuthorityVersion && intent.ObservedDigest == input.ObservedDigest && intent.Requester.Equal(input.Requester)
}

var _ application.ConfigurationRecoveryStore = (*Store)(nil)
