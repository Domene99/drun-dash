package vault

import (
	"context"
	"fmt"
	"strings"
)

// CompositeVault routes [Vault.Resolve] to a backend by the ref's scheme
// (ADR-0009): "vault://db/*" resolves through the encrypted-at-rest [DBVault],
// "vault://dev/*" through the environment-backed [EnvVault]. It is the single
// vault the llm-proxy holds, so a credential sealed by the control plane
// (vault://db/*) and a dev credential (vault://dev/*) both resolve at proxy
// time with no per-call branching in the broker.
//
// A ref naming any other backend — or not matching the "vault://<backend>/…"
// grammar at all — returns [ErrInvalidRef]. CompositeVault is safe for
// concurrent use (both backends are).
type CompositeVault struct {
	db  DBVault
	env EnvVault
}

// CompositeVault implements [Vault].
var _ Vault = CompositeVault{}

// NewCompositeVault returns a [CompositeVault] routing db-scheme refs to db and
// dev-scheme refs to env.
func NewCompositeVault(db DBVault, env EnvVault) CompositeVault {
	return CompositeVault{db: db, env: env}
}

// Resolve implements [Vault] by dispatching on the ref's backend segment. It
// does not itself read any secret material; it only chooses the backend, whose
// own Resolve enforces that backend's grammar and returns the [Secret] or the
// appropriate sentinel ([ErrNotFound], [ErrKeyNotConfigured]).
func (c CompositeVault) Resolve(ctx context.Context, secretRef string) (Secret, error) {
	switch backendOf(secretRef) {
	case dbBackend:
		return c.db.Resolve(ctx, secretRef)
	case devBackend:
		return c.env.Resolve(ctx, secretRef)
	default:
		return Secret{}, fmt.Errorf("%w: %q (unknown backend)", ErrInvalidRef, secretRef)
	}
}

// backendOf returns the backend segment of a "vault://<backend>/<rest>" ref, or
// "" when secretRef does not match the scheme or names no backend. It is total
// and never panics.
func backendOf(secretRef string) string {
	rest, ok := strings.CutPrefix(secretRef, refScheme)
	if !ok {
		return ""
	}
	backend, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return backend
}
