package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const schemaVersion = 5

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = absolute
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SQLite database path must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
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

func sqliteDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
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
		case 2:
			statements = migrationV2
		case 3:
			statements = migrationV3
		case 4:
			statements = migrationV4
		case 5:
			statements = migrationV5
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

var migrationV2 = []string{
	`ALTER TABLE runs ADD COLUMN lease_owner TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE runs ADD COLUMN lease_expires_unix INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE attempts ADD COLUMN stdout_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN stderr_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN stdout_size INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE attempts ADD COLUMN stderr_size INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE verifications ADD COLUMN stdout_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE verifications ADD COLUMN stderr_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE verifications ADD COLUMN stdout_size INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE verifications ADD COLUMN stderr_size INTEGER NOT NULL DEFAULT 0`,
}

var migrationV3 = []string{
	`ALTER TABLE reviews RENAME TO reviews_v2`,
	`CREATE TABLE reviews (review_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), attempt_id INTEGER NOT NULL REFERENCES attempts(attempt_id), review_session_id TEXT NOT NULL, reviewed_head TEXT NOT NULL, verdict TEXT NOT NULL, outcome_path TEXT NOT NULL, outcome_hash TEXT NOT NULL, created_at TEXT NOT NULL)`,
	`INSERT INTO reviews(review_id,run_id,attempt_id,review_session_id,reviewed_head,verdict,outcome_path,outcome_hash,created_at) SELECT review_id,run_id,attempt_id,review_session_id,reviewed_head,verdict,outcome_path,outcome_hash,created_at FROM reviews_v2`,
	`DROP TABLE reviews_v2`,
}

var migrationV4 = []string{
	`ALTER TABLE runs ADD COLUMN implementation_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE runs ADD COLUMN review_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''`,
}

var migrationV5 = []string{
	`CREATE TABLE side_effects (side_effect_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), kind TEXT NOT NULL, idempotency_key TEXT NOT NULL, intent_json TEXT NOT NULL, status TEXT NOT NULL, result_json TEXT NOT NULL DEFAULT '', stdout_path TEXT NOT NULL DEFAULT '', stderr_path TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL, created_at TEXT NOT NULL, observed_at TEXT NOT NULL DEFAULT '', UNIQUE(run_id,kind,idempotency_key))`,
	`CREATE TABLE pull_requests (run_id TEXT PRIMARY KEY REFERENCES runs(run_id), number INTEGER NOT NULL, url TEXT NOT NULL, node_id TEXT NOT NULL, head_branch TEXT NOT NULL, base_branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, body_digest TEXT NOT NULL, ownership_key TEXT NOT NULL, state TEXT NOT NULL, merged INTEGER NOT NULL DEFAULT 0, merge_sha TEXT NOT NULL DEFAULT '', merged_at TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE poll_observations (observation_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), pr_number INTEGER NOT NULL, attempt INTEGER NOT NULL, head_sha TEXT NOT NULL, status TEXT NOT NULL, snapshot_json TEXT NOT NULL, observed_at TEXT NOT NULL)`,
	`CREATE TABLE review_findings (finding_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), source_id TEXT NOT NULL, thread_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, file TEXT NOT NULL DEFAULT '', line INTEGER NOT NULL DEFAULT 0, severity TEXT NOT NULL, body_digest TEXT NOT NULL, resolved INTEGER NOT NULL, outdated INTEGER NOT NULL, head_sha TEXT NOT NULL, observed_at TEXT NOT NULL, UNIQUE(run_id,source,source_id,head_sha))`,
	`CREATE TABLE human_approvals (run_id TEXT PRIMARY KEY REFERENCES runs(run_id), pr_number INTEGER NOT NULL, approver TEXT NOT NULL, source TEXT NOT NULL, approved_sha TEXT NOT NULL, ci_status TEXT NOT NULL, coderabbit_status TEXT NOT NULL, internal_review_sha TEXT NOT NULL, approved_at TEXT NOT NULL)`,
	`CREATE TABLE merge_results (run_id TEXT PRIMARY KEY REFERENCES runs(run_id), pr_number INTEGER NOT NULL, pre_merge_head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, merge_method TEXT NOT NULL CHECK(merge_method='squash'), merge_sha TEXT NOT NULL, merged_at TEXT NOT NULL)`,
	`CREATE TABLE cleanup_results (cleanup_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), resource_kind TEXT NOT NULL, resource_name TEXT NOT NULL, status TEXT NOT NULL, last_error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL, UNIQUE(run_id,resource_kind,resource_name))`,
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
		normalized_task_json,task_hash,repository,repository_config_json,base_branch,working_branch,worktree_path,artifact_root,current_state,implementation_model,review_model,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.IssueID, run.IdempotencyKey, run.SourceRevision, run.RawIssueJSON,
		run.RawIssueHash, run.NormalizedTaskJSON, run.TaskHash, run.Repository, run.RepositoryConfigJSON, run.BaseBranch, run.WorkingBranch,
		run.WorktreePath, run.ArtifactRoot, domain.StateReceived, run.ImplementationModel, run.ReviewModel, formatTime(now), formatTime(now))
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
	current_state,candidate_head,implementation_session_id,implementation_model,review_model,last_error,lease_owner,lease_expires_unix,created_at,updated_at FROM runs`

type rowScanner interface{ Scan(...any) error }

func scanRun(row rowScanner) (application.Run, error) {
	var run application.Run
	var state, created, updated string
	var leaseExpires int64
	err := row.Scan(&run.ID, &run.IssueID, &run.IdempotencyKey, &run.SourceRevision, &run.RawIssueJSON, &run.RawIssueHash,
		&run.NormalizedTaskJSON, &run.TaskHash, &run.Repository, &run.RepositoryConfigJSON, &run.BaseBranch, &run.WorkingBranch, &run.BaseSHA, &run.WorktreePath,
		&run.ArtifactRoot, &state, &run.CandidateHead, &run.ImplementationSession, &run.ImplementationModel, &run.ReviewModel, &run.LastError, &run.LeaseOwner, &leaseExpires, &created, &updated)
	if err != nil {
		return run, err
	}
	run.State = domain.State(state)
	run.CreatedAt = parseTime(created)
	run.UpdatedAt = parseTime(updated)
	if leaseExpires > 0 {
		run.LeaseExpiresAt = time.Unix(0, leaseExpires).UTC()
	}
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
func (s *Store) BeginRepair(ctx context.Context, id, oldHead string) error {
	if strings.TrimSpace(oldHead) == "" {
		return errors.New("repair base head must not be blank")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, candidate string
	if err := tx.QueryRowContext(ctx, `SELECT current_state,candidate_head FROM runs WHERE run_id=?`, id).Scan(&current, &candidate); err != nil {
		return err
	}
	if domain.State(current) != domain.StateRepairing || candidate != oldHead {
		return errors.New("repair state or candidate compare failed")
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, id).Scan(&sequence); err != nil {
		return err
	}
	now := nowText()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,candidate_head='',updated_at=? WHERE run_id=? AND current_state=? AND candidate_head=?`, domain.StateExecuting, now, id, domain.StateRepairing, oldHead)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("repair compare update lost")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions VALUES(?,?,?,?,?,?,?,?)`, id, sequence, domain.StateRepairing, domain.StateExecuting, "begin normalized GitHub finding repair", "controller-normalized finding digests", oldHead, now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SetLastError(ctx context.Context, id, message string) error {
	return execOne(ctx, s.db, `UPDATE runs SET last_error=?,updated_at=? WHERE run_id=?`, message, nowText(), id)
}

func (s *Store) AcquireLease(ctx context.Context, id, owner string, expires time.Time) (bool, error) {
	if strings.TrimSpace(owner) == "" {
		return false, errors.New("lease owner must not be blank")
	}
	now := time.Now().UTC().UnixNano()
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET lease_owner=?,lease_expires_unix=?,updated_at=? WHERE run_id=? AND (lease_owner='' OR lease_expires_unix<=? OR lease_owner=?)`, owner, expires.UTC().UnixNano(), nowText(), id, now, owner)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}
func (s *Store) RenewLease(ctx context.Context, id, owner string, expires time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET lease_expires_unix=?,updated_at=? WHERE run_id=? AND lease_owner=? AND lease_expires_unix>?`, expires.UTC().UnixNano(), nowText(), id, owner, time.Now().UTC().UnixNano())
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}
func (s *Store) ReleaseLease(ctx context.Context, id, owner string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET lease_owner='',lease_expires_unix=0,updated_at=? WHERE run_id=? AND lease_owner=?`, nowText(), id, owner)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return errors.New("lease release owner mismatch")
	}
	return nil
}

func (s *Store) BeginAttempt(ctx context.Context, runID, kind, requestedModel, artifactDir string) (application.Attempt, error) {
	if strings.TrimSpace(requestedModel) == "" {
		return application.Attempt{}, errors.New("attempt requested model must not be blank")
	}
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
	result, err := tx.ExecContext(ctx, `INSERT INTO attempts(run_id,number,kind,status,requested_model,started_at,artifact_dir) VALUES(?,?,?,'started',?,?,?)`, runID, number, kind, requestedModel, now, artifactDir)
	if err != nil {
		return application.Attempt{}, err
	}
	id, _ := result.LastInsertId()
	if err := tx.Commit(); err != nil {
		return application.Attempt{}, err
	}
	return application.Attempt{ID: id, RunID: runID, Number: number, Kind: kind, Status: "started", RequestedModel: requestedModel, StartedAt: parseTime(now), ArtifactDir: artifactDir}, nil
}

func (s *Store) FinishAttempt(ctx context.Context, attempt application.Attempt) error {
	return execOne(ctx, s.db, `UPDATE attempts SET status=?,codex_session_id=?,finished_at=?,exit_code=?,stdout_path=?,stderr_path=?,stdout_hash=?,stderr_hash=?,stdout_size=?,stderr_size=?,outcome_path=?,outcome_hash=?,error_category=? WHERE attempt_id=?`,
		attempt.Status, attempt.SessionID, formatTime(attempt.FinishedAt), attempt.ExitCode, attempt.StdoutPath, attempt.StderrPath, attempt.StdoutHash, attempt.StderrHash, attempt.StdoutSize, attempt.StderrSize, attempt.OutcomePath, attempt.OutcomeHash, attempt.ErrorCategory, attempt.ID)
}

func (s *Store) SaveVerification(ctx context.Context, record application.VerificationRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO verifications(run_id,attempt_id,verifier_id,phase,verified_head,exit_code,stdout_path,stderr_path,stdout_hash,stderr_hash,stdout_size,stderr_size,evidence_path,evidence_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		record.RunID, record.AttemptID, record.VerifierID, record.Phase, record.VerifiedHead, record.ExitCode, record.StdoutPath, record.StderrPath, record.StdoutHash, record.StderrHash, record.StdoutSize, record.StderrSize, record.EvidencePath, record.EvidenceHash, nowText())
	return err
}
func (s *Store) SaveReview(ctx context.Context, record application.ReviewRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO reviews(run_id,attempt_id,review_session_id,reviewed_head,verdict,outcome_path,outcome_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`,
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

func (s *Store) BeginSideEffect(ctx context.Context, record application.SideEffectRecord) (application.SideEffectRecord, bool, error) {
	if strings.TrimSpace(record.IdempotencyKey) == "" || strings.TrimSpace(record.IntentJSON) == "" {
		return record, false, errors.New("side-effect intent and idempotency key are required")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO side_effects(run_id,kind,idempotency_key,intent_json,status,attempt,created_at) VALUES(?,?,?,?,?,?,?)`, record.RunID, record.Kind, record.IdempotencyKey, record.IntentJSON, "intent", record.Attempt, nowText())
	if err == nil {
		record.ID, _ = result.LastInsertId()
		record.Status = "intent"
		return record, true, nil
	}
	requested := record
	row := s.db.QueryRowContext(ctx, `SELECT side_effect_id,run_id,kind,idempotency_key,intent_json,status,result_json,stdout_path,stderr_path,attempt,created_at,observed_at FROM side_effects WHERE run_id=? AND kind=? AND idempotency_key=?`, record.RunID, record.Kind, record.IdempotencyKey)
	var created, observed string
	if scanErr := row.Scan(&record.ID, &record.RunID, &record.Kind, &record.IdempotencyKey, &record.IntentJSON, &record.Status, &record.ResultJSON, &record.StdoutPath, &record.StderrPath, &record.Attempt, &created, &observed); scanErr != nil {
		return record, false, err
	}
	record.CreatedAt, record.ObservedAt = parseTime(created), parseTime(observed)
	if record.RunID != requested.RunID || record.Kind != requested.Kind || record.IdempotencyKey != requested.IdempotencyKey || record.IntentJSON != requested.IntentJSON || record.Attempt != requested.Attempt {
		return record, false, errors.New("conflicting immutable side-effect intent")
	}
	return record, false, nil
}

func (s *Store) FinishSideEffect(ctx context.Context, record application.SideEffectRecord) error {
	if record.Status != "observed" && record.Status != "failed" {
		return errors.New("side-effect result status must be observed or failed")
	}
	return execOne(ctx, s.db, `UPDATE side_effects SET status=?,result_json=?,stdout_path=?,stderr_path=?,observed_at=? WHERE side_effect_id=? AND status IN ('intent','failed')`, record.Status, record.ResultJSON, record.StdoutPath, record.StderrPath, formatTime(record.ObservedAt), record.ID)
}

func (s *Store) SavePullRequest(ctx context.Context, runID string, pr domain.PullRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pull_requests(run_id,number,url,node_id,head_branch,base_branch,head_sha,base_sha,body_digest,ownership_key,state,merged,merge_sha,merged_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, pr.Number, pr.URL, pr.NodeID, pr.HeadBranch, pr.BaseBranch, pr.HeadSHA, pr.BaseSHA, pr.BodyDigest, pr.OwnershipKey, pr.State, pr.Merged, pr.MergeSHA, formatTime(pr.MergedAt))
	if err == nil {
		return nil
	}
	var existing domain.PullRequest
	var merged int
	var mergedAt string
	if scanErr := s.db.QueryRowContext(ctx, `SELECT number,url,node_id,head_branch,base_branch,head_sha,base_sha,body_digest,ownership_key,state,merged,merge_sha,merged_at FROM pull_requests WHERE run_id=?`, runID).Scan(&existing.Number, &existing.URL, &existing.NodeID, &existing.HeadBranch, &existing.BaseBranch, &existing.HeadSHA, &existing.BaseSHA, &existing.BodyDigest, &existing.OwnershipKey, &existing.State, &merged, &existing.MergeSHA, &mergedAt); scanErr != nil {
		return err
	}
	existing.Merged = merged != 0
	existing.MergedAt = parseTime(mergedAt)
	if existing.Number != pr.Number || existing.NodeID != pr.NodeID || existing.HeadBranch != pr.HeadBranch || existing.BaseBranch != pr.BaseBranch || existing.HeadSHA != pr.HeadSHA || existing.BaseSHA != pr.BaseSHA || existing.BodyDigest != pr.BodyDigest || existing.OwnershipKey != pr.OwnershipKey {
		return errors.New("conflicting immutable pull request evidence")
	}
	return execOne(ctx, s.db, `UPDATE pull_requests SET url=?,state=?,merged=?,merge_sha=?,merged_at=? WHERE run_id=?`, pr.URL, pr.State, pr.Merged, pr.MergeSHA, formatTime(pr.MergedAt), runID)
}

func (s *Store) SavePollObservation(ctx context.Context, record application.PollObservation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO poll_observations(run_id,pr_number,attempt,head_sha,status,snapshot_json,observed_at) VALUES(?,?,?,?,?,?,?)`, record.RunID, record.PRNumber, record.Attempt, record.HeadSHA, record.Status, record.SnapshotJSON, formatTime(record.ObservedAt))
	return err
}

func (s *Store) SaveFinding(ctx context.Context, record application.FindingRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO review_findings(run_id,source_id,thread_id,source,file,line,severity,body_digest,resolved,outdated,head_sha,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,source,source_id,head_sha) DO UPDATE SET thread_id=excluded.thread_id,file=excluded.file,line=excluded.line,severity=excluded.severity,body_digest=excluded.body_digest,resolved=excluded.resolved,outdated=excluded.outdated,observed_at=excluded.observed_at`, record.RunID, record.SourceID, record.ThreadID, record.Source, record.File, record.Line, record.Severity, record.BodyDigest, record.Resolved, record.Outdated, record.HeadSHA, formatTime(record.ObservedAt))
	return err
}

func (s *Store) SaveHumanApproval(ctx context.Context, runID string, approval domain.HumanApproval) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO human_approvals(run_id,pr_number,approver,source,approved_sha,ci_status,coderabbit_status,internal_review_sha,approved_at) VALUES(?,?,?,?,?,?,?,?,?)`, runID, approval.PRNumber, approval.Approver, approval.Source, approval.ApprovedSHA, approval.CIStatus, approval.CodeRabbit, approval.ReviewSHA, formatTime(approval.ApprovedAt))
	if err == nil {
		return nil
	}
	var existing domain.HumanApproval
	var approvedAt string
	if scanErr := s.db.QueryRowContext(ctx, `SELECT pr_number,approver,source,approved_sha,ci_status,coderabbit_status,internal_review_sha,approved_at FROM human_approvals WHERE run_id=?`, runID).Scan(&existing.PRNumber, &existing.Approver, &existing.Source, &existing.ApprovedSHA, &existing.CIStatus, &existing.CodeRabbit, &existing.ReviewSHA, &approvedAt); scanErr != nil {
		return err
	}
	existing.ApprovedAt = parseTime(approvedAt)
	if existing != approval {
		return errors.New("conflicting immutable human approval evidence")
	}
	return nil
}

func (s *Store) SaveMerge(ctx context.Context, record application.MergeRecord) error {
	if record.Method != "squash" {
		return errors.New("only squash merge evidence is accepted")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO merge_results(run_id,pr_number,pre_merge_head_sha,base_sha,merge_method,merge_sha,merged_at) VALUES(?,?,?,?,?,?,?)`, record.RunID, record.PRNumber, record.PreMergeSHA, record.BaseSHA, record.Method, record.MergeSHA, formatTime(record.MergedAt))
	if err == nil {
		return nil
	}
	var existing application.MergeRecord
	var mergedAt string
	if scanErr := s.db.QueryRowContext(ctx, `SELECT run_id,pr_number,pre_merge_head_sha,base_sha,merge_method,merge_sha,merged_at FROM merge_results WHERE run_id=?`, record.RunID).Scan(&existing.RunID, &existing.PRNumber, &existing.PreMergeSHA, &existing.BaseSHA, &existing.Method, &existing.MergeSHA, &mergedAt); scanErr != nil {
		return err
	}
	existing.MergedAt = parseTime(mergedAt)
	if existing != record {
		return errors.New("conflicting immutable merge evidence")
	}
	return nil
}

func (s *Store) UpsertCleanup(ctx context.Context, record application.CleanupRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cleanup_results(run_id,resource_kind,resource_name,status,last_error,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,resource_kind,resource_name) DO UPDATE SET status=excluded.status,last_error=excluded.last_error,updated_at=excluded.updated_at`, record.RunID, record.Kind, record.Name, record.Status, record.LastError, nowText())
	return err
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
	rows, err = s.db.QueryContext(ctx, `SELECT attempt_id,run_id,number,kind,status,codex_session_id,requested_model,started_at,finished_at,exit_code,stdout_path,stderr_path,stdout_hash,stderr_hash,stdout_size,stderr_size,outcome_path,outcome_hash,artifact_dir,error_category FROM attempts WHERE run_id=? ORDER BY number`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.Attempt
		var started, finished string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Number, &v.Kind, &v.Status, &v.SessionID, &v.RequestedModel, &started, &finished, &v.ExitCode, &v.StdoutPath, &v.StderrPath, &v.StdoutHash, &v.StderrHash, &v.StdoutSize, &v.StderrSize, &v.OutcomePath, &v.OutcomeHash, &v.ArtifactDir, &v.ErrorCategory); err != nil {
			rows.Close()
			return inspection, err
		}
		v.StartedAt = parseTime(started)
		v.FinishedAt = parseTime(finished)
		inspection.Attempts = append(inspection.Attempts, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT verification_id,run_id,attempt_id,verifier_id,phase,verified_head,exit_code,stdout_path,stderr_path,stdout_hash,stderr_hash,stdout_size,stderr_size,evidence_path,evidence_hash,created_at FROM verifications WHERE run_id=? ORDER BY verification_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.VerificationRecord
		var created string
		if err := rows.Scan(&v.ID, &v.RunID, &v.AttemptID, &v.VerifierID, &v.Phase, &v.VerifiedHead, &v.ExitCode, &v.StdoutPath, &v.StderrPath, &v.StdoutHash, &v.StderrHash, &v.StdoutSize, &v.StderrSize, &v.EvidencePath, &v.EvidenceHash, &created); err != nil {
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
	rows, err = s.db.QueryContext(ctx, `SELECT side_effect_id,run_id,kind,idempotency_key,intent_json,status,result_json,stdout_path,stderr_path,attempt,created_at,observed_at FROM side_effects WHERE run_id=? ORDER BY side_effect_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.SideEffectRecord
		var created, observed string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Kind, &v.IdempotencyKey, &v.IntentJSON, &v.Status, &v.ResultJSON, &v.StdoutPath, &v.StderrPath, &v.Attempt, &created, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		v.CreatedAt = parseTime(created)
		v.ObservedAt = parseTime(observed)
		inspection.SideEffects = append(inspection.SideEffects, v)
	}
	rows.Close()
	var pr domain.PullRequest
	var merged int
	var mergedAt string
	if err := s.db.QueryRowContext(ctx, `SELECT number,url,node_id,head_branch,base_branch,head_sha,base_sha,body_digest,ownership_key,state,merged,merge_sha,merged_at FROM pull_requests WHERE run_id=?`, id).Scan(&pr.Number, &pr.URL, &pr.NodeID, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.BaseSHA, &pr.BodyDigest, &pr.OwnershipKey, &pr.State, &merged, &pr.MergeSHA, &mergedAt); err == nil {
		pr.Merged = merged != 0
		pr.MergedAt = parseTime(mergedAt)
		inspection.PullRequest = &pr
	} else if !errors.Is(err, sql.ErrNoRows) {
		return inspection, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT observation_id,run_id,pr_number,attempt,head_sha,status,snapshot_json,observed_at FROM poll_observations WHERE run_id=? ORDER BY observation_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.PollObservation
		var observed string
		if err := rows.Scan(&v.ID, &v.RunID, &v.PRNumber, &v.Attempt, &v.HeadSHA, &v.Status, &v.SnapshotJSON, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		v.ObservedAt = parseTime(observed)
		inspection.Polls = append(inspection.Polls, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT finding_id,run_id,source_id,thread_id,source,file,line,severity,body_digest,resolved,outdated,head_sha,observed_at FROM review_findings WHERE run_id=? ORDER BY finding_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.FindingRecord
		var resolved, outdated int
		var observed string
		if err := rows.Scan(&v.ID, &v.RunID, &v.SourceID, &v.ThreadID, &v.Source, &v.File, &v.Line, &v.Severity, &v.BodyDigest, &resolved, &outdated, &v.HeadSHA, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		v.Resolved = resolved != 0
		v.Outdated = outdated != 0
		v.ObservedAt = parseTime(observed)
		inspection.Findings = append(inspection.Findings, v)
	}
	rows.Close()
	var approval domain.HumanApproval
	var approvedAt string
	if err := s.db.QueryRowContext(ctx, `SELECT pr_number,approver,source,approved_sha,ci_status,coderabbit_status,internal_review_sha,approved_at FROM human_approvals WHERE run_id=?`, id).Scan(&approval.PRNumber, &approval.Approver, &approval.Source, &approval.ApprovedSHA, &approval.CIStatus, &approval.CodeRabbit, &approval.ReviewSHA, &approvedAt); err == nil {
		approval.ApprovedAt = parseTime(approvedAt)
		inspection.Approval = &approval
	} else if !errors.Is(err, sql.ErrNoRows) {
		return inspection, err
	}
	var merge application.MergeRecord
	var mergeAt string
	if err := s.db.QueryRowContext(ctx, `SELECT run_id,pr_number,pre_merge_head_sha,base_sha,merge_method,merge_sha,merged_at FROM merge_results WHERE run_id=?`, id).Scan(&merge.RunID, &merge.PRNumber, &merge.PreMergeSHA, &merge.BaseSHA, &merge.Method, &merge.MergeSHA, &mergeAt); err == nil {
		merge.MergedAt = parseTime(mergeAt)
		inspection.Merge = &merge
	} else if !errors.Is(err, sql.ErrNoRows) {
		return inspection, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT cleanup_id,run_id,resource_kind,resource_name,status,last_error,updated_at FROM cleanup_results WHERE run_id=? ORDER BY cleanup_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.CleanupRecord
		var updated string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Kind, &v.Name, &v.Status, &v.LastError, &updated); err != nil {
			rows.Close()
			return inspection, err
		}
		v.UpdatedAt = parseTime(updated)
		inspection.Cleanup = append(inspection.Cleanup, v)
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
