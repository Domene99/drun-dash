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
| `policies list`     | real   | 0/1  | Lists policies via `GET /policies`, with `--limit`/`--cursor` paging. |
| `policies get <id>` | real   | 0/1  | Shows one policy via `GET /policies/{policyId}`; a `404` is a clear not-found. |
| `policies versions <id>` | real | 0/1 | Lists a policy's signed versions via `GET /policies/{policyId}/versions`, each with its digest. |
| `sessions list`     | real   | 0/1  | Lists governed sessions via `GET /sessions`, with `--limit`/`--cursor` paging. |
| `sessions get <id>` | real   | 0/1  | Shows one session via `GET /sessions/{sessionId}`; a `404` is a clear not-found. |
| `sessions launch --agent <id>` | real | 0/1 | Launches a governed session via `POST /sessions/launch`; prints a summary + one-time `proxy_token`, `--json` dumps the full response; `403` is a clear "not entitled". |
| `sessions terminate <id>` | real | 0/1 | Terminates a session via `POST /sessions/{sessionId}/terminate` (idempotent); unknown/forbidden surfaces the flat `Error`. |
| `approvals list`    | real   | 0/1  | Lists escalation-queue requests via `GET /approvals`, with `--limit`/`--cursor` paging; pending rows are marked. |
| `approvals decide <id>` | real | 0/1/2 | Decides a request via `POST /approvals/{approvalId}/decision`; `--decision` is validated to `approve`/`deny` before the call; a `409` (already decided) is a clear message. |
| `spend`             | real   | 0/1  | Queries `GET /analytics/spend` with `--from`/`--to`/`--group-by`; prints total + groups. |
| `config` (`env`)    | real   | 0    | Loads `config.Load()` and prints the resolved runtime configuration. |
| `version` / `--version` | real | 0 | Prints `Version`. |
| `help` / `-h` / `--help` | real | 0 | Prints usage listing every subcommand. |
| no args / unknown   | —      | 2    | Prints usage on stderr. |

The singular `session` is an alias for `sessions`, so `session launch …` and
`sessions launch …` are equivalent (likewise for `get`, `list`, `terminate`).

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

## Policies (`policies`)

Read-only access to the organization's policy boundaries and their immutable,
signed versions, using the same Bearer-token and `--server` mechanism as
`agents`, against `openapi/v1.yaml`. Create/publish (`POST /policies`,
`POST /policies/{policyId}/versions`) are deliberately not exposed — the CLI is
read-only here. All three live in `policies.go`.

- `policies list [--limit N] [--cursor C]` — `GET /policies` (`listPolicies`).
  Prints the returned `Policy` items as an aligned table (id, name, current
  version, description). `--limit` / `--cursor` are passed through as query
  params, and a `next_cursor` yields a "More available — re-run with --cursor …"
  hint.
- `policies get <policyId>` — `GET /policies/{policyId}` (`getPolicy`). Prints
  the policy detail (including its `current_version` pointer); a `404` prints a
  clear not-found message and exits non-zero.
- `policies versions <policyId>` — `GET /policies/{policyId}/versions`
  (`listPolicyVersions`). Lists the policy's signed versions as an aligned table,
  each row carrying the version number, content `digest`, and signing
  algorithm/key; a `404` (unknown policy) is a clear not-found.

With no stored token, all three print "Not logged in." and exit non-zero without
sending a request. Any other non-2xx response is surfaced via the flat `Error`
envelope.

## Sessions (`sessions` / `session`)

Governed agent sessions, using the same Bearer-token and `--server` mechanism as
`agents`, against `openapi/v1.yaml`. The read-only halves (`list.go`/`get`) live
in `sessions.go`; the lifecycle halves (`launch`/`terminate`) in
`session_launch.go`.

- `sessions list [--limit N] [--cursor C]` — `GET /sessions` (`listSessions`).
  Prints the returned `Session` items as an aligned table (session id, agent id,
  status, fan-out ceiling, created-at). `--limit` / `--cursor` are passed through
  as query params, and a `next_cursor` yields a "More available — re-run with
  --cursor …" hint.
- `sessions get <sessionId>` — `GET /sessions/{sessionId}` (`getSession`). Prints
  the session detail; a `404` prints a clear not-found message and exits
  non-zero.
- `sessions launch --agent <id> [--agent-version N] [--fan-out N] [--json]` —
  `POST /sessions/launch` (`launchSession`). Sends the `SessionLaunchRequest`
  (`--agent-version` and `--fan-out` are omitted from the body when unset), and
  on `201` prints a readable summary — session id, proxy base URL, fan-out
  ceiling, expiry — followed by the scoped **`proxy_token`**. That token is the
  session's LLM-proxy credential: it is a **secret shown once**, printed to
  stdout for the operator but never written to any file or log. `--json` dumps
  the full `SessionLaunchResponse` (including the large `policy_bundle` and the
  `proxy_token`) as indented JSON for scripting. A `403` prints a clear
  "not entitled to launch agent …" message and exits non-zero.
- `sessions terminate <sessionId>` — `POST /sessions/{sessionId}/terminate`
  (`terminateSession`). Reports success; the endpoint is idempotent once the
  session is terminal. An unknown or forbidden session surfaces the flat `Error`
  envelope and exits non-zero.

> The engine-side consumption of the policy bundle (writing it to disk,
> configuring the local drun engine) is a later batch — for now `launch` only
> displays / `--json`-dumps the response.

## Approvals (`approvals`)

The human reviewer's view of the escalation queue, using the same Bearer-token
and `--server` mechanism, against `openapi/v1.yaml`. Creating approvals is a
server/agent-side action (the LLM proxy raises them on a fail-closed decision),
so the CLI focuses on the reviewer's list + decide. Both live in `approvals.go`.

- `approvals list [--limit N] [--cursor C]` — `GET /approvals` (`listApprovals`).
  Prints the returned `Approval` items as an aligned table (id, session id,
  action, status, created-at), marking **pending** rows (the ones awaiting a
  decision) with a `>`. `--limit` / `--cursor` are passed through as query
  params, and a `next_cursor` yields a "More available — re-run with --cursor …"
  hint.
- `approvals decide <approvalId> --decision approve|deny [--reason TEXT]` —
  `POST /approvals/{approvalId}/decision` (`decideApproval`). Sends the
  `ApprovalDecision` and prints the recorded approval detail. `--decision` is
  validated to `approve`/`deny` **before** the call — an invalid or missing
  value is a usage error (exit 2, no request sent). A `409` (the request was
  already decided) prints a clear message and exits non-zero.

## Spend (`spend`)

Read-only access to aggregated LLM spend, using the same Bearer-token and
`--server` mechanism, against `openapi/v1.yaml`:

- `spend [--from RFC3339] [--to RFC3339] [--group-by agent|policy|principal]` —
  `GET /analytics/spend` (`querySpend`). Prints the query window and total, then
  a per-group table (key, amount, tokens). `--from` / `--to` / `--group-by` are
  passed through as query params, and a bad value (e.g. an unknown `group_by`)
  surfaces the server's `400` flat `Error` (`code` + `message`).

With no stored token, `sessions`, `approvals`, and `spend` print "Not logged in."
and exit non-zero without sending a request. Any other non-2xx response is
surfaced via the flat `Error` envelope.

### Configuration

| Setting | Flag | Env | Default |
|---------|------|-----|---------|
| Control-plane base URL | `--server URL` | `DRUN_DASH_SERVER` | `http://localhost:8080` |
| CLI client id | `--client-id ID` | — | `drun-dash-cli` |
| Requested scopes | `--scope SCOPES` | — | (none) |
| Token storage dir | — | `DRUN_DASH_CONFIG_DIR` | `os.UserConfigDir()/drun-dash` |

`whoami`, `agents`, `policies`, `sessions`, `approvals`, and `spend` accept `--server` to
override the base URL stored with the token; otherwise they reuse the server
`login` authenticated against.

## Testing

Commands are driven against an `httptest` mock of the control-plane endpoints —
no real network (an in-memory `http.RoundTripper` serves the handler, so nothing
binds a socket). The device poll loop takes an injectable `clock` so `slow_down`
and expiry are tested deterministically without real sleeps, and the `agents`,
`sessions`, `approvals`, and `spend` commands assert the Bearer token,
query/pagination pass-through, the not-found and bad-request paths, and the
not-logged-in path. The `sessions launch`/`terminate` tests additionally assert
the POST body, the summary vs. `--json` output, the one-time `proxy_token`
surfacing, and the `403`/flat-`Error` paths. The `approvals decide` tests assert
the POST body, `--decision` validation, and the `409` (already decided) path.
Token storage is redirected to a temp dir via `DRUN_DASH_CONFIG_DIR`.

## Not implemented yet

Policy sync and drun engine control are deferred to later batches and marked
with `// TODO(batch/…)` comments in their stub files.
