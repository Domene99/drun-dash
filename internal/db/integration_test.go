//go:build integration

// The db integration tests exercise a real Postgres 16 instance. They are
// excluded from the default build and only compile/run under the `integration`
// build tag:
//
//	go test -tags integration ./internal/db/...
//
// Requirements:
//   - A reachable Postgres. By default they use config.DefaultDatabaseURL (the
//     local Docker dev stack in deploy/docker); set DATABASE_URL to override.
//   - The repository migrations directory. By default it is resolved relative
//     to this package (../../migrations); set MIGRATIONS_PATH to override.
//
// If Postgres is unreachable the tests SKIP rather than fail, so a machine with
// no database does not produce a red build — mirroring pkg/drunmcp.
package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/config"
)

func integrationDatabaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return config.DefaultDatabaseURL
}

func integrationMigrationsPath() string {
	if v := os.Getenv("MIGRATIONS_PATH"); v != "" {
		return v
	}
	return "../../migrations"
}

// requirePool connects and pings, skipping the test if Postgres is unreachable.
func requirePool(t *testing.T) (*Pool, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	cfg := &config.Config{DatabaseURL: integrationDatabaseURL()}
	pool, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable at %s (%v); start it and rerun with -tags integration", cfg.DatabaseURL, err)
	}
	return pool, ctx
}

func TestIntegrationConnectPingClose(t *testing.T) {
	pool, ctx := requirePool(t)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestIntegrationMigrateVersionDown(t *testing.T) {
	// Ensure the database is reachable before running migrations.
	requirePool(t)

	url := integrationDatabaseURL()
	path := integrationMigrationsPath()

	// Apply every migration, then confirm the schema is at the latest version.
	if err := Migrate(url, path); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	version, dirty, err := Version(url, path)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if dirty {
		t.Fatalf("schema is dirty at version %d", version)
	}
	if version == 0 {
		t.Fatal("Version = 0 after Migrate, want the latest applied version")
	}
	t.Logf("migrated to schema version %d", version)

	// Migrate again: "no change" must be reported as success.
	if err := Migrate(url, path); err != nil {
		t.Fatalf("re-running Migrate should be a no-op, got: %v", err)
	}

	// Roll back one migration, verify the version dropped, then restore.
	if err := Down(url, path, 1); err != nil {
		t.Fatalf("Down(1): %v", err)
	}
	rolledBack, _, err := Version(url, path)
	if err != nil {
		t.Fatalf("Version after Down: %v", err)
	}
	if rolledBack != version-1 {
		t.Fatalf("version after Down(1) = %d, want %d", rolledBack, version-1)
	}

	if err := Migrate(url, path); err != nil {
		t.Fatalf("restoring migrations: %v", err)
	}
	restored, _, err := Version(url, path)
	if err != nil {
		t.Fatalf("Version after restore: %v", err)
	}
	if restored != version {
		t.Fatalf("version after restore = %d, want %d", restored, version)
	}
}
