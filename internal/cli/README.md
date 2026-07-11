# internal/cli

Command tree and dispatch for the `drun-dash` developer CLI. The
`cmd/drun-dash` binary is a one-liner that calls [`Run`](cli.go); all logic
lives here so tests exercise every subcommand in-process with buffers.

## Design

- **stdlib only** — dispatch is a `switch` on the first argument with a
  per-command `flag.FlagSet`, mirroring `cmd/policyctl`. No cobra/urfave or any
  CLI framework, and no new dependencies.
- **`Run(args, stdout, stderr) int`** — returns a process exit code and never
  calls `os.Exit`, so `main` stays trivial and tests need no subprocess.
- **Version** — `Version` defaults to `"dev"` and is overridable at build time:
  `-ldflags "-X github.com/Domene99/drun-dash/internal/cli.Version=v1.2.3"`.

## Commands

| Command             | Status | Exit | Notes |
|---------------------|--------|------|-------|
| `login`             | real   | 0/1  | Runs the OAuth 2.0 device-authorization flow and stores the session token. |
| `whoami`            | real   | 0/1  | Reports the authenticated member via `GET /auth/whoami`, or "Not logged in". |
| `agents list`       | real   | 0/1  | Lists catalog agents via `GET /agents`, with `--limit`/`--cursor` paging. |
| `agents get <id>`   | real   | 0/1  | Shows one agent via `GET /agents/{agentId}`; a `404` is a clear not-found. |
| `config` (`env`)    | real   | 0    | Loads `config.Load()` and prints the resolved runtime configuration. |
| `version` / `--version` | real | 0 | Prints `Version`. |
| `help` / `-h` / `--help` | real | 0 | Prints usage listing every subcommand. |
| no args / unknown   | —      | 2    | Prints usage on stderr. |

## Authentication (`login` / `whoami`)

`login` implements the **client side** of the OAuth 2.0 device-authorization
grant (RFC 8628), per [ADR 0001](../../docs/adr/0001-authentication-and-session-model.md)
§2, against the control-plane contract in `openapi/v1.yaml`:

1. `POST /auth/device/code` — request a `device_code` + `user_code`.
2. Print the `verification_uri` and `user_code` for the operator to open in a
   browser.
3. `POST /auth/device/token` — poll at the server-supplied `interval`, honoring
   `authorization_pending` (keep waiting), `slow_down` (back off by 5s),
   `access_denied` / `expired_token` (stop), until the code expires or a
   `TokenResponse` is returned.
4. Persist the token via [`internal/clitoken`](../clitoken) (a `0600` file).

`whoami` loads the stored token and calls `GET /auth/whoami` with
`Authorization: Bearer <token>`, printing the returned `Identity`.

## Agent catalog (`agents`)

Read-only access to the organization's agent catalog, using the token stored by
`login` (`Authorization: Bearer <token>`), against `openapi/v1.yaml`:

- `agents list [--limit N] [--cursor C]` — `GET /agents` (`listAgents`). Prints
  the returned `Agent` items as an aligned table. `--limit` / `--cursor` are
  passed through as query params, and when the response carries a `next_cursor`
  a "More available — re-run with --cursor …" hint is printed.
- `agents get <agentId>` — `GET /agents/{agentId}` (`getAgent`). Prints the
  agent detail; a `404` prints a clear not-found message and exits non-zero.

With no stored token, both print "Not logged in." and exit non-zero without
sending a request. Any other non-2xx response is surfaced via the flat `Error`
envelope (`code` + `message`). `--server` overrides the base URL stored with the
token, exactly as `whoami` does.

### Configuration

| Setting | Flag | Env | Default |
|---------|------|-----|---------|
| Control-plane base URL | `--server URL` | `DRUN_DASH_SERVER` | `http://localhost:8080` |
| CLI client id | `--client-id ID` | — | `drun-dash-cli` |
| Requested scopes | `--scope SCOPES` | — | (none) |
| Token storage dir | — | `DRUN_DASH_CONFIG_DIR` | `os.UserConfigDir()/drun-dash` |

`whoami` and `agents` accept `--server` to override the base URL stored with the
token; otherwise they reuse the server `login` authenticated against.

## Testing

Commands are driven against an `httptest` mock of the control-plane endpoints —
no real network. The device poll loop takes an injectable `clock` so `slow_down`
and expiry are tested deterministically without real sleeps, and the `agents`
commands assert the Bearer token, pagination pass-through, the not-found path,
and the not-logged-in path. Token storage is redirected to a temp dir via
`DRUN_DASH_CONFIG_DIR`.

## Not implemented yet

Policy sync and drun session control are deferred to later batches and marked
with `// TODO(batch/…)` comments in their stub files.
