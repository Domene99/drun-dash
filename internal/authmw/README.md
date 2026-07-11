# authmw

HTTP authentication middleware that fronts drun-dash's protected control-plane
and proxy routes. It turns a validated session JWT into a request-scoped
principal (an `auth.Claims`) per
[ADR-0001 §5](../../docs/adr/0001-authentication-and-session-model.md) and
[ADR-0002 §1–§2](../../docs/adr/0002-rest-conventions.md).

This package is a thin adapter over the token core in
[`internal/auth`](../auth): it extracts the Bearer token from the
`Authorization` header and delegates every cryptographic and revocation check to
`auth.Verifier.Verify`. It wires **no routes** and is mounted into no binary
here — a later batch mounts `RequireAuth` in front of the protected route
groups.

Standard library only (`net/http`, `context`) plus `internal/auth` and
`internal/httpx`. No new dependencies.

## API

| Symbol | Purpose |
| --- | --- |
| `RequireAuth(v *auth.Verifier) httpx.Middleware` | Authenticate each request: extract the Bearer token, `v.Verify` it, and on success store the `*auth.Claims` in the request context before calling the next handler. |
| `PrincipalFromContext(ctx) (*auth.Claims, bool)` | Read the principal stored by `RequireAuth`; `ok` is false for any context that did not pass through the middleware. |
| `MustPrincipal(ctx) *auth.Claims` | Read the principal for handlers mounted **behind** `RequireAuth`; panics if absent (a wiring bug, not an auth failure). |

## 401 is minted here, not in handlers

Per ADR-0002 §2, a missing, malformed, expired, or revoked token yields a flat
HTTP 401 with code `unauthorized` (`httpx.CodeUnauthorized`). `RequireAuth` is
the single place that mints that 401, so handlers behind it can assume a
principal is always present.

Every failure path — absent `Authorization` header, non-`Bearer` scheme, empty
token, or any `Verify` error (expired, bad signature, wrong `iss`/`aud`,
revoked, missing `jti`) — returns the **same generic 401 body**. The response
never discloses which check failed, so it hands an attacker no oracle;
per-failure detail is a server-side logging concern handled elsewhere.

```json
{ "code": "unauthorized", "message": "authentication required" }
```

## Usage

Mount `RequireAuth` in front of a protected route group, then read the principal
in handlers with `MustPrincipal`:

```go
import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/Domene99/drun-dash/internal/authmw"
)

func protectedRoutes(v *auth.Verifier) http.Handler {
	r := chi.NewRouter()
	r.Use(authmw.RequireAuth(v)) // 401s before any handler runs

	r.Get("/v1/whoami", func(w http.ResponseWriter, req *http.Request) {
		// Guaranteed present: the request reached here only past RequireAuth.
		p := authmw.MustPrincipal(req.Context())
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"org_id": p.OrgID,
			"mid":    p.MemberID,
			"roles":  p.Roles,
		})
	})
	return r
}
```

Outside a guaranteed-authenticated path — for example middleware that may run
before or without `RequireAuth` — use the comma-ok form instead of risking a
panic:

```go
if p, ok := authmw.PrincipalFromContext(req.Context()); ok {
	// ... use p ...
}
```

## The principal is unforgeable from outside

The context key is an unexported type, so the stored `*auth.Claims` cannot
collide with another package's context keys and cannot be read, set, or forged
from outside `authmw`. The only way a principal lands in the context is a
successful `RequireAuth` verification.

## Tests

`authmw_test.go` is table-driven over a **real** `auth.Verifier` — built from a
generated ed25519 keypair via `auth.NewIssuer`/`auth.NewVerifier` and an
in-test `fakeRevoker` — with `net/http/httptest`. No Redis or network. It
covers: a valid token (principal in context + 200), absent header, empty header,
non-`Bearer` scheme, bearer-without-token, malformed token, expired token, wrong
audience, bad signature, revoked token, and a fail-closed revocation backend
(all 401 flat `Error`), plus `MustPrincipal` panicking without the middleware.
