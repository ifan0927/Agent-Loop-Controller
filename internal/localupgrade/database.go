package localupgrade

import (
	"context"
	"database/sql"
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
