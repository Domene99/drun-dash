package db

import (
	"context"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/config"
)

// These tests exercise the URL-handling and error paths that need no live
// database. Anything requiring a real Postgres lives in integration_test.go,
// behind the `integration` build tag.

func TestConnectNilConfig(t *testing.T) {
	if _, err := Connect(context.Background(), nil); err == nil {
		t.Fatal("Connect(nil) = nil error, want error")
	}
}

func TestConnectInvalidDatabaseURL(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://user:pass@host:notaport/db"}
	if _, err := Connect(context.Background(), cfg); err == nil {
		t.Fatal("Connect with invalid URL = nil error, want error")
	}
}

func TestNormalizeDatabaseURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "postgres scheme rewritten to pgx5",
			in:   "postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable",
			want: "pgx5://drundash:drundash@localhost:5432/drundash?sslmode=disable",
		},
		{
			name: "postgresql scheme rewritten to pgx5",
			in:   "postgresql://u:p@db:5432/app",
			want: "pgx5://u:p@db:5432/app",
		},
		{
			name: "pgx5 scheme preserved",
			in:   "pgx5://u:p@db:5432/app",
			want: "pgx5://u:p@db:5432/app",
		},
		{
			name:    "unsupported scheme rejected",
			in:      "mysql://u:p@db:3306/app",
			wantErr: true,
		},
		{
			name:    "unparseable url rejected",
			in:      "://not a url",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeDatabaseURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeDatabaseURL(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDatabaseURL(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeDatabaseURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFileSourceURL(t *testing.T) {
	got, err := fileSourceURL("migrations")
	if err != nil {
		t.Fatalf("fileSourceURL: %v", err)
	}
	if !strings.HasPrefix(got, "file://") {
		t.Fatalf("fileSourceURL = %q, want file:// prefix", got)
	}
	if !strings.HasSuffix(got, "/migrations") {
		t.Fatalf("fileSourceURL = %q, want /migrations suffix", got)
	}
	// The path must be absolute so the migrator is independent of the working
	// directory: file:// followed by a leading slash.
	if !strings.HasPrefix(got, "file:///") {
		t.Fatalf("fileSourceURL = %q, want absolute path", got)
	}
}

// The migration entry points must reject an unusable database URL before they
// ever attempt to open a connection, so these paths are exercisable with no DB.
func TestMigrationFuncsRejectBadScheme(t *testing.T) {
	const badURL = "mysql://u:p@db:3306/app"

	if err := Migrate(badURL, "migrations"); err == nil {
		t.Error("Migrate with bad scheme = nil error, want error")
	}
	if _, _, err := Version(badURL, "migrations"); err == nil {
		t.Error("Version with bad scheme = nil error, want error")
	}
	if err := Down(badURL, "migrations", 1); err == nil {
		t.Error("Down with bad scheme = nil error, want error")
	}
}
