package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func (s *Store) ObserveCIWait(ctx context.Context, runID string, prNumber int64, headSHA, profileDigest string, threshold time.Duration, firstSeenCandidate, evaluatedAt time.Time) (application.CIWaitEvidence, error) {
	if runID == "" || prNumber < 1 || headSHA == "" || profileDigest == "" || threshold < time.Minute || threshold > 24*time.Hour || firstSeenCandidate.IsZero() || evaluatedAt.IsZero() || firstSeenCandidate.After(evaluatedAt) {
		return application.CIWaitEvidence{}, errors.New("CI wait authority is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.CIWaitEvidence{}, err
	}
	defer tx.Rollback()
	var state, candidate, profile string
	var persistedPR int64
	if err := tx.QueryRowContext(ctx, `SELECT r.current_state,r.candidate_head,r.profile_digest,p.number FROM runs r JOIN pull_requests p ON p.run_id=r.run_id WHERE r.run_id=?`, runID).Scan(&state, &candidate, &profile, &persistedPR); err != nil {
		return application.CIWaitEvidence{}, err
	}
	if (domain.State(state) != domain.StatePROpen && domain.State(state) != domain.StateReconcilingReviews) || candidate != headSHA || profile != profileDigest || persistedPR != prNumber {
		return application.CIWaitEvidence{}, errors.New("CI wait exact authority changed")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ci_waits SET closed_at=? WHERE run_id=? AND closed_at='' AND (pr_number<>? OR head_sha<>? OR profile_digest<>?)`, formatTime(evaluatedAt), runID, prNumber, headSHA, profileDigest); err != nil {
		return application.CIWaitEvidence{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ci_waits(run_id,pr_number,head_sha,profile_digest,first_seen_at) VALUES(?,?,?,?,?) ON CONFLICT(run_id,pr_number,head_sha,profile_digest) DO NOTHING`, runID, prNumber, headSHA, profileDigest, formatTime(firstSeenCandidate)); err != nil {
		return application.CIWaitEvidence{}, err
	}
	var first, warning, closed string
	if err := tx.QueryRowContext(ctx, `SELECT first_seen_at,warning_at,closed_at FROM ci_waits WHERE run_id=? AND pr_number=? AND head_sha=? AND profile_digest=?`, runID, prNumber, headSHA, profileDigest).Scan(&first, &warning, &closed); err != nil {
		return application.CIWaitEvidence{}, err
	}
	firstAt := parseTime(first)
	if warning == "" && !evaluatedAt.Before(firstAt.Add(threshold)) {
		warning = formatTime(firstAt.Add(threshold))
		result, err := tx.ExecContext(ctx, `UPDATE ci_waits SET warning_at=? WHERE run_id=? AND pr_number=? AND head_sha=? AND profile_digest=? AND warning_at='' AND closed_at=''`, warning, runID, prNumber, headSHA, profileDigest)
		if err != nil {
			return application.CIWaitEvidence{}, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return application.CIWaitEvidence{}, errors.New("CI wait warning compare update lost")
		}
	}
	if err := tx.Commit(); err != nil {
		return application.CIWaitEvidence{}, err
	}
	return application.CIWaitEvidence{RunID: runID, PRNumber: prNumber, HeadSHA: headSHA, ProfileDigest: profileDigest, FirstSeenAt: firstAt, WarningAt: parseTime(warning), ClosedAt: parseTime(closed)}, nil
}

func (s *Store) CloseCIWaits(ctx context.Context, runID string, closedAt time.Time) error {
	if runID == "" || closedAt.IsZero() {
		return errors.New("CI wait close authority is incomplete")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE ci_waits SET closed_at=? WHERE run_id=? AND closed_at=''`, formatTime(closedAt), runID)
	return err
}

func (s *Store) CloseInactiveCIWaits(ctx context.Context, closedAt time.Time) error {
	if closedAt.IsZero() {
		return errors.New("inactive CI wait close time is required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE ci_waits SET closed_at=? WHERE closed_at='' AND EXISTS (SELECT 1 FROM runs WHERE runs.run_id=ci_waits.run_id AND runs.current_state<>?)`, formatTime(closedAt), domain.StateReconcilingReviews)
	return err
}

func (s *Store) listCIWaits(ctx context.Context, runID string) ([]application.CIWaitEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,pr_number,head_sha,profile_digest,first_seen_at,warning_at,closed_at FROM ci_waits WHERE run_id=? ORDER BY first_seen_at,head_sha`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []application.CIWaitEvidence
	for rows.Next() {
		var item application.CIWaitEvidence
		var first, warning, closed string
		if err := rows.Scan(&item.RunID, &item.PRNumber, &item.HeadSHA, &item.ProfileDigest, &first, &warning, &closed); err != nil {
			return nil, err
		}
		item.FirstSeenAt, item.WarningAt, item.ClosedAt = parseTime(first), parseTime(warning), parseTime(closed)
		result = append(result, item)
	}
	return result, rows.Err()
}
