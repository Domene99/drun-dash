# httpapi

Shared helper library for the drun-dash control-plane REST handlers (agents,
policies, sessions, approvals, analytics). It holds the cross-cutting primitives
those handlers reuse so their behavior stays identical without the handlers
coordinating. It contains **no routes and no resource handlers** — only the
shared primitives.

Every convention here is decided in
[ADR-0002](../../docs/adr/0002-rest-api-conventions.md) (REST API conventions).
Read §1 (org scoping), §2 (error mapping), and §3 (pagination) — this package
implements them exactly. Response bodies come from
[`internal/httpx`](../httpx) (the flat `Error` envelope, the `Code…` constants);
this package layers the control-plane semantics on top.

Standard library only, plus `internal/httpx` and `internal/store`. No new deps.

## Error mapping (`errors.go`)

`WriteStoreError(w, err)` is the canonical ADR-0002 §2 mapping from a store
error to an HTTP response:

| `err` | HTTP | code |
| --- | --- | --- |
| `errors.Is(err, store.ErrNotFound)` | 404 | `not_found` |
| `errors.Is(err, store.ErrConflict)` | 409 | `conflict` |
| anything else | 500 | `internal_error` |

The 500 path writes a generic message and **never** echoes the raw error to the
client — log the underlying error yourself (with the request id) before/after the
call. `store.ErrConflict` is wrapped with the constraint name (`%w on
<constraint>`); `errors.Is` still matches and the constraint name is not
surfaced.

Convenience wrappers for the non-store cases:

| Helper | Result |
| --- | --- |
| `WriteValidationError(w, field, msg)` | 400 `bad_request` with `details.field = field` |
| `WriteForbidden(w, msg)` | 403 `forbidden` (only where the contract documents a 403) |
| `WriteNotFound(w, msg)` | 404 `not_found` (missing rows **and** cross-org id references) |

```go
row, err := s.GetByID(ctx, id)
if err != nil {
	httpapi.WriteStoreError(w, err) // ErrNotFound → 404, else → 500
	return
}
if req.Name == "" {
	httpapi.WriteValidationError(w, "name", "name is required")
	return
}
```

## Ownership (`ownership.go`)

`SameOrg(callerOrgID, rowOrgID) bool` is the ADR-0002 §1 tenant-isolation check.
The store's by-id lookups are **not** org-scoped, so after loading a row by id a
handler must confirm it belongs to the caller's org (derived from the token, per
ADR-0001). **Contract: on a mismatch the handler returns `404 not_found`, never
`403`** — a 403 would leak that the resource exists in another tenant.

```go
if !httpapi.SameOrg(caller.OrgID, row.OrgID) {
	httpapi.WriteNotFound(w, "resource not found") // 404, not 403
	return
}
```

For **list** and **create**, prefer the store's org-scoped methods
(`ListByOrg(ctx, orgID)`, `Create(ctx, orgID, …)`) over load-then-check — the
`WHERE org_id = $1` guarantees the result never crosses tenants.

## Pagination (`pagination.go`)

Keyset (seek) pagination over `(created_at, id)`, per ADR-0002 §3. Applies only
to the five paginated list operations (`listAgents`, `listPolicies`,
`listSessions`, `listTrajectoryEvents`, `listApprovals`); the nested lists and
analytics reads are unpaginated. This package encodes/decodes the cursor and
parses params — the **store** performs the seek.

| Symbol | Behavior |
| --- | --- |
| `ParseLimit(r) (int, error)` | `?limit`: absent → 50; clamped to `[1, 200]`; non-integer → `ErrInvalidLimit` (map to 400) |
| `Cursor{CreatedAt, ID}` | the last returned row's keyset position |
| `EncodeCursor(Cursor) string` | `base64url(RFC3339Nano + "\x00" + id)`, no padding — opaque `next_cursor` |
| `DecodeCursor(s) (*Cursor, error)` | `""` → `nil` (start); malformed → `ErrInvalidCursor` (map to 400) |

Out-of-range limits are **clamped**, not rejected (`limit=1000` → 200). A
malformed cursor is a **400**, never a silent reset to the first page (which
would loop clients forever). The cursor carries no org id — the org is always
re-derived from the token, so it cannot be replayed across tenants.

```go
limit, err := httpapi.ParseLimit(r)
if err != nil {
	httpx.WriteError(w, 400, httpx.CodeBadRequest, "invalid limit")
	return
}
cursor, err := httpapi.DecodeCursor(r.URL.Query().Get("cursor"))
if err != nil {
	httpx.WriteError(w, 400, httpx.CodeBadRequest, "invalid cursor")
	return
}
// store seeks: WHERE (created_at, id) > (cursor…) ORDER BY created_at, id LIMIT limit+1
rows, err := s.ListByOrgPage(ctx, caller.OrgID, limit, cursor)
// … drop the lookahead row; set next_cursor = EncodeCursor(last kept row) or null
```

## JSON success helpers (`json.go`)

Thin wrappers over `httpx.WriteJSON` for a uniform success path:

| Helper | Result |
| --- | --- |
| `WriteOK(w, v)` | 200 JSON |
| `WriteCreated(w, v)` | 201 JSON |

## Device-authorization flow (`device.go`)

The OAuth 2.0 device grant (RFC 8628) that lets the browserless `drun-dash`
CLI obtain a session token, implementing [ADR-0001](../../docs/adr/0001-authentication-and-session-model.md)
§2. State lives in Redis (`Deps.Redis`, a narrow `DeviceStore`) with a **600s**
TTL, so pending authorizations expire on their own with no reaper.

| Endpoint | Auth | Behavior |
| --- | --- | --- |
| `POST /auth/device/code` | none | Mint an opaque `device_code` (32 `crypto/rand` bytes, base64url) and a short `user_code` (`XXXX-XXXX`, ambiguity-free alphabet). Store both keys, return `verification_uri` = `<AdminUIBaseURL>/device`, `verification_uri_complete`, `expires_in`, `interval`. |
| `POST /auth/device/token` | none | Poll with the `device_code`. Returns the RFC 8628 `OAuthError` codes (`authorization_pending`, `slow_down`, `access_denied`, `expired_token`) as `400`s; on `approved`, mints the session JWT, deletes both keys (single-use), and returns `200` `TokenResponse`. |
| `POST /auth/device/approve` | Bearer | **Extension, not in `openapi/v1.yaml`.** The admin UI (behind OIDC login) calls this to approve/deny a pending request; the approving member's principal is bound onto the record. |

Redis key layout (`device_code` is never stored raw):

- `authz:device:<sha256hex(device_code)>` → JSON record
  `{state, user_code, client_id, scope, last_poll_unix, interval, sub, member_id, org_id, roles}`.
- `authz:user_code:<user_code>` → `device_code` (the browser approval lookup).

Both keys share the 600s TTL; re-stores (slow_down bookkeeping, approval) preserve
the record's **remaining** TTL rather than resetting it.

### `POST /auth/device/approve` (the extension)

Body `{user_code, decision}` where `decision` is `"approve"` or `"deny"`. It
resolves `authz:user_code:<user_code>` → `device_code` → record, then:

- **approve** — sets `state="approved"` and binds `sub`/`member_id`/`org_id`/`roles`
  from `authmw.MustPrincipal` (the OIDC-authenticated admin-UI session). `sub`
  mirrors `members.idp_subject` so the CLI's minted token resolves the same
  `Member` as `/auth/session`.
- **deny** — sets `state="denied"`.

An unknown or expired `user_code` is a **404**. It is deliberately outside the
OpenAPI contract because it is an admin-UI→control-plane internal step, not part
of the public CLI-facing surface.
