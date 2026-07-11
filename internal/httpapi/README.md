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
