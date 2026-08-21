package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const configurationDraftSelect = `SELECT draft_id,base_generation_id,base_digest,revision,lifecycle,
	run_timeout_ns,admission_enabled,admission_poll_interval_ns,delivery_poll_interval_ns,scheduler_lease_ttl_ns,scheduler_lease_renewal_interval_ns,max_candidates,max_pages,heavy_capacity,settings_digest,
	last_edit_field,last_edit_base_revision,last_edit_digest,
	validation_revision,validation_candidate_digest,validation_digest,validation_valid,validation_findings_json,validated_at,
	preview_revision,preview_candidate_digest,preview_digest,preview_changes_json,preview_impacts_json,previewed_at,
	result_operation_id,COALESCE(result_generation_id,0),result_no_op,created_at,updated_at,settled_at,reason_code
	FROM configuration_drafts`

func (s *Store) OpenConfigurationDraft(ctx context.Context, input application.ConfigurationDraftOpenInput) (application.ConfigurationDraft, bool, error) {
	if !validDraftID(input.DraftID) || input.BaseGenerationID <= 0 || !validConfigurationDigest(input.BaseDigest) || input.SettingsDigest != application.ConfigurationSettingsDigest(input.Settings) || input.OpenedAt.IsZero() {
		return application.ConfigurationDraft{}, false, errors.New("configuration draft input is invalid")
	}
	settings := input.Settings
	_, err := s.db.ExecContext(ctx, `INSERT INTO configuration_drafts(
		draft_id,base_generation_id,base_digest,revision,lifecycle,run_timeout_ns,admission_enabled,admission_poll_interval_ns,delivery_poll_interval_ns,scheduler_lease_ttl_ns,scheduler_lease_renewal_interval_ns,max_candidates,max_pages,heavy_capacity,settings_digest,created_at,updated_at
	) VALUES(?,?,?,1,'open',?,?,?,?,?,?,?,?,?,?,?,?)`,
		input.DraftID, input.BaseGenerationID, input.BaseDigest,
		int64(settings.RunTimeout), boolInt(settings.Admission.Enabled), int64(settings.Admission.PollInterval), int64(settings.Admission.DeliveryPollInterval), int64(settings.Admission.SchedulerLeaseTTL), int64(settings.Admission.SchedulerLeaseRenewalInterval), settings.Admission.MaxCandidates, settings.Admission.MaxPages, settings.Admission.HeavyCapacity, input.SettingsDigest, formatTime(input.OpenedAt), formatTime(input.OpenedAt))
	if err == nil {
		draft, found, readErr := s.ConfigurationDraft(ctx, input.DraftID)
		return draft, true, readErrIfMissing(readErr, found)
	}
	draft, found, readErr := s.ActiveConfigurationDraft(ctx)
	if readErr != nil || !found {
		return application.ConfigurationDraft{}, false, err
	}
	return draft, false, nil
}

func (s *Store) ConfigurationDraft(ctx context.Context, draftID string) (application.ConfigurationDraft, bool, error) {
	if !validDraftID(draftID) {
		return application.ConfigurationDraft{}, false, nil
	}
	draft, err := scanConfigurationDraft(s.db.QueryRowContext(ctx, configurationDraftSelect+` WHERE draft_id=?`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.ConfigurationDraft{}, false, nil
	}
	return draft, err == nil, err
}

func (s *Store) ActiveConfigurationDraft(ctx context.Context) (application.ConfigurationDraft, bool, error) {
	draft, err := scanConfigurationDraft(s.db.QueryRowContext(ctx, configurationDraftSelect+` WHERE lifecycle IN ('open','applying','ambiguous') ORDER BY updated_at DESC,draft_id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return application.ConfigurationDraft{}, false, nil
	}
	return draft, err == nil, err
}

func (s *Store) EditConfigurationDraft(ctx context.Context, input application.ConfigurationDraftEditInput) (application.ConfigurationDraft, bool, error) {
	if !validDraftID(input.DraftID) || input.ExpectedRevision < 1 || input.SettingsDigest != application.ConfigurationSettingsDigest(input.Settings) || !validConfigurationDigest(input.EditDigest) || input.EditedAt.IsZero() || !validConfigurationField(input.Field) {
		return application.ConfigurationDraft{}, false, errors.New("configuration draft edit is invalid")
	}
	settings := input.Settings
	result, err := s.db.ExecContext(ctx, `UPDATE configuration_drafts SET
		revision=revision+1,run_timeout_ns=?,admission_enabled=?,admission_poll_interval_ns=?,delivery_poll_interval_ns=?,scheduler_lease_ttl_ns=?,scheduler_lease_renewal_interval_ns=?,max_candidates=?,max_pages=?,heavy_capacity=?,settings_digest=?,
		last_edit_field=?,last_edit_base_revision=?,last_edit_digest=?,
		validation_revision=0,validation_candidate_digest='',validation_digest='',validation_valid=0,validation_findings_json='',validated_at='',
		preview_revision=0,preview_candidate_digest='',preview_digest='',preview_changes_json='',preview_impacts_json='',previewed_at='',updated_at=?,reason_code=''
		WHERE draft_id=? AND lifecycle='open' AND revision=?`,
		int64(settings.RunTimeout), boolInt(settings.Admission.Enabled), int64(settings.Admission.PollInterval), int64(settings.Admission.DeliveryPollInterval), int64(settings.Admission.SchedulerLeaseTTL), int64(settings.Admission.SchedulerLeaseRenewalInterval), settings.Admission.MaxCandidates, settings.Admission.MaxPages, settings.Admission.HeavyCapacity, input.SettingsDigest,
		string(input.Field), input.ExpectedRevision, input.EditDigest, formatTime(input.EditedAt), input.DraftID, input.ExpectedRevision)
	if err != nil {
		return application.ConfigurationDraft{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		draft, found, readErr := s.ConfigurationDraft(ctx, input.DraftID)
		return draft, true, readErrIfMissing(readErr, found)
	}
	draft, found, readErr := s.ConfigurationDraft(ctx, input.DraftID)
	if readErr != nil || !found {
		return application.ConfigurationDraft{}, false, errors.New("configuration draft changed")
	}
	if draft.State == application.ConfigurationDraftOpen && draft.Revision == input.ExpectedRevision+1 && draft.LastEditBaseRevision == input.ExpectedRevision && draft.LastEditDigest == input.EditDigest && draft.SettingsDigest == input.SettingsDigest {
		return draft, false, nil
	}
	return application.ConfigurationDraft{}, false, errors.New("configuration draft changed")
}

func (s *Store) RecordConfigurationDraftMetadata(ctx context.Context, input application.ConfigurationDraftMetadataInput) (application.ConfigurationDraft, error) {
	if !validDraftID(input.DraftID) || input.ExpectedRevision < 1 || input.UpdatedAt.IsZero() || input.Validation == nil {
		return application.ConfigurationDraft{}, errors.New("configuration draft metadata is invalid")
	}
	validation := input.Validation
	if validation.DraftID != input.DraftID || validation.Revision != input.ExpectedRevision || !validConfigurationDigest(validation.CandidateDigest) || !validConfigurationDigest(validation.ValidationDigest) || validation.ValidatedAt.IsZero() {
		return application.ConfigurationDraft{}, errors.New("configuration validation metadata is invalid")
	}
	findings, err := json.Marshal(validation.Findings)
	if err != nil {
		return application.ConfigurationDraft{}, err
	}
	previewRevision, previewCandidate, previewDigest, changesJSON, impactsJSON, previewedAt := int64(0), "", "", "", "", ""
	if input.Preview != nil {
		preview := input.Preview
		if preview.DraftID != input.DraftID || preview.Revision != input.ExpectedRevision || preview.CandidateDigest != validation.CandidateDigest || !validConfigurationDigest(preview.PreviewDigest) || preview.PreviewedAt.IsZero() {
			return application.ConfigurationDraft{}, errors.New("configuration preview metadata is invalid")
		}
		changes, marshalErr := json.Marshal(preview.Changes)
		if marshalErr != nil {
			return application.ConfigurationDraft{}, marshalErr
		}
		impacts, marshalErr := json.Marshal(preview.Impacts)
		if marshalErr != nil {
			return application.ConfigurationDraft{}, marshalErr
		}
		previewRevision, previewCandidate, previewDigest, changesJSON, impactsJSON, previewedAt = preview.Revision, preview.CandidateDigest, preview.PreviewDigest, string(changes), string(impacts), formatTime(preview.PreviewedAt)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE configuration_drafts SET
		validation_revision=?,validation_candidate_digest=?,validation_digest=?,validation_valid=?,validation_findings_json=?,validated_at=?,
		preview_revision=?,preview_candidate_digest=?,preview_digest=?,preview_changes_json=?,preview_impacts_json=?,previewed_at=?,updated_at=?
		WHERE draft_id=? AND lifecycle='open' AND revision=?`,
		validation.Revision, validation.CandidateDigest, validation.ValidationDigest, boolInt(validation.Valid), string(findings), formatTime(validation.ValidatedAt),
		previewRevision, previewCandidate, previewDigest, changesJSON, impactsJSON, previewedAt, formatTime(input.UpdatedAt), input.DraftID, input.ExpectedRevision)
	if err != nil {
		return application.ConfigurationDraft{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return application.ConfigurationDraft{}, errors.New("configuration draft changed")
	}
	draft, found, err := s.ConfigurationDraft(ctx, input.DraftID)
	if err := readErrIfMissing(err, found); err != nil {
		return application.ConfigurationDraft{}, err
	}
	return draft, nil
}

func (s *Store) BindConfigurationDraftApply(ctx context.Context, input application.ConfigurationDraftApplyBinding) (application.ConfigurationDraft, bool, error) {
	if !validDraftID(input.DraftID) || input.ExpectedRevision < 1 || !validConfigurationDigest(input.PreviewDigest) || input.UpdatedAt.IsZero() {
		return application.ConfigurationDraft{}, false, errors.New("configuration draft apply binding is invalid")
	}
	var result sql.Result
	var err error
	switch input.State {
	case application.ConfigurationDraftOpen:
		result, err = s.db.ExecContext(ctx, `UPDATE configuration_drafts SET lifecycle='open',validation_revision=0,validation_candidate_digest='',validation_digest='',validation_valid=0,validation_findings_json='',validated_at='',preview_revision=0,preview_candidate_digest='',preview_digest='',preview_changes_json='',preview_impacts_json='',previewed_at='',reason_code=?,updated_at=? WHERE draft_id=? AND lifecycle='applying' AND revision=? AND preview_digest=? AND result_operation_id=''`, input.Reason, formatTime(input.UpdatedAt), input.DraftID, input.ExpectedRevision, input.PreviewDigest)
	case application.ConfigurationDraftApplying:
		if input.OperationID == "" {
			result, err = s.db.ExecContext(ctx, `UPDATE configuration_drafts SET lifecycle='applying',updated_at=? WHERE draft_id=? AND lifecycle='open' AND revision=? AND preview_revision=? AND preview_digest=?`, formatTime(input.UpdatedAt), input.DraftID, input.ExpectedRevision, input.ExpectedRevision, input.PreviewDigest)
		} else {
			result, err = s.db.ExecContext(ctx, `UPDATE configuration_drafts SET result_operation_id=?,result_generation_id=?,reason_code=?,updated_at=? WHERE draft_id=? AND lifecycle='applying' AND revision=? AND preview_digest=? AND (result_operation_id='' OR result_operation_id=?)`, input.OperationID, nullableGeneration(input.GenerationID), input.Reason, formatTime(input.UpdatedAt), input.DraftID, input.ExpectedRevision, input.PreviewDigest, input.OperationID)
		}
	case application.ConfigurationDraftApplied:
		if input.OperationID == "" || input.GenerationID <= 0 {
			return application.ConfigurationDraft{}, false, errors.New("configuration draft result is incomplete")
		}
		result, err = s.db.ExecContext(ctx, `UPDATE configuration_drafts SET lifecycle='applied',result_operation_id=?,result_generation_id=?,result_no_op=?,reason_code=?,updated_at=?,settled_at=? WHERE draft_id=? AND lifecycle='applying' AND revision=? AND preview_digest=? AND (result_operation_id='' OR result_operation_id=?)`, input.OperationID, input.GenerationID, boolInt(input.NoOp), input.Reason, formatTime(input.UpdatedAt), formatTime(input.UpdatedAt), input.DraftID, input.ExpectedRevision, input.PreviewDigest, input.OperationID)
	case application.ConfigurationDraftAmbiguous:
		if input.OperationID == "" || input.GenerationID <= 0 {
			return application.ConfigurationDraft{}, false, errors.New("configuration draft ambiguity is incomplete")
		}
		result, err = s.db.ExecContext(ctx, `UPDATE configuration_drafts SET lifecycle='ambiguous',result_operation_id=?,result_generation_id=?,reason_code=?,updated_at=? WHERE draft_id=? AND lifecycle='applying' AND revision=? AND preview_digest=? AND (result_operation_id='' OR result_operation_id=?)`, input.OperationID, input.GenerationID, input.Reason, formatTime(input.UpdatedAt), input.DraftID, input.ExpectedRevision, input.PreviewDigest, input.OperationID)
	default:
		return application.ConfigurationDraft{}, false, errors.New("configuration draft apply state is invalid")
	}
	if err != nil {
		return application.ConfigurationDraft{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		draft, found, readErr := s.ConfigurationDraft(ctx, input.DraftID)
		return draft, true, readErrIfMissing(readErr, found)
	}
	draft, found, readErr := s.ConfigurationDraft(ctx, input.DraftID)
	if readErr != nil || !found {
		return application.ConfigurationDraft{}, false, errors.New("configuration draft changed")
	}
	if draft.Revision == input.ExpectedRevision && draft.Preview != nil && draft.Preview.PreviewDigest == input.PreviewDigest {
		switch input.State {
		case application.ConfigurationDraftApplying:
			if draft.State == application.ConfigurationDraftApplying && (input.OperationID == "" || draft.ResultOperationID == input.OperationID) {
				return draft, false, nil
			}
		case application.ConfigurationDraftApplied:
			if draft.State == application.ConfigurationDraftApplied && draft.ResultOperationID == input.OperationID && draft.ResultGenerationID == input.GenerationID && draft.ResultNoOp == input.NoOp {
				return draft, false, nil
			}
		case application.ConfigurationDraftAmbiguous:
			if draft.State == application.ConfigurationDraftAmbiguous && draft.ResultOperationID == input.OperationID && draft.ResultGenerationID == input.GenerationID {
				return draft, false, nil
			}
		}
	}
	if input.State == application.ConfigurationDraftOpen && draft.State == application.ConfigurationDraftOpen && draft.Revision == input.ExpectedRevision && draft.Preview == nil && draft.ResultOperationID == "" {
		return draft, false, nil
	}
	return application.ConfigurationDraft{}, false, errors.New("configuration draft changed")
}

func (s *Store) DiscardConfigurationDraft(ctx context.Context, input application.ConfigurationDraftDiscardInput) (application.ConfigurationDraft, bool, error) {
	if !validDraftID(input.DraftID) || input.ExpectedRevision < 1 || input.DiscardedAt.IsZero() {
		return application.ConfigurationDraft{}, false, errors.New("configuration draft discard is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE configuration_drafts SET lifecycle='discarded',reason_code='discarded',updated_at=?,settled_at=? WHERE draft_id=? AND lifecycle='open' AND revision=?`, formatTime(input.DiscardedAt), formatTime(input.DiscardedAt), input.DraftID, input.ExpectedRevision)
	if err != nil {
		return application.ConfigurationDraft{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		draft, found, readErr := s.ConfigurationDraft(ctx, input.DraftID)
		return draft, true, readErrIfMissing(readErr, found)
	}
	draft, found, readErr := s.ConfigurationDraft(ctx, input.DraftID)
	if readErr == nil && found && draft.State == application.ConfigurationDraftDiscarded && draft.Revision == input.ExpectedRevision {
		return draft, false, nil
	}
	return application.ConfigurationDraft{}, false, errors.New("configuration draft changed")
}

type configurationDraftScanner interface{ Scan(...any) error }

func scanConfigurationDraft(row configurationDraftScanner) (application.ConfigurationDraft, error) {
	var draft application.ConfigurationDraft
	var lifecycle, lastField string
	var runTimeout, poll, delivery, ttl, renewal int64
	var enabled, validationValid, resultNoOp int
	var validationRevision int64
	var validationCandidate, validationDigest, findingsJSON, validatedAt string
	var previewRevision int64
	var previewCandidate, previewDigest, changesJSON, impactsJSON, previewedAt string
	var createdAt, updatedAt, settledAt string
	err := row.Scan(&draft.DraftID, &draft.BaseGenerationID, &draft.BaseDigest, &draft.Revision, &lifecycle,
		&runTimeout, &enabled, &poll, &delivery, &ttl, &renewal, &draft.Settings.Admission.MaxCandidates, &draft.Settings.Admission.MaxPages, &draft.Settings.Admission.HeavyCapacity, &draft.SettingsDigest,
		&lastField, &draft.LastEditBaseRevision, &draft.LastEditDigest,
		&validationRevision, &validationCandidate, &validationDigest, &validationValid, &findingsJSON, &validatedAt,
		&previewRevision, &previewCandidate, &previewDigest, &changesJSON, &impactsJSON, &previewedAt,
		&draft.ResultOperationID, &draft.ResultGenerationID, &resultNoOp, &createdAt, &updatedAt, &settledAt, &draft.Reason)
	if err != nil {
		return application.ConfigurationDraft{}, err
	}
	draft.State = application.ConfigurationDraftState(lifecycle)
	draft.Settings.RunTimeout = application.ConfigurationDuration(runTimeout)
	draft.Settings.Admission.Enabled = enabled == 1
	draft.Settings.Admission.PollInterval = application.ConfigurationDuration(poll)
	draft.Settings.Admission.DeliveryPollInterval = application.ConfigurationDuration(delivery)
	draft.Settings.Admission.SchedulerLeaseTTL = application.ConfigurationDuration(ttl)
	draft.Settings.Admission.SchedulerLeaseRenewalInterval = application.ConfigurationDuration(renewal)
	draft.LastEditField = application.ConfigurationFieldID(lastField)
	draft.ResultNoOp = resultNoOp == 1
	draft.CreatedAt, draft.UpdatedAt, draft.SettledAt = parseTime(createdAt), parseTime(updatedAt), parseTime(settledAt)
	if validationRevision > 0 {
		var findings []application.ConfigurationValidationFinding
		if json.Unmarshal([]byte(findingsJSON), &findings) != nil {
			return application.ConfigurationDraft{}, errors.New("configuration validation evidence is malformed")
		}
		draft.Validation = &application.ConfigurationValidationResult{DraftID: draft.DraftID, Revision: validationRevision, CandidateDigest: validationCandidate, ValidationDigest: validationDigest, Valid: validationValid == 1, Findings: findings, ValidatedAt: parseTime(validatedAt)}
	}
	if previewRevision > 0 {
		var changes []application.ConfigurationPreviewChange
		var impacts []application.ConfigurationPreviewImpact
		if json.Unmarshal([]byte(changesJSON), &changes) != nil || json.Unmarshal([]byte(impactsJSON), &impacts) != nil {
			return application.ConfigurationDraft{}, errors.New("configuration preview evidence is malformed")
		}
		draft.Preview = &application.ConfigurationPreview{DraftID: draft.DraftID, Revision: previewRevision, BaseGenerationID: draft.BaseGenerationID, BaseDigest: draft.BaseDigest, CandidateDigest: previewCandidate, PreviewDigest: previewDigest, Changes: changes, Impacts: impacts, PreviewedAt: parseTime(previewedAt)}
	}
	if err := validatePersistedDraft(draft); err != nil {
		return application.ConfigurationDraft{}, err
	}
	return draft, nil
}

func validatePersistedDraft(draft application.ConfigurationDraft) error {
	validState := draft.State == application.ConfigurationDraftOpen || draft.State == application.ConfigurationDraftApplying || draft.State == application.ConfigurationDraftApplied || draft.State == application.ConfigurationDraftDiscarded || draft.State == application.ConfigurationDraftAmbiguous
	settings := draft.Settings
	validSettings := settings.RunTimeout.Duration() > 0 && settings.RunTimeout.Duration() <= 2*time.Hour &&
		settings.Admission.PollInterval.Duration() >= time.Minute && settings.Admission.PollInterval.Duration() <= time.Hour &&
		settings.Admission.DeliveryPollInterval.Duration() >= 30*time.Second && settings.Admission.DeliveryPollInterval.Duration() <= 5*time.Minute &&
		settings.Admission.SchedulerLeaseTTL.Duration() >= 30*time.Second && settings.Admission.SchedulerLeaseTTL.Duration() <= 10*time.Minute &&
		settings.Admission.SchedulerLeaseRenewalInterval.Duration() >= 5*time.Second && settings.Admission.SchedulerLeaseRenewalInterval.Duration() <= 5*time.Minute &&
		settings.Admission.MaxCandidates >= 1 && settings.Admission.MaxCandidates <= 100 && settings.Admission.MaxPages >= 1 && settings.Admission.MaxPages <= 20 && settings.Admission.HeavyCapacity >= 1 && settings.Admission.HeavyCapacity <= application.MaxHeavyCapacity
	validEditReplay := draft.Revision == 1 && draft.LastEditField == "" && draft.LastEditBaseRevision == 0 && draft.LastEditDigest == "" || draft.Revision > 1 && validConfigurationField(draft.LastEditField) && draft.LastEditBaseRevision == draft.Revision-1 && validConfigurationDigest(draft.LastEditDigest)
	if !validDraftID(draft.DraftID) || draft.BaseGenerationID <= 0 || !validConfigurationDigest(draft.BaseDigest) || draft.Revision < 1 || !validState || !validSettings || !validEditReplay || draft.SettingsDigest != application.ConfigurationSettingsDigest(draft.Settings) || draft.CreatedAt.IsZero() || draft.UpdatedAt.IsZero() {
		return errors.New("configuration draft evidence is malformed")
	}
	if draft.Validation != nil && (draft.Validation.Revision != draft.Revision || !validConfigurationDigest(draft.Validation.CandidateDigest) || !validConfigurationDigest(draft.Validation.ValidationDigest) || draft.Validation.ValidatedAt.IsZero()) {
		return errors.New("configuration validation evidence is malformed")
	}
	if draft.Preview != nil && (draft.Validation == nil || !draft.Validation.Valid || draft.Preview.Revision != draft.Revision || draft.Preview.CandidateDigest != draft.Validation.CandidateDigest || !validConfigurationDigest(draft.Preview.PreviewDigest) || draft.Preview.PreviewedAt.IsZero()) {
		return errors.New("configuration preview evidence is malformed")
	}
	if draft.State == application.ConfigurationDraftApplied && (draft.ResultOperationID == "" || draft.ResultGenerationID <= 0 || draft.SettledAt.IsZero()) {
		return errors.New("configuration draft result evidence is malformed")
	}
	if draft.State == application.ConfigurationDraftAmbiguous && (draft.ResultOperationID == "" || draft.ResultGenerationID <= 0) {
		return errors.New("configuration draft ambiguity evidence is malformed")
	}
	return nil
}

func validDraftID(value string) bool {
	if !strings.HasPrefix(value, "configuration-draft-") || len(value) != len("configuration-draft-")+32 {
		return false
	}
	for _, char := range value[len("configuration-draft-"):] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func validConfigurationField(field application.ConfigurationFieldID) bool {
	switch field {
	case application.ConfigurationFieldRunTimeout, application.ConfigurationFieldAdmissionEnabled, application.ConfigurationFieldAdmissionPollInterval, application.ConfigurationFieldDeliveryPollInterval, application.ConfigurationFieldSchedulerLeaseTTL, application.ConfigurationFieldSchedulerLeaseRenewalInterval, application.ConfigurationFieldAdmissionMaxCandidates, application.ConfigurationFieldAdmissionMaxPages, application.ConfigurationFieldAdmissionHeavyCapacity:
		return true
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func nullableGeneration(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
func readErrIfMissing(err error, found bool) error {
	if err != nil {
		return err
	}
	if !found {
		return errors.New("configuration draft is missing")
	}
	return nil
}
