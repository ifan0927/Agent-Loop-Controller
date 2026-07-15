package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// PersistFreshReviewFindings atomically persists one controller fresh-review
// finding set and advances the run into repairing. A replay is accepted only
// when the exact review attempt, outcome digest, transition reference, and
// immutable finding rows are still present.
func (s *Store) PersistFreshReviewFindings(ctx context.Context, evidence application.FreshReviewRepairEvidence) (bool, error) {
	if err := evidence.Validate(); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var currentState, candidateHead string
	if err := tx.QueryRowContext(ctx, `SELECT current_state,candidate_head FROM runs WHERE run_id=?`, evidence.RunID).Scan(&currentState, &candidateHead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, application.ErrRunNotFound
		}
		return false, err
	}
	if candidateHead != evidence.ReviewedHead {
		return false, errors.New("fresh review repair candidate head authority changed")
	}

	if domain.State(currentState) == domain.StateRepairing {
		if err := verifyFreshReviewRepairReplayTx(ctx, tx, evidence); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if domain.State(currentState) != domain.StateFreshReview {
		return false, fmt.Errorf("fresh review repair requires fresh_review state, got %s", currentState)
	}
	if err := verifyFreshReviewAttemptTx(ctx, tx, evidence); err != nil {
		return false, err
	}
	if err := verifyFreshReviewFindingRowsTx(ctx, tx, evidence, false); err != nil {
		return false, err
	}
	var existingTransitionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE run_id=? AND from_state=? AND to_state=? AND bound_head=? AND evidence_reference=?`, evidence.RunID, domain.StateFreshReview, domain.StateRepairing, evidence.ReviewedHead, evidence.TransitionReference()).Scan(&existingTransitionCount); err != nil {
		return false, err
	}
	if existingTransitionCount != 0 {
		return false, errors.New("fresh review repair transition exists while run is not repairing")
	}

	for _, finding := range evidence.Findings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_findings(run_id,source_id,thread_id,source,file,line,severity,body_digest,body_text,resolved,outdated,head_sha,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,source,source_id,head_sha) DO NOTHING`, finding.RunID, finding.SourceID, finding.ThreadID, finding.Source, finding.File, finding.Line, finding.Severity, finding.BodyDigest, finding.Body, finding.Resolved, finding.Outdated, finding.HeadSHA, findingObservedAt(finding)); err != nil {
			return false, err
		}
	}
	sequence, err := nextTransitionSequenceTx(ctx, tx, evidence.RunID)
	if err != nil {
		return false, err
	}
	now := nowText()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,last_error='',updated_at=? WHERE run_id=? AND current_state=? AND candidate_head=?`, domain.StateRepairing, now, evidence.RunID, domain.StateFreshReview, evidence.ReviewedHead)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, errors.New("fresh review repair state compare update lost")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, evidence.RunID, sequence, domain.StateFreshReview, domain.StateRepairing, "fresh structured review findings persisted", evidence.TransitionReference(), evidence.ReviewedHead, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func verifyFreshReviewRepairReplayTx(ctx context.Context, tx *sql.Tx, evidence application.FreshReviewRepairEvidence) error {
	var transitionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE run_id=? AND from_state=? AND to_state=? AND bound_head=? AND evidence_reference=?`, evidence.RunID, domain.StateFreshReview, domain.StateRepairing, evidence.ReviewedHead, evidence.TransitionReference()).Scan(&transitionCount); err != nil {
		return err
	}
	if transitionCount != 1 {
		return errors.New("fresh review repair replay transition evidence is missing or conflicting")
	}
	if err := verifyFreshReviewAttemptTx(ctx, tx, evidence); err != nil {
		return err
	}
	return verifyFreshReviewFindingRowsTx(ctx, tx, evidence, true)
}

func verifyFreshReviewAttemptTx(ctx context.Context, tx *sql.Tx, evidence application.FreshReviewRepairEvidence) error {
	var reviewCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reviews WHERE run_id=? AND attempt_id=? AND reviewed_head=? AND verdict=? AND outcome_path=? AND outcome_hash=?`, evidence.RunID, evidence.AttemptID, evidence.ReviewedHead, string(domain.ReviewFindings), evidence.OutcomePath, evidence.OutcomeHash).Scan(&reviewCount); err != nil {
		return err
	}
	if reviewCount != 1 {
		return errors.New("fresh review finding attempt evidence is missing or conflicting")
	}
	var attemptRun, kind, status, sessionID, reviewSessionID, outcomePath, outcomeHash string
	var exitCode int
	if err := tx.QueryRowContext(ctx, `SELECT a.run_id,a.kind,a.status,a.codex_session_id,a.outcome_path,a.outcome_hash,a.exit_code,r.review_session_id FROM attempts a JOIN reviews r ON r.attempt_id=a.attempt_id WHERE a.attempt_id=? AND r.run_id=?`, evidence.AttemptID, evidence.RunID).Scan(&attemptRun, &kind, &status, &sessionID, &outcomePath, &outcomeHash, &exitCode, &reviewSessionID); err != nil {
		return err
	}
	if attemptRun != evidence.RunID || kind != "review" || status != "succeeded" || exitCode != 0 || sessionID == "" || sessionID != reviewSessionID || outcomePath != evidence.OutcomePath || outcomeHash != evidence.OutcomeHash {
		return errors.New("fresh review finding attempt is not a successful exact persisted review")
	}
	return nil
}

type freshReviewFindingRow struct {
	SourceID   string
	ThreadID   string
	Source     string
	File       string
	Line       int
	Severity   string
	BodyDigest string
	Body       string
	Resolved   bool
	Outdated   bool
	HeadSHA    string
}

func verifyFreshReviewFindingRowsTx(ctx context.Context, tx *sql.Tx, evidence application.FreshReviewRepairEvidence, requireComplete bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT source_id,thread_id,source,file,line,severity,body_digest,body_text,resolved,outdated,head_sha FROM review_findings WHERE run_id=? AND source=? AND head_sha=?`, evidence.RunID, application.FreshReviewFindingSource, evidence.ReviewedHead)
	if err != nil {
		return err
	}
	defer rows.Close()
	existing := make(map[string]freshReviewFindingRow)
	for rows.Next() {
		var row freshReviewFindingRow
		var resolved, outdated int
		if err := rows.Scan(&row.SourceID, &row.ThreadID, &row.Source, &row.File, &row.Line, &row.Severity, &row.BodyDigest, &row.Body, &resolved, &outdated, &row.HeadSHA); err != nil {
			return err
		}
		row.Resolved, row.Outdated = resolved != 0, outdated != 0
		if _, duplicate := existing[row.SourceID]; duplicate {
			return errors.New("fresh review finding persistence contains duplicate source ID")
		}
		existing[row.SourceID] = row
	}
	if err := rows.Err(); err != nil {
		return err
	}
	expected := make(map[string]struct{}, len(evidence.Findings))
	for _, finding := range evidence.Findings {
		expected[finding.SourceID] = struct{}{}
	}
	if len(existing) > len(evidence.Findings) || requireComplete && len(existing) != len(evidence.Findings) {
		return errors.New("fresh review finding persistence contains an unexpected or missing source ID")
	}
	for sourceID := range existing {
		if _, found := expected[sourceID]; !found {
			return errors.New("unexpected fresh review finding source ID")
		}
	}
	for _, finding := range evidence.Findings {
		row, found := existing[finding.SourceID]
		if !found {
			continue
		}
		if row.SourceID != finding.SourceID || row.ThreadID != finding.ThreadID || row.Source != finding.Source || row.File != finding.File || row.Line != finding.Line || row.Severity != finding.Severity || row.BodyDigest != finding.BodyDigest || row.Body != finding.Body || row.Resolved != finding.Resolved || row.Outdated != finding.Outdated || row.HeadSHA != finding.HeadSHA {
			return errors.New("conflicting fresh review finding source ID")
		}
	}
	return nil
}

func findingObservedAt(finding application.FindingRecord) string {
	if finding.ObservedAt.IsZero() {
		return nowText()
	}
	return formatTime(finding.ObservedAt)
}
