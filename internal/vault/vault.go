// Package vault brokers access to master LLM provider credentials.
//
// Credential custody is drun-dash's core governance lever: master provider
// keys never reach a developer's machine or the sandbox. The control plane
// holds them and brokers every upstream model call. In the schema,
// model_endpoints.secret_ref is an opaque handle — a "vault://" reference —
// into a secret store; the raw provider key is never persisted in Postgres.
//
// This package defines the [Vault] abstraction that resolves such a
// secret_ref to a live provider [Secret]. Backends implement the interface:
// [EnvVault] is the development backend (see envvault.go) that reads material
// from the process environment. Production backends (cloud KMS / secret
// managers) implement the same interface in a later batch and are the only
// place raw keys are ever handled.
//
// The llm-proxy will later use [Vault.Resolve] to inject upstream credentials
// per call (short-lived, scoped, revocable). This package delivers the seam
// and the dev backend only; it does not persist secrets, log raw values, or
// wire itself into the proxy or control plane.
package vault

import (
	"context"
	"errors"
)

// Sentinel errors returned by [Vault] implementations. Callers should compare
// with [errors.Is] rather than by value or string, so backends may wrap these
// with additional (non-sensitive) context.
var (
	// ErrNotFound means the secret_ref is well-formed for this backend but no
	// credential is currently registered for it (e.g. the mapped env var is
	// unset). It never carries the missing value.
	ErrNotFound = errors.New("vault: secret not found")

	// ErrInvalidRef means the secret_ref does not conform to the ref grammar
	// this backend understands (see [EnvVault] for the "vault://dev/<name>"
	// scheme). It signals a caller/config bug, not a missing secret.
	ErrInvalidRef = errors.New("vault: invalid secret ref")
)

// Secret is an opaque provider credential resolved from a secret_ref.
//
// The raw credential lives in Value and must be treated as sensitive: never
// persist it to disk or Postgres, and never log it. To guard against
// accidental leakage through fmt verbs, Secret redacts itself under both the
// [fmt.Stringer] (%s, %v) and [fmt.GoStringer] (%#v) interfaces — read Value
// explicitly when the raw credential is genuinely needed.
type Secret struct {
	// Value is the raw provider credential (e.g. an API key). Access it
	// deliberately; it is deliberately excluded from every string rendering.
	Value string
}

// redacted is the placeholder rendered in place of a Secret's raw value.
const redacted = "Secret(***)"

// String implements [fmt.Stringer], redacting the raw credential so a Secret
// printed with %s or %v (including inside a containing struct) cannot leak.
func (Secret) String() string { return redacted }

// GoString implements [fmt.GoStringer], redacting the raw credential under the
// %#v verb as well.
func (Secret) GoString() string { return redacted }

// Vault resolves an opaque secret_ref handle to a live provider [Secret].
//
// Implementations must be safe for concurrent use. Resolve reports
// [ErrInvalidRef] for a malformed ref and [ErrNotFound] when the referenced
// secret is not registered; both are total outcomes — Resolve never panics on
// caller input.
type Vault interface {
	Resolve(ctx context.Context, secretRef string) (Secret, error)
}
