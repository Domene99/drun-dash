package vault

import (
	"context"
	"errors"
	"testing"
)

// TestCompositeVaultRouting proves Resolve dispatches by ref scheme: db-scheme
// refs go to the DBVault, dev-scheme refs to the EnvVault, and anything else is
// an invalid ref — without either backend seeing the other's refs.
func TestCompositeVaultRouting(t *testing.T) {
	fs := newFakeSecretStore()
	db, err := NewDBVault(fs, testMasterKey(1))
	if err != nil {
		t.Fatalf("NewDBVault: %v", err)
	}
	c := NewCompositeVault(db, NewEnvVault())
	ctx := context.Background()

	// A db-scheme ref sealed through the DBVault resolves through the composite.
	ref, err := db.Seal(ctx, "org-1", "sk-db-key")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := c.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(db ref): %v", err)
	}
	if got.Value != "sk-db-key" {
		t.Fatalf("Resolve(db ref).Value = %q, want sk-db-key", got.Value)
	}

	// A dev-scheme ref routes to the EnvVault, which reads the environment.
	t.Setenv("DRUNDASH_SECRET_ANTHROPIC", "sk-env-key")
	got, err = c.Resolve(ctx, "vault://dev/anthropic")
	if err != nil {
		t.Fatalf("Resolve(dev ref): %v", err)
	}
	if got.Value != "sk-env-key" {
		t.Fatalf("Resolve(dev ref).Value = %q, want sk-env-key", got.Value)
	}

	// An unknown backend, or a non-scheme string, is an invalid ref.
	for _, ref := range []string{"vault://prod/x", "vault://anthropic", "not-a-ref", ""} {
		if _, err := c.Resolve(ctx, ref); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("Resolve(%q) = %v, want ErrInvalidRef", ref, err)
		}
	}
}

// TestCompositeVaultDBErrorsPropagate proves the composite forwards the DB
// backend's sentinels unchanged (a missing db row is ErrNotFound, not an
// invalid ref).
func TestCompositeVaultDBErrorsPropagate(t *testing.T) {
	fs := newFakeSecretStore()
	db, _ := NewDBVault(fs, testMasterKey(1))
	c := NewCompositeVault(db, NewEnvVault())

	_, err := c.Resolve(context.Background(), "vault://db/00000000-0000-4000-8000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(missing db row) = %v, want ErrNotFound", err)
	}
}

// TestCompositeVaultImplementsVault is a compile-and-run check through the
// interface.
func TestCompositeVaultImplementsVault(t *testing.T) {
	fs := newFakeSecretStore()
	db, _ := NewDBVault(fs, testMasterKey(1))
	var v Vault = NewCompositeVault(db, NewEnvVault())
	t.Setenv("DRUNDASH_SECRET_ANTHROPIC", "sk-ok")
	got, err := v.Resolve(context.Background(), "vault://dev/anthropic")
	if err != nil {
		t.Fatalf("Resolve via interface: %v", err)
	}
	if got.Value != "sk-ok" {
		t.Errorf("Value = %q, want sk-ok", got.Value)
	}
}
