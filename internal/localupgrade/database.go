package localupgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"
	sqlitedriver "modernc.org/sqlite"
)

func sqliteURI(path string, readOnly bool) string {
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	if readOnly {
		return uri + "?mode=ro"
	}
	return uri
}

func inspectDatabaseReadOnly(path string, uid int) (databaseEvidence, error) {
	info, stat, err := safeRegularFile(path, uid, false)
	if err != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
		return databaseEvidence{}, errors.New("Controller database is unsafe")
	}
	db, err := sql.Open("sqlite", sqliteURI(path, true))
	if err != nil {
		return databaseEvidence{}, errors.New("Controller database evidence is unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil || version < 1 {
		return databaseEvidence{}, errors.New("Controller database schema is unavailable")
	}
	return databaseEvidence{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), SchemaVersion: version}, nil
}

func databaseStillMatches(path string, uid int, expected databaseEvidence) bool {
	current, err := inspectDatabaseReadOnly(path, uid)
	return err == nil && current == expected
}

type replacementAuthorityState struct {
	CanonicalConfigPath string `json:"canonical_config_path"`
	DatabasePath        string `json:"database_path"`
	DesiredID           int64  `json:"desired_id"`
	EffectiveID         int64  `json:"effective_id"`
	AuthorityVersion    int64  `json:"authority_version"`
	DesiredDigest       string `json:"desired_digest"`
	DesiredLifecycle    string `json:"desired_lifecycle"`
	EffectiveDigest     string `json:"effective_digest"`
	EffectiveLifecycle  string `json:"effective_lifecycle"`
	IntegrityGeneration int64  `json:"integrity_generation"`
	PublishedGeneration int64  `json:"published_generation"`
	IntegrityReadiness  string `json:"integrity_readiness"`
}

func verifyReplacementDatabase(ctx context.Context, path, configPath, configDigest, expectedReason string, expectedSchema, uid int) (databaseEvidence, replacementDatabaseVerification, error) {
	info, stat, err := safeRegularFile(path, uid, false)
	if err != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database is unsafe")
	}
	identity := databaseEvidence{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), SchemaVersion: expectedSchema}
	contentBefore, err := replacementDatabaseContentDigest(path, uid, identity)
	if err != nil {
		return databaseEvidence{}, replacementDatabaseVerification{}, err
	}
	dsn := sqliteURI(path, true) + "&_pragma=query_only(1)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database verification is unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database verification is unavailable")
	}
	defer conn.Close()
	var queryOnly, schema int
	if err := conn.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database is not query-only")
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&schema); err != nil || schema != expectedSchema {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database schema is incompatible")
	}
	identity.SchemaVersion = schema
	integrityRows, err := conn.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database integrity is unavailable")
	}
	integrityOK, integrityCount := true, 0
	for integrityRows.Next() {
		var value string
		if integrityRows.Scan(&value) != nil || value != "ok" {
			integrityOK = false
		}
		integrityCount++
	}
	if integrityRows.Err() != nil || integrityRows.Close() != nil || !integrityOK || integrityCount != 1 {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database integrity verification failed")
	}
	var foreignKeyViolations int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil || foreignKeyViolations != 0 {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database foreign-key verification failed")
	}
	var state replacementAuthorityState
	err = conn.QueryRowContext(ctx, `SELECT a.canonical_config_path,a.database_path,a.desired_generation_id,COALESCE(a.effective_generation_id,0),a.authority_version,d.digest,d.lifecycle,COALESCE(e.digest,''),COALESCE(e.lifecycle,''),g.generation,COALESCE(c.published_generation,0),COALESCE(o.effective_readiness,'') FROM configuration_authority a JOIN configuration_generations d ON d.generation_id=a.desired_generation_id LEFT JOIN configuration_generations e ON e.generation_id=a.effective_generation_id JOIN controller_integrity_generation g ON g.singleton=1 LEFT JOIN controller_integrity_current c ON c.singleton=1 LEFT JOIN controller_integrity_observations o ON o.observation_id=c.observation_id WHERE a.authority_id=1`).Scan(&state.CanonicalConfigPath, &state.DatabasePath, &state.DesiredID, &state.EffectiveID, &state.AuthorityVersion, &state.DesiredDigest, &state.DesiredLifecycle, &state.EffectiveDigest, &state.EffectiveLifecycle, &state.IntegrityGeneration, &state.PublishedGeneration, &state.IntegrityReadiness)
	if err != nil || state.CanonicalConfigPath != configPath || state.DatabasePath != path {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database binding conflicts")
	}
	var incompleteApplies, incompleteRecoveries int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM configuration_apply_intents WHERE status IN ('accepted','ambiguous')`).Scan(&incompleteApplies); err != nil {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement configuration authority is unavailable")
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM configuration_recovery_intents WHERE status IN ('accepted','ambiguous')`).Scan(&incompleteRecoveries); err != nil || incompleteApplies != 0 || incompleteRecoveries != 0 {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement configuration authority is unresolved")
	}
	if state.DesiredDigest != configDigest || state.DesiredLifecycle != "effective" && state.DesiredLifecycle != "pending_restart" {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement desired configuration authority conflicts")
	}
	readinessReason := replacementReadinessReason(state, configDigest)
	if readinessReason != expectedReason {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller readiness evidence changed")
	}
	authorityRaw, err := json.Marshal(state)
	if err != nil {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement configuration authority evidence is unavailable")
	}
	contentAfter, err := replacementDatabaseContentDigest(path, uid, identity)
	if err != nil || contentAfter != contentBefore {
		return databaseEvidence{}, replacementDatabaseVerification{}, errors.New("replacement Controller database changed during verification")
	}
	verification := replacementDatabaseVerification{
		ContentDigest: contentBefore, AuthorityDigest: sha256Hex(authorityRaw), SchemaVersion: schema,
		IntegrityOK: true, ForeignKeysOK: true, BindingMatches: true, DesiredConfigurationMatch: true, ReadinessReason: readinessReason,
	}
	return identity, verification, nil
}

func replacementReadinessReason(state replacementAuthorityState, configDigest string) string {
	if state.DesiredID != state.EffectiveID || state.DesiredDigest != configDigest || state.EffectiveDigest != configDigest || state.DesiredLifecycle != "effective" || state.EffectiveLifecycle != "effective" {
		return "configuration_not_converged"
	}
	if state.IntegrityGeneration != state.PublishedGeneration {
		return "integrity_pending"
	}
	switch state.IntegrityReadiness {
	case "ready":
		return "controller_ready"
	case "unknown":
		return "integrity_pending"
	case "not_ready", "conflict":
		return "integrity_" + state.IntegrityReadiness
	default:
		return "integrity_state_invalid"
	}
}

func replacementDatabaseContentDigest(path string, uid int, expected databaseEvidence) (string, error) {
	current, err := inspectDatabaseReadOnly(path, uid)
	if err != nil || current != expected {
		return "", errors.New("replacement Controller database identity changed")
	}
	mainDigest, err := digestFile(path)
	if err != nil {
		return "", errors.New("replacement Controller database content is unavailable")
	}
	evidence := struct {
		Main string `json:"main"`
		WAL  string `json:"wal"`
	}{Main: mainDigest, WAL: "absent"}
	walPath := path + "-wal"
	if _, err := os.Lstat(walPath); err == nil {
		info, stat, safeErr := safeRegularFile(walPath, uid, false)
		if safeErr != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
			return "", errors.New("replacement Controller database WAL is unsafe")
		}
		if evidence.WAL, err = digestFile(walPath); err != nil {
			return "", errors.New("replacement Controller database WAL is unavailable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("replacement Controller database WAL is unavailable")
	}
	if shmInfo, err := os.Lstat(path + "-shm"); err == nil {
		if !shmInfo.Mode().IsRegular() || shmInfo.Mode().Perm() != 0o600 || !ownedByUID(shmInfo, uid) {
			return "", errors.New("replacement Controller database shared memory is unsafe")
		}
		if stat, ok := shmInfo.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
			return "", errors.New("replacement Controller database shared memory is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("replacement Controller database shared memory is unavailable")
	}
	raw, _ := json.Marshal(evidence)
	return sha256Hex(raw), nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

type sqliteBackuper interface {
	NewBackup(string) (*sqlitedriver.Backup, error)
}

func createConsistentSnapshot(ctx context.Context, source, destination string, uid int, expected databaseEvidence) (string, error) {
	if exists(destination) || !databaseStillMatches(source, uid, expected) {
		return "", errors.New("SQLite snapshot authority is invalid")
	}
	temporary := destination + ".tmp"
	if exists(temporary) {
		return "", errors.New("SQLite snapshot temporary artifact already exists")
	}
	db, err := sql.Open("sqlite", sqliteURI(source, true))
	if err != nil {
		return "", errors.New("SQLite backup source is unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	connection, err := db.Conn(ctx)
	if err != nil {
		return "", errors.New("SQLite backup connection is unavailable")
	}
	defer connection.Close()
	err = connection.Raw(func(raw any) error {
		backuper, ok := raw.(sqliteBackuper)
		if !ok {
			return errors.New("pinned SQLite backup API is unavailable")
		}
		backup, err := backuper.NewBackup(sqliteURI(temporary, false))
		if err != nil {
			return err
		}
		more, stepErr := backup.Step(-1)
		finishErr := backup.Finish()
		if stepErr != nil || finishErr != nil || more {
			return errors.New("SQLite online backup did not complete")
		}
		return nil
	})
	if err != nil {
		_ = os.Remove(temporary)
		return "", errors.New("SQLite online backup failed")
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return "", errors.New("SQLite snapshot permissions are unavailable")
	}
	if err := verifySnapshot(temporary, uid, expected.SchemaVersion); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	file, err := os.OpenFile(temporary, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil || !safePrivateFileHandle(file, uid, 0) || file.Sync() != nil || file.Close() != nil {
		if file != nil {
			_ = file.Close()
		}
		_ = os.Remove(temporary)
		return "", errors.New("SQLite snapshot could not be synchronized")
	}
	if err := os.Rename(temporary, destination); err != nil || syncDirectory(filepath.Dir(destination)) != nil {
		_ = os.Remove(temporary)
		return "", errors.New("SQLite snapshot could not be published")
	}
	return digestFile(destination)
}

func verifySnapshot(path string, uid, expectedSchema int) error {
	info, stat, err := safeRegularFile(path, uid, false)
	if err != nil || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
		return errors.New("SQLite snapshot is unsafe")
	}
	db, err := sql.Open("sqlite", sqliteURI(path, true))
	if err != nil {
		return errors.New("SQLite snapshot verification is unavailable")
	}
	defer db.Close()
	var version int
	var integrity string
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil || version != expectedSchema {
		return errors.New("SQLite snapshot schema mismatch")
	}
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return errors.New("SQLite snapshot integrity verification failed")
	}
	return nil
}

func configurationAndIntegrityReadiness(ctx context.Context, path, configDigest string, expectedSchema int) (string, string) {
	db, err := sql.Open("sqlite", sqliteURI(path, true))
	if err != nil {
		return "conflict", "database_unavailable"
	}
	defer db.Close()
	var schema int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&schema); err != nil || schema != expectedSchema {
		return "conflict", "schema_mismatch"
	}
	var desiredID, effectiveID int64
	var desiredDigest, effectiveDigest, desiredLifecycle, effectiveLifecycle string
	err = db.QueryRowContext(ctx, `SELECT a.desired_generation_id,COALESCE(a.effective_generation_id,0),d.digest,d.lifecycle,COALESCE(e.digest,''),COALESCE(e.lifecycle,'') FROM configuration_authority a JOIN configuration_generations d ON d.generation_id=a.desired_generation_id LEFT JOIN configuration_generations e ON e.generation_id=a.effective_generation_id WHERE a.authority_id=1`).Scan(&desiredID, &effectiveID, &desiredDigest, &desiredLifecycle, &effectiveDigest, &effectiveLifecycle)
	if err != nil || desiredID != effectiveID || desiredDigest != configDigest || effectiveDigest != configDigest || desiredLifecycle != "effective" || effectiveLifecycle != "effective" {
		return "not_ready", "configuration_not_converged"
	}
	var generation, published int64
	var readiness string
	err = db.QueryRowContext(ctx, `SELECT g.generation,c.published_generation,o.effective_readiness FROM controller_integrity_generation g LEFT JOIN controller_integrity_current c ON c.singleton=1 LEFT JOIN controller_integrity_observations o ON o.observation_id=c.observation_id WHERE g.singleton=1`).Scan(&generation, &published, &readiness)
	if err != nil || generation != published {
		return "pending", "integrity_pending"
	}
	switch readiness {
	case "ready":
		return "ready", "controller_ready"
	case "unknown":
		return "pending", "integrity_pending"
	case "not_ready", "conflict":
		return "not_ready", "integrity_" + readiness
	default:
		return "conflict", "integrity_state_invalid"
	}
}
