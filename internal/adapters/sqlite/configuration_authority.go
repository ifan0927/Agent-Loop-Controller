package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const configurationGenerationSelect = `SELECT generation_id,COALESCE(parent_generation_id,0),digest,target_size,schema_version,origin,requester_login,requester_database_id,requester_node_id,requester_actor_type,configured_operator_login,configured_operator_database_id,configured_operator_node_id,configured_operator_actor_type,COALESCE(operation_id,''),lifecycle,raw_retained,created_at,committed_at,effective_at,superseded_at,settled_at,reason_code FROM configuration_generations`

func (s *Store) ConfigurationAuthority(ctx context.Context) (application.ConfigurationAuthority, bool, error) {
	return configurationAuthorityQuery(ctx, s.db)
}

func configurationAuthorityQuery(ctx context.Context, query queryRower) (application.ConfigurationAuthority, bool, error) {
	var authority application.ConfigurationAuthority
	var effective sql.NullInt64
	var updated string
	err := query.QueryRowContext(ctx, `SELECT canonical_config_path,database_path,desired_generation_id,effective_generation_id,authority_version,updated_at FROM configuration_authority WHERE authority_id=1`).
		Scan(&authority.CanonicalConfigPath, &authority.DatabasePath, &authority.Desired.GenerationID, &effective, &authority.Version, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return application.ConfigurationAuthority{}, false, nil
	}
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	authority.EffectiveID = effective.Int64
	authority.UpdatedAt = parseTime(updated)
	desired, err := scanConfigurationGeneration(query.QueryRowContext(ctx, configurationGenerationSelect+` WHERE generation_id=?`, authority.Desired.GenerationID))
	if err != nil {
		return application.ConfigurationAuthority{}, false, errors.New("configuration authority is corrupt")
	}
	authority.Desired = desired
	intent, found, err := configurationIncompleteIntent(ctx, query)
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	if found {
		authority.Incomplete = &intent
	}
	return authority, true, nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func configurationIncompleteIntent(ctx context.Context, query queryRower) (application.ConfigurationApplyIntent, bool, error) {
	var intent application.ConfigurationApplyIntent
	var state, accepted, settled, reason string
	err := query.QueryRowContext(ctx, `SELECT generation_id,parent_generation_id,parent_digest,target_digest,operation_id,status,accepted_at,settled_at,reason_code FROM configuration_apply_intents WHERE status IN ('accepted','ambiguous') ORDER BY generation_id DESC LIMIT 1`).
		Scan(&intent.GenerationID, &intent.ParentID, &intent.ParentDigest, &intent.TargetDigest, &intent.OperationID, &state, &accepted, &settled, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return application.ConfigurationApplyIntent{}, false, nil
	}
	if err != nil {
		return application.ConfigurationApplyIntent{}, false, err
	}
	intent.State = application.ConfigurationApplyState(state)
	intent.AcceptedAt, intent.SettledAt = parseTime(accepted), parseTime(settled)
	intent.Reason = application.ConfigurationReason(reason)
	return intent, true, nil
}

func (s *Store) PrepareConfigurationBaseline(ctx context.Context, input application.ConfigurationBaselineInput) error {
	if err := validateConfigurationBaselineInput(input); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if authority, found, authorityErr := configurationAuthorityQuery(ctx, tx); authorityErr != nil {
		return authorityErr
	} else if found {
		if authority.CanonicalConfigPath != input.CanonicalConfigPath || authority.DatabasePath != input.Candidate.DatabasePath || authority.Desired.Digest != input.Candidate.Digest {
			return application.ErrConfigurationAuthorityConflict
		}
		return tx.Commit()
	}
	var configPath, databasePath, digest string
	var size int64
	var schema int
	err = tx.QueryRowContext(ctx, `SELECT canonical_config_path,database_path,digest,target_size,schema_version FROM configuration_baseline_anchor WHERE authority_id=1`).Scan(&configPath, &databasePath, &digest, &size, &schema)
	if err == nil {
		if configPath != input.CanonicalConfigPath || databasePath != input.Candidate.DatabasePath || digest != input.Candidate.Digest || size != input.Candidate.Size || schema != input.Candidate.SchemaVersion {
			return application.ErrConfigurationAuthorityConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_baseline_anchor(authority_id,canonical_config_path,database_path,digest,target_size,schema_version,prepared_at) VALUES(1,?,?,?,?,?,?)`, input.CanonicalConfigPath, input.Candidate.DatabasePath, input.Candidate.Digest, input.Candidate.Size, input.Candidate.SchemaVersion, formatTime(input.ObservedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func validateConfigurationBaselineInput(input application.ConfigurationBaselineInput) error {
	if input.ObservedAt.IsZero() || !validConfigurationMetadata(input.Candidate.Digest, input.Candidate.Size, input.Candidate.SchemaVersion) || strings.TrimSpace(input.CanonicalConfigPath) == "" || strings.TrimSpace(input.Candidate.DatabasePath) == "" || input.Candidate.Operator.Validate() != nil {
		return errors.New("configuration baseline input is invalid")
	}
	return nil
}

func (s *Store) AdoptConfigurationBaseline(ctx context.Context, input application.ConfigurationBaselineInput) (application.ConfigurationAuthority, bool, error) {
	if err := validateConfigurationBaselineInput(input); err != nil {
		return application.ConfigurationAuthority{}, false, errors.New("configuration baseline input is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	defer tx.Rollback()
	if existing, found, err := configurationAuthorityQuery(ctx, tx); err != nil {
		return application.ConfigurationAuthority{}, false, err
	} else if found {
		if existing.CanonicalConfigPath != input.CanonicalConfigPath || existing.DatabasePath != input.Candidate.DatabasePath || existing.Desired.Digest != input.Candidate.Digest {
			return application.ConfigurationAuthority{}, false, application.ErrConfigurationAuthorityConflict
		}
		return existing, false, tx.Commit()
	}
	var anchorConfig, anchorDatabase, anchorDigest string
	var anchorSize int64
	var anchorSchema int
	if err := tx.QueryRowContext(ctx, `SELECT canonical_config_path,database_path,digest,target_size,schema_version FROM configuration_baseline_anchor WHERE authority_id=1`).Scan(&anchorConfig, &anchorDatabase, &anchorDigest, &anchorSize, &anchorSchema); err != nil || anchorConfig != input.CanonicalConfigPath || anchorDatabase != input.Candidate.DatabasePath || anchorDigest != input.Candidate.Digest || anchorSize != input.Candidate.Size || anchorSchema != input.Candidate.SchemaVersion {
		return application.ConfigurationAuthority{}, false, application.ErrConfigurationAuthorityConflict
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO configuration_generations(parent_generation_id,digest,target_size,schema_version,origin,configured_operator_login,configured_operator_database_id,configured_operator_node_id,configured_operator_actor_type,operation_id,lifecycle,raw_retained,created_at,committed_at,settled_at,reason_code) VALUES(NULL,?,?,?,?,?,?,?,?,NULL,'pending_restart',1,?,?,?,?)`, input.Candidate.Digest, input.Candidate.Size, input.Candidate.SchemaVersion, string(application.ConfigurationOriginBaseline), input.Candidate.Operator.Login, input.Candidate.Operator.DatabaseID, input.Candidate.Operator.NodeID, input.Candidate.Operator.ActorType, formatTime(input.ObservedAt), formatTime(input.ObservedAt), formatTime(input.ObservedAt), string(application.ConfigurationReasonBaselinePending))
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	generationID, err := result.LastInsertId()
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_authority(authority_id,canonical_config_path,database_path,desired_generation_id,effective_generation_id,authority_version,created_at,updated_at) VALUES(1,?,?,?,NULL,1,?,?)`, input.CanonicalConfigPath, input.Candidate.DatabasePath, generationID, formatTime(input.ObservedAt), formatTime(input.ObservedAt)); err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	evidence := configurationEvidence("baseline", generationID, input.Candidate.Digest)
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_convergence_events(event_type,generation_id,digest,reason_code,evidence_digest,observed_at) VALUES('baseline_established',?,?,?,?,?)`, generationID, input.Candidate.Digest, string(application.ConfigurationReasonBaselinePending), evidence, formatTime(input.ObservedAt)); err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found {
		return application.ConfigurationAuthority{}, false, errors.New("configuration baseline settlement is unavailable")
	}
	if err := tx.Commit(); err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	return authority, true, nil
}

func (s *Store) BeginConfigurationApply(ctx context.Context, input application.ConfigurationApplyAcceptance) (application.ConfigurationGeneration, application.OperationReceipt, bool, error) {
	if input.AcceptedAt.IsZero() || input.ExpectedGenerationID <= 0 || !validConfigurationMetadata(input.Candidate.Digest, input.Candidate.Size, input.Candidate.SchemaVersion) || input.Candidate.SchemaVersion != 5 || input.Candidate.Operator.Validate() != nil || input.Requester.Validate() != nil || !input.Receipt.Requester.Equal(input.Requester) || application.ValidateOperationReceipt(input.Receipt) != nil || input.Receipt.OperationType != application.OperationApplyConfiguration || input.Receipt.Scope != application.ScopeController || input.Receipt.TargetID != application.ConfigurationTargetID {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, errors.New("configuration apply acceptance is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	if existing, found, err := getOperationReceiptByIDTx(ctx, tx, input.Receipt.OperationID); err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	} else if found {
		if !sameAcceptedOperationReceipt(existing, input.Receipt) {
			return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, application.ErrOperationReceiptConflict
		}
		generation, generationErr := scanConfigurationGeneration(tx.QueryRowContext(ctx, configurationGenerationSelect+` WHERE operation_id=?`, input.Receipt.OperationID))
		if generationErr != nil {
			return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
		}
		return generation, existing, false, tx.Commit()
	}
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if authority.Incomplete != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, application.ErrConfigurationApplyInProgress
	}
	if authority.Desired.GenerationID != input.ExpectedGenerationID || authority.Desired.Digest != input.ExpectedDigest || authority.DatabasePath != input.Candidate.DatabasePath {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	runs, err := listNonterminalRunsQuery(ctx, tx)
	if err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	if application.ConfigurationCompatibleWithActiveRuns(authority.Desired.ConfiguredOperator, input.Candidate, runs) != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if err := insertOperationReceiptTx(ctx, tx, input.Receipt, ""); err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO configuration_generations(parent_generation_id,digest,target_size,schema_version,origin,requester_login,requester_database_id,requester_node_id,requester_actor_type,configured_operator_login,configured_operator_database_id,configured_operator_node_id,configured_operator_actor_type,operation_id,lifecycle,raw_retained,created_at,reason_code) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'accepted',1,?,'')`, authority.Desired.GenerationID, input.Candidate.Digest, input.Candidate.Size, input.Candidate.SchemaVersion, string(application.ConfigurationOriginApply), input.Requester.Login, input.Requester.DatabaseID, input.Requester.NodeID, input.Requester.ActorType, input.Candidate.Operator.Login, input.Candidate.Operator.DatabaseID, input.Candidate.Operator.NodeID, input.Candidate.Operator.ActorType, input.Receipt.OperationID, formatTime(input.AcceptedAt))
	if err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	generationID, err := result.LastInsertId()
	if err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_apply_intents(generation_id,parent_generation_id,parent_digest,target_digest,operation_id,status,accepted_at) VALUES(?,?,?,?,?,'accepted',?)`, generationID, authority.Desired.GenerationID, authority.Desired.Digest, input.Candidate.Digest, input.Receipt.OperationID, formatTime(input.AcceptedAt)); err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	evidence := configurationEvidence("apply-accepted", generationID, input.Candidate.Digest, input.Receipt.OperationID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_convergence_events(event_type,generation_id,operation_id,digest,reason_code,evidence_digest,observed_at) VALUES('apply_accepted',?,?,?,?,?,?)`, generationID, input.Receipt.OperationID, input.Candidate.Digest, string(application.ConfigurationReasonApplyIncomplete), evidence, formatTime(input.AcceptedAt)); err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	generation, err := scanConfigurationGeneration(tx.QueryRowContext(ctx, configurationGenerationSelect+` WHERE generation_id=?`, generationID))
	if err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, err
	}
	return generation, input.Receipt, true, nil
}

func requireConfigurationAdmissionAuthorityTx(ctx context.Context, tx *sql.Tx, expected application.ConfigurationAdmissionAuthority) error {
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version < 31 {
		return nil
	}
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil {
		return err
	}
	// Stores created before configuration authority is established retain their
	// legacy/test behavior. Every production composition establishes authority
	// before it can reach a new-admission path.
	if !found {
		return nil
	}
	if !expected.Valid() || time.Now().UTC().After(expected.ValidThrough) || authority.Incomplete != nil || authority.Desired.GenerationID != expected.GenerationID || authority.Desired.Digest != expected.Digest || authority.Version != expected.AuthorityVersion || authority.EffectiveID != authority.Desired.GenerationID || authority.Desired.State != application.ConfigurationGenerationEffective {
		return application.ErrConfigurationAuthorityConflict
	}
	var lastDrift string
	err = tx.QueryRowContext(ctx, `SELECT event_type FROM configuration_convergence_events WHERE generation_id=? AND event_type IN ('drift_entered','drift_cleared') ORDER BY event_id DESC LIMIT 1`, authority.Desired.GenerationID).Scan(&lastDrift)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if lastDrift == "drift_entered" {
		return application.ErrConfigurationAuthorityConflict
	}
	return nil
}

func (s *Store) SettleConfigurationApply(ctx context.Context, settlement application.ConfigurationApplySettlement) (application.ConfigurationAuthority, application.OperationReceipt, bool, error) {
	if settlement.GenerationID <= 0 || settlement.ParentID <= 0 || strings.TrimSpace(settlement.OperationID) == "" || settlement.SettledAt.IsZero() || settlement.Outcome == application.ConfigurationApplyAccepted {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, errors.New("configuration apply settlement is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	defer tx.Rollback()
	var parentID int64
	var status, operationID string
	if err := tx.QueryRowContext(ctx, `SELECT parent_generation_id,status,operation_id FROM configuration_apply_intents WHERE generation_id=?`, settlement.GenerationID).Scan(&parentID, &status, &operationID); err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	receipt, found, err := getOperationReceiptByIDTx(ctx, tx, settlement.OperationID)
	if err != nil || !found {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if parentID != settlement.ParentID || operationID != settlement.OperationID {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if status != string(application.ConfigurationApplyAccepted) {
		if status == string(settlement.Outcome) {
			authority, _, authorityErr := configurationAuthorityQuery(ctx, tx)
			return authority, receipt, false, authorityErr
		}
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	authority, present, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !present || authority.Desired.GenerationID != settlement.ParentID {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	generation, err := scanConfigurationGeneration(tx.QueryRowContext(ctx, configurationGenerationSelect+` WHERE generation_id=?`, settlement.GenerationID))
	if err != nil || generation.OperationID != settlement.OperationID || generation.ParentID != settlement.ParentID {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	var lifecycle application.ConfigurationGenerationState
	var outcome application.OperationOutcome
	var eventType string
	resultingDigest := authority.Desired.Digest
	resultingState := authority.Desired.State
	resultingVersion := authority.Desired.GenerationID
	switch settlement.Outcome {
	case application.ConfigurationApplyCommitted:
		lifecycle, outcome, eventType = application.ConfigurationGenerationPendingRestart, application.OperationOutcomeSucceeded, "apply_committed"
		resultingDigest, resultingState, resultingVersion = generation.Digest, lifecycle, settlement.GenerationID
		if _, err := tx.ExecContext(ctx, `UPDATE configuration_generations SET lifecycle='superseded',superseded_at=?,settled_at=?,reason_code='' WHERE generation_id=?`, formatTime(settlement.SettledAt), formatTime(settlement.SettledAt), settlement.ParentID); err != nil {
			return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE configuration_authority SET desired_generation_id=?,authority_version=authority_version+1,updated_at=? WHERE authority_id=1 AND desired_generation_id=?`, settlement.GenerationID, formatTime(settlement.SettledAt), settlement.ParentID)
		if err != nil {
			return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
		}
		if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
			return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
		}
	case application.ConfigurationApplyFailed:
		lifecycle, outcome, eventType = application.ConfigurationGenerationFailed, application.OperationOutcomeFailed, "apply_failed"
	case application.ConfigurationApplyAmbiguous:
		lifecycle, outcome, eventType = application.ConfigurationGenerationAmbiguous, application.OperationOutcomeAmbiguous, "apply_ambiguous"
	default:
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, errors.New("configuration apply settlement outcome is invalid")
	}
	committedAt := ""
	if settlement.Outcome == application.ConfigurationApplyCommitted {
		committedAt = formatTime(settlement.SettledAt)
	}
	if err := configurationOne(tx.ExecContext(ctx, `UPDATE configuration_generations SET lifecycle=?,committed_at=?,settled_at=?,reason_code=? WHERE generation_id=? AND lifecycle='accepted'`, string(lifecycle), committedAt, formatTime(settlement.SettledAt), string(settlement.Reason), settlement.GenerationID)); err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	if err := configurationOne(tx.ExecContext(ctx, `UPDATE configuration_apply_intents SET status=?,settled_at=?,reason_code=? WHERE generation_id=? AND status='accepted'`, string(settlement.Outcome), formatTime(settlement.SettledAt), string(settlement.Reason), settlement.GenerationID)); err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	resultDigest := configurationEvidence("apply-result", settlement.GenerationID, string(settlement.Outcome), settlement.EvidenceDigest)
	receiptResult, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='applied',outcome='pending',resulting_authority_digest=?,resulting_state=?,resulting_version=?,evidence_digest=?,applied_at=? WHERE operation_id=? AND phase='accepted'`, resultingDigest, string(resultingState), resultingVersion, settlement.EvidenceDigest, formatTime(settlement.SettledAt), settlement.OperationID)
	if err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	if changed, rowsErr := receiptResult.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, application.ErrConfigurationAuthorityConflict
	}
	if err := configurationOne(tx.ExecContext(ctx, `UPDATE operation_receipts SET phase='observed',outcome=?,result_digest=?,settled_at=? WHERE operation_id=? AND phase='applied'`, string(outcome), resultDigest, formatTime(settlement.SettledAt), settlement.OperationID)); err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_convergence_events(event_type,generation_id,operation_id,digest,reason_code,evidence_digest,observed_at) VALUES(?,?,?,?,?,?,?)`, eventType, settlement.GenerationID, settlement.OperationID, generation.Digest, string(settlement.Reason), settlement.EvidenceDigest, formatTime(settlement.SettledAt)); err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	if settlement.Outcome == application.ConfigurationApplyCommitted {
		desiredEvidence := configurationEvidence("desired-changed", settlement.ParentID, settlement.GenerationID, generation.Digest)
		if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_convergence_events(event_type,generation_id,operation_id,digest,reason_code,evidence_digest,observed_at) VALUES('desired_changed',?,?,?,?,?,?)`, settlement.GenerationID, settlement.OperationID, generation.Digest, string(settlement.Reason), desiredEvidence, formatTime(settlement.SettledAt)); err != nil {
			return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
		}
	}
	authority, _, err = configurationAuthorityQuery(ctx, tx)
	if err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	receipt, _, err = getOperationReceiptByIDTx(ctx, tx, settlement.OperationID)
	if err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	if err := application.ValidateOperationReceipt(receipt); err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, errors.New("configuration operation receipt settlement is corrupt")
	}
	if err := tx.Commit(); err != nil {
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, err
	}
	return authority, receipt, true, nil
}

func (s *Store) ObserveConfigurationEffective(ctx context.Context, observation application.ConfigurationEffectiveObservation) (application.ConfigurationAuthority, bool, error) {
	if observation.ExpectedGenerationID <= 0 || !validConfigurationDigest(observation.ExpectedDigest) || observation.ObservedAt.IsZero() || !validConfigurationDigest(observation.EvidenceDigest) || strings.TrimSpace(observation.WorkerInstanceID) == "" || strings.TrimSpace(observation.BuildIdentity) == "" {
		return application.ConfigurationAuthority{}, false, errors.New("configuration effective observation is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	defer tx.Rollback()
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || authority.Incomplete != nil || authority.Desired.GenerationID != observation.ExpectedGenerationID || authority.Desired.Digest != observation.ExpectedDigest {
		return application.ConfigurationAuthority{}, false, application.ErrConfigurationAuthorityConflict
	}
	if authority.EffectiveID == observation.ExpectedGenerationID && !authority.Desired.EffectiveAt.IsZero() {
		return authority, false, tx.Commit()
	}
	generationResult, err := tx.ExecContext(ctx, `UPDATE configuration_generations SET lifecycle='effective',effective_at=?,reason_code='' WHERE generation_id=? AND digest=? AND lifecycle IN ('pending_restart','effective')`, formatTime(observation.ObservedAt), observation.ExpectedGenerationID, observation.ExpectedDigest)
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	if changed, rowsErr := generationResult.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ConfigurationAuthority{}, false, application.ErrConfigurationAuthorityConflict
	}
	authorityResult, err := tx.ExecContext(ctx, `UPDATE configuration_authority SET effective_generation_id=?,authority_version=authority_version+1,updated_at=? WHERE authority_id=1 AND desired_generation_id=? AND (effective_generation_id IS NULL OR effective_generation_id<>?)`, observation.ExpectedGenerationID, formatTime(observation.ObservedAt), observation.ExpectedGenerationID, observation.ExpectedGenerationID)
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	if changed, rowsErr := authorityResult.RowsAffected(); rowsErr != nil || changed != 1 {
		return application.ConfigurationAuthority{}, false, application.ErrConfigurationAuthorityConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO configuration_convergence_events(event_type,generation_id,digest,worker_instance_id,build_identity,reason_code,evidence_digest,observed_at) VALUES('effective_observed',?,?,?,?,?,?,?)`, observation.ExpectedGenerationID, observation.ExpectedDigest, observation.WorkerInstanceID, observation.BuildIdentity, string(application.ConfigurationReasonReady), observation.EvidenceDigest, formatTime(observation.ObservedAt)); err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	authority, _, err = configurationAuthorityQuery(ctx, tx)
	if err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.ConfigurationAuthority{}, false, err
	}
	return authority, true, nil
}

func (s *Store) ObserveConfigurationDrift(ctx context.Context, observation application.ConfigurationDriftObservation) (bool, error) {
	if observation.ExpectedGenerationID <= 0 || !validConfigurationDigest(observation.ExpectedDigest) || observation.ObservedAt.IsZero() {
		return false, errors.New("configuration drift observation is invalid")
	}
	if observation.Drifted && observation.Reason != application.ConfigurationReasonExternalDrift && observation.Reason != application.ConfigurationReasonUnsafeLiveFile {
		return false, errors.New("configuration drift reason is invalid")
	}
	if observation.ObservedDigest != "" && !validConfigurationDigest(observation.ObservedDigest) {
		return false, errors.New("configuration drift digest is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	authority, found, err := configurationAuthorityQuery(ctx, tx)
	if err != nil || !found || authority.Desired.GenerationID != observation.ExpectedGenerationID || authority.Desired.Digest != observation.ExpectedDigest {
		return false, application.ErrConfigurationAuthorityConflict
	}
	var lastID int64
	var lastType string
	err = tx.QueryRowContext(ctx, `SELECT event_id,event_type FROM configuration_convergence_events WHERE generation_id=? AND event_type IN ('drift_entered','drift_cleared') ORDER BY event_id DESC LIMIT 1`, observation.ExpectedGenerationID).Scan(&lastID, &lastType)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	eventType := "drift_cleared"
	if observation.Drifted {
		eventType = "drift_entered"
	}
	if lastType == eventType || lastType == "" && eventType == "drift_cleared" {
		return false, tx.Commit()
	}
	evidence := configurationEvidence("drift-transition", observation.ExpectedGenerationID, eventType, observation.ObservedDigest, lastID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_convergence_events(event_type,generation_id,digest,reason_code,evidence_digest,observed_at) VALUES(?,?,?,?,?,?)`, eventType, observation.ExpectedGenerationID, observation.ObservedDigest, string(observation.Reason), evidence, formatTime(observation.ObservedAt)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ListConfigurationGenerations(ctx context.Context) ([]application.ConfigurationGeneration, error) {
	rows, err := s.db.QueryContext(ctx, configurationGenerationSelect+` ORDER BY generation_id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []application.ConfigurationGeneration
	for rows.Next() {
		generation, err := scanConfigurationGeneration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, generation)
	}
	return result, rows.Err()
}

func (s *Store) ConfigurationRawPruneCandidates(ctx context.Context, keep int) ([]string, error) {
	if keep < 1 {
		return nil, errors.New("configuration retention bound is invalid")
	}
	authority, found, err := s.ConfigurationAuthority(ctx)
	if err != nil || !found {
		return nil, err
	}
	generations, err := s.ListConfigurationGenerations(ctx)
	if err != nil {
		return nil, err
	}
	protected := map[string]bool{authority.Desired.Digest: true}
	if authority.Incomplete != nil {
		protected[authority.Incomplete.TargetDigest] = true
		protected[authority.Incomplete.ParentDigest] = true
	}
	// The current desired raw snapshot is always one member of the fixed ten.
	// Unresolved evidence is protected beyond this bound but does not displace
	// settled history until reconciliation makes it eligible.
	settledKept := 1
	candidates := map[string]bool{}
	for _, generation := range generations {
		if generation.GenerationID == authority.Desired.GenerationID {
			continue
		}
		if generation.State == application.ConfigurationGenerationAccepted || generation.State == application.ConfigurationGenerationAmbiguous || generation.State == application.ConfigurationGenerationPendingRestart {
			protected[generation.Digest] = true
			continue
		}
		if settledKept < keep {
			protected[generation.Digest] = true
			settledKept++
			continue
		}
		if generation.RawRetained {
			candidates[generation.Digest] = true
		}
	}
	for digest := range protected {
		delete(candidates, digest)
	}
	result := make([]string, 0, len(candidates))
	for digest := range candidates {
		result = append(result, digest)
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) MarkConfigurationRawPruned(ctx context.Context, digest string) error {
	if !validConfigurationDigest(digest) {
		return errors.New("configuration raw digest is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE configuration_generations SET raw_retained=0 WHERE digest=? AND raw_retained=1 AND lifecycle IN ('superseded','failed') AND generation_id<>(SELECT desired_generation_id FROM configuration_authority WHERE authority_id=1) AND generation_id NOT IN (SELECT generation_id FROM configuration_apply_intents WHERE status IN ('accepted','ambiguous'))`, digest)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return application.ErrConfigurationAuthorityConflict
	}
	return nil
}

func scanConfigurationGeneration(row rowScanner) (application.ConfigurationGeneration, error) {
	var generation application.ConfigurationGeneration
	var origin, lifecycle, created, committed, effective, superseded, settled, reason string
	var raw int
	if err := row.Scan(&generation.GenerationID, &generation.ParentID, &generation.Digest, &generation.Size, &generation.SchemaVersion, &origin, &generation.Requester.Login, &generation.Requester.DatabaseID, &generation.Requester.NodeID, &generation.Requester.ActorType, &generation.ConfiguredOperator.Login, &generation.ConfiguredOperator.DatabaseID, &generation.ConfiguredOperator.NodeID, &generation.ConfiguredOperator.ActorType, &generation.OperationID, &lifecycle, &raw, &created, &committed, &effective, &superseded, &settled, &reason); err != nil {
		return application.ConfigurationGeneration{}, err
	}
	generation.Origin = application.ConfigurationGenerationOrigin(origin)
	generation.State = application.ConfigurationGenerationState(lifecycle)
	generation.RawRetained = raw == 1
	generation.CreatedAt, generation.CommittedAt, generation.EffectiveAt = parseTime(created), parseTime(committed), parseTime(effective)
	generation.SupersededAt, generation.SettledAt = parseTime(superseded), parseTime(settled)
	generation.Reason = application.ConfigurationReason(reason)
	if !validConfigurationMetadata(generation.Digest, generation.Size, generation.SchemaVersion) || generation.GenerationID <= 0 || generation.CreatedAt.IsZero() {
		return application.ConfigurationGeneration{}, errors.New("configuration generation is corrupt")
	}
	return generation, nil
}

func validConfigurationMetadata(digest string, size int64, schema int) bool {
	return validConfigurationDigest(digest) && size >= 0 && size <= 256<<10 && schema >= 1 && schema <= 5
}

func validConfigurationDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' && char < 'a' || char > 'f' {
			return false
		}
	}
	return true
}

func configurationOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return application.ErrConfigurationAuthorityConflict
	}
	return nil
}

func configurationEvidence(parts ...any) string {
	text := make([]string, 0, len(parts))
	for _, part := range parts {
		text = append(text, fmt.Sprint(part))
	}
	return applicationDigest("configuration-evidence-v1", text...)
}

func applicationDigest(prefix string, parts ...string) string {
	return application.ConfigurationEvidenceDigest(append([]string{prefix}, parts...)...)
}
