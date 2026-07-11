// Package clitoken persists the drun-dash CLI's session token to a local file.
//
// Per ADR 0001 (§4, "CLI token storage"), the CLI stores its session token —
// the access token, optional refresh token, expiry, and API base URL — in a
// 0600 file inside a 0700 directory under the user config directory
// (os.UserConfigDir()/drun-dash/credentials.json, e.g.
// ~/.config/drun-dash/credentials.json on Linux or
// ~/Library/Application Support/drun-dash/credentials.json on macOS).
//
// This is a *user session* token, not an LLM master credential: a stolen file
// yields only a short-lived, centrally revocable session (see the ADR's threat
// model). OS-keychain storage is a documented future enhancement; file storage
// is the v1 mechanism.
//
// The directory can be overridden with the DRUN_DASH_CONFIG_DIR environment
// variable (see [ConfigDirEnv]), which is primarily useful for tests and for
// operators who keep configuration outside the platform default location.
package clitoken

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// appDir is the per-user subdirectory under os.UserConfigDir().
	appDir = "drun-dash"
	// fileName is the credentials file name inside appDir.
	fileName = "credentials.json"

	// ConfigDirEnv names the environment variable that overrides the directory
	// holding the credentials file. When set, its value is used verbatim as the
	// directory (no "drun-dash" suffix is appended).
	ConfigDirEnv = "DRUN_DASH_CONFIG_DIR"

	// dirPerm and filePerm are the required permissions for the config
	// directory and the credentials file respectively.
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// ErrNotFound is returned by [Store.Load] when no credentials file exists yet.
// Callers can detect "not logged in" with errors.Is(err, ErrNotFound).
var ErrNotFound = errors.New("clitoken: no stored token")

// Token is the CLI's persisted session token. It mirrors the fields the CLI
// needs from the control plane's TokenResponse plus the base URL it was
// obtained from, so later commands can reach the same server.
type Token struct {
	// AccessToken is the control-plane Bearer JWT.
	AccessToken string `json:"access_token"`
	// TokenType is the OAuth token type; always "Bearer" in v1.
	TokenType string `json:"token_type"`
	// ExpiresAt is the absolute expiry of the access token. Computed by the
	// caller from TokenResponse.expires_in at the time the token was issued.
	ExpiresAt time.Time `json:"expires_at"`
	// RefreshToken is the optional refresh token, used to mint a new access
	// token before this one expires. Empty when the server issued none.
	RefreshToken string `json:"refresh_token,omitempty"`
	// ServerURL is the API base URL the token was obtained from, so `whoami`
	// and other commands can default to the same control plane.
	ServerURL string `json:"server_url,omitempty"`
}

// Expired reports whether the access token's expiry has passed. A zero
// ExpiresAt is treated as "never expires" (unknown), returning false.
func (t Token) Expired() bool {
	return !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(time.Now())
}

// Store reads and writes the CLI credentials file in a single directory.
type Store struct {
	dir string
}

// New returns a Store rooted at an explicit directory. This is the form used
// by tests (with a temp dir) and by callers that resolve the directory
// themselves.
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Default returns the Store for the platform default location:
// os.UserConfigDir()/drun-dash, unless DRUN_DASH_CONFIG_DIR overrides it.
func Default() (*Store, error) {
	if d := os.Getenv(ConfigDirEnv); d != "" {
		return &Store{dir: d}, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("clitoken: resolve user config dir: %w", err)
	}
	return &Store{dir: filepath.Join(base, appDir)}, nil
}

// Dir returns the directory holding the credentials file.
func (s *Store) Dir() string { return s.dir }

// Path returns the full path to the credentials file.
func (s *Store) Path() string { return filepath.Join(s.dir, fileName) }

// Save writes the token as pretty-printed JSON, creating the config directory
// (mode 0700) if needed and forcing the file to mode 0600 even if it already
// existed with looser permissions.
func (s *Store) Save(t Token) error {
	if err := os.MkdirAll(s.dir, dirPerm); err != nil {
		return fmt.Errorf("clitoken: create config dir: %w", err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("clitoken: encode token: %w", err)
	}
	data = append(data, '\n')

	path := s.Path()
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("clitoken: write token: %w", err)
	}
	// os.WriteFile only applies the mode when creating the file; force it so a
	// pre-existing, world-readable file is tightened to 0600.
	if err := os.Chmod(path, filePerm); err != nil {
		return fmt.Errorf("clitoken: set token permissions: %w", err)
	}
	return nil
}

// Load reads and decodes the stored token. It returns [ErrNotFound] when no
// credentials file exists.
func (s *Store) Load() (Token, error) {
	data, err := os.ReadFile(s.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Token{}, ErrNotFound
		}
		return Token{}, fmt.Errorf("clitoken: read token: %w", err)
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return Token{}, fmt.Errorf("clitoken: decode token: %w", err)
	}
	return t, nil
}

// Clear removes the credentials file. Removing a token that is already absent
// is not an error.
func (s *Store) Clear() error {
	if err := os.Remove(s.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clitoken: remove token: %w", err)
	}
	return nil
}
