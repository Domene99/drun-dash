//go:build integration

// Integration coverage for the SecretStore (ADR-0009): the encrypted-at-rest
// secrets table backing the DB vault. Mirrors the other *_integration_test.go
// files — shares requirePool / newOrg / uniq and skips when Postgres is
// unreachable; each test hangs its rows off a throwaway org whose cleanup
// cascades. These tests store opaque bytes; the AES-256-GCM sealing itself is
// unit-tested in internal/vault.
package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestIntegrationSecrets(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	secrets := NewSecretStore(pool)

	ciphertext := []byte{0x01, 0x02, 0x03, 0xff}
	nonce := []byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b}

	created, err := secrets.Create(ctx, CreateSecretParams{
		OrgID:      org.ID,
		Ciphertext: ciphertext,
		Nonce:      nonce,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || created.OrgID != org.ID || created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("Create returned incomplete row: %+v", created)
	}
	if !bytes.Equal(created.Ciphertext, ciphertext) || !bytes.Equal(created.Nonce, nonce) {
		t.Fatalf("Create round-trip mismatch: %+v", created)
	}

	got, err := secrets.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != created.ID || !bytes.Equal(got.Ciphertext, ciphertext) || !bytes.Equal(got.Nonce, nonce) {
		t.Fatalf("GetByID = %+v, want ciphertext/nonce round-trip", got)
	}

	// Update replaces the sealed bytes (credential/key rotation).
	newCiphertext := []byte{0xaa, 0xbb, 0xcc}
	newNonce := []byte{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b}
	if err := secrets.Update(ctx, created.ID, newCiphertext, newNonce); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reloaded, err := secrets.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if !bytes.Equal(reloaded.Ciphertext, newCiphertext) || !bytes.Equal(reloaded.Nonce, newNonce) {
		t.Fatalf("Update did not replace sealed bytes: %+v", reloaded)
	}
	if !reloaded.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("Update did not advance updated_at: %v !> %v", reloaded.UpdatedAt, created.UpdatedAt)
	}

	// Delete removes the row.
	if err := secrets.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := secrets.GetByID(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID after delete = %v, want ErrNotFound", err)
	}

	// Missing-target mutations map to ErrNotFound.
	missing := "00000000-0000-0000-0000-000000000000"
	if _, err := secrets.GetByID(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if err := secrets.Update(ctx, missing, newCiphertext, newNonce); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update(missing) = %v, want ErrNotFound", err)
	}
	if err := secrets.Delete(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
}

// TestIntegrationSecretsCascade proves deleting the owning org cascades to its
// secrets (ON DELETE CASCADE), so a decommissioned org leaves no sealed keys.
func TestIntegrationSecretsCascade(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	secrets := NewSecretStore(pool)

	created, err := secrets.Create(ctx, CreateSecretParams{
		OrgID:      org.ID,
		Ciphertext: []byte{0x01},
		Nonce:      bytes.Repeat([]byte{0x00}, 12),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, org.ID); err != nil {
		t.Fatalf("delete org: %v", err)
	}
	if _, err := secrets.GetByID(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("secret survived org delete = %v, want ErrNotFound", err)
	}
}
