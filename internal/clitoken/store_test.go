package clitoken

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	s := New(t.TempDir())

	want := Token{
		AccessToken:  "access-abc",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second),
		RefreshToken: "refresh-xyz",
		ServerURL:    "https://cp.example.com",
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken ||
		got.TokenType != want.TokenType ||
		got.RefreshToken != want.RefreshToken ||
		got.ServerURL != want.ServerURL {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
}

func TestSavePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	s := New(dir)
	if err := s.Save(Token{AccessToken: "a", TokenType: "Bearer"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != filePerm {
		t.Errorf("file perm = %o, want %o", perm, filePerm)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != dirPerm {
		t.Errorf("dir perm = %o, want %o", perm, dirPerm)
	}
}

func TestSaveTightensExistingLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	path := s.Path()

	// Pre-create a world-readable credentials file.
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if err := s.Save(Token{AccessToken: "a", TokenType: "Bearer"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != filePerm {
		t.Errorf("file perm after overwrite = %o, want %o", perm, filePerm)
	}
}

func TestLoadMissingReturnsErrNotFound(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load on empty dir = %v, want ErrNotFound", err)
	}
}

func TestClear(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Save(Token{AccessToken: "a", TokenType: "Bearer"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load after Clear = %v, want ErrNotFound", err)
	}
	// Clearing an already-absent token is not an error.
	if err := s.Clear(); err != nil {
		t.Errorf("Clear on missing file = %v, want nil", err)
	}
}

func TestDefaultHonorsConfigDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ConfigDirEnv, dir)

	s, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if s.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", s.Dir(), dir)
	}
	if err := s.Save(Token{AccessToken: "a", TokenType: "Bearer"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Errorf("expected credentials file in override dir: %v", err)
	}
}

func TestExpired(t *testing.T) {
	cases := []struct {
		name string
		tok  Token
		want bool
	}{
		{"zero expiry treated as never", Token{}, false},
		{"future", Token{ExpiresAt: time.Now().Add(time.Hour)}, false},
		{"past", Token{ExpiresAt: time.Now().Add(-time.Hour)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tok.Expired(); got != tc.want {
				t.Errorf("Expired() = %v, want %v", got, tc.want)
			}
		})
	}
}
