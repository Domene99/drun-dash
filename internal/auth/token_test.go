package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeRevoker is an in-memory [Revoker] for the unit tests: no Redis required.
// A non-nil err makes both methods fail, exercising the verifier's fail-closed
// path.
type fakeRevoker struct {
	revoked map[string]bool
	err     error
}

func newFakeRevoker() *fakeRevoker { return &fakeRevoker{revoked: map[string]bool{}} }

func (f *fakeRevoker) Revoke(_ context.Context, jti string, _ time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.revoked[jti] = true
	return nil
}

func (f *fakeRevoker) IsRevoked(_ context.Context, jti string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.revoked[jti], nil
}

const (
	testKID      = "test-key-1"
	testIssuer   = "https://control-plane.drun-dash.test"
	testAudience = "drun-dash-control-plane"
	testTTL      = 15 * time.Minute
)

// testHarness bundles a matched issuer/verifier pair over a fresh keypair.
type testHarness struct {
	issuer   *Issuer
	verifier *Verifier
	revoker  *fakeRevoker
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
}

// newHarness builds an issuer and verifier sharing one keypair, kid, issuer,
// and audience. now, if non-nil, drives both clocks so tests can control time.
func newHarness(t *testing.T, now func() time.Time) *testHarness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	issuer, err := NewIssuer(IssuerConfig{
		SigningKey: priv,
		KID:        testKID,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        testTTL,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	revoker := newFakeRevoker()
	verifier, err := NewVerifier(VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{testKID: pub},
		Issuer:   testIssuer,
		Audience: testAudience,
		Revoker:  revoker,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return &testHarness{issuer: issuer, verifier: verifier, revoker: revoker, pub: pub, priv: priv}
}

func sampleClaims() Claims {
	return Claims{
		OrgID:    "org-123",
		MemberID: "member-abc",
		Roles:    []string{"member", "auditor"},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "idp-subject-xyz",
		},
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	token, err := h.issuer.Mint(ctx, sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	got, err := h.verifier.Verify(ctx, token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if got.Subject != "idp-subject-xyz" {
		t.Errorf("Subject = %q, want %q", got.Subject, "idp-subject-xyz")
	}
	if got.OrgID != "org-123" {
		t.Errorf("OrgID = %q, want %q", got.OrgID, "org-123")
	}
	if got.MemberID != "member-abc" {
		t.Errorf("MemberID = %q, want %q", got.MemberID, "member-abc")
	}
	if strings.Join(got.Roles, ",") != "member,auditor" {
		t.Errorf("Roles = %v, want [member auditor]", got.Roles)
	}
	if got.Issuer != testIssuer {
		t.Errorf("Issuer = %q, want %q", got.Issuer, testIssuer)
	}
	if len(got.Audience) != 1 || got.Audience[0] != testAudience {
		t.Errorf("Audience = %v, want [%s]", got.Audience, testAudience)
	}
	if got.ID == "" {
		t.Error("jti is empty; Mint should generate one")
	}
	if got.ExpiresAt == nil || got.IssuedAt == nil {
		t.Fatal("exp/iat not set")
	}
	if d := got.ExpiresAt.Sub(got.IssuedAt.Time); d != testTTL {
		t.Errorf("exp - iat = %s, want %s", d, testTTL)
	}
}

// The header must carry the issuer's kid so a verifier can select the key.
func TestMintStampsKIDHeader(t *testing.T) {
	h := newHarness(t, nil)
	token, err := h.issuer.Mint(context.Background(), sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(token, &Claims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	if kid, _ := parsed.Header["kid"].(string); kid != testKID {
		t.Errorf("header kid = %q, want %q", kid, testKID)
	}
	if alg, _ := parsed.Header["alg"].(string); alg != signingAlg {
		t.Errorf("header alg = %q, want %q", alg, signingAlg)
	}
}

func TestMintRejectsIncompleteClaims(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	if _, err := h.issuer.Mint(ctx, Claims{OrgID: "org-1"}); err == nil {
		t.Error("Mint with empty subject: want error")
	}
	if _, err := h.issuer.Mint(ctx, Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "s"}}); err == nil {
		t.Error("Mint with empty org_id: want error")
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	// Issue at base; verify 20 minutes later, past the 15-minute TTL.
	issueClock := func() time.Time { return base }
	h := newHarness(t, issueClock)
	ctx := context.Background()

	token, err := h.issuer.Mint(ctx, sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	h.verifier.now = func() time.Time { return base.Add(20 * time.Minute) }
	if _, err := h.verifier.Verify(ctx, token); err == nil {
		t.Fatal("Verify expired token: want error, got nil")
	} else if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Errorf("Verify expired token: err = %v, want ErrTokenExpired", err)
	}
}

func TestVerifyBadSignature(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	token, err := h.issuer.Mint(ctx, sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// A verifier holding a different public key under the same kid must reject.
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	badVerifier, err := NewVerifier(VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{testKID: otherPub},
		Issuer:   testIssuer,
		Audience: testAudience,
		Revoker:  newFakeRevoker(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := badVerifier.Verify(ctx, token); err == nil {
		t.Error("Verify with wrong key: want error, got nil")
	}
}

func TestVerifyUnknownKID(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	token, err := h.issuer.Mint(ctx, sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	otherPub, _, _ := ed25519.GenerateKey(nil)
	v, err := NewVerifier(VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{"other-kid": otherPub},
		Issuer:   testIssuer,
		Audience: testAudience,
		Revoker:  newFakeRevoker(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := v.Verify(ctx, token); err == nil {
		t.Error("Verify with unknown kid: want error, got nil")
	}
}

func TestVerifyWrongIssuerAndAudience(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	token, err := h.issuer.Mint(ctx, sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	tests := []struct {
		name     string
		issuer   string
		audience string
	}{
		{"wrong issuer", "https://evil.test", testAudience},
		{"wrong audience", testIssuer, "drun-dash-proxy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewVerifier(VerifierConfig{
				Keys:     map[string]ed25519.PublicKey{testKID: h.pub},
				Issuer:   tt.issuer,
				Audience: tt.audience,
				Revoker:  newFakeRevoker(),
			})
			if err != nil {
				t.Fatalf("NewVerifier: %v", err)
			}
			if _, err := v.Verify(ctx, token); err == nil {
				t.Errorf("Verify with %s: want error, got nil", tt.name)
			}
		})
	}
}

func TestVerifyRevokedJTI(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	token, err := h.issuer.Mint(ctx, sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Not revoked yet: passes.
	claims, err := h.verifier.Verify(ctx, token)
	if err != nil {
		t.Fatalf("Verify (pre-revoke): %v", err)
	}

	// Revoke by jti, then the same token must be rejected.
	if err := h.revoker.Revoke(ctx, claims.ID, testTTL); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := h.verifier.Verify(ctx, token); !errors.Is(err, ErrRevoked) {
		t.Errorf("Verify (post-revoke): err = %v, want ErrRevoked", err)
	}
}

// A revocation-check failure must fail closed: the token is rejected even
// though its signature and claims are valid.
func TestVerifyFailsClosedOnRevokerError(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	token, err := h.issuer.Mint(ctx, sampleClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	h.revoker.err = errors.New("redis down")
	if _, err := h.verifier.Verify(ctx, token); err == nil {
		t.Error("Verify with unreachable revoker: want error (fail closed), got nil")
	}
}

// A token with no jti cannot be revocation-checked and must be rejected.
func TestVerifyMissingJTI(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	now := time.Now()

	// Hand-mint a valid-signature token that deliberately omits the jti.
	claims := sampleClaims()
	claims.Issuer = testIssuer
	claims.Audience = jwt.ClaimStrings{testAudience}
	claims.IssuedAt = jwt.NewNumericDate(now)
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(testTTL))
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, &claims)
	tok.Header["kid"] = testKID
	signed, err := tok.SignedString(h.priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	if _, err := h.verifier.Verify(ctx, signed); !errors.Is(err, ErrMissingJTI) {
		t.Errorf("Verify token without jti: err = %v, want ErrMissingJTI", err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.verifier.Verify(context.Background(), "not-a-jwt"); err == nil {
		t.Error("Verify garbage: want error, got nil")
	}
}

func TestNewIssuerValidation(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	good := IssuerConfig{SigningKey: priv, KID: testKID, Issuer: testIssuer, Audience: testAudience, TTL: testTTL}

	tests := []struct {
		name  string
		mutfn func(c *IssuerConfig)
	}{
		{"short key", func(c *IssuerConfig) { c.SigningKey = ed25519.PrivateKey{1, 2, 3} }},
		{"empty kid", func(c *IssuerConfig) { c.KID = "" }},
		{"empty issuer", func(c *IssuerConfig) { c.Issuer = "" }},
		{"empty audience", func(c *IssuerConfig) { c.Audience = "" }},
		{"zero ttl", func(c *IssuerConfig) { c.TTL = 0 }},
		{"negative ttl", func(c *IssuerConfig) { c.TTL = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := good
			tt.mutfn(&cfg)
			if _, err := NewIssuer(cfg); err == nil {
				t.Errorf("NewIssuer(%s): want error, got nil", tt.name)
			}
		})
	}
}

func TestNewVerifierValidation(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	good := VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{testKID: pub},
		Issuer:   testIssuer,
		Audience: testAudience,
		Revoker:  newFakeRevoker(),
	}

	tests := []struct {
		name  string
		mutfn func(c *VerifierConfig)
	}{
		{"no keys", func(c *VerifierConfig) { c.Keys = nil }},
		{"empty kid", func(c *VerifierConfig) { c.Keys = map[string]ed25519.PublicKey{"": pub} }},
		{"bad key", func(c *VerifierConfig) { c.Keys = map[string]ed25519.PublicKey{testKID: {1, 2, 3}} }},
		{"empty issuer", func(c *VerifierConfig) { c.Issuer = "" }},
		{"empty audience", func(c *VerifierConfig) { c.Audience = "" }},
		{"nil revoker", func(c *VerifierConfig) { c.Revoker = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := good
			tt.mutfn(&cfg)
			if _, err := NewVerifier(cfg); err == nil {
				t.Errorf("NewVerifier(%s): want error, got nil", tt.name)
			}
		})
	}
}
