// Package httpapi is the shared helper library for the drun-dash control-plane
// REST handlers (agents, policies, sessions, approvals, analytics). It holds
// the cross-cutting primitives those handlers reuse so their behavior stays
// identical without the handlers coordinating: the canonical store-error → HTTP
// mapping, keyset pagination (limit parsing + opaque cursor encode/decode), and
// the org-ownership check. It contains no routes and no resource handlers.
//
// The conventions implemented here are decided in ADR-0002 (REST API
// conventions); see docs/adr/0002-rest-api-conventions.md. Handlers should
// route every store/validation failure through this package rather than
// hand-rolling status codes, and must use internal/httpx for the response
// bodies (the flat Error envelope).
package httpapi

import (
	"errors"
	"net/http"

	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// WriteStoreError maps a store-layer error to an HTTP response using the
// canonical ADR-0002 §2 table:
//
//   - errors.Is(err, store.ErrNotFound) → 404 not_found
//   - errors.Is(err, store.ErrConflict) → 409 conflict
//   - anything else                     → 500 internal_error
//
// The 500 path deliberately writes a generic message and never echoes the raw
// error string to the client, so query internals and constraint details do not
// leak across the API boundary. Callers that need the underlying error for
// logging (with the request id) should log it before or after calling this
// helper — the error is not consumed here.
//
// store.ErrConflict is wrapped with the offending constraint name
// ("%w on <constraint>"), so errors.Is still matches; the constraint name is
// intentionally not surfaced to the client by this helper.
func WriteStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		_ = httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "resource not found")
	case errors.Is(err, store.ErrConflict):
		_ = httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "resource already exists")
	default:
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
	}
}

// WriteValidationError writes a 400 bad_request for a semantic validation
// failure on a specific field, attaching {"field": field} as the structured
// details (ADR-0002 §2). Use it for missing-required-field, bad-enum, and
// out-of-range failures; decode-layer failures (httpx.DecodeJSON) should be
// mapped straight to a 400 with the decoder's message and no details.
func WriteValidationError(w http.ResponseWriter, field, msg string) {
	_ = httpx.WriteErrorDetails(w, http.StatusBadRequest, httpx.CodeBadRequest, msg, map[string]any{"field": field})
}

// WriteForbidden writes a 403 forbidden. Per ADR-0002 §2 this is only emitted
// where the OpenAPI contract documents a 403 (e.g. launchSession); a cross-org
// id reference is a 404 (see WriteNotFound and SameOrg), never a 403.
func WriteForbidden(w http.ResponseWriter, msg string) {
	_ = httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, msg)
}

// WriteNotFound writes a 404 not_found. Handlers use it both for genuinely
// missing rows and for cross-org id references (ADR-0002 §1): returning 404
// rather than 403 on a tenant mismatch avoids leaking the existence of another
// organization's resource.
func WriteNotFound(w http.ResponseWriter, msg string) {
	_ = httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, msg)
}
