// Package authmw is the HTTP authentication middleware that fronts drun-dash's
// protected control-plane and proxy routes. It turns a validated session JWT
// into a request-scoped principal (an [auth.Claims]) per
// docs/adr/0001-authentication-and-session-model.md (§5) and
// docs/adr/0002-rest-conventions.md (§1–§2).
//
// It is a thin adapter over the already-verified token core in
// [github.com/Domene99/drun-dash/internal/auth]: it extracts the Bearer token
// from the Authorization header, delegates all cryptographic and revocation
// checks to [auth.Verifier.Verify], and — on success — stashes the returned
// claims in the request context for downstream handlers to read via
// [PrincipalFromContext] or [MustPrincipal].
//
// # 401 is minted here, not in handlers
//
// Per ADR-0002 §2, a missing, malformed, expired, or revoked token yields a
// flat HTTP 401 with error code "unauthorized" ([httpx.CodeUnauthorized]).
// [RequireAuth] is the single place that mints that 401 so handlers behind it
// can assume a principal is always present. To avoid handing an attacker an
// oracle, the response body never reveals which check failed — every failure
// path returns the same generic 401.
//
// This package wires no routes and is mounted into no binary here; a later
// batch mounts [RequireAuth] in front of the protected route groups.
package authmw

import (
	"context"
	"net/http"
	"strings"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/httpx"
)

// bearerPrefix is the case-insensitive scheme expected on the Authorization
// header, per RFC 6750: "Bearer <token>".
const bearerPrefix = "Bearer "

// contextKey is an unexported type so the principal stored in the request
// context cannot collide with keys from other packages and cannot be read or
// forged from outside this package.
type contextKey int

const principalKey contextKey = iota

// RequireAuth returns middleware that authenticates requests using v. For each
// request it extracts the Bearer token from the Authorization header and calls
// v.Verify; on success it stores the resulting *[auth.Claims] in the request
// context and invokes next. Handlers wrapped by this middleware read the
// principal with [PrincipalFromContext] or [MustPrincipal].
//
// Any failure — an absent Authorization header, a non-Bearer scheme, an empty
// token, or any error from v.Verify (expired, bad signature, wrong iss/aud,
// revoked, missing jti) — is answered with a flat HTTP 401 whose code is
// [httpx.CodeUnauthorized], and next is not called. The body is deliberately
// generic so it does not disclose which check failed; per-failure detail is a
// server-side logging concern handled elsewhere.
func RequireAuth(v *auth.Verifier) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w)
				return
			}
			claims, err := v.Verify(r.Context(), token)
			if err != nil {
				unauthorized(w)
				return
			}
			ctx := context.WithValue(r.Context(), principalKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// PrincipalFromContext returns the *[auth.Claims] stored by [RequireAuth], and
// reports whether one was present. It returns (nil, false) for any context that
// did not pass through the middleware.
func PrincipalFromContext(ctx context.Context) (*auth.Claims, bool) {
	claims, ok := ctx.Value(principalKey).(*auth.Claims)
	return claims, ok
}

// MustPrincipal returns the *[auth.Claims] stored by [RequireAuth]. It is for
// handlers mounted behind [RequireAuth], where a principal is guaranteed to be
// present; it panics if none is — signalling a wiring bug (the handler was not
// placed behind the middleware) rather than an authentication failure, which
// [RequireAuth] would already have turned into a 401.
func MustPrincipal(ctx context.Context) *auth.Claims {
	claims, ok := PrincipalFromContext(ctx)
	if !ok {
		panic("authmw: no principal in context; handler is not mounted behind RequireAuth")
	}
	return claims
}

// bearerToken extracts the token from an Authorization header value. It reports
// ok=false when the header is empty, does not use the Bearer scheme (matched
// case-insensitively per RFC 6750), or carries an empty token.
func bearerToken(header string) (string, bool) {
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// unauthorized writes the single, generic 401 used for every authentication
// failure, per ADR-0002 §2.
func unauthorized(w http.ResponseWriter) {
	_ = httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
}
