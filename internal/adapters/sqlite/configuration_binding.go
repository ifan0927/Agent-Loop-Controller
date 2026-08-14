package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// InspectConfigurationBindingReadOnly proves a locator target without
// creating or migrating it. It accepts either completed authority or the
// prepared baseline anchor that exists before locator publication. A trusted
// binding from the configuration-authority schema may be older than this
// binary so Open can perform the normal forward migration afterwards.
func InspectConfigurationBindingReadOnly(ctx context.Context, path string) (string, string, bool, error) {
	return inspectConfigurationBindingReadOnly(ctx, path, schemaVersion)
}

func inspectConfigurationBindingReadOnly(ctx context.Context, path string, supportedVersion int) (string, string, bool, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", false, errors.New("configuration database binding is invalid")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", "", false, errors.New("configuration database binding is unsafe")
	}
	defer file.Close()
	identity, err := safeDatabaseFileIdentity(file)
	if err != nil {
		return "", "", false, errors.New("configuration database binding is unsafe")
	}
	if info, statErr := file.Stat(); statErr != nil || info.Mode().Perm() != 0o600 {
		return "", "", false, errors.New("configuration database binding is unsafe")
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", "", false, errors.New("configuration database binding is unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", "", false, errors.New("configuration database binding is unavailable")
	}
	defer conn.Close()
	if err := conn.QueryRowContext(ctx, `SELECT 1`).Scan(new(int)); err != nil || !databasePathStillIdentifies(path, identity) {
		return "", "", false, errors.New("configuration database binding changed while opening")
	}
	var version int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil || supportedVersion < 31 || version < 31 || version > supportedVersion {
		return "", "", false, errors.New("configuration database binding is unavailable")
	}
	return configurationBindingQuery(ctx, conn)
}

type configurationBindingQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func configurationBindingQuery(ctx context.Context, db configurationBindingQuerier) (string, string, bool, error) {
	var configPath, databasePath string
	err := db.QueryRowContext(ctx, `SELECT canonical_config_path,database_path FROM configuration_authority WHERE authority_id=1`).Scan(&configPath, &databasePath)
	if err == nil {
		return configPath, databasePath, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", false, errors.New("configuration database binding is unavailable")
	}
	err = db.QueryRowContext(ctx, `SELECT canonical_config_path,database_path FROM configuration_baseline_anchor WHERE authority_id=1`).Scan(&configPath, &databasePath)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, errors.New("configuration database binding is unavailable")
	}
	return configPath, databasePath, true, nil
}
