# internal/clitoken

Local persistence for the `drun-dash` CLI's **session token** — the token
obtained from the OAuth 2.0 device-authorization flow (see
[`internal/cli`](../cli)).

## Storage

Per [ADR 0001](../../docs/adr/0001-authentication-and-session-model.md) §4, the
token is written to a **`0600` file** inside a **`0700` directory** under the
user config directory:

```
os.UserConfigDir()/drun-dash/credentials.json
  ~/.config/drun-dash/credentials.json                          (Linux)
  ~/Library/Application Support/drun-dash/credentials.json      (macOS)
```

The directory can be overridden with the `DRUN_DASH_CONFIG_DIR` environment
variable (used verbatim, no `drun-dash` suffix) — primarily for tests and for
operators who keep config outside the platform default.

`Save` forces mode `0600` even if the file already existed with looser
permissions.

## Why a file (not the keychain)

This is a **user session token**, not an LLM master credential. A stolen file
yields only a short-lived, scoped, centrally revocable session — the exact case
the threat model already bounds. OS-keychain storage (macOS Keychain, Windows
Credential Manager, libsecret) behind the same `Store` interface is a
documented **future enhancement**, not part of v1.

## API

```go
type Token struct {
    AccessToken  string    // control-plane Bearer JWT
    TokenType    string    // "Bearer"
    ExpiresAt    time.Time // absolute expiry
    RefreshToken string    // optional
    ServerURL    string    // API base URL the token came from
}

func New(dir string) *Store       // explicit directory (tests)
func Default() (*Store, error)     // platform default, honoring DRUN_DASH_CONFIG_DIR

func (s *Store) Save(t Token) error
func (s *Store) Load() (Token, error)  // ErrNotFound when absent
func (s *Store) Clear() error          // removing an absent token is not an error
func (s *Store) Path() string
func (t Token) Expired() bool
```
