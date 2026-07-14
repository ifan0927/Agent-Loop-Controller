package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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

const schemaVersion = 16

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
		case 6:
			statements = migrationV6
		case 7:
			statements = migrationV7
		case 8:
			statements = migrationV8
		case 9:
			statements = migrationV9
		case 10:
			statements = migrationV10
		case 11:
			statements = migrationV11
		case 12:
			statements = migrationV12
		case 13:
			statements = migrationV13
		case 14:
			statements = migrationV14
		case 15:
			statements = migrationV15
		case 16:
			statements = migrationV16
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
	`CREATE TABLE review_findings (finding_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), source_id TEXT NOT NULL, thread_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, file TEXT NOT NULL DEFAULT '', line INTEGER NOT NULL DEFAULT 0, severity TEXT NOT NULL, body_digest TEXT NOT NULL, body_text TEXT NOT NULL, resolved INTEGER NOT NULL, outdated INTEGER NOT NULL, head_sha TEXT NOT NULL, observed_at TEXT NOT NULL, UNIQUE(run_id,source,source_id,head_sha))`,
	`CREATE TABLE human_approvals (approval_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), pr_number INTEGER NOT NULL, approver TEXT NOT NULL, source TEXT NOT NULL, approved_sha TEXT NOT NULL, ci_status TEXT NOT NULL, internal_review_sha TEXT NOT NULL, approved_at TEXT NOT NULL, UNIQUE(run_id,approved_sha))`,
	`CREATE TABLE merge_results (run_id TEXT PRIMARY KEY REFERENCES runs(run_id), pr_number INTEGER NOT NULL, pre_merge_head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, merge_method TEXT NOT NULL CHECK(merge_method='squash'), merge_sha TEXT NOT NULL, merged_at TEXT NOT NULL)`,
	`CREATE TABLE cleanup_results (cleanup_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), resource_kind TEXT NOT NULL, resource_name TEXT NOT NULL, status TEXT NOT NULL, last_error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL, UNIQUE(run_id,resource_kind,resource_name))`,
}

var migrationV6 = []string{
	`ALTER TABLE pull_requests ADD COLUMN database_id INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE github_installations (observation_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), app_id INTEGER NOT NULL, installation_id INTEGER NOT NULL, repository_id INTEGER NOT NULL, repository_node_id TEXT NOT NULL, repository_owner TEXT NOT NULL, repository_name TEXT NOT NULL, token_expires_at TEXT NOT NULL, permissions_digest TEXT NOT NULL, observed_at TEXT NOT NULL, UNIQUE(run_id,app_id,installation_id,repository_id,token_expires_at,permissions_digest))`,
	`CREATE TABLE github_request_observations (observation_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), operation_name TEXT NOT NULL, endpoint_category TEXT NOT NULL, http_status INTEGER NOT NULL, request_id TEXT NOT NULL DEFAULT '', rate_limit_limit INTEGER NOT NULL DEFAULT 0, rate_limit_remaining INTEGER NOT NULL DEFAULT 0, rate_limit_reset TEXT NOT NULL DEFAULT '', response_digest TEXT NOT NULL, error_class TEXT NOT NULL DEFAULT '', installation_id INTEGER NOT NULL, repository_id INTEGER NOT NULL, repository_node_id TEXT NOT NULL, repository_owner TEXT NOT NULL, repository_name TEXT NOT NULL, observed_at TEXT NOT NULL)`,
	`CREATE TABLE github_read_evidence (evidence_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), head_sha TEXT NOT NULL, repository_id INTEGER NOT NULL, evidence_json TEXT NOT NULL, evidence_digest TEXT NOT NULL, observed_at TEXT NOT NULL, UNIQUE(run_id,head_sha,evidence_digest))`,
}

var migrationV7 = []string{
	`ALTER TABLE runs ADD COLUMN registry_version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE runs ADD COLUMN registry_digest TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE runs ADD COLUMN repository_binding_digest TEXT NOT NULL DEFAULT ''`,
}

var migrationV8 = []string{
	`ALTER TABLE runs ADD COLUMN profile_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE runs ADD COLUMN profile_snapshot_version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE runs ADD COLUMN profile_digest TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE runs ADD COLUMN profile_snapshot_json TEXT NOT NULL DEFAULT ''`,
}

var migrationV9 = []string{
	`ALTER TABLE human_approvals ADD COLUMN actor_database_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE human_approvals ADD COLUMN actor_node_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE human_approvals ADD COLUMN actor_login TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE human_approvals ADD COLUMN actor_type TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE human_approvals ADD COLUMN review_database_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE human_approvals ADD COLUMN review_node_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE human_approvals ADD COLUMN observed_at TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE human_approval_observations (observation_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), pr_number INTEGER NOT NULL, candidate_head TEXT NOT NULL, status TEXT NOT NULL, review_database_id INTEGER NOT NULL DEFAULT 0, review_node_id TEXT NOT NULL DEFAULT '', actor_database_id INTEGER NOT NULL DEFAULT 0, actor_node_id TEXT NOT NULL DEFAULT '', actor_login TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL DEFAULT '', review_head_sha TEXT NOT NULL DEFAULT '', source_at TEXT NOT NULL DEFAULT '', observed_at TEXT NOT NULL, evidence_digest TEXT NOT NULL)`,
}

var migrationV10 = []string{
	`CREATE TABLE linear_completion_observations (observation_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), merge_sha TEXT NOT NULL, linear_issue_id TEXT NOT NULL DEFAULT '', issue_identifier TEXT NOT NULL, source_revision TEXT NOT NULL DEFAULT '', state_id TEXT NOT NULL DEFAULT '', state_name TEXT NOT NULL DEFAULT '', state_type TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, error_class TEXT NOT NULL DEFAULT '', observed_at TEXT NOT NULL)`,
	`CREATE TABLE linear_request_observations (observation_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), operation_name TEXT NOT NULL, http_status INTEGER NOT NULL, request_id TEXT NOT NULL DEFAULT '', rate_limit_limit INTEGER NOT NULL DEFAULT 0, rate_limit_remaining INTEGER NOT NULL DEFAULT 0, rate_limit_reset TEXT NOT NULL DEFAULT '', response_digest TEXT NOT NULL DEFAULT '', error_class TEXT NOT NULL DEFAULT '', observed_at TEXT NOT NULL)`,
}

var migrationV11 = []string{
	`ALTER TABLE cleanup_results ADD COLUMN error_class TEXT NOT NULL DEFAULT ''`,
}

// migrationV12 removes the retired CodeRabbit evidence column. Rebuilding the
// table keeps existing human approvals valid while supporting databases created
// by the earlier controller policy.
var migrationV12 = []string{
	`ALTER TABLE human_approvals RENAME TO human_approvals_v12`,
	`CREATE TABLE human_approvals (approval_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), pr_number INTEGER NOT NULL, approver TEXT NOT NULL, source TEXT NOT NULL, approved_sha TEXT NOT NULL, ci_status TEXT NOT NULL, internal_review_sha TEXT NOT NULL, approved_at TEXT NOT NULL, actor_database_id INTEGER NOT NULL DEFAULT 0, actor_node_id TEXT NOT NULL DEFAULT '', actor_login TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL DEFAULT '', review_database_id INTEGER NOT NULL DEFAULT 0, review_node_id TEXT NOT NULL DEFAULT '', observed_at TEXT NOT NULL DEFAULT '', UNIQUE(run_id,approved_sha))`,
	`INSERT INTO human_approvals(approval_id,run_id,pr_number,approver,source,approved_sha,ci_status,internal_review_sha,approved_at,actor_database_id,actor_node_id,actor_login,actor_type,review_database_id,review_node_id,observed_at) SELECT approval_id,run_id,pr_number,approver,source,approved_sha,ci_status,internal_review_sha,approved_at,actor_database_id,actor_node_id,actor_login,actor_type,review_database_id,review_node_id,observed_at FROM human_approvals_v12`,
	`DROP TABLE human_approvals_v12`,
}

// migrationV13 adds a separate authority store for trusted inline feedback.
// It intentionally does not reuse review_findings, whose mutable prompt input
// cannot authorize a future GitHub side effect.
var migrationV13 = []string{
	`CREATE TABLE trusted_review_feedback (feedback_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), root_comment_node_id TEXT NOT NULL, pr_number INTEGER NOT NULL, pr_database_id INTEGER NOT NULL, pr_node_id TEXT NOT NULL, review_database_id INTEGER NOT NULL, review_node_id TEXT NOT NULL, thread_node_id TEXT NOT NULL, root_comment_database_id INTEGER NOT NULL, author_database_id INTEGER NOT NULL, author_node_id TEXT NOT NULL, author_login TEXT NOT NULL, author_type TEXT NOT NULL, original_review_head_sha TEXT NOT NULL, path TEXT NOT NULL DEFAULT '', line INTEGER, body_text TEXT NOT NULL, body_digest TEXT NOT NULL, source_at TEXT NOT NULL, observed_at TEXT NOT NULL, lifecycle TEXT NOT NULL CHECK(lifecycle IN ('observed','selected_for_repair','repair_verified','reply_pending','replied','resolved','superseded')), bound_repair_head TEXT NOT NULL DEFAULT '', reply_intent_key TEXT NOT NULL DEFAULT '', reply_database_id INTEGER NOT NULL DEFAULT 0, reply_node_id TEXT NOT NULL DEFAULT '', resolved INTEGER NOT NULL DEFAULT 0, outdated INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL, UNIQUE(run_id,root_comment_node_id))`,
	`CREATE TABLE trusted_review_feedback_conflicts (conflict_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), root_comment_node_id TEXT NOT NULL, observed_body_digest TEXT NOT NULL DEFAULT '', reason_code TEXT NOT NULL, observed_at TEXT NOT NULL)`,
	`CREATE INDEX trusted_review_feedback_run_head ON trusted_review_feedback(run_id,original_review_head_sha)`,
}

var migrationV14 = []string{
	`CREATE TABLE trusted_review_reply_evidence (reply_evidence_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id), root_comment_node_id TEXT NOT NULL, pr_number INTEGER NOT NULL, root_comment_database_id INTEGER NOT NULL, repaired_head TEXT NOT NULL, marker_digest TEXT NOT NULL, reply_database_id INTEGER NOT NULL, reply_node_id TEXT NOT NULL, app_id INTEGER NOT NULL, observed_at TEXT NOT NULL, UNIQUE(run_id,root_comment_node_id), UNIQUE(run_id,reply_database_id), UNIQUE(run_id,reply_node_id))`,
}

// migrationV15 records a durable execution claim for the one Linear status
// mutation. It prevents concurrent callers from sharing an intent and
// emitting duplicate writes; it is audit evidence, not a liveness lease.
var migrationV15 = []string{
	`ALTER TABLE side_effects ADD COLUMN claimed_at TEXT NOT NULL DEFAULT ''`,
}

// migrationV16 persists automatic-admission authority separately from per-run
// controller leases. The journal stores immutable identifiers and digests only.
var migrationV16 = []string{
	`CREATE TABLE IF NOT EXISTS linear_todo_admission_lease (
		namespace TEXT PRIMARY KEY CHECK(namespace='linear_todo_admission'),
		owner_nonce TEXT NOT NULL,
		version INTEGER NOT NULL CHECK(version > 0),
		acquired_at TEXT NOT NULL,
		renewed_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS linear_todo_admission_journal (
		issue_uuid TEXT NOT NULL UNIQUE,
		run_id TEXT NOT NULL PRIMARY KEY REFERENCES runs(run_id),
		scan_digest TEXT NOT NULL,
		task_digest TEXT NOT NULL,
		profile_digest TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('reserved','mutation_intent','started','manual_intervention')),
		mutation_intent_ref TEXT NOT NULL DEFAULT '',
		reason_code TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS linear_todo_admission_journal_status ON linear_todo_admission_journal(status,updated_at)`,
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
		normalized_task_json,task_hash,repository,repository_config_json,profile_id,profile_snapshot_version,profile_digest,profile_snapshot_json,registry_version,registry_digest,repository_binding_digest,base_branch,working_branch,worktree_path,artifact_root,current_state,implementation_model,review_model,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.IssueID, run.IdempotencyKey, run.SourceRevision, run.RawIssueJSON,
		run.RawIssueHash, run.NormalizedTaskJSON, run.TaskHash, run.Repository, run.RepositoryConfigJSON,
		run.ProfileID, run.ProfileSnapshotVersion, run.ProfileDigest, run.ProfileSnapshotJSON,
		run.RegistryVersion, run.RegistryDigest, run.RepositoryBindingDigest, run.BaseBranch, run.WorkingBranch,
		run.WorktreePath, run.ArtifactRoot, domain.StateReceived, run.ImplementationModel, run.ReviewModel, formatTime(now), formatTime(now))
	if err != nil {
		_ = tx.Rollback()
		existing, getErr := s.getByIdempotency(ctx, run.IdempotencyKey)
		if getErr == nil {
			if existing.TaskHash != run.TaskHash || existing.SourceRevision != run.SourceRevision {
				return application.Run{}, false, errors.New("idempotency key conflicts with a different task snapshot")
			}
			profileConflict := existing.ProfileID != run.ProfileID || existing.ProfileSnapshotVersion != run.ProfileSnapshotVersion || existing.ProfileDigest != run.ProfileDigest || existing.ProfileSnapshotJSON != run.ProfileSnapshotJSON
			legacyConflict := existing.ProfileSnapshotVersion == 0 && run.ProfileSnapshotVersion == 0 && (existing.RegistryVersion != run.RegistryVersion || existing.RegistryDigest != run.RegistryDigest || existing.RepositoryBindingDigest != run.RepositoryBindingDigest)
			if existing.Repository != run.Repository || profileConflict || legacyConflict || localOwnershipConflict(existing.RepositoryConfigJSON, run.RepositoryConfigJSON) {
				return application.Run{}, false, errors.New("idempotency key conflicts with a different repository authority binding")
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

func localOwnershipConflict(existingJSON, currentJSON string) bool {
	var existing, current application.LocalRepository
	if json.Unmarshal([]byte(existingJSON), &existing) != nil || json.Unmarshal([]byte(currentJSON), &current) != nil {
		return existingJSON != currentJSON
	}
	return existing.OriginPath != current.OriginPath || existing.SourcePath != current.SourcePath || existing.RunRoot != current.RunRoot || existing.WorktreeRoot != current.WorktreeRoot
}

func (s *Store) GetRun(ctx context.Context, id string) (application.Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return application.Run{}, application.ErrRunNotFound
	}
	return run, err
}

func (s *Store) GetRunByIdempotency(ctx context.Context, key string) (application.Run, bool, error) {
	run, err := s.getByIdempotency(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return application.Run{}, false, nil
	}
	return run, err == nil, err
}

// ListRuns returns a deterministic page ordered by newest creation time and run ID.
// The application query service owns cursor validation and authorization.
func (s *Store) ListRuns(ctx context.Context, repository string, beforeCreatedAt time.Time, beforeID string, limit int) ([]application.Run, error) {
	if limit < 1 || limit > 101 {
		return nil, errors.New("run list limit is out of bounds")
	}
	query := runSelect + ` WHERE repository=?`
	args := []any{repository}
	if !beforeCreatedAt.IsZero() {
		query += ` AND (created_at < ? OR (created_at = ? AND run_id < ?))`
		before := formatTime(beforeCreatedAt)
		args = append(args, before, before, beforeID)
	}
	query += ` ORDER BY created_at DESC, run_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []application.Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) GetRunByIssue(ctx context.Context, issueID string) (application.Run, bool, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE issue_id=? AND current_state NOT IN ('rejected','failed','completed')`, issueID))
	if errors.Is(err, sql.ErrNoRows) {
		return application.Run{}, false, nil
	}
	return run, err == nil, err
}

func (s *Store) MarkLinearSourceDrift(ctx context.Context, runID string, expectedState domain.State, expectedSourceRevision, evidence string) (bool, error) {
	if !domain.CanRequireManualIntervention(expectedState) || strings.TrimSpace(expectedSourceRevision) == "" || strings.TrimSpace(evidence) == "" {
		return false, errors.New("invalid Linear source drift authority")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var currentState, currentRevision string
	if err := tx.QueryRowContext(ctx, `SELECT current_state,source_revision FROM runs WHERE run_id=?`, runID).Scan(&currentState, &currentRevision); err != nil {
		return false, err
	}
	if domain.State(currentState) != expectedState || currentRevision != expectedSourceRevision {
		return false, nil
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, runID).Scan(&sequence); err != nil {
		return false, err
	}
	now := nowText()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,last_error=?,updated_at=? WHERE run_id=? AND current_state=? AND source_revision=?`, domain.StateManualIntervention, "Linear source drift requires a human decision", now, runID, expectedState, expectedSourceRevision)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions VALUES(?,?,?,?,?,?,?,?)`, runID, sequence, expectedState, domain.StateManualIntervention, "Linear source drift requires a human decision", evidence, "", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) getByIdempotency(ctx context.Context, key string) (application.Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE idempotency_key=?`, key))
}

const runSelect = `SELECT run_id,issue_id,idempotency_key,source_revision,raw_issue_json,raw_issue_hash,
	normalized_task_json,task_hash,repository,repository_config_json,profile_id,profile_snapshot_version,profile_digest,profile_snapshot_json,registry_version,registry_digest,repository_binding_digest,base_branch,working_branch,base_sha,worktree_path,artifact_root,
	current_state,candidate_head,implementation_session_id,implementation_model,review_model,last_error,lease_owner,lease_expires_unix,created_at,updated_at FROM runs`

type rowScanner interface{ Scan(...any) error }

func scanRun(row rowScanner) (application.Run, error) {
	var run application.Run
	var state, created, updated string
	var leaseExpires int64
	err := row.Scan(&run.ID, &run.IssueID, &run.IdempotencyKey, &run.SourceRevision, &run.RawIssueJSON, &run.RawIssueHash,
		&run.NormalizedTaskJSON, &run.TaskHash, &run.Repository, &run.RepositoryConfigJSON, &run.ProfileID, &run.ProfileSnapshotVersion, &run.ProfileDigest, &run.ProfileSnapshotJSON, &run.RegistryVersion, &run.RegistryDigest, &run.RepositoryBindingDigest, &run.BaseBranch, &run.WorkingBranch, &run.BaseSHA, &run.WorktreePath,
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
func (s *Store) BeginRepair(ctx context.Context, id, oldHead, evidence string) error {
	if strings.TrimSpace(oldHead) == "" {
		return errors.New("repair base head must not be blank")
	}
	if strings.TrimSpace(evidence) == "" {
		return errors.New("repair evidence must not be blank")
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions VALUES(?,?,?,?,?,?,?,?)`, id, sequence, domain.StateRepairing, domain.StateExecuting, "begin normalized GitHub finding repair", evidence, oldHead, now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SetLastError(ctx context.Context, id, message string) error {
	return execOne(ctx, s.db, `UPDATE runs SET last_error=?,updated_at=? WHERE run_id=?`, message, nowText(), id)
}

func (s *Store) SaveGitHubInstallation(ctx context.Context, runID string, m application.GitHubInstallationMetadata) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO github_installations(run_id,app_id,installation_id,repository_id,repository_node_id,repository_owner,repository_name,token_expires_at,permissions_digest,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,app_id,installation_id,repository_id,token_expires_at,permissions_digest) DO NOTHING`, runID, m.AppID, m.InstallationID, m.Repository.ID, m.Repository.NodeID, m.Repository.Owner, m.Repository.Name, formatTime(m.TokenExpiresAt), m.PermissionsDigest, formatTime(m.ObservedAt))
	return err
}

func (s *Store) SaveGitHubRequest(ctx context.Context, o application.GitHubRequestObservation) error {
	if strings.TrimSpace(o.RunID) == "" {
		return errors.New("GitHub request observation lacks run")
	}
	if strings.TrimSpace(o.ResponseDigest) == "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", o.Operation, o.Category, o.HTTPStatus, o.ErrorClass)))
		o.ResponseDigest = hex.EncodeToString(sum[:])
	}
	_, err := s.db.ExecContext(ctx, githubRequestInsert, githubRequestArgs(o)...)
	return err
}

const githubRequestInsert = `INSERT INTO github_request_observations(run_id,operation_name,endpoint_category,http_status,request_id,rate_limit_limit,rate_limit_remaining,rate_limit_reset,response_digest,error_class,installation_id,repository_id,repository_node_id,repository_owner,repository_name,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

func githubRequestArgs(o application.GitHubRequestObservation) []any {
	if strings.TrimSpace(o.ResponseDigest) == "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", o.Operation, o.Category, o.HTTPStatus, o.ErrorClass)))
		o.ResponseDigest = hex.EncodeToString(sum[:])
	}
	return []any{o.RunID, o.Operation, o.Category, o.HTTPStatus, o.RequestID, o.RateLimitLimit, o.RateLimitRemaining, formatTime(o.RateLimitReset), o.ResponseDigest, o.ErrorClass, o.InstallationID, o.Repository.ID, o.Repository.NodeID, o.Repository.Owner, o.Repository.Name, formatTime(o.ObservedAt)}
}
func (s *Store) SaveGitHubRequests(ctx context.Context, observations []application.GitHubRequestObservation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range observations {
		if strings.TrimSpace(o.RunID) == "" {
			return errors.New("GitHub request observation lacks run")
		}
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(o)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveReviewReplyObservations preserves sanitized reply-list/write telemetry
// when no immutable reply completion can be recorded. The lease check keeps a
// stale worker from appending audit facts after another controller takes over.
func (s *Store) SaveReviewReplyObservations(ctx context.Context, runID, leaseOwner string, observations []application.GitHubRequestObservation) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(leaseOwner) == "" || len(observations) == 0 || len(observations) > 10021 {
		return errors.New("review reply observation authority is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&owner, &expires); err != nil {
		return err
	}
	if owner != leaseOwner || expires <= time.Now().UTC().UnixNano() {
		return errors.New("review reply lease authority changed")
	}
	for _, observation := range observations {
		if observation.RunID != runID || strings.TrimSpace(observation.Operation) == "" || strings.TrimSpace(observation.Category) == "" || observation.ObservedAt.IsZero() {
			return errors.New("review reply request observation is incomplete")
		}
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(observation)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveGitHubReadSuccess(ctx context.Context, runID, leaseOwner string, expectedState domain.State, idempotencyKey string, observations []application.GitHubRequestObservation, pr domain.PullRequest, m application.GitHubInstallationMetadata, e domain.GitHubReadEvidence, feedback []application.TrustedReviewFeedbackRecord, approvalObservation *domain.HumanApprovalObservation, approval *domain.HumanApproval, nextState domain.State, transitionReason string) error {
	if len(e.Findings) > application.MaxNormalizedFindings {
		return errors.New("GitHub finding count exceeds controller bounds")
	}
	for _, finding := range e.Findings {
		if len([]byte(finding.Body)) > application.MaxNormalizedFindingBodyBytes || strings.ContainsRune(finding.Body, '\x00') {
			return errors.New("GitHub finding body exceeds controller bounds")
		}
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validatePersistedGitHubReadAuthority(ctx, tx, runID, m, e, observations, false); err != nil {
		return err
	}
	// Admission, immutable feedback persistence, selection, and the repairing
	// state edge share this transaction. A crash can therefore never leave a
	// prompt-authorizing feedback row detached from its repair transition.
	if len(feedback) > 0 || nextState == domain.StateRepairing {
		hasHumanFinding := false
		for _, finding := range e.Findings {
			hasHumanFinding = hasHumanFinding || finding.Source == "github_human_review_comment"
		}
		if hasHumanFinding && len(feedback) == 0 {
			return errors.New("repair transition lacks trusted inline feedback")
		}
		for _, item := range feedback {
			if item.RunID != runID {
				return errors.New("trusted feedback run mismatch")
			}
			if item.BodyDigest == "" {
				item.BodyDigest = domain.TrustedReviewFeedbackDigest(item.Body)
			}
			if err := item.ValidateObservation(); err != nil {
				return err
			}
			if item.OriginalReviewHeadSHA != pr.HeadSHA || item.PRNumber != pr.Number || item.PRDatabaseID != pr.DatabaseID || item.PRNodeID != pr.NodeID {
				return errors.New("trusted feedback is not bound to observed pull request")
			}
			if item.Lifecycle == "" {
				item.Lifecycle = domain.TrustedReviewFeedbackObserved
			}
			if item.UpdatedAt.IsZero() {
				item.UpdatedAt = item.ObservedAt.UTC()
			}
			current, found, err := trustedReviewFeedbackByRoot(ctx, tx, runID, item.RootCommentNodeID)
			if err != nil {
				return err
			}
			if found {
				if !current.ImmutableEqual(item.TrustedReviewFeedback) {
					if _, err := tx.ExecContext(ctx, `INSERT INTO trusted_review_feedback_conflicts(run_id,root_comment_node_id,observed_body_digest,reason_code,observed_at) VALUES(?,?,?,?,?)`, runID, item.RootCommentNodeID, item.BodyDigest, "immutable_authority_conflict", formatTime(item.ObservedAt)); err != nil {
						return err
					}
					if nextState == domain.StateManualIntervention {
						continue
					}
					return errors.New("trusted review feedback immutable authority conflict")
				}
				if nextState == domain.StateRepairing && current.Lifecycle == domain.TrustedReviewFeedbackObserved {
					result, err := tx.ExecContext(ctx, `UPDATE trusted_review_feedback SET lifecycle=?,updated_at=? WHERE run_id=? AND root_comment_node_id=? AND lifecycle=?`, domain.TrustedReviewFeedbackSelectedForRepair, nowText(), runID, item.RootCommentNodeID, domain.TrustedReviewFeedbackObserved)
					if err != nil {
						return err
					}
					if count, _ := result.RowsAffected(); count != 1 {
						return errors.New("trusted feedback selection compare failed")
					}
				}
				continue
			}
			var count, bytes int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(body_text AS BLOB))),0) FROM trusted_review_feedback WHERE run_id=? AND original_review_head_sha=?`, runID, item.OriginalReviewHeadSHA).Scan(&count, &bytes); err != nil {
				return err
			}
			if count >= domain.MaxTrustedReviewFeedbackPerHead || bytes+len([]byte(item.Body)) > domain.MaxTrustedReviewFeedbackTextBytes {
				return errors.New("trusted review feedback head bound exceeded")
			}
			lifecycle := domain.TrustedReviewFeedbackObserved
			if nextState == domain.StateRepairing {
				lifecycle = domain.TrustedReviewFeedbackSelectedForRepair
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO trusted_review_feedback(run_id,root_comment_node_id,pr_number,pr_database_id,pr_node_id,review_database_id,review_node_id,thread_node_id,root_comment_database_id,author_database_id,author_node_id,author_login,author_type,original_review_head_sha,path,line,body_text,body_digest,source_at,observed_at,lifecycle,bound_repair_head,reply_intent_key,reply_database_id,reply_node_id,resolved,outdated,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, item.RootCommentNodeID, item.PRNumber, item.PRDatabaseID, item.PRNodeID, item.ReviewDatabaseID, item.ReviewNodeID, item.ThreadNodeID, item.RootCommentDatabaseID, item.Author.DatabaseID, item.Author.NodeID, item.Author.Login, item.Author.Type, item.OriginalReviewHeadSHA, item.Path, item.Line, item.Body, item.BodyDigest, formatTime(item.SourceAt), formatTime(item.ObservedAt), lifecycle, "", "", 0, "", false, false, formatTime(item.UpdatedAt)); err != nil {
				return err
			}
		}
	}
	var state, key, owner string
	var leaseExpires int64
	if err := tx.QueryRowContext(ctx, `SELECT current_state,idempotency_key,lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&state, &key, &owner, &leaseExpires); err != nil {
		return err
	}
	if domain.State(state) != expectedState || key != idempotencyKey || owner != leaseOwner || leaseExpires <= time.Now().UTC().UnixNano() {
		return errors.New("GitHub reconciliation run authority changed")
	}
	for _, o := range observations {
		if o.RunID != runID {
			return errors.New("GitHub observation run mismatch")
		}
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(o)...); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE pull_requests SET database_id=?,state=?,merged=?,merge_sha=?,merged_at=? WHERE run_id=? AND number=? AND node_id=? AND url=? AND head_branch=? AND base_branch=? AND head_sha=? AND base_sha=? AND body_digest=? AND ownership_key=? AND database_id IN (0,?)`, pr.DatabaseID, pr.State, pr.Merged, pr.MergeSHA, formatTime(pr.MergedAt), runID, pr.Number, pr.NodeID, pr.URL, pr.HeadBranch, pr.BaseBranch, pr.HeadSHA, pr.BaseSHA, pr.BodyDigest, pr.OwnershipKey, pr.DatabaseID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("atomic GitHub PR identity update mismatch")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_installations(run_id,app_id,installation_id,repository_id,repository_node_id,repository_owner,repository_name,token_expires_at,permissions_digest,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,app_id,installation_id,repository_id,token_expires_at,permissions_digest) DO NOTHING`, runID, m.AppID, m.InstallationID, m.Repository.ID, m.Repository.NodeID, m.Repository.Owner, m.Repository.Name, formatTime(m.TokenExpiresAt), m.PermissionsDigest, formatTime(m.ObservedAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_read_evidence(run_id,head_sha,repository_id,evidence_json,evidence_digest,observed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,head_sha,evidence_digest) DO NOTHING`, runID, e.PullRequest.HeadSHA, e.Repository.ID, string(raw), hex.EncodeToString(sum[:]), formatTime(e.ObservedAt)); err != nil {
		return err
	}
	if approvalObservation != nil {
		if approvalObservation.PRNumber != pr.Number || approvalObservation.CandidateHead != pr.HeadSHA || approvalObservation.ObservedAt.IsZero() {
			return errors.New("human approval observation is not bound to the observed pull request")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO human_approval_observations(run_id,pr_number,candidate_head,status,review_database_id,review_node_id,actor_database_id,actor_node_id,actor_login,actor_type,review_head_sha,source_at,observed_at,evidence_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, approvalObservation.PRNumber, approvalObservation.CandidateHead, approvalObservation.Status, approvalObservation.ReviewDatabaseID, approvalObservation.ReviewNodeID, approvalObservation.Actor.DatabaseID, approvalObservation.Actor.NodeID, approvalObservation.Actor.Login, approvalObservation.Actor.Type, approvalObservation.ReviewHeadSHA, formatTime(approvalObservation.SourceAt), formatTime(approvalObservation.ObservedAt), hex.EncodeToString(sum[:])); err != nil {
			return err
		}
	}
	if approval != nil {
		if err := saveHumanApprovalTx(ctx, tx, runID, *approval); err != nil {
			return err
		}
	}
	for _, finding := range e.Findings {
		if finding.Body == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_findings(run_id,source_id,thread_id,source,file,line,severity,body_digest,body_text,resolved,outdated,head_sha,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,source,source_id,head_sha) DO UPDATE SET thread_id=excluded.thread_id,file=excluded.file,line=excluded.line,severity=excluded.severity,body_digest=excluded.body_digest,body_text=excluded.body_text,resolved=excluded.resolved,outdated=excluded.outdated,observed_at=excluded.observed_at`, runID, finding.SourceID, finding.ThreadID, finding.Source, finding.File, finding.Line, finding.Classification, finding.BodyDigest, finding.Body, finding.Resolved, finding.Outdated, finding.HeadSHA, formatTime(finding.ObservedAt)); err != nil {
			return err
		}
	}
	if nextState != expectedState {
		if err := domain.ValidateTransition(expectedState, nextState); err != nil {
			return err
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, runID).Scan(&sequence); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,updated_at=? WHERE run_id=? AND current_state=? AND idempotency_key=? AND lease_owner=? AND lease_expires_unix>?`, nextState, nowText(), runID, expectedState, idempotencyKey, leaseOwner, time.Now().UTC().UnixNano())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return errors.New("atomic GitHub reconciliation state update mismatch")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, runID, sequence, expectedState, nextState, transitionReason, "github_read_evidence:"+hex.EncodeToString(sum[:]), e.PullRequest.HeadSHA, nowText()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveGitHubReadFailure(ctx context.Context, runID, leaseOwner string, expectedState domain.State, idempotencyKey string, observations []application.GitHubRequestObservation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validatePersistedGitHubRequestAuthority(ctx, tx, runID, observations); err != nil {
		return err
	}
	var state, key, owner string
	var leaseExpires int64
	if err := tx.QueryRowContext(ctx, `SELECT current_state,idempotency_key,lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&state, &key, &owner, &leaseExpires); err != nil {
		return err
	}
	if domain.State(state) != expectedState || key != idempotencyKey || owner != leaseOwner || leaseExpires <= time.Now().UTC().UnixNano() {
		return errors.New("GitHub reconciliation run authority changed")
	}
	for _, o := range observations {
		if o.RunID != runID {
			return errors.New("GitHub observation run mismatch")
		}
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(o)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveGitHubManualPRTargetDrift commits a read-only conflict record and the
// manual state edge together. It never updates pull_requests: the observed
// target is untrusted conflict evidence, not a new controller binding.
func (s *Store) SaveGitHubManualPRTargetDrift(ctx context.Context, runID, leaseOwner string, expectedState domain.State, idempotencyKey string, expectedRepository domain.RepositoryIdentity, persisted domain.PullRequest, observations []application.GitHubRequestObservation, metadata application.GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, reason string) error {
	if expectedState != domain.StateAwaitingGitHubMergeability || !samePullRequestIdentity(persisted, evidence.PullRequest) || !pullRequestTargetDrift(persisted, evidence.PullRequest) || strings.TrimSpace(reason) == "" {
		return errors.New("manual GitHub target-drift evidence is incomplete")
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validatePersistedGitHubReadAuthority(ctx, tx, runID, metadata, evidence, observations, true); err != nil {
		return err
	}
	if !sameRepositoryIdentity(expectedRepository, evidence.Repository) || !sameRepositoryIdentity(expectedRepository, metadata.Repository) {
		return errors.New("manual GitHub target-drift repository authority changed")
	}
	durable, err := loadPullRequestBindingTx(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !sameImmutablePullRequestBinding(persisted, durable) || !sameExactPullRequestIdentity(durable, evidence.PullRequest) || !pullRequestTargetDrift(durable, evidence.PullRequest) {
		return errors.New("manual GitHub target-drift pull request binding changed")
	}
	var state, key, owner string
	var leaseExpires int64
	if err := tx.QueryRowContext(ctx, `SELECT current_state,idempotency_key,lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&state, &key, &owner, &leaseExpires); err != nil {
		return err
	}
	if domain.State(state) != expectedState || key != idempotencyKey || owner != leaseOwner || leaseExpires <= time.Now().UTC().UnixNano() {
		return errors.New("GitHub reconciliation run authority changed")
	}
	for _, observation := range observations {
		if observation.RunID != runID {
			return errors.New("GitHub observation run mismatch")
		}
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(observation)...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_installations(run_id,app_id,installation_id,repository_id,repository_node_id,repository_owner,repository_name,token_expires_at,permissions_digest,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,app_id,installation_id,repository_id,token_expires_at,permissions_digest) DO NOTHING`, runID, metadata.AppID, metadata.InstallationID, metadata.Repository.ID, metadata.Repository.NodeID, metadata.Repository.Owner, metadata.Repository.Name, formatTime(metadata.TokenExpiresAt), metadata.PermissionsDigest, formatTime(metadata.ObservedAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_read_evidence(run_id,head_sha,repository_id,evidence_json,evidence_digest,observed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,head_sha,evidence_digest) DO NOTHING`, runID, evidence.PullRequest.HeadSHA, evidence.Repository.ID, string(raw), hex.EncodeToString(sum[:]), formatTime(evidence.ObservedAt)); err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, runID).Scan(&sequence); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,last_error=?,updated_at=? WHERE run_id=? AND current_state=? AND idempotency_key=? AND lease_owner=? AND lease_expires_unix>?`, domain.StateManualIntervention, reason, nowText(), runID, expectedState, idempotencyKey, leaseOwner, time.Now().UTC().UnixNano())
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("atomic GitHub target-drift state update mismatch")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, runID, sequence, expectedState, domain.StateManualIntervention, reason, "github_read_evidence:"+hex.EncodeToString(sum[:]), persisted.HeadSHA, nowText()); err != nil {
		return err
	}
	return tx.Commit()
}

func sameRepositoryIdentity(expected, observed domain.RepositoryIdentity) bool {
	return expected.ID == observed.ID && (expected.NodeID == "" || expected.NodeID == observed.NodeID) && strings.EqualFold(expected.Owner, observed.Owner) && strings.EqualFold(expected.Name, observed.Name)
}

func samePullRequestIdentity(expected, observed domain.PullRequest) bool {
	return expected.Number == observed.Number && expected.NodeID == observed.NodeID && expected.URL == observed.URL && (expected.DatabaseID == 0 || expected.DatabaseID == observed.DatabaseID)
}

func pullRequestTargetDrift(expected, observed domain.PullRequest) bool {
	return expected.BodyDigest == observed.BodyDigest && (expected.HeadBranch != observed.HeadBranch || expected.BaseBranch != observed.BaseBranch || expected.HeadSHA != observed.HeadSHA || expected.BaseSHA != observed.BaseSHA || expected.OwnershipKey != observed.OwnershipKey)
}

func loadPullRequestBindingTx(ctx context.Context, tx *sql.Tx, runID string) (domain.PullRequest, error) {
	var pr domain.PullRequest
	var merged int
	var mergedAt string
	err := tx.QueryRowContext(ctx, `SELECT number,database_id,url,node_id,head_branch,base_branch,head_sha,base_sha,body_digest,ownership_key,state,merged,merge_sha,merged_at FROM pull_requests WHERE run_id=?`, runID).Scan(&pr.Number, &pr.DatabaseID, &pr.URL, &pr.NodeID, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.BaseSHA, &pr.BodyDigest, &pr.OwnershipKey, &pr.State, &merged, &pr.MergeSHA, &mergedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.PullRequest{}, errors.New("persisted pull request binding is required")
		}
		return domain.PullRequest{}, err
	}
	pr.Merged = merged != 0
	pr.MergedAt = parseTime(mergedAt)
	return pr, nil
}

func sameImmutablePullRequestBinding(left, right domain.PullRequest) bool {
	return left.Number == right.Number && left.DatabaseID == right.DatabaseID && left.URL == right.URL && left.NodeID == right.NodeID && left.HeadBranch == right.HeadBranch && left.BaseBranch == right.BaseBranch && left.HeadSHA == right.HeadSHA && left.BaseSHA == right.BaseSHA && left.BodyDigest == right.BodyDigest && left.OwnershipKey == right.OwnershipKey
}

func sameExactPullRequestIdentity(left, right domain.PullRequest) bool {
	return left.Number == right.Number && left.DatabaseID == right.DatabaseID && left.URL == right.URL && left.NodeID == right.NodeID
}

// validatePersistedGitHubReadAuthority rejects a changed GitHub authority
// before a transaction writes any observation. Older local-only fixtures have
// no GitHub binding; production manual target-drift handling requires one.
func validatePersistedGitHubReadAuthority(ctx context.Context, tx *sql.Tx, runID string, metadata application.GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, observations []application.GitHubRequestObservation, required bool) error {
	appID, installationID, expected, configured, err := persistedGitHubAuthorityTx(ctx, tx, runID, required)
	if err != nil || !configured {
		return err
	}
	if metadata.AppID != appID || metadata.InstallationID != installationID || !sameRepositoryIdentity(expected, metadata.Repository) || !sameRepositoryIdentity(expected, evidence.Repository) {
		return errors.New("GitHub repository or installation authority mismatch")
	}
	for _, observation := range observations {
		if observation.RunID != runID || observation.InstallationID != installationID || !sameRepositoryIdentity(expected, observation.Repository) {
			return errors.New("GitHub request observation authority mismatch")
		}
	}
	return nil
}

func validatePersistedGitHubRequestAuthority(ctx context.Context, tx *sql.Tx, runID string, observations []application.GitHubRequestObservation) error {
	_, installationID, expected, configured, err := persistedGitHubAuthorityTx(ctx, tx, runID, false)
	if err != nil || !configured {
		return err
	}
	for _, observation := range observations {
		if observation.RunID != runID || observation.InstallationID != installationID || !sameRepositoryIdentity(expected, observation.Repository) {
			return errors.New("GitHub request observation authority mismatch")
		}
	}
	return nil
}

func persistedGitHubAuthorityTx(ctx context.Context, tx *sql.Tx, runID string, required bool) (int64, int64, domain.RepositoryIdentity, bool, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT repository_config_json FROM runs WHERE run_id=?`, runID).Scan(&raw); err != nil {
		return 0, 0, domain.RepositoryIdentity{}, false, err
	}
	var binding application.LocalRepository
	if err := json.Unmarshal([]byte(raw), &binding); err != nil {
		return 0, 0, domain.RepositoryIdentity{}, false, errors.New("persisted GitHub repository authority is invalid")
	}
	configured := binding.GitHubAppID != 0 || binding.GitHubInstallationID != 0 || binding.ExpectedRepositoryID != 0 || binding.CanonicalRepository != ""
	if !configured && !required {
		return 0, 0, domain.RepositoryIdentity{}, false, nil
	}
	parts := strings.Split(binding.CanonicalRepository, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || binding.GitHubAppID < 1 || binding.GitHubInstallationID < 1 || binding.ExpectedRepositoryID < 1 {
		return 0, 0, domain.RepositoryIdentity{}, false, errors.New("persisted GitHub repository authority is incomplete")
	}
	expected := domain.RepositoryIdentity{ID: binding.ExpectedRepositoryID, Owner: parts[0], Name: parts[1]}
	return binding.GitHubAppID, binding.GitHubInstallationID, expected, true, nil
}

func (s *Store) SaveGitHubEvidence(ctx context.Context, runID string, e domain.GitHubReadEvidence) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	_, err = s.db.ExecContext(ctx, `INSERT INTO github_read_evidence(run_id,head_sha,repository_id,evidence_json,evidence_digest,observed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,head_sha,evidence_digest) DO NOTHING`, runID, e.PullRequest.HeadSHA, e.Repository.ID, string(raw), hex.EncodeToString(sum[:]), formatTime(e.ObservedAt))
	return err
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
	row := s.db.QueryRowContext(ctx, `SELECT side_effect_id,run_id,kind,idempotency_key,intent_json,status,result_json,stdout_path,stderr_path,attempt,created_at,claimed_at,observed_at FROM side_effects WHERE run_id=? AND kind=? AND idempotency_key=?`, record.RunID, record.Kind, record.IdempotencyKey)
	var created, claimed, observed string
	if scanErr := row.Scan(&record.ID, &record.RunID, &record.Kind, &record.IdempotencyKey, &record.IntentJSON, &record.Status, &record.ResultJSON, &record.StdoutPath, &record.StderrPath, &record.Attempt, &created, &claimed, &observed); scanErr != nil {
		return record, false, err
	}
	record.CreatedAt, record.ClaimedAt, record.ObservedAt = parseTime(created), parseTime(claimed), parseTime(observed)
	if record.RunID != requested.RunID || record.Kind != requested.Kind || record.IdempotencyKey != requested.IdempotencyKey || record.IntentJSON != requested.IntentJSON {
		return record, false, errors.New("conflicting immutable side-effect intent")
	}
	return record, false, nil
}

func (s *Store) FinishSideEffect(ctx context.Context, record application.SideEffectRecord) error {
	if record.Status != "observed" && record.Status != "failed" {
		return errors.New("side-effect result status must be observed or failed")
	}
	return execOne(ctx, s.db, `UPDATE side_effects SET status=?,result_json=?,stdout_path=?,stderr_path=?,claimed_at='',observed_at=? WHERE side_effect_id=? AND status IN ('intent','in_flight','failed')`, record.Status, record.ResultJSON, record.StdoutPath, record.StderrPath, formatTime(record.ObservedAt), record.ID)
}

// RetryLinearIssueStartSideEffect grants the one bounded reconciliation retry
// for the dedicated Todo-to-In Progress intent. It cannot revive any other
// side effect or a completed/failed second attempt.
func (s *Store) RetryLinearIssueStartSideEffect(ctx context.Context, record application.SideEffectRecord) (application.SideEffectRecord, bool, error) {
	if record.ID < 1 || record.Kind != "linear_move_to_started" || record.Attempt != 1 || record.Status != "failed" {
		return record, false, errors.New("invalid Linear issue start retry authority")
	}
	if record.ResultJSON != application.LinearIssueStartTodoRetryResult {
		return record, false, errors.New("Linear issue start retry result is not conclusive Todo evidence")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE side_effects SET status='intent',result_json='',claimed_at='',observed_at='',attempt=2 WHERE side_effect_id=? AND run_id=? AND kind='linear_move_to_started' AND idempotency_key=? AND status='failed' AND attempt=1 AND result_json=?`, record.ID, record.RunID, record.IdempotencyKey, application.LinearIssueStartTodoRetryResult)
	if err != nil {
		return record, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return record, false, err
	}
	record.Status, record.ResultJSON, record.ClaimedAt, record.ObservedAt, record.Attempt = "intent", "", time.Time{}, time.Time{}, 2
	return record, true, nil
}

// FinishLinearIssueStartSideEffect completes only the exact claimed (or
// adoption-only intent) attempt. A losing caller cannot overwrite a newer
// retry claim because both prior status and attempt are compared.
func (s *Store) FinishLinearIssueStartSideEffect(ctx context.Context, record application.SideEffectRecord, expectedStatus string, expectedAttempt int) error {
	if record.ID < 1 || record.Kind != "linear_move_to_started" || (record.Status != "observed" && record.Status != "failed") || (expectedStatus != "intent" && expectedStatus != "in_flight" && expectedStatus != "failed") || (expectedAttempt != 1 && expectedAttempt != 2) || record.Attempt != expectedAttempt {
		return errors.New("invalid Linear issue start finish authority")
	}
	return execOne(ctx, s.db, `UPDATE side_effects SET status=?,result_json=?,stdout_path=?,stderr_path=?,claimed_at='',observed_at=? WHERE side_effect_id=? AND run_id=? AND kind='linear_move_to_started' AND idempotency_key=? AND status=? AND attempt=?`, record.Status, record.ResultJSON, record.StdoutPath, record.StderrPath, formatTime(record.ObservedAt), record.ID, record.RunID, record.IdempotencyKey, expectedStatus, expectedAttempt)
}

// ClaimLinearIssueStartSideEffect is an execution compare-and-swap. The
// caller owns the single remote mutation only when this returns claimed=true.
func (s *Store) ClaimLinearIssueStartSideEffect(ctx context.Context, record application.SideEffectRecord, claimedAt time.Time) (application.SideEffectRecord, bool, error) {
	if record.ID < 1 || record.Kind != "linear_move_to_started" || record.Status != "intent" || (record.Attempt != 1 && record.Attempt != 2) || claimedAt.IsZero() {
		return record, false, errors.New("invalid Linear issue start claim authority")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE side_effects SET status='in_flight',claimed_at=? WHERE side_effect_id=? AND run_id=? AND kind='linear_move_to_started' AND idempotency_key=? AND status='intent' AND attempt=?`, formatTime(claimedAt), record.ID, record.RunID, record.IdempotencyKey, record.Attempt)
	if err != nil {
		return record, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return record, false, err
	}
	if changed == 1 {
		record.Status, record.ClaimedAt = "in_flight", claimedAt.UTC()
		return record, true, nil
	}
	current, _, getErr := linearIssueStartSideEffectByID(ctx, s.db, record.ID)
	return current, false, getErr
}

func linearIssueStartSideEffectByID(ctx context.Context, db *sql.DB, id int64) (application.SideEffectRecord, bool, error) {
	var record application.SideEffectRecord
	var created, claimed, observed string
	err := db.QueryRowContext(ctx, `SELECT side_effect_id,run_id,kind,idempotency_key,intent_json,status,result_json,stdout_path,stderr_path,attempt,created_at,claimed_at,observed_at FROM side_effects WHERE side_effect_id=?`, id).Scan(&record.ID, &record.RunID, &record.Kind, &record.IdempotencyKey, &record.IntentJSON, &record.Status, &record.ResultJSON, &record.StdoutPath, &record.StderrPath, &record.Attempt, &created, &claimed, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	record.CreatedAt, record.ClaimedAt, record.ObservedAt = parseTime(created), parseTime(claimed), parseTime(observed)
	return record, true, nil
}

// RecordMergePolicyPending atomically records the rejected merge-policy
// topology and moves the run into its read-only wait state. Keeping both facts
// in one transaction prevents a restart from treating an unrecorded policy
// rejection as a normal merge retry.
func (s *Store) RecordMergePolicyPending(ctx context.Context, runID string, record application.SideEffectRecord, candidateHead string) error {
	if runID == "" || candidateHead == "" || record.ID < 1 || record.RunID != runID || record.Kind != "squash_merge" || record.IdempotencyKey != candidateHead || record.Status != "failed" || !strings.Contains(record.ResultJSON, `"category":"merge_policy_pending"`) {
		return errors.New("merge policy pending record is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, head string
	if err := tx.QueryRowContext(ctx, `SELECT current_state,candidate_head FROM runs WHERE run_id=?`, runID).Scan(&current, &head); err != nil {
		return err
	}
	if domain.State(current) != domain.StateMerging || head != candidateHead {
		return errors.New("merge policy pending state authority changed")
	}
	result, err := tx.ExecContext(ctx, `UPDATE side_effects SET status='failed',result_json=?,stdout_path=?,stderr_path=?,observed_at=? WHERE side_effect_id=? AND run_id=? AND kind='squash_merge' AND idempotency_key=? AND status IN ('intent','failed')`, record.ResultJSON, record.StdoutPath, record.StderrPath, formatTime(record.ObservedAt), record.ID, runID, candidateHead)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("merge policy pending side-effect compare lost")
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, runID).Scan(&sequence); err != nil {
		return err
	}
	now := nowText()
	result, err = tx.ExecContext(ctx, `UPDATE runs SET current_state=?,last_error='merge_policy_pending',updated_at=? WHERE run_id=? AND current_state=? AND candidate_head=?`, domain.StateAwaitingGitHubMergeability, now, runID, domain.StateMerging, candidateHead)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("merge policy pending state compare lost")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, runID, sequence, domain.StateMerging, domain.StateAwaitingGitHubMergeability, "GitHub merge protection awaits human thread resolution", "merge_policy_pending", candidateHead, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryMergeSideEffect atomically claims one retry after a persisted
// merge_policy_pending result. The compare-and-swap prevents a restarted or
// concurrent driver from issuing a duplicate merge write for the same policy
// resolution observation.
func (s *Store) RetryMergeSideEffect(ctx context.Context, record application.SideEffectRecord) (application.SideEffectRecord, bool, error) {
	if record.ID < 1 || record.RunID == "" || record.Kind != "squash_merge" || record.IdempotencyKey == "" || record.Status != "failed" {
		return application.SideEffectRecord{}, false, errors.New("merge retry intent is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.SideEffectRecord{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE side_effects SET status='intent',attempt=attempt+1,result_json='{"category":"merge_policy_retry_claimed"}',observed_at='' WHERE side_effect_id=? AND run_id=? AND kind='squash_merge' AND idempotency_key=? AND status='failed' AND result_json LIKE '%"category":"merge_policy_pending"%'`, record.ID, record.RunID, record.IdempotencyKey)
	if err != nil {
		return application.SideEffectRecord{}, false, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		if err := tx.Commit(); err != nil {
			return application.SideEffectRecord{}, false, err
		}
		return record, false, nil
	}
	var created, observed string
	if err := tx.QueryRowContext(ctx, `SELECT side_effect_id,run_id,kind,idempotency_key,intent_json,status,result_json,stdout_path,stderr_path,attempt,created_at,observed_at FROM side_effects WHERE side_effect_id=?`, record.ID).Scan(&record.ID, &record.RunID, &record.Kind, &record.IdempotencyKey, &record.IntentJSON, &record.Status, &record.ResultJSON, &record.StdoutPath, &record.StderrPath, &record.Attempt, &created, &observed); err != nil {
		return application.SideEffectRecord{}, false, err
	}
	record.CreatedAt, record.ObservedAt = parseTime(created), parseTime(observed)
	if err := tx.Commit(); err != nil {
		return application.SideEffectRecord{}, false, err
	}
	return record, true, nil
}

func (s *Store) requireReviewReplyLease(ctx context.Context, runID, owner string) error {
	var current string
	var expires int64
	if err := s.db.QueryRowContext(ctx, `SELECT lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&current, &expires); err != nil {
		return err
	}
	if current != owner || expires <= time.Now().UTC().UnixNano() {
		return errors.New("review reply lease authority changed")
	}
	return nil
}

func (s *Store) BeginReviewReplySideEffect(ctx context.Context, owner string, record application.SideEffectRecord) (application.SideEffectRecord, bool, error) {
	if strings.TrimSpace(record.IdempotencyKey) == "" || strings.TrimSpace(record.IntentJSON) == "" {
		return record, false, errors.New("side-effect intent and idempotency key are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return record, false, err
	}
	defer tx.Rollback()
	var leaseOwner string
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, record.RunID).Scan(&leaseOwner, &expires); err != nil {
		return record, false, err
	}
	if leaseOwner != owner || expires <= time.Now().UTC().UnixNano() {
		return record, false, errors.New("review reply lease authority changed")
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO side_effects(run_id,kind,idempotency_key,intent_json,status,attempt,created_at) VALUES(?,?,?,?,?,?,?)`, record.RunID, record.Kind, record.IdempotencyKey, record.IntentJSON, "intent", record.Attempt, nowText())
	if err == nil {
		record.ID, _ = result.LastInsertId()
		record.Status = "intent"
		return record, true, tx.Commit()
	}
	requested := record
	var created, observed string
	if scanErr := tx.QueryRowContext(ctx, `SELECT side_effect_id,run_id,kind,idempotency_key,intent_json,status,result_json,stdout_path,stderr_path,attempt,created_at,observed_at FROM side_effects WHERE run_id=? AND kind=? AND idempotency_key=?`, record.RunID, record.Kind, record.IdempotencyKey).Scan(&record.ID, &record.RunID, &record.Kind, &record.IdempotencyKey, &record.IntentJSON, &record.Status, &record.ResultJSON, &record.StdoutPath, &record.StderrPath, &record.Attempt, &created, &observed); scanErr != nil {
		return record, false, err
	}
	record.CreatedAt, record.ObservedAt = parseTime(created), parseTime(observed)
	if record.RunID != requested.RunID || record.Kind != requested.Kind || record.IdempotencyKey != requested.IdempotencyKey || record.IntentJSON != requested.IntentJSON {
		return record, false, errors.New("conflicting immutable side-effect intent")
	}
	return record, false, tx.Commit()
}

func (s *Store) FinishReviewReplySideEffect(ctx context.Context, owner string, record application.SideEffectRecord) error {
	if record.Status != "observed" && record.Status != "failed" {
		return errors.New("side-effect result status must be observed or failed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=updated_at WHERE run_id=? AND lease_owner=? AND lease_expires_unix>?`, record.RunID, owner, time.Now().UTC().UnixNano())
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("review reply lease compare update lost")
	}
	result, err = tx.ExecContext(ctx, `UPDATE side_effects SET status=?,result_json=?,stdout_path=?,stderr_path=?,observed_at=? WHERE side_effect_id=? AND run_id=? AND status IN ('intent','failed')`, record.Status, record.ResultJSON, record.StdoutPath, record.StderrPath, formatTime(record.ObservedAt), record.ID, record.RunID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("review reply side effect compare update lost")
	}
	return tx.Commit()
}

// RetryReviewReplySideEffect turns only a persisted failed intent back into
// intent, incrementing its durable attempt counter under the same lease CAS.
func (s *Store) RetryReviewReplySideEffect(ctx context.Context, owner string, record application.SideEffectRecord, maximum int) (application.SideEffectRecord, bool, error) {
	if maximum < 1 || record.ID < 1 || record.Attempt < 1 || record.RunID == "" {
		return record, false, errors.New("review reply retry authority is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return record, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=updated_at WHERE run_id=? AND lease_owner=? AND lease_expires_unix>?`, record.RunID, owner, time.Now().UTC().UnixNano())
	if err != nil {
		return record, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return record, false, errors.New("review reply lease compare update lost")
	}
	result, err = tx.ExecContext(ctx, `UPDATE side_effects SET status='intent',attempt=attempt+1,result_json='',observed_at='' WHERE side_effect_id=? AND run_id=? AND kind='reply_to_review_comment' AND idempotency_key=? AND status='failed' AND attempt<?`, record.ID, record.RunID, record.IdempotencyKey, maximum)
	if err != nil {
		return record, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return record, false, nil
	}
	var created, observed string
	if err := tx.QueryRowContext(ctx, `SELECT side_effect_id,run_id,kind,idempotency_key,intent_json,status,result_json,stdout_path,stderr_path,attempt,created_at,observed_at FROM side_effects WHERE side_effect_id=?`, record.ID).Scan(&record.ID, &record.RunID, &record.Kind, &record.IdempotencyKey, &record.IntentJSON, &record.Status, &record.ResultJSON, &record.StdoutPath, &record.StderrPath, &record.Attempt, &created, &observed); err != nil {
		return record, false, err
	}
	record.CreatedAt, record.ObservedAt = parseTime(created), parseTime(observed)
	return record, true, tx.Commit()
}

func (s *Store) TransitionReviewReplyFeedback(ctx context.Context, runID, owner, rootCommentNodeID string, expected, next domain.TrustedReviewFeedbackLifecycle, boundRepairHead, replyIntentKey string, replyDatabaseID int64, replyNodeID string, resolved, outdated bool) (application.TrustedReviewFeedbackRecord, bool, error) {
	if strings.TrimSpace(runID) == "" || !validTrustedReviewFeedbackNodeID(rootCommentNodeID) || domain.ValidateTrustedReviewFeedbackTransition(expected, next) != nil {
		return application.TrustedReviewFeedbackRecord{}, false, errors.New("invalid trusted review feedback lifecycle transition")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	defer tx.Rollback()
	var leaseOwner string
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&leaseOwner, &expires); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if leaseOwner != owner || expires <= time.Now().UTC().UnixNano() {
		return application.TrustedReviewFeedbackRecord{}, false, errors.New("review reply lease authority changed")
	}
	current, found, err := trustedReviewFeedbackByRoot(ctx, tx, runID, rootCommentNodeID)
	if err != nil || !found {
		return current, false, err
	}
	if current.Lifecycle != expected {
		return current, false, nil
	}
	if err := validateTrustedReviewFeedbackTransitionEvidence(current, next, boundRepairHead, replyIntentKey, replyDatabaseID, replyNodeID, resolved); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	// The lease CAS is in the same write transaction as the lifecycle update;
	// no stale owner can change a repair/reply lifecycle after takeover.
	result, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=updated_at WHERE run_id=? AND lease_owner=? AND lease_expires_unix>?`, runID, owner, time.Now().UTC().UnixNano())
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.TrustedReviewFeedbackRecord{}, false, errors.New("review reply lease compare update lost")
	}
	result, err = tx.ExecContext(ctx, `UPDATE trusted_review_feedback SET lifecycle=?,bound_repair_head=CASE WHEN ?='' THEN bound_repair_head ELSE ? END,reply_intent_key=CASE WHEN ?='' THEN reply_intent_key ELSE ? END,reply_database_id=CASE WHEN ?=0 THEN reply_database_id ELSE ? END,reply_node_id=CASE WHEN ?='' THEN reply_node_id ELSE ? END,resolved=?,outdated=?,updated_at=? WHERE run_id=? AND root_comment_node_id=? AND lifecycle=?`, next, boundRepairHead, boundRepairHead, replyIntentKey, replyIntentKey, replyDatabaseID, replyDatabaseID, replyNodeID, replyNodeID, resolved, outdated, nowText(), runID, rootCommentNodeID, expected)
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.TrustedReviewFeedbackRecord{}, false, nil
	}
	record, found, err := trustedReviewFeedbackByRoot(ctx, tx, runID, rootCommentNodeID)
	if err != nil || !found {
		return record, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	return record, true, nil
}

// ResolveReviewReplyFeedback atomically records the authority reread telemetry
// with the terminal resolved lifecycle; neither fact may survive alone.
func (s *Store) ResolveReviewReplyFeedback(ctx context.Context, runID, owner string, feedback application.TrustedReviewFeedbackRecord, head string, outdated bool, observations []application.GitHubRequestObservation) (bool, error) {
	if len(observations) > 10021 {
		return false, errors.New("review reply request observations exceed bounded limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var leaseOwner string
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&leaseOwner, &expires); err != nil {
		return false, err
	}
	if leaseOwner != owner || expires <= time.Now().UTC().UnixNano() {
		return false, errors.New("review reply lease authority changed")
	}
	current, found, err := trustedReviewFeedbackByRoot(ctx, tx, runID, feedback.RootCommentNodeID)
	if err != nil || !found {
		return false, err
	}
	if current.Lifecycle != feedback.Lifecycle || current.BoundRepairHead != head {
		return false, nil
	}
	if err := validateTrustedReviewFeedbackTransitionEvidence(current, domain.TrustedReviewFeedbackResolved, head, current.ReplyIntentKey, 0, "", true); err != nil {
		return false, err
	}
	for _, observation := range observations {
		if observation.RunID != runID || observation.Operation == "" || observation.Category == "" || observation.ObservedAt.IsZero() {
			return false, errors.New("review reply request observation is incomplete")
		}
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(observation)...); err != nil {
			return false, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=updated_at WHERE run_id=? AND lease_owner=? AND lease_expires_unix>?`, runID, owner, time.Now().UTC().UnixNano())
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, errors.New("review reply lease compare update lost")
	}
	result, err = tx.ExecContext(ctx, `UPDATE trusted_review_feedback SET lifecycle=?,resolved=1,outdated=?,updated_at=? WHERE run_id=? AND root_comment_node_id=? AND lifecycle=?`, domain.TrustedReviewFeedbackResolved, outdated, nowText(), runID, current.RootCommentNodeID, feedback.Lifecycle)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, nil
	}
	return true, tx.Commit()
}

func (s *Store) TransitionReviewReplyRun(ctx context.Context, runID, owner string, expected, next domain.State, reason, evidence, head string) error {
	if err := domain.ValidateTransition(expected, next); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, leaseOwner string
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT current_state,lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&current, &leaseOwner, &expires); err != nil {
		return err
	}
	if domain.State(current) == next && leaseOwner == owner && expires > time.Now().UTC().UnixNano() {
		return tx.Commit()
	}
	if domain.State(current) != expected || leaseOwner != owner || expires <= time.Now().UTC().UnixNano() {
		return errors.New("review reply run authority changed")
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, runID).Scan(&sequence); err != nil {
		return err
	}
	now := nowText()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET current_state=?,updated_at=?,last_error='' WHERE run_id=? AND current_state=? AND lease_owner=? AND lease_expires_unix>?`, next, now, runID, expected, owner, time.Now().UTC().UnixNano())
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("review reply state compare update lost")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions VALUES(?,?,?,?,?,?,?,?)`, runID, sequence, expected, next, reason, evidence, head, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SavePullRequest(ctx context.Context, runID string, pr domain.PullRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pull_requests(run_id,number,database_id,url,node_id,head_branch,base_branch,head_sha,base_sha,body_digest,ownership_key,state,merged,merge_sha,merged_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, pr.Number, pr.DatabaseID, pr.URL, pr.NodeID, pr.HeadBranch, pr.BaseBranch, pr.HeadSHA, pr.BaseSHA, pr.BodyDigest, pr.OwnershipKey, pr.State, pr.Merged, pr.MergeSHA, formatTime(pr.MergedAt))
	if err == nil {
		return nil
	}
	var existing domain.PullRequest
	var merged int
	var mergedAt string
	if scanErr := s.db.QueryRowContext(ctx, `SELECT number,database_id,url,node_id,head_branch,base_branch,head_sha,base_sha,body_digest,ownership_key,state,merged,merge_sha,merged_at FROM pull_requests WHERE run_id=?`, runID).Scan(&existing.Number, &existing.DatabaseID, &existing.URL, &existing.NodeID, &existing.HeadBranch, &existing.BaseBranch, &existing.HeadSHA, &existing.BaseSHA, &existing.BodyDigest, &existing.OwnershipKey, &existing.State, &merged, &existing.MergeSHA, &mergedAt); scanErr != nil {
		return err
	}
	existing.Merged = merged != 0
	existing.MergedAt = parseTime(mergedAt)
	if existing.Number != pr.Number || existing.NodeID != pr.NodeID || existing.URL != pr.URL || existing.HeadBranch != pr.HeadBranch || existing.BaseBranch != pr.BaseBranch || existing.BaseSHA != pr.BaseSHA || existing.OwnershipKey != pr.OwnershipKey || existing.DatabaseID != 0 && existing.DatabaseID != pr.DatabaseID {
		return errors.New("conflicting immutable pull request evidence")
	}
	return execOne(ctx, s.db, `UPDATE pull_requests SET database_id=?,url=?,head_sha=?,body_digest=?,state=?,merged=?,merge_sha=?,merged_at=? WHERE run_id=? AND database_id IN (0,?)`, pr.DatabaseID, pr.URL, pr.HeadSHA, pr.BodyDigest, pr.State, pr.Merged, pr.MergeSHA, formatTime(pr.MergedAt), runID, pr.DatabaseID)
}

func (s *Store) SavePollObservation(ctx context.Context, record application.PollObservation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO poll_observations(run_id,pr_number,attempt,head_sha,status,snapshot_json,observed_at) VALUES(?,?,?,?,?,?,?)`, record.RunID, record.PRNumber, record.Attempt, record.HeadSHA, record.Status, record.SnapshotJSON, formatTime(record.ObservedAt))
	return err
}

func (s *Store) SaveFinding(ctx context.Context, record application.FindingRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO review_findings(run_id,source_id,thread_id,source,file,line,severity,body_digest,body_text,resolved,outdated,head_sha,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,source,source_id,head_sha) DO UPDATE SET thread_id=excluded.thread_id,file=excluded.file,line=excluded.line,severity=excluded.severity,body_digest=excluded.body_digest,body_text=excluded.body_text,resolved=excluded.resolved,outdated=excluded.outdated,observed_at=excluded.observed_at`, record.RunID, record.SourceID, record.ThreadID, record.Source, record.File, record.Line, record.Severity, record.BodyDigest, record.Body, record.Resolved, record.Outdated, record.HeadSHA, formatTime(record.ObservedAt))
	return err
}

// SaveTrustedReviewFeedback accepts a bounded observation once. A duplicate
// must be exactly identical in authority fields and first accepted body. Drift
// is retained only as a sanitized conflict row and cannot replace authority.
func (s *Store) SaveTrustedReviewFeedback(ctx context.Context, record application.TrustedReviewFeedbackRecord) (application.TrustedReviewFeedbackRecord, bool, error) {
	if record.BodyDigest == "" {
		record.BodyDigest = domain.TrustedReviewFeedbackDigest(record.Body)
	}
	if err := record.ValidateObservation(); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if record.Lifecycle == "" {
		record.Lifecycle = domain.TrustedReviewFeedbackObserved
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.ObservedAt.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	defer tx.Rollback()
	var count, bytes int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(body_text AS BLOB))),0) FROM trusted_review_feedback WHERE run_id=? AND original_review_head_sha=?`, record.RunID, record.OriginalReviewHeadSHA).Scan(&count, &bytes); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	existing, found, err := trustedReviewFeedbackByRoot(ctx, tx, record.RunID, record.RootCommentNodeID)
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if found {
		if !existing.ImmutableEqual(record.TrustedReviewFeedback) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO trusted_review_feedback_conflicts(run_id,root_comment_node_id,observed_body_digest,reason_code,observed_at) VALUES(?,?,?,?,?)`, record.RunID, record.RootCommentNodeID, record.BodyDigest, "immutable_authority_conflict", formatTime(record.ObservedAt)); err != nil {
				return application.TrustedReviewFeedbackRecord{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return application.TrustedReviewFeedbackRecord{}, false, err
			}
			return existing, false, errors.New("trusted review feedback immutable authority conflict")
		}
		if err := tx.Commit(); err != nil {
			return application.TrustedReviewFeedbackRecord{}, false, err
		}
		return existing, false, nil
	}
	if count >= domain.MaxTrustedReviewFeedbackPerHead || bytes+len([]byte(record.Body)) > domain.MaxTrustedReviewFeedbackTextBytes {
		return application.TrustedReviewFeedbackRecord{}, false, errors.New("trusted review feedback head bound exceeded")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO trusted_review_feedback(run_id,root_comment_node_id,pr_number,pr_database_id,pr_node_id,review_database_id,review_node_id,thread_node_id,root_comment_database_id,author_database_id,author_node_id,author_login,author_type,original_review_head_sha,path,line,body_text,body_digest,source_at,observed_at,lifecycle,bound_repair_head,reply_intent_key,reply_database_id,reply_node_id,resolved,outdated,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.RunID, record.RootCommentNodeID, record.PRNumber, record.PRDatabaseID, record.PRNodeID, record.ReviewDatabaseID, record.ReviewNodeID, record.ThreadNodeID, record.RootCommentDatabaseID, record.Author.DatabaseID, record.Author.NodeID, record.Author.Login, record.Author.Type, record.OriginalReviewHeadSHA, record.Path, record.Line, record.Body, record.BodyDigest, formatTime(record.SourceAt), formatTime(record.ObservedAt), record.Lifecycle, "", "", 0, "", record.Resolved, record.Outdated, formatTime(record.UpdatedAt))
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	return record, true, nil
}

// TransitionTrustedReviewFeedback is the only lifecycle mutation. It uses an
// explicit expected lifecycle in its UPDATE predicate, so stale callers cannot
// advance a comment after another controller attempt has changed it.
func (s *Store) TransitionTrustedReviewFeedback(ctx context.Context, runID, rootCommentNodeID string, expected, next domain.TrustedReviewFeedbackLifecycle, boundRepairHead, replyIntentKey string, replyDatabaseID int64, replyNodeID string, resolved, outdated bool) (application.TrustedReviewFeedbackRecord, bool, error) {
	if strings.TrimSpace(runID) == "" || !validTrustedReviewFeedbackNodeID(rootCommentNodeID) || domain.ValidateTrustedReviewFeedbackTransition(expected, next) != nil {
		return application.TrustedReviewFeedbackRecord{}, false, errors.New("invalid trusted review feedback lifecycle transition")
	}
	if boundRepairHead != "" && !isFullSHA(boundRepairHead) {
		return application.TrustedReviewFeedbackRecord{}, false, errors.New("bound repair head must be a full SHA")
	}
	if replyDatabaseID < 0 || (replyDatabaseID == 0 && replyNodeID != "") || (replyDatabaseID > 0 && !validTrustedReviewFeedbackNodeID(replyNodeID)) {
		return application.TrustedReviewFeedbackRecord{}, false, errors.New("reply identity is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	defer tx.Rollback()
	current, found, err := trustedReviewFeedbackByRoot(ctx, tx, runID, rootCommentNodeID)
	if err != nil || !found {
		return current, false, err
	}
	if current.Lifecycle != expected {
		return current, false, nil
	}
	if err := validateTrustedReviewFeedbackTransitionEvidence(current, next, boundRepairHead, replyIntentKey, replyDatabaseID, replyNodeID, resolved); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	now := nowText()
	result, err := tx.ExecContext(ctx, `UPDATE trusted_review_feedback SET lifecycle=?,bound_repair_head=CASE WHEN ?='' THEN bound_repair_head ELSE ? END,reply_intent_key=CASE WHEN ?='' THEN reply_intent_key ELSE ? END,reply_database_id=CASE WHEN ?=0 THEN reply_database_id ELSE ? END,reply_node_id=CASE WHEN ?='' THEN reply_node_id ELSE ? END,resolved=?,outdated=?,updated_at=? WHERE run_id=? AND root_comment_node_id=? AND lifecycle=?`, next, boundRepairHead, boundRepairHead, replyIntentKey, replyIntentKey, replyDatabaseID, replyDatabaseID, replyNodeID, replyNodeID, resolved, outdated, now, runID, rootCommentNodeID, expected)
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return application.TrustedReviewFeedbackRecord{}, false, nil
	}
	record, found, err := trustedReviewFeedbackByRoot(ctx, tx, runID, rootCommentNodeID)
	if err != nil || !found {
		return record, false, err
	}
	if err := tx.Commit(); err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	return record, true, nil
}

// RequireManualInterventionForTrustedFeedbackDrift records only digest/identity
// evidence and the terminal operator stop in one transaction. Raw GitHub body
// text remains exclusively in the bounded feedback/finding rows.
func (s *Store) RequireManualInterventionForTrustedFeedbackDrift(ctx context.Context, runID, leaseOwner string, expectedState domain.State, idempotencyKey string, observations []application.GitHubRequestObservation, pr domain.PullRequest, metadata application.GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, rootCommentNodeID, observedDigest string) error {
	if strings.TrimSpace(runID) == "" || !validTrustedReviewFeedbackNodeID(rootCommentNodeID) || strings.TrimSpace(observedDigest) == "" {
		return errors.New("trusted feedback drift identity is incomplete")
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, key, owner string
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT current_state,idempotency_key,lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, runID).Scan(&state, &key, &owner, &expires); err != nil {
		return err
	}
	if domain.State(state) != expectedState || key != idempotencyKey || owner != leaseOwner || expires <= time.Now().UTC().UnixNano() {
		return errors.New("trusted feedback drift run authority changed")
	}
	for _, observation := range observations {
		if observation.RunID != runID {
			return errors.New("GitHub observation run mismatch")
		}
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(observation)...); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE pull_requests SET database_id=?,url=?,head_sha=?,body_digest=?,state=?,merged=?,merge_sha=?,merged_at=? WHERE run_id=? AND number=? AND node_id=? AND head_branch=? AND base_branch=? AND base_sha=? AND ownership_key=? AND database_id IN (0,?)`, pr.DatabaseID, pr.URL, pr.HeadSHA, pr.BodyDigest, pr.State, pr.Merged, pr.MergeSHA, formatTime(pr.MergedAt), runID, pr.Number, pr.NodeID, pr.HeadBranch, pr.BaseBranch, pr.BaseSHA, pr.OwnershipKey, pr.DatabaseID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("atomic GitHub PR identity update mismatch")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_installations(run_id,app_id,installation_id,repository_id,repository_node_id,repository_owner,repository_name,token_expires_at,permissions_digest,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,app_id,installation_id,repository_id,token_expires_at,permissions_digest) DO NOTHING`, runID, metadata.AppID, metadata.InstallationID, metadata.Repository.ID, metadata.Repository.NodeID, metadata.Repository.Owner, metadata.Repository.Name, formatTime(metadata.TokenExpiresAt), metadata.PermissionsDigest, formatTime(metadata.ObservedAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_read_evidence(run_id,head_sha,repository_id,evidence_json,evidence_digest,observed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,head_sha,evidence_digest) DO NOTHING`, runID, evidence.PullRequest.HeadSHA, evidence.Repository.ID, string(raw), hex.EncodeToString(sum[:]), formatTime(evidence.ObservedAt)); err != nil {
		return err
	}
	if _, found, err := trustedReviewFeedbackByRoot(ctx, tx, runID, rootCommentNodeID); err != nil || !found {
		if err != nil {
			return err
		}
		return errors.New("trusted feedback drift root is not persisted")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trusted_review_feedback_conflicts(run_id,root_comment_node_id,observed_body_digest,reason_code,observed_at) VALUES(?,?,?,?,?)`, runID, rootCommentNodeID, observedDigest, "immutable_authority_conflict", nowText()); err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM transitions WHERE run_id=?`, runID).Scan(&sequence); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE runs SET current_state=?,updated_at=? WHERE run_id=? AND current_state=? AND idempotency_key=? AND lease_owner=? AND lease_expires_unix>?`, domain.StateManualIntervention, nowText(), runID, expectedState, idempotencyKey, leaseOwner, time.Now().UTC().UnixNano())
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("trusted feedback drift state update lost")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, runID, sequence, expectedState, domain.StateManualIntervention, "trusted inline feedback authority drift", "trusted_feedback_conflict:"+observedDigest, "", nowText()); err != nil {
		return err
	}
	return tx.Commit()
}

func validateTrustedReviewFeedbackTransitionEvidence(current application.TrustedReviewFeedbackRecord, next domain.TrustedReviewFeedbackLifecycle, boundRepairHead, replyIntentKey string, replyDatabaseID int64, replyNodeID string, resolved bool) error {
	noReplyIdentity := replyDatabaseID == 0 && replyNodeID == ""
	switch next {
	case domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackSuperseded:
		if boundRepairHead != "" || replyIntentKey != "" || !noReplyIdentity || resolved {
			return errors.New("feedback transition includes evidence before its lifecycle stage")
		}
	case domain.TrustedReviewFeedbackRepairVerified:
		if !isFullSHA(boundRepairHead) || replyIntentKey != "" || !noReplyIdentity || resolved || current.BoundRepairHead != "" {
			return errors.New("repair verification requires one new bound repair head only")
		}
	case domain.TrustedReviewFeedbackReplyPending:
		if !isFullSHA(current.BoundRepairHead) || strings.TrimSpace(replyIntentKey) == "" || !noReplyIdentity || resolved || current.ReplyIntentKey != "" {
			return errors.New("reply pending requires prior repair verification and one reply intent key")
		}
	case domain.TrustedReviewFeedbackReplied:
		if !isFullSHA(current.BoundRepairHead) || strings.TrimSpace(current.ReplyIntentKey) == "" || replyDatabaseID < 1 || !validTrustedReviewFeedbackNodeID(replyNodeID) || resolved || current.ReplyDatabaseID != 0 || current.ReplyNodeID != "" || boundRepairHead != "" || replyIntentKey != "" {
			return errors.New("reply observation requires prior immutable repair and reply intent evidence")
		}
	case domain.TrustedReviewFeedbackResolved:
		if !resolved || !isFullSHA(current.BoundRepairHead) || boundRepairHead != "" || replyIntentKey != "" || !noReplyIdentity {
			return errors.New("resolution requires consistent repair evidence")
		}
		if current.Lifecycle == domain.TrustedReviewFeedbackReplied && strings.TrimSpace(current.ReplyIntentKey) != "" && current.ReplyDatabaseID > 0 && validTrustedReviewFeedbackNodeID(current.ReplyNodeID) {
			break
		}
		if (current.Lifecycle == domain.TrustedReviewFeedbackRepairVerified || current.Lifecycle == domain.TrustedReviewFeedbackReplyPending) && current.ReplyDatabaseID == 0 && current.ReplyNodeID == "" {
			break
		}
		return errors.New("resolution lifecycle evidence is inconsistent")
	default:
		return errors.New("unknown trusted review feedback lifecycle")
	}
	return nil
}

// SaveReviewReplyEvidence records immutable, sanitized reply authority. It
// intentionally cannot store the public body or any GitHub token material.
func (s *Store) SaveReviewReplyEvidence(ctx context.Context, value application.ReviewReplyEvidence) error {
	if strings.TrimSpace(value.RunID) == "" || !validTrustedReviewFeedbackNodeID(value.RootCommentNodeID) || value.PullRequestNumber < 1 || value.RootCommentID < 1 || !isFullSHA(value.RepairedHead) || len(value.MarkerDigest) != 64 || value.ReplyDatabaseID < 1 || !validTrustedReviewFeedbackNodeID(value.ReplyNodeID) || value.AppID < 1 || value.ObservedAt.IsZero() {
		return errors.New("review reply evidence is incomplete")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO trusted_review_reply_evidence(run_id,root_comment_node_id,pr_number,root_comment_database_id,repaired_head,marker_digest,reply_database_id,reply_node_id,app_id,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,root_comment_node_id) DO UPDATE SET root_comment_node_id=excluded.root_comment_node_id WHERE pr_number=excluded.pr_number AND root_comment_database_id=excluded.root_comment_database_id AND repaired_head=excluded.repaired_head AND marker_digest=excluded.marker_digest AND reply_database_id=excluded.reply_database_id AND reply_node_id=excluded.reply_node_id AND app_id=excluded.app_id`, value.RunID, value.RootCommentNodeID, value.PullRequestNumber, value.RootCommentID, value.RepairedHead, value.MarkerDigest, value.ReplyDatabaseID, value.ReplyNodeID, value.AppID, formatTime(value.ObservedAt))
	return err
}

// FinalizeReviewReply is the one transaction that records a remote reply. A
// restart can therefore observe either the still-pending intent or all three
// completion facts; it can never skip an evidence-less replied lifecycle.
func (s *Store) FinalizeReviewReply(ctx context.Context, value application.ReviewReplyCompletion) (bool, error) {
	caller, reply, side := value.Feedback, value.Reply, value.Side
	if strings.TrimSpace(value.Head) == "" || !isFullSHA(value.Head) || caller.RunID == "" || caller.RootCommentNodeID == "" || strings.TrimSpace(value.LeaseOwner) == "" || side.ID < 1 || side.RunID != caller.RunID || side.Kind != "reply_to_review_comment" || side.IdempotencyKey == "" || reply.DatabaseID < 1 || !validTrustedReviewFeedbackNodeID(reply.NodeID) || reply.Actor.AppID < 1 || reply.CreatedAt.IsZero() {
		return false, errors.New("review reply completion authority is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var owner string
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner,lease_expires_unix FROM runs WHERE run_id=?`, caller.RunID).Scan(&owner, &expires); err != nil {
		return false, err
	}
	if owner != value.LeaseOwner || expires <= time.Now().UTC().UnixNano() {
		return false, nil
	}
	current, found, err := trustedReviewFeedbackByRoot(ctx, tx, caller.RunID, caller.RootCommentNodeID)
	if err != nil || !found {
		return false, err
	}
	if caller.PRNumber != current.PRNumber || caller.PRDatabaseID != current.PRDatabaseID || caller.PRNodeID != current.PRNodeID || caller.RootCommentDatabaseID != current.RootCommentDatabaseID || caller.RootCommentNodeID != current.RootCommentNodeID || side.RunID != current.RunID || reply.ReplyToID != current.RootCommentDatabaseID {
		return false, nil
	}
	if current.Lifecycle != domain.TrustedReviewFeedbackReplyPending || current.BoundRepairHead != value.Head || current.ReplyIntentKey != side.IdempotencyKey {
		return false, nil
	}
	evidence := application.ReviewReplyEvidence{RunID: current.RunID, RootCommentNodeID: current.RootCommentNodeID, PullRequestNumber: current.PRNumber, RootCommentID: current.RootCommentDatabaseID, RepairedHead: current.BoundRepairHead, MarkerDigest: current.ReplyIntentKey, ReplyDatabaseID: reply.DatabaseID, ReplyNodeID: reply.NodeID, AppID: reply.Actor.AppID, ObservedAt: reply.CreatedAt}
	if err := validateReviewReplyEvidence(evidence); err != nil {
		return false, err
	}
	if len(value.Observations) > 10021 {
		return false, errors.New("review reply request observations exceed bounded limit")
	}
	for _, observation := range value.Observations {
		if observation.RunID != current.RunID || observation.Operation == "" || observation.Category == "" || observation.ObservedAt.IsZero() {
			return false, errors.New("review reply request observation is incomplete")
		}
	}
	// The remaining side-effect update is bound to the row just read, not the
	// caller-provided snapshot used only to locate and compare authority.
	feedback := current
	if err := validateTrustedReviewFeedbackTransitionEvidence(current, domain.TrustedReviewFeedbackReplied, "", "", reply.DatabaseID, reply.NodeID, false); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE trusted_review_feedback SET lifecycle=?,reply_database_id=?,reply_node_id=?,updated_at=? WHERE run_id=? AND root_comment_node_id=? AND lifecycle=?`, domain.TrustedReviewFeedbackReplied, reply.DatabaseID, reply.NodeID, nowText(), current.RunID, current.RootCommentNodeID, domain.TrustedReviewFeedbackReplyPending)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trusted_review_reply_evidence(run_id,root_comment_node_id,pr_number,root_comment_database_id,repaired_head,marker_digest,reply_database_id,reply_node_id,app_id,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, evidence.RunID, evidence.RootCommentNodeID, evidence.PullRequestNumber, evidence.RootCommentID, evidence.RepairedHead, evidence.MarkerDigest, evidence.ReplyDatabaseID, evidence.ReplyNodeID, evidence.AppID, formatTime(evidence.ObservedAt)); err != nil {
		return false, err
	}
	for _, observation := range value.Observations {
		if _, err := tx.ExecContext(ctx, githubRequestInsert, githubRequestArgs(observation)...); err != nil {
			return false, err
		}
	}
	result, err = tx.ExecContext(ctx, `UPDATE side_effects SET status='observed',result_json='{\"reply_recorded\":true}',observed_at=? WHERE side_effect_id=? AND run_id=? AND kind='reply_to_review_comment' AND idempotency_key=? AND status IN ('intent','failed')`, formatTime(reply.CreatedAt), side.ID, feedback.RunID, side.IdempotencyKey)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func validateReviewReplyEvidence(value application.ReviewReplyEvidence) error {
	if strings.TrimSpace(value.RunID) == "" || !validTrustedReviewFeedbackNodeID(value.RootCommentNodeID) || value.PullRequestNumber < 1 || value.RootCommentID < 1 || !isFullSHA(value.RepairedHead) || len(value.MarkerDigest) != 64 || value.ReplyDatabaseID < 1 || !validTrustedReviewFeedbackNodeID(value.ReplyNodeID) || value.AppID < 1 || value.ObservedAt.IsZero() {
		return errors.New("review reply evidence is incomplete")
	}
	return nil
}

type trustedReviewFeedbackQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func trustedReviewFeedbackByRoot(ctx context.Context, query trustedReviewFeedbackQuery, runID, rootCommentNodeID string) (application.TrustedReviewFeedbackRecord, bool, error) {
	var record application.TrustedReviewFeedbackRecord
	var sourceAt, observedAt, updatedAt string
	var line sql.NullInt64
	var resolved, outdated int
	err := query.QueryRowContext(ctx, `SELECT run_id,pr_number,pr_database_id,pr_node_id,review_database_id,review_node_id,thread_node_id,root_comment_database_id,root_comment_node_id,author_database_id,author_node_id,author_login,author_type,original_review_head_sha,path,line,body_text,body_digest,source_at,observed_at,lifecycle,bound_repair_head,reply_intent_key,reply_database_id,reply_node_id,resolved,outdated,updated_at FROM trusted_review_feedback WHERE run_id=? AND root_comment_node_id=?`, runID, rootCommentNodeID).Scan(&record.RunID, &record.PRNumber, &record.PRDatabaseID, &record.PRNodeID, &record.ReviewDatabaseID, &record.ReviewNodeID, &record.ThreadNodeID, &record.RootCommentDatabaseID, &record.RootCommentNodeID, &record.Author.DatabaseID, &record.Author.NodeID, &record.Author.Login, &record.Author.Type, &record.OriginalReviewHeadSHA, &record.Path, &line, &record.Body, &record.BodyDigest, &sourceAt, &observedAt, &record.Lifecycle, &record.BoundRepairHead, &record.ReplyIntentKey, &record.ReplyDatabaseID, &record.ReplyNodeID, &resolved, &outdated, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return application.TrustedReviewFeedbackRecord{}, false, nil
	}
	if err != nil {
		return application.TrustedReviewFeedbackRecord{}, false, err
	}
	if line.Valid {
		value := int(line.Int64)
		record.Line = &value
	}
	record.SourceAt, record.ObservedAt, record.UpdatedAt = parseTime(sourceAt), parseTime(observedAt), parseTime(updatedAt)
	record.Resolved, record.Outdated = resolved != 0, outdated != 0
	return record, true, nil
}

func isFullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func validTrustedReviewFeedbackNodeID(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00')
}

func (s *Store) SaveHumanApproval(ctx context.Context, runID string, approval domain.HumanApproval) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := saveHumanApprovalTx(ctx, tx, runID, approval); err != nil {
		return err
	}
	return tx.Commit()
}

func saveHumanApprovalTx(ctx context.Context, tx *sql.Tx, runID string, approval domain.HumanApproval) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO human_approvals(run_id,pr_number,approver,source,approved_sha,ci_status,internal_review_sha,approved_at,actor_database_id,actor_node_id,actor_login,actor_type,review_database_id,review_node_id,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, approval.PRNumber, approval.Approver, approval.Source, approval.ApprovedSHA, approval.CIStatus, approval.ReviewSHA, formatTime(approval.ApprovedAt), approval.Actor.DatabaseID, approval.Actor.NodeID, approval.Actor.Login, approval.Actor.Type, approval.ReviewDatabaseID, approval.ReviewNodeID, formatTime(approval.ObservedAt))
	if err == nil {
		return nil
	}
	var existing domain.HumanApproval
	var approvedAt, observedAt string
	if scanErr := tx.QueryRowContext(ctx, `SELECT pr_number,approver,source,approved_sha,ci_status,internal_review_sha,approved_at,actor_database_id,actor_node_id,actor_login,actor_type,review_database_id,review_node_id,observed_at FROM human_approvals WHERE run_id=? AND approved_sha=?`, runID, approval.ApprovedSHA).Scan(&existing.PRNumber, &existing.Approver, &existing.Source, &existing.ApprovedSHA, &existing.CIStatus, &existing.ReviewSHA, &approvedAt, &existing.Actor.DatabaseID, &existing.Actor.NodeID, &existing.Actor.Login, &existing.Actor.Type, &existing.ReviewDatabaseID, &existing.ReviewNodeID, &observedAt); scanErr != nil {
		return err
	}
	existing.ApprovedAt = parseTime(approvedAt)
	existing.ObservedAt = parseTime(observedAt)
	if !sameHumanApprovalAuthority(existing, approval) {
		return errors.New("conflicting immutable human approval evidence")
	}
	if approval.ObservedAt.Equal(existing.ObservedAt) {
		return nil
	}
	if !approval.ObservedAt.After(existing.ObservedAt) {
		return errors.New("human approval observation is not newer")
	}
	result, err := tx.ExecContext(ctx, `UPDATE human_approvals SET observed_at=? WHERE run_id=? AND approved_sha=? AND observed_at=?`, formatTime(approval.ObservedAt), runID, approval.ApprovedSHA, observedAt)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("human approval observation update lost")
	}
	return nil
}

func sameHumanApprovalAuthority(left, right domain.HumanApproval) bool {
	return left.PRNumber == right.PRNumber &&
		left.Approver == right.Approver &&
		left.Actor == right.Actor &&
		left.ReviewDatabaseID == right.ReviewDatabaseID &&
		left.ReviewNodeID == right.ReviewNodeID &&
		left.Source == right.Source &&
		left.ApprovedSHA == right.ApprovedSHA &&
		left.CIStatus == right.CIStatus &&
		left.ReviewSHA == right.ReviewSHA &&
		left.ApprovedAt.Equal(right.ApprovedAt)
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

func (s *Store) SaveLinearCompletionObservation(ctx context.Context, record application.LinearCompletionObservation) error {
	if record.RunID == "" || record.MergeSHA == "" || record.Identifier == "" || record.Status == "" || record.ObservedAt.IsZero() {
		return errors.New("incomplete Linear completion observation")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO linear_completion_observations(run_id,merge_sha,linear_issue_id,issue_identifier,source_revision,state_id,state_name,state_type,status,error_class,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.RunID, record.MergeSHA, record.LinearIssueID, record.Identifier, record.SourceRevision, record.StateID, record.StateName, record.StateType, record.Status, record.ErrorClass, formatTime(record.ObservedAt))
	return err
}

func (s *Store) SaveLinearRequestObservation(ctx context.Context, runID string, record application.LinearRequestObservation) error {
	if runID == "" || record.Operation == "" || record.ObservedAt.IsZero() {
		return errors.New("incomplete Linear request observation")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO linear_request_observations(run_id,operation_name,http_status,request_id,rate_limit_limit,rate_limit_remaining,rate_limit_reset,response_digest,error_class,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, runID, record.Operation, record.HTTPStatus, record.RequestID, record.RateLimitLimit, record.RateLimitRemaining, formatTime(record.RateLimitReset), record.ResponseDigest, record.ErrorClass, formatTime(record.ObservedAt))
	return err
}

func (s *Store) UpsertCleanup(ctx context.Context, record application.CleanupRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cleanup_results(run_id,resource_kind,resource_name,status,error_class,last_error,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id,resource_kind,resource_name) DO UPDATE SET status=excluded.status,error_class=excluded.error_class,last_error=excluded.last_error,updated_at=excluded.updated_at`, record.RunID, record.Kind, record.Name, record.Status, record.ErrorClass, record.LastError, nowText())
	return err
}
func (s *Store) CleanupProgress(ctx context.Context, runID string) ([]application.CleanupRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cleanup_id,run_id,resource_kind,resource_name,status,error_class,last_error,updated_at FROM cleanup_results WHERE run_id=? ORDER BY cleanup_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []application.CleanupRecord
	for rows.Next() {
		var item application.CleanupRecord
		var updated string
		if err := rows.Scan(&item.ID, &item.RunID, &item.Kind, &item.Name, &item.Status, &item.ErrorClass, &item.LastError, &updated); err != nil {
			return nil, err
		}
		item.UpdatedAt = parseTime(updated)
		result = append(result, item)
	}
	return result, rows.Err()
}
func (s *Store) PollProgress(ctx context.Context, runID string, pr int64, head string) ([]application.PollObservation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT observation_id,run_id,pr_number,attempt,head_sha,status,snapshot_json,observed_at FROM poll_observations WHERE run_id=? AND pr_number=? AND head_sha=? ORDER BY observation_id`, runID, pr, head)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []application.PollObservation
	for rows.Next() {
		var item application.PollObservation
		var observed string
		if err := rows.Scan(&item.ID, &item.RunID, &item.PRNumber, &item.Attempt, &item.HeadSHA, &item.Status, &item.SnapshotJSON, &observed); err != nil {
			return nil, err
		}
		item.ObservedAt = parseTime(observed)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Inspect(ctx context.Context, id string) (application.RunInspection, error) {
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return application.RunInspection{}, err
	}
	inspection := application.RunInspection{Run: run}
	if run.RegistryVersion > 0 {
		var binding application.LocalRepository
		if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &binding); err != nil {
			return application.RunInspection{}, errors.New("persisted repository binding is invalid")
		}
		inspection.RepositoryBinding = &application.SanitizedRepositoryBinding{
			ProfileID: binding.ProfileID, ProfileSnapshotVersion: binding.ProfileSnapshotVersion, ProfileDigest: binding.ProfileDigest,
			CanonicalRepository: binding.CanonicalRepository, BaseBranch: binding.BaseBranch,
			VerifierRegistryRef: binding.VerifierRegistryRef, VerifierIDs: append([]string(nil), binding.VerifierIDs...),
			GitHubAppProfileRef: binding.GitHubAppProfileRef, GitHubAppID: binding.GitHubAppID, GitHubInstallationID: binding.GitHubInstallationID,
			ExpectedRepositoryID: binding.ExpectedRepositoryID, AllowedOperatorLogins: append([]string(nil), binding.AllowedOperatorLogins...),
			TrustedOperatorActors: append([]application.TrustedActorIdentity(nil), binding.TrustedOperatorActors...),
		}
	}
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
	if err := s.db.QueryRowContext(ctx, `SELECT number,database_id,url,node_id,head_branch,base_branch,head_sha,base_sha,body_digest,ownership_key,state,merged,merge_sha,merged_at FROM pull_requests WHERE run_id=?`, id).Scan(&pr.Number, &pr.DatabaseID, &pr.URL, &pr.NodeID, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.BaseSHA, &pr.BodyDigest, &pr.OwnershipKey, &pr.State, &merged, &pr.MergeSHA, &mergedAt); err == nil {
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
	rows, err = s.db.QueryContext(ctx, `SELECT finding_id,run_id,source_id,thread_id,source,file,line,severity,body_digest,body_text,resolved,outdated,head_sha,observed_at FROM review_findings WHERE run_id=? ORDER BY finding_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.FindingRecord
		var resolved, outdated int
		var observed string
		if err := rows.Scan(&v.ID, &v.RunID, &v.SourceID, &v.ThreadID, &v.Source, &v.File, &v.Line, &v.Severity, &v.BodyDigest, &v.Body, &resolved, &outdated, &v.HeadSHA, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		v.Resolved = resolved != 0
		v.Outdated = outdated != 0
		v.ObservedAt = parseTime(observed)
		inspection.Findings = append(inspection.Findings, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT run_id,pr_number,pr_database_id,pr_node_id,review_database_id,review_node_id,thread_node_id,root_comment_database_id,root_comment_node_id,author_database_id,author_node_id,author_login,author_type,original_review_head_sha,path,line,body_text,body_digest,source_at,observed_at,lifecycle,bound_repair_head,reply_intent_key,reply_database_id,reply_node_id,resolved,outdated,updated_at FROM trusted_review_feedback WHERE run_id=? ORDER BY feedback_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var record application.TrustedReviewFeedbackRecord
		var sourceAt, observedAt, updatedAt string
		var line sql.NullInt64
		var resolved, outdated int
		if err := rows.Scan(&record.RunID, &record.PRNumber, &record.PRDatabaseID, &record.PRNodeID, &record.ReviewDatabaseID, &record.ReviewNodeID, &record.ThreadNodeID, &record.RootCommentDatabaseID, &record.RootCommentNodeID, &record.Author.DatabaseID, &record.Author.NodeID, &record.Author.Login, &record.Author.Type, &record.OriginalReviewHeadSHA, &record.Path, &line, &record.Body, &record.BodyDigest, &sourceAt, &observedAt, &record.Lifecycle, &record.BoundRepairHead, &record.ReplyIntentKey, &record.ReplyDatabaseID, &record.ReplyNodeID, &resolved, &outdated, &updatedAt); err != nil {
			rows.Close()
			return inspection, err
		}
		if line.Valid {
			value := int(line.Int64)
			record.Line = &value
		}
		record.SourceAt, record.ObservedAt, record.UpdatedAt = parseTime(sourceAt), parseTime(observedAt), parseTime(updatedAt)
		record.Resolved, record.Outdated = resolved != 0, outdated != 0
		inspection.TrustedFeedback = append(inspection.TrustedFeedback, record)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT run_id,root_comment_node_id,pr_number,root_comment_database_id,repaired_head,marker_digest,reply_database_id,reply_node_id,app_id,observed_at FROM trusted_review_reply_evidence WHERE run_id=? ORDER BY reply_evidence_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var reply application.ReviewReplyEvidence
		var observed string
		if err := rows.Scan(&reply.RunID, &reply.RootCommentNodeID, &reply.PullRequestNumber, &reply.RootCommentID, &reply.RepairedHead, &reply.MarkerDigest, &reply.ReplyDatabaseID, &reply.ReplyNodeID, &reply.AppID, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		reply.ObservedAt = parseTime(observed)
		inspection.ReviewReplies = append(inspection.ReviewReplies, reply)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT conflict_id,run_id,root_comment_node_id,observed_body_digest,reason_code,observed_at FROM trusted_review_feedback_conflicts WHERE run_id=? ORDER BY conflict_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var conflict application.TrustedReviewFeedbackConflict
		var observed string
		if err := rows.Scan(&conflict.ID, &conflict.RunID, &conflict.RootCommentNodeID, &conflict.ObservedDigest, &conflict.ReasonCode, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		conflict.ObservedAt = parseTime(observed)
		inspection.FeedbackConflicts = append(inspection.FeedbackConflicts, conflict)
	}
	rows.Close()
	var approval domain.HumanApproval
	var approvedAt, approvalObservedAt string
	if err := s.db.QueryRowContext(ctx, `SELECT pr_number,approver,source,approved_sha,ci_status,internal_review_sha,approved_at,actor_database_id,actor_node_id,actor_login,actor_type,review_database_id,review_node_id,observed_at FROM human_approvals WHERE run_id=? AND approved_sha=? ORDER BY approval_id DESC LIMIT 1`, id, run.CandidateHead).Scan(&approval.PRNumber, &approval.Approver, &approval.Source, &approval.ApprovedSHA, &approval.CIStatus, &approval.ReviewSHA, &approvedAt, &approval.Actor.DatabaseID, &approval.Actor.NodeID, &approval.Actor.Login, &approval.Actor.Type, &approval.ReviewDatabaseID, &approval.ReviewNodeID, &approvalObservedAt); err == nil {
		approval.ApprovedAt, approval.ObservedAt = parseTime(approvedAt), parseTime(approvalObservedAt)
		inspection.Approval = &approval
	} else if !errors.Is(err, sql.ErrNoRows) {
		return inspection, err
	}
	var approvalObservation domain.HumanApprovalObservation
	var sourceAt, observedAt string
	if err := s.db.QueryRowContext(ctx, `SELECT pr_number,candidate_head,status,review_database_id,review_node_id,actor_database_id,actor_node_id,actor_login,actor_type,review_head_sha,source_at,observed_at FROM human_approval_observations WHERE run_id=? AND candidate_head=? ORDER BY observation_id DESC LIMIT 1`, id, run.CandidateHead).Scan(&approvalObservation.PRNumber, &approvalObservation.CandidateHead, &approvalObservation.Status, &approvalObservation.ReviewDatabaseID, &approvalObservation.ReviewNodeID, &approvalObservation.Actor.DatabaseID, &approvalObservation.Actor.NodeID, &approvalObservation.Actor.Login, &approvalObservation.Actor.Type, &approvalObservation.ReviewHeadSHA, &sourceAt, &observedAt); err == nil {
		approvalObservation.SourceAt, approvalObservation.ObservedAt = parseTime(sourceAt), parseTime(observedAt)
		inspection.ApprovalObservation = &approvalObservation
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
	rows, err = s.db.QueryContext(ctx, `SELECT observation_id,run_id,merge_sha,linear_issue_id,issue_identifier,source_revision,state_id,state_name,state_type,status,error_class,observed_at FROM linear_completion_observations WHERE run_id=? ORDER BY observation_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.LinearCompletionObservation
		var observed string
		if err := rows.Scan(&v.ID, &v.RunID, &v.MergeSHA, &v.LinearIssueID, &v.Identifier, &v.SourceRevision, &v.StateID, &v.StateName, &v.StateType, &v.Status, &v.ErrorClass, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		v.ObservedAt = parseTime(observed)
		inspection.LinearCompletion = append(inspection.LinearCompletion, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT cleanup_id,run_id,resource_kind,resource_name,status,error_class,last_error,updated_at FROM cleanup_results WHERE run_id=? ORDER BY cleanup_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var v application.CleanupRecord
		var updated string
		if err := rows.Scan(&v.ID, &v.RunID, &v.Kind, &v.Name, &v.Status, &v.ErrorClass, &v.LastError, &updated); err != nil {
			rows.Close()
			return inspection, err
		}
		v.UpdatedAt = parseTime(updated)
		inspection.Cleanup = append(inspection.Cleanup, v)
	}
	rows.Close()
	var installation application.GitHubInstallationMetadata
	var tokenExpiry, installationObserved string
	if err := s.db.QueryRowContext(ctx, `SELECT app_id,installation_id,repository_id,repository_node_id,repository_owner,repository_name,token_expires_at,permissions_digest,observed_at FROM github_installations WHERE run_id=? ORDER BY observation_id DESC LIMIT 1`, id).Scan(&installation.AppID, &installation.InstallationID, &installation.Repository.ID, &installation.Repository.NodeID, &installation.Repository.Owner, &installation.Repository.Name, &tokenExpiry, &installation.PermissionsDigest, &installationObserved); err == nil {
		installation.TokenExpiresAt = parseTime(tokenExpiry)
		installation.ObservedAt = parseTime(installationObserved)
		inspection.GitHubInstallation = &installation
	} else if !errors.Is(err, sql.ErrNoRows) {
		return inspection, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT operation_name,endpoint_category,http_status,request_id,rate_limit_limit,rate_limit_remaining,rate_limit_reset,response_digest,error_class,installation_id,repository_id,repository_node_id,repository_owner,repository_name,observed_at FROM github_request_observations WHERE run_id=? ORDER BY observation_id`, id)
	if err != nil {
		return inspection, err
	}
	for rows.Next() {
		var o application.GitHubRequestObservation
		var reset, observed string
		o.RunID = id
		if err := rows.Scan(&o.Operation, &o.Category, &o.HTTPStatus, &o.RequestID, &o.RateLimitLimit, &o.RateLimitRemaining, &reset, &o.ResponseDigest, &o.ErrorClass, &o.InstallationID, &o.Repository.ID, &o.Repository.NodeID, &o.Repository.Owner, &o.Repository.Name, &observed); err != nil {
			rows.Close()
			return inspection, err
		}
		o.RateLimitReset = parseTime(reset)
		o.ObservedAt = parseTime(observed)
		inspection.GitHubRequests = append(inspection.GitHubRequests, o)
	}
	rows.Close()
	var evidenceJSON, evidenceDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT evidence_json,evidence_digest FROM github_read_evidence WHERE run_id=? ORDER BY evidence_id DESC LIMIT 1`, id).Scan(&evidenceJSON, &evidenceDigest); err == nil {
		sum := sha256.Sum256([]byte(evidenceJSON))
		if hex.EncodeToString(sum[:]) != evidenceDigest {
			return inspection, errors.New("persisted GitHub evidence digest mismatch")
		}
		var e domain.GitHubReadEvidence
		if err := json.Unmarshal([]byte(evidenceJSON), &e); err != nil {
			return inspection, err
		}
		inspection.GitHubEvidence = &e
	} else if !errors.Is(err, sql.ErrNoRows) {
		return inspection, err
	}
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
