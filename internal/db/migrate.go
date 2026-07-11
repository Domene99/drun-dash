package db

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/golang-migrate/migrate/v4"
	// Register the pgx/v5 database driver (URL scheme "pgx5") and the
	// filesystem source driver (URL scheme "file"). Both are blank imports:
	// golang-migrate resolves them by scheme at runtime.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// DefaultMigrationsPath is the repository-relative directory holding the
// golang-migrate SQL files (000001_*.up.sql … 000006_*.down.sql). Callers that
// run from the repo root can pass "" to accept this default.
const DefaultMigrationsPath = "migrations"

// Migrate applies every pending up migration found under migrationsPath against
// the database at databaseURL. An empty migrationsPath falls back to
// [DefaultMigrationsPath]. "No change" — the database is already at the latest
// version — is treated as success, not an error, so Migrate is safe to run on
// every boot.
func Migrate(databaseURL, migrationsPath string) error {
	m, err := newMigrator(databaseURL, migrationsPath)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}

// Version reports the schema version currently recorded in the database and
// whether that version is dirty (a previous migration failed part-way and left
// the schema in an unknown state). A database with no migrations applied yet
// returns version 0 with dirty false and a nil error.
func Version(databaseURL, migrationsPath string) (version uint, dirty bool, err error) {
	m, err := newMigrator(databaseURL, migrationsPath)
	if err != nil {
		return 0, false, err
	}
	defer m.Close()

	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("db: migrate version: %w", err)
	}
	return version, dirty, nil
}

// Down reverses migrations. When steps is positive it rolls back that many
// migrations; when steps is zero or negative it rolls the schema all the way
// back down to nothing. "No change" — there is nothing left to reverse — is
// treated as success.
func Down(databaseURL, migrationsPath string, steps int) error {
	m, err := newMigrator(databaseURL, migrationsPath)
	if err != nil {
		return err
	}
	defer m.Close()

	if steps > 0 {
		err = m.Steps(-steps)
	} else {
		err = m.Down()
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate down: %w", err)
	}
	return nil
}

// newMigrator wires a golang-migrate instance from a filesystem source and the
// pgx/v5 database driver. The caller owns the returned *migrate.Migrate and
// must Close it.
func newMigrator(databaseURL, migrationsPath string) (*migrate.Migrate, error) {
	if migrationsPath == "" {
		migrationsPath = DefaultMigrationsPath
	}

	dbURL, err := normalizeDatabaseURL(databaseURL)
	if err != nil {
		return nil, err
	}

	sourceURL, err := fileSourceURL(migrationsPath)
	if err != nil {
		return nil, err
	}

	m, err := migrate.New(sourceURL, dbURL)
	if err != nil {
		return nil, fmt.Errorf("db: init migrator: %w", err)
	}
	return m, nil
}

// normalizeDatabaseURL rewrites a Postgres connection string to the "pgx5"
// scheme expected by golang-migrate's pgx/v5 database driver. The rest of the
// URL (host, credentials, query parameters such as sslmode) is preserved so a
// single DATABASE_URL works for both the runtime pool and the migrator.
func normalizeDatabaseURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("db: parse database url: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql", "pgx", "pgx5":
		u.Scheme = "pgx5"
	default:
		return "", fmt.Errorf("db: unsupported database url scheme %q (want postgres or postgresql)", u.Scheme)
	}
	return u.String(), nil
}

// fileSourceURL builds an absolute file:// source URL for golang-migrate's
// filesystem source driver. The path is resolved to an absolute location so the
// migrator does not depend on the process working directory.
func fileSourceURL(migrationsPath string) (string, error) {
	abs, err := filepath.Abs(migrationsPath)
	if err != nil {
		return "", fmt.Errorf("db: resolve migrations path: %w", err)
	}
	return "file://" + filepath.ToSlash(abs), nil
}
