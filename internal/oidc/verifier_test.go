package oidc_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Domene99/drun-dash/internal/oidc"
)

const (
	testIssuer   = "https://idp.example.com"
	testClientID = "drun-dash-org-client"
)

// fixedNow is the reference clock the static verifier uses in these tests, so
// token expiry is deterministic and independent of wall-clock time.
var fixedNow = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

// newKey returns a fresh RSA signing key. RS256 is the OIDC default and
// go-oidc's default accepted algorithm, so no algorithm configuration is
// needed on the verifier.
func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return key
}

// signToken mints a compact RS256 JWT for claims, signed with key. Using the
// existing golang-jwt library to produce the token keeps the test self-
// contained: go-oidc verifies the resulting standard JWS against the matching
// public key.
func signToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test-key-1"
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// validClaims returns a standard, currently-valid ID-token claim set.
func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   testIssuer,
		"aud":   testClientID,
		"sub":   "idp-subject-abc123",
		"email": "member@example.com",
		"iat":   fixedNow.Add(-1 * time.Minute).Unix(),
		"exp":   fixedNow.Add(1 * time.Hour).Unix(),
	}
}

// staticVerifier builds a NewStaticVerifier trusting pubKeys, pinned to
// fixedNow.
func staticVerifier(t *testing.T, pubKeys ...crypto.PublicKey) oidc.Verifier {
	t.Helper()
	v, err := oidc.NewStaticVerifier(oidc.StaticConfig{
		Issuer:     testIssuer,
		ClientID:   testClientID,
		PublicKeys: pubKeys,
		Now:        func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("new static verifier: %v", err)
	}
	return v
}

func TestVerify_HappyPath(t *testing.T) {
	key := newKey(t)
	v := staticVerifier(t, key.Public())

	raw := signToken(t, key, validClaims())
	tok, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}

	if tok.Subject != "idp-subject-abc123" {
		t.Errorf("Subject = %q, want %q", tok.Subject, "idp-subject-abc123")
	}
	if tok.Email != "member@example.com" {
		t.Errorf("Email = %q, want %q", tok.Email, "member@example.com")
	}
	if tok.Issuer != testIssuer {
		t.Errorf("Issuer = %q, want %q", tok.Issuer, testIssuer)
	}
	if len(tok.Audience) != 1 || tok.Audience[0] != testClientID {
		t.Errorf("Audience = %v, want [%q]", tok.Audience, testClientID)
	}
	// The raw claim map must expose IdP-specific fields verbatim.
	if got, _ := tok.Claims["email"].(string); got != "member@example.com" {
		t.Errorf("Claims[email] = %v, want %q", tok.Claims["email"], "member@example.com")
	}
	if got, _ := tok.Claims["sub"].(string); got != "idp-subject-abc123" {
		t.Errorf("Claims[sub] = %v, want %q", tok.Claims["sub"], "idp-subject-abc123")
	}
}

func TestVerify_Expired(t *testing.T) {
	key := newKey(t)
	v := staticVerifier(t, key.Public())

	claims := validClaims()
	claims["exp"] = fixedNow.Add(-1 * time.Minute).Unix()
	raw := signToken(t, key, claims)

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("Verify accepted an expired token, want error")
	}
}

func TestVerify_WrongAudience(t *testing.T) {
	key := newKey(t)
	v := staticVerifier(t, key.Public())

	claims := validClaims()
	claims["aud"] = "some-other-client"
	raw := signToken(t, key, claims)

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("Verify accepted a token with the wrong audience, want error")
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	key := newKey(t)
	v := staticVerifier(t, key.Public())

	claims := validClaims()
	claims["iss"] = "https://evil.example.com"
	raw := signToken(t, key, claims)

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("Verify accepted a token with the wrong issuer, want error")
	}
}

func TestVerify_BadSignature(t *testing.T) {
	signingKey := newKey(t)
	otherKey := newKey(t)
	// The verifier trusts otherKey, but the token is signed with signingKey.
	v := staticVerifier(t, otherKey.Public())

	raw := signToken(t, signingKey, validClaims())

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("Verify accepted a token with an untrusted signature, want error")
	}
}

func TestVerify_MissingSubject(t *testing.T) {
	key := newKey(t)
	v := staticVerifier(t, key.Public())

	claims := validClaims()
	delete(claims, "sub")
	raw := signToken(t, key, claims)

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, oidc.ErrMissingSubject) {
		t.Fatalf("Verify error = %v, want ErrMissingSubject", err)
	}
}

func TestVerify_EmailOptional(t *testing.T) {
	key := newKey(t)
	v := staticVerifier(t, key.Public())

	claims := validClaims()
	delete(claims, "email")
	raw := signToken(t, key, claims)

	tok, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if tok.Email != "" {
		t.Errorf("Email = %q, want empty", tok.Email)
	}
	if tok.Subject != "idp-subject-abc123" {
		t.Errorf("Subject = %q, want %q", tok.Subject, "idp-subject-abc123")
	}
}

func TestNewStaticVerifier_Validation(t *testing.T) {
	key := newKey(t)
	tests := []struct {
		name string
		cfg  oidc.StaticConfig
	}{
		{
			name: "no keys",
			cfg:  oidc.StaticConfig{Issuer: testIssuer, ClientID: testClientID},
		},
		{
			name: "empty issuer",
			cfg:  oidc.StaticConfig{ClientID: testClientID, PublicKeys: []crypto.PublicKey{key.Public()}},
		},
		{
			name: "empty client id",
			cfg:  oidc.StaticConfig{Issuer: testIssuer, PublicKeys: []crypto.PublicKey{key.Public()}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := oidc.NewStaticVerifier(tc.cfg); err == nil {
				t.Fatal("NewStaticVerifier accepted an invalid config, want error")
			}
		})
	}
}

func TestNewStaticVerifier_SkipChecks(t *testing.T) {
	key := newKey(t)
	// With the checks disabled, issuer/client id may be empty.
	v, err := oidc.NewStaticVerifier(oidc.StaticConfig{
		PublicKeys:        []crypto.PublicKey{key.Public()},
		Now:               func() time.Time { return fixedNow },
		SkipClientIDCheck: true,
		SkipIssuerCheck:   true,
	})
	if err != nil {
		t.Fatalf("NewStaticVerifier with skips: %v", err)
	}

	claims := validClaims()
	claims["iss"] = "https://anything.example.com"
	claims["aud"] = "anything"
	raw := signToken(t, key, claims)

	tok, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if tok.Subject != "idp-subject-abc123" {
		t.Errorf("Subject = %q, want %q", tok.Subject, "idp-subject-abc123")
	}
}

func TestNewRemoteVerifier_Validation(t *testing.T) {
	// Empty inputs must fail before any network I/O is attempted.
	if _, err := oidc.NewRemoteVerifier(context.Background(), "", testClientID); err == nil {
		t.Error("NewRemoteVerifier accepted an empty issuer url, want error")
	}
	if _, err := oidc.NewRemoteVerifier(context.Background(), testIssuer, ""); err == nil {
		t.Error("NewRemoteVerifier accepted an empty client id, want error")
	}
}
