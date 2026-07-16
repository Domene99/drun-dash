package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakeSecretStore is an in-memory vault.SecretStore: it assigns uuid-shaped ids
// and stores sealed rows in a map, so the DBVault seal/resolve/reseal/delete
// paths can be exercised without a database. It is safe for concurrent use.
type fakeSecretStore struct {
	mu   sync.Mutex
	seq  int
	rows map[string]store.Secret
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{rows: map[string]store.Secret{}}
}

func (f *fakeSecretStore) Create(_ context.Context, p store.CreateSecretParams) (store.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("%08x-0000-4000-8000-%012x", f.seq, f.seq)
	sec := store.Secret{ID: id, OrgID: p.OrgID, Ciphertext: p.Ciphertext, Nonce: p.Nonce}
	f.rows[id] = sec
	return sec, nil
}

func (f *fakeSecretStore) GetByID(_ context.Context, id string) (store.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sec, ok := f.rows[id]
	if !ok {
		return store.Secret{}, store.ErrNotFound
	}
	return sec, nil
}

func (f *fakeSecretStore) Update(_ context.Context, id string, ciphertext, nonce []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sec, ok := f.rows[id]
	if !ok {
		return store.ErrNotFound
	}
	sec.Ciphertext, sec.Nonce = ciphertext, nonce
	f.rows[id] = sec
	return nil
}

func (f *fakeSecretStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}

// testMasterKey returns a deterministic 32-byte AES-256 key seeded by b, so two
// distinct keys can be built for the wrong-key test.
func testMasterKey(b byte) []byte {
	key := make([]byte, masterKeySize)
	for i := range key {
		key[i] = b + byte(i)
	}
	return key
}

func TestNewDBVaultKeyValidation(t *testing.T) {
	fs := newFakeSecretStore()

	// A 32-byte key builds an enabled vault.
	if _, err := NewDBVault(fs, testMasterKey(1)); err != nil {
		t.Fatalf("NewDBVault(32 bytes): %v", err)
	}
	// A nil/empty key builds a disabled vault (no error) — the service still boots.
	if _, err := NewDBVault(fs, nil); err != nil {
		t.Fatalf("NewDBVault(nil): %v", err)
	}
	// A wrong-length non-empty key is an operator error.
	if _, err := NewDBVault(fs, make([]byte, 16)); err == nil {
		t.Fatal("NewDBVault(16 bytes) = nil error, want an error")
	}
}

func TestDBVaultSealResolveRoundTrip(t *testing.T) {
	fs := newFakeSecretStore()
	v, err := NewDBVault(fs, testMasterKey(1))
	if err != nil {
		t.Fatalf("NewDBVault: %v", err)
	}
	ctx := context.Background()

	const plaintext = "sk-ant-real-key"
	ref, err := v.Seal(ctx, "org-1", plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(ref, dbRefPrefix) {
		t.Fatalf("Seal ref = %q, want %s<uuid> prefix", ref, dbRefPrefix)
	}
	// The persisted row is ciphertext, never the plaintext.
	for _, row := range fs.rows {
		if strings.Contains(string(row.Ciphertext), plaintext) {
			t.Fatal("sealed row ciphertext contains the plaintext key")
		}
		if len(row.Nonce) == 0 {
			t.Fatal("sealed row has an empty nonce")
		}
	}

	got, err := v.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Value != plaintext {
		t.Fatalf("Resolve.Value = %q, want %q", got.Value, plaintext)
	}
}

func TestDBVaultDistinctNoncesPerSeal(t *testing.T) {
	fs := newFakeSecretStore()
	v, _ := NewDBVault(fs, testMasterKey(1))
	ctx := context.Background()

	if _, err := v.Seal(ctx, "org-1", "same"); err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	if _, err := v.Seal(ctx, "org-1", "same"); err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	nonces := map[string]bool{}
	for _, row := range fs.rows {
		nonces[string(row.Nonce)] = true
	}
	if len(nonces) != 2 {
		t.Fatalf("distinct nonces = %d, want 2 (a fresh nonce per seal)", len(nonces))
	}
}

func TestDBVaultResolveWrongKeyFails(t *testing.T) {
	fs := newFakeSecretStore()
	sealer, _ := NewDBVault(fs, testMasterKey(1))
	ctx := context.Background()

	const plaintext = "sk-must-not-leak"
	ref, err := sealer.Seal(ctx, "org-1", plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// A vault with a different master key over the same store cannot open it.
	opener, _ := NewDBVault(fs, testMasterKey(9))
	got, err := opener.Resolve(ctx, ref)
	if err == nil {
		t.Fatal("Resolve under wrong key = nil error, want a GCM open failure")
	}
	// The failure is a genuine open error, not ErrNotFound (the row exists).
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve under wrong key = ErrNotFound, want an open failure")
	}
	if got.Value != "" {
		t.Fatal("Resolve under wrong key returned a non-empty Secret")
	}
	if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("wrong-key error leaked plaintext: %v", err)
	}
}

func TestDBVaultResolveErrors(t *testing.T) {
	fs := newFakeSecretStore()
	v, _ := NewDBVault(fs, testMasterKey(1))
	ctx := context.Background()

	tests := []struct {
		name    string
		ref     string
		wantErr error
	}{
		{"malformed ref", "not-a-vault-ref", ErrInvalidRef},
		{"wrong backend", "vault://dev/anthropic", ErrInvalidRef},
		{"non-uuid id", "vault://db/not-a-uuid", ErrInvalidRef},
		{"missing row", "vault://db/00000000-0000-4000-8000-000000000000", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.Resolve(ctx, tt.ref)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Resolve(%q) error = %v, want %v", tt.ref, err, tt.wantErr)
			}
		})
	}
}

func TestDBVaultDisabledWithoutKey(t *testing.T) {
	fs := newFakeSecretStore()
	v, err := NewDBVault(fs, nil)
	if err != nil {
		t.Fatalf("NewDBVault(nil): %v", err)
	}
	ctx := context.Background()

	if _, err := v.Seal(ctx, "org-1", "k"); !errors.Is(err, ErrKeyNotConfigured) {
		t.Fatalf("Seal without key = %v, want ErrKeyNotConfigured", err)
	}
	if err := v.Reseal(ctx, "vault://db/00000000-0000-4000-8000-000000000000", "k"); !errors.Is(err, ErrKeyNotConfigured) {
		t.Fatalf("Reseal without key = %v, want ErrKeyNotConfigured", err)
	}
	if _, err := v.Resolve(ctx, "vault://db/00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrKeyNotConfigured) {
		t.Fatalf("Resolve without key = %v, want ErrKeyNotConfigured", err)
	}
	// A malformed ref is still rejected as invalid, before the key check.
	if _, err := v.Resolve(ctx, "bad-ref"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Resolve(bad) without key = %v, want ErrInvalidRef", err)
	}
}

func TestDBVaultReseal(t *testing.T) {
	fs := newFakeSecretStore()
	v, _ := NewDBVault(fs, testMasterKey(1))
	ctx := context.Background()

	ref, err := v.Seal(ctx, "org-1", "old-key")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := v.Reseal(ctx, ref, "new-key"); err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	got, err := v.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve after reseal: %v", err)
	}
	if got.Value != "new-key" {
		t.Fatalf("Resolve.Value after reseal = %q, want new-key", got.Value)
	}

	// Reseal of a missing row is ErrNotFound; a malformed ref is ErrInvalidRef.
	if err := v.Reseal(ctx, "vault://db/00000000-0000-4000-8000-000000000000", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reseal(missing) = %v, want ErrNotFound", err)
	}
	if err := v.Reseal(ctx, "bad-ref", "x"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Reseal(bad) = %v, want ErrInvalidRef", err)
	}
}

func TestDBVaultDelete(t *testing.T) {
	fs := newFakeSecretStore()
	v, _ := NewDBVault(fs, testMasterKey(1))
	ctx := context.Background()

	ref, err := v.Seal(ctx, "org-1", "key")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := v.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Resolve(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve after delete = %v, want ErrNotFound", err)
	}
	// Deleting again is ErrNotFound; a malformed ref is ErrInvalidRef. Delete
	// does not need a master key, so it works on a disabled vault too.
	if err := v.Delete(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
	if err := v.Delete(ctx, "bad-ref"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Delete(bad) = %v, want ErrInvalidRef", err)
	}
}

func TestIsDBRef(t *testing.T) {
	cases := map[string]bool{
		"vault://db/00000000-0000-4000-8000-000000000000": true,
		"vault://dev/anthropic":                           false,
		"vault://db/not-a-uuid":                           false,
		"":                                                false,
	}
	for ref, want := range cases {
		if got := IsDBRef(ref); got != want {
			t.Errorf("IsDBRef(%q) = %v, want %v", ref, got, want)
		}
	}
}
