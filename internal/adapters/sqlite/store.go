package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const schemaVersion = 1

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SQLite database path must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&version)
	return version, err
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported %d", current, schemaVersion)
	}
	if current == schemaVersion {
		return tx.Commit()
	}
	for version := current + 1; version <= schemaVersion; version++ {
		var statements []string
		switch version {
		case 1:
			statements = migrationV1
		default:
			return fmt.Errorf("missing migration version %d", version)
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate schema to version %d: %w", version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, version, nowText()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var migrationV1 = []string{
	`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY, issue_id TEXT NOT NULL, idempotency_key TEXT NOT NULL UNIQUE,
			source_revision TEXT NOT NULL, raw_issue_json TEXT NOT NULL, raw_issue_hash TEXT NOT NULL,
			normalized_task_json TEXT NOT NULL, task_hash TEXT NOT NULL, repository TEXT NOT NULL,
			repository_config_json TEXT NOT NULL,
			base_branch TEXT NOT NULL, working_branch TEXT NOT NULL, base_sha TEXT NOT NULL DEFAULT '',
			worktree_path TEXT NOT NULL DEFAULT '', artifact_root TEXT NOT NULL, current_state TEXT NOT NULL,
			candidate_head TEXT NOT NULL DEFAULT '', implementation_session_id TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS runs_one_active_issue ON runs(issue_id)
			WHERE current_state NOT IN ('rejected','failed','completed')`,
	`CREATE TABLE IF NOT EXISTS transitions (
			run_id TEXT NOT NULL REFERENCES runs(run_id), sequence INTEGER NOT NULL,
			from_state TEXT NOT NULL, to_state TEXT NOT NULL, reason TEXT NOT NULL,
			evidence_reference TEXT NOT NULL, bound_head TEXT NOT NULL, created_at TEXT NOT NULL,
			PRIMARY KEY(run_id, sequence))`,
	`CREATE TABLE IF NOT EXISTS attempts (
			attempt_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id),
			number INTEGER NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('implementation','resume','review')),
			status TEXT NOT NULL, codex_session_id TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL DEFAULT '', exit_code INTEGER NOT NULL DEFAULT 0,
			stdout_path TEXT NOT NULL DEFAULT '', stderr_path TEXT NOT NULL DEFAULT '',
			outcome_path TEXT NOT NULL DEFAULT '', outcome_hash TEXT NOT NULL DEFAULT '',
			artifact_dir TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', UNIQUE(run_id, number),
			UNIQUE(artifact_dir))`,
	`CREATE TABLE IF NOT EXISTS verifications (
			verification_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id),
			attempt_id INTEGER, verifier_id TEXT NOT NULL, phase TEXT NOT NULL, verified_head TEXT NOT NULL,
			exit_code INTEGER NOT NULL, stdout_path TEXT NOT NULL, stderr_path TEXT NOT NULL,
			evidence_path TEXT NOT NULL, evidence_hash TEXT NOT NULL, created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS reviews (
			review_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id),
			attempt_id INTEGER NOT NULL REFERENCES attempts(attempt_id), review_session_id TEXT NOT NULL,
			reviewed_head TEXT NOT NULL, verdict TEXT NOT NULL, outcome_path TEXT NOT NULL,
			outcome_hash TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(run_id, reviewed_head))`,
	`CREATE TABLE IF NOT EXISTS owned_resources (
			resource_id INTEGER PRIMARY KEY AUTOINCREMENT, resource_kind TEXT NOT NULL, resource_name TEXT NOT NULL,
			owning_run TEXT NOT NULL REFERENCES runs(run_id), creation_evidence TEXT NOT NULL,
			ownership_status TEXT NOT NULL, created_at TEXT NOT NULL,
			UNIQUE(resource_kind, resource_name))`,
}

func (s *Store) CreateRun(ctx context.Context, input application.CreateRunInput) (application.Run, bool, error) {
	run := input.Run
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Run{}, false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(run_id,issue_id,idempotency_key,source_revision,raw_issue_json,raw_issue_hash,
		normalized_task_json,task_hash,repository,repository_config_json,base_branch,working_branch,worktree_path,artifact_root,current_state,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.IssueID, run.IdempotencyKey, run.SourceRevision, run.RawIssueJSON,
		run.RawIssueHash, run.NormalizedTaskJSON, run.TaskHash, run.Repository, run.RepositoryConfigJSON, run.BaseBranch, run.WorkingBranch,
		run.WorktreePath, run.ArtifactRoot, domain.StateReceived, formatTime(now), formatTime(now))
	if err != nil {
		_ = tx.Rollback()
		existing, getErr := s.getByIdempotency(ctx, run.IdempotencyKey)
		if getErr == nil {
			if existing.TaskHash != run.TaskHash || existing.SourceRevision != run.SourceRevision {
				return application.Run{}, false, errors.New("idempotency key conflicts with a different task snapshot")
			}
			return existing, false, nil
		}
		return application.Run{}, false, fmt.Errorf("create run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at)
		VALUES(?,1,'',?,'run created','task snapshot','',?)`, run.ID, domain.StateReceived, formatTime(now)); err != nil {
		return application.Run{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.Run{}, false, err
	}
	created, err := s.GetRun(ctx, run.ID)
	return created, true, err
}

func (s *Store) GetRun(ctx context.Context, id string) (application.Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, id))
}

func (s *Store) getByIdempotency(ctx context.Context, key string) (application.Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE idempotency_key=?`, key))
}

const runSelect = `SELECT run_id,issue_id,idempotency_key,source_revision,raw_issue_json,raw_issue_hash,
	normalized_task_json,task_hash,repository,repository_config_json,base_branch,working_branch,base_sha,worktree_path,artifact_root,
	current_state,candidate_head,implementation_session_id,last_error,created_at,updated_at FROM runs`

type rowScanner interface{ Scan(...any) error }

func scanRun(row rowScanner) (application.Run, error) {
	var run application.Run
	var state, created, updated string
	err := row.Scan(&run.ID, &run.IssueID, &run.IdempotencyKey, &run.SourceRevision, &run.RawIssueJSON, &run.RawIssueHash,
		&run.NormalizedTaskJSON, &run.TaskHash, &run.Repository, &run.RepositoryConfigJSON, &run.BaseBranch, &run.WorkingBranch, &run.BaseSHA, &run.WorktreePath,
		&run.ArtifactRoot, &state, &run.CandidateHead, &run.ImplementationSession, &run.LastError, &created, &updated)
	if err != nil {
		return run, err
	}
	run.State = domain.State(state)
	run.CreatedAt = parseTime(created)
	run.UpdatedAt = parseTime(updated)
	return run, nil
}

func (s *Store) Transition(ctx context.Context, id string, expected, next domain.State, reason, evidence, head string) error {
	if err := domain.ValidateTransition(expected, next); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT current_state FROM runs WHERE run_id=?`, id).Scan(&current); err != nil {
		return err
	}
	if domain.State(current) == next {
		return nil
	}
	if domain.State(current) != expected {
		return fmt.Errorf("state compare failed: current=%s expected=%s", current, expected)
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, id).Scan(&sequence); err != nil {
		return err
	}
	now := nowText()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,updated_at=?,last_error='' WHERE run_id=? AND current_state=?`, next, now, id, expected)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("state compare update lost")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions VALUES(?,?,?,?,?,?,?,?)`, id, sequence, expected, next, reason, evidence, head, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetWorkspace(ctx context.Context, id, baseSHA, path string) error {
	return execOne(ctx, s.db, `UPDATE runs SET base_sha=?,worktree_path=?,updated_at=? WHERE run_id=?`, baseSHA, path, nowText(), id)
}
func (s *Store) SetImplementationSession(ctx context.Context, id, session string) error {
	return execOne(ctx, s.db, `UPDATE runs SET implementation_session_id=?,updated_at=? WHERE run_id=?`, session, nowText(), id)
}
func (s *Store) SetCandidateHead(ctx context.Context, id, head string) error {
	return execOne(ctx, s.db, `UPDATE runs SET candidate_head=?,updated_at=? WHERE run_id=?`, head, nowText(), id)
}
func (s *Store) SetLastError(ctx context.Context, id, message string) error {
	return execOne(ctx, s.db, `UPDATE runs SET last_error=?,updated_at=? WHERE run_id=?`, message, nowText(), id)
}

func (s *Store) BeginAttempt(ctx context.Context, runID, kind, artifactDir string) (application.Attempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.Attempt{}, err
	}
	defer tx.Rollback()
	var number int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0)+1 FROM attempts WHERE run_id=?`, runID).Scan(&number); err != nil {
		return application.Attempt{}, err
	}
	now := nowText()
	result, err := tx.ExecContext(ctx, `INSERT INTO attempts(run_id,number,kind,status,started_at,artifact_dir) VALUES(?,?,?,'started',?,?)`, runID, number, kind, now, artifactDir)
	if err != nil {
		return application.Attempt{}, err
	}
	id, _ := result.LastInsertId()
	if err := tx.Commit(); err != nil {
		return application.Attempt{}, err
	}
	return application.Attempt{ID: id, RunID: runID, Number: number, Kind: kind, Status: "started", StartedAt: parseTime(now), ArtifactDir: artifactDir}, nil
}

func (s *Store) FinishAttempt(ctx context.Context, attempt application.Attempt) error {
	return execOne(ctx, s.db, `UPDATE attempts SET status=?,codex_session_id=?,finished_at=?,exit_code=?,stdout_path=?,stderr_path=?,outcome_path=?,outcome_hash=?,error_category=? WHERE attempt_id=?`,
		attempt.Status, attempt.SessionID, formatTime(attempt.FinishedAt), attempt.ExitCode, attempt.StdoutPath, attempt.StderrPath, attempt.OutcomePath, attempt.OutcomeHash, attempt.ErrorCategory, attempt.ID)
}

func (s *Store) SaveVerification(ctx context.Context, record application.VerificationRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO verifications(run_id,attempt_id,verifier_id,phase,verified_head,exit_code,stdout_path,stderr_path,evidence_path,evidence_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		record.RunID, record.AttemptID, record.VerifierID, record.Phase, record.VerifiedHead, record.ExitCode, record.StdoutPath, record.StderrPath, record.EvidencePath, record.EvidenceHash, nowText())
	return err
}
func (s *Store) SaveReview(ctx context.Context, record application.ReviewRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO reviews(run_id,attempt_id,review_session_id,reviewed_head,verdict,outcome_path,outcome_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		record.RunID, record.AttemptID, record.SessionID, record.ReviewedHead, record.Verdict, record.OutcomePath, record.OutcomeHash, nowText())
	return err
}
func (s *Store) AddOwnedResource(ctx context.Context, record application.OwnedResource) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO owned_resources(resource_kind,resource_name,owning_run,creation_evidence,ownership_status,created_at) VALUES(?,?,?,?,?,?)`,
		record.Kind, record.Name, record.RunID, record.CreationEvidence, record.Status, nowText())
	if err == nil {
		return nil
	}
	var owner string
	if queryErr := s.db.QueryRowContext(ctx, `SELECT owning_run FROM owned_resources WHERE resource_kind=? AND resource_name=?`, record.Kind, record.Name).Scan(&owner); queryErr != nil {
		return err
	}
	if owner != record.RunID {
		return fmt.Errorf("resource %s %s is owned by active run %s", record.Kind, record.Name, owner)
	}
	_, updateErr := s.db.ExecContext(ctx, `UPDATE owned_resources SET creation_evidence=?,ownership_status=? WHERE resource_kind=? AND resource_name=? AND owning_run=?`, record.CreationEvidence, record.Status, record.Kind, record.Name, record.RunID)
	return updateErr
}

func (s *Store) Inspect(ctx context.Context, id string) (application.RunInspection, error) {
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return application.RunInspection{}, err
	}
	inspection := application.RunInspection{Run: run}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at FROM transitions WHERE run_id=? ORDER BY sequence`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.Transition
		var from, to, created string
		if err := rows.Scan(&v.Sequence, &from, &to, &v.Reason, &v.EvidenceReference, &v.BoundHead, &created); err != nil {
			rows.Close()
			return inspection, err
		}
		v.From = domain.State(from)
		v.To = domain.State(to)
		v.CreatedAt = parseTime(created)
		inspection.Timeline = append(inspection.Timeline, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT attempt_id,run_id,number,kind,status,codex_session_id,started_at,finished_at,exit_code,stdout_path,stderr_path,outcome_path,outcome_hash,artifact_dir,error_category FROM attempts WHERE run_id=? ORDER BY number`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.Attempt
		var started, finished string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Number, &v.Kind, &v.Status, &v.SessionID, &started, &finished, &v.ExitCode, &v.StdoutPath, &v.StderrPath, &v.OutcomePath, &v.OutcomeHash, &v.ArtifactDir, &v.ErrorCategory); err != nil {
			rows.Close()
			return inspection, err
		}
		v.StartedAt = parseTime(started)
		v.FinishedAt = parseTime(finished)
		inspection.Attempts = append(inspection.Attempts, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT verification_id,run_id,attempt_id,verifier_id,phase,verified_head,exit_code,stdout_path,stderr_path,evidence_path,evidence_hash,created_at FROM verifications WHERE run_id=? ORDER BY verification_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.VerificationRecord
		var created string
		if err := rows.Scan(&v.ID, &v.RunID, &v.AttemptID, &v.VerifierID, &v.Phase, &v.VerifiedHead, &v.ExitCode, &v.StdoutPath, &v.StderrPath, &v.EvidencePath, &v.EvidenceHash, &created); err != nil {
			rows.Close()
			return inspection, err
		}
		v.CreatedAt = parseTime(created)
		inspection.Verifications = append(inspection.Verifications, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT review_id,run_id,attempt_id,review_session_id,reviewed_head,verdict,outcome_path,outcome_hash,created_at FROM reviews WHERE run_id=? ORDER BY review_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.ReviewRecord
		var created string
		if err := rows.Scan(&v.ID, &v.RunID, &v.AttemptID, &v.SessionID, &v.ReviewedHead, &v.Verdict, &v.OutcomePath, &v.OutcomeHash, &created); err != nil {
			rows.Close()
			return inspection, err
		}
		v.CreatedAt = parseTime(created)
		inspection.Reviews = append(inspection.Reviews, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT resource_id,owning_run,resource_kind,resource_name,creation_evidence,ownership_status,created_at FROM owned_resources WHERE owning_run=? ORDER BY resource_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.OwnedResource
		var created string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Kind, &v.Name, &v.CreationEvidence, &v.Status, &created); err != nil {
			rows.Close()
			return inspection, err
		}
		v.CreatedAt = parseTime(created)
		inspection.Resources = append(inspection.Resources, v)
	}
	rows.Close()
	return inspection, nil
}

func execOne(ctx context.Context, db *sql.DB, query string, args ...any) error {
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func nowText() string { return formatTime(time.Now().UTC()) }
func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return parsed
}
