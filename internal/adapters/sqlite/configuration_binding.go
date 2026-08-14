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
// prepared baseline anchor that exists before locator publication.
func InspectConfigurationBindingReadOnly(ctx context.Context, path string) (string, string, bool, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", false, errors.New("configuration database binding is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", "", false, errors.New("configuration database binding is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		return "", "", false, errors.New("configuration database binding is unsafe")
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", "", false, errors.New("configuration database binding is unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil || version != schemaVersion {
		return "", "", false, errors.New("configuration database binding is unavailable")
	}
	var configPath, databasePath string
	err = db.QueryRowContext(ctx, `SELECT canonical_config_path,database_path FROM configuration_authority WHERE authority_id=1`).Scan(&configPath, &databasePath)
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
