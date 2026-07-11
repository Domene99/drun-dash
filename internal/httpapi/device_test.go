package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Domene99/drun-dash/internal/oidc"
)

// errFakeRedis is the failure a fakeRedis returns when a fault is injected, to
// exercise the handlers' 500 paths.
var errFakeRedis = errors.New("fake redis failure")

// fakeRedis is an in-memory DeviceStore for the device-flow handler tests. It
// stores each key's value and TTL and mimics go-redis's redis.Nil-on-miss and
// negative-TTL sentinels, so the handlers run unmodified against it.
type fakeRedis struct {
	mu      sync.Mutex
	data    map[string]string
	exp     map[string]time.Duration
	failSet bool
	failGet bool
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{data: map[string]string{}, exp: map[string]time.Duration{}}
}

func redisValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func (f *fakeRedis) Set(_ context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet {
		return redis.NewStatusResult("", errFakeRedis)
	}
	f.data[key] = redisValueString(value)
	f.exp[key] = ttl
	return redis.NewStatusResult("OK", nil)
}

func (f *fakeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return redis.NewStringResult("", errFakeRedis)
	}
	v, ok := f.data[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(v, nil)
}

func (f *fakeRedis) Del(_ context.Context, keys ...string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, k := range keys {
		if _, ok := f.data[k]; ok {
			delete(f.data, k)
			delete(f.exp, k)
			n++
		}
	}
	return redis.NewIntResult(n, nil)
}

func (f *fakeRedis) TTL(_ context.Context, key string) *redis.DurationCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[key]; !ok {
		return redis.NewDurationResult(-2*time.Second, nil) // key missing
	}
	ttl, ok := f.exp[key]
	if !ok {
		return redis.NewDurationResult(-1*time.Second, nil) // no expiry
	}
	return redis.NewDurationResult(ttl, nil)
}

// getRecord decodes the stored record for a device_code, for assertions.
func (f *fakeRedis) getRecord(t *testing.T, deviceCode string) deviceRecord {
	t.Helper()
	f.mu.Lock()
	raw, ok := f.data[deviceKeyName(deviceCode)]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("no record stored for device_code")
	}
	var rec deviceRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	return rec
}

// deviceDeps builds a Deps wired with a fresh fakeRedis plus the shared auth
// stack from testDeps (so a minted device token verifies) and returns both.
func deviceDeps(t *testing.T) (Deps, *fakeRedis) {
	t.Helper()
	deps := testDeps(t,
		// The device flow itself does not touch OIDC, but tests reach the
		// protected approve endpoint by first exchanging an OIDC session for a
		// bearer token, so a valid stub identity is wired here.
		fakeOIDC{token: &oidc.IDToken{Subject: "idp-sub-1", Email: "admin@example.com"}},
		fakeMembers{member: testMember()},
	)
	fr := newFakeRedis()
	deps.Redis = fr
	deps.AdminUIBaseURL = "https://admin.example.test"
	return deps, fr
}

// seedDevice writes a pending-authorization record and its user_code lookup
// directly, bypassing the code endpoint so the token-poll states can be set up
// deterministically.
func seedDevice(t *testing.T, fr *fakeRedis, deviceCode string, rec deviceRecord, ttl time.Duration) {
	t.Helper()
	payload, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	fr.Set(context.Background(), deviceKeyName(deviceCode), string(payload), ttl)
	fr.Set(context.Background(), userCodeKeyName(rec.UserCode), deviceCode, ttl)
}

func tokenBody(deviceCode string) string {
	return fmt.Sprintf(`{"client_id":"cli","device_code":%q,"grant_type":%q}`, deviceCode, deviceGrantType)
}

// --- code endpoint ---

func TestStartDeviceAuthorization(t *testing.T) {
	deps, fr := deviceDeps(t)
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/device/code", `{"client_id":"cli","scope":"agents:read"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp deviceAuthorizationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DeviceCode == "" {
		t.Error("device_code is empty")
	}
	if resp.ExpiresIn != deviceCodeExpiresIn {
		t.Errorf("expires_in = %d, want %d", resp.ExpiresIn, deviceCodeExpiresIn)
	}
	if resp.Interval != devicePollInterval {
		t.Errorf("interval = %d, want %d", resp.Interval, devicePollInterval)
	}
	if resp.VerificationURI != "https://admin.example.test/device" {
		t.Errorf("verification_uri = %q", resp.VerificationURI)
	}
	if want := "https://admin.example.test/device?user_code=" + resp.UserCode; resp.VerificationURIComplete != want {
		t.Errorf("verification_uri_complete = %q, want %q", resp.VerificationURIComplete, want)
	}
	assertUserCodeFormat(t, resp.UserCode)

	// Both Redis keys must be present and consistent.
	got := fr.getRecord(t, resp.DeviceCode)
	if got.State != deviceStatePending {
		t.Errorf("state = %q, want pending", got.State)
	}
	if got.UserCode != resp.UserCode {
		t.Errorf("record user_code = %q, want %q", got.UserCode, resp.UserCode)
	}
	if got.ClientID != "cli" || got.Scope != "agents:read" {
		t.Errorf("record client/scope = %q/%q", got.ClientID, got.Scope)
	}
	mappedCode, err := fr.Get(context.Background(), userCodeKeyName(resp.UserCode)).Result()
	if err != nil || mappedCode != resp.DeviceCode {
		t.Errorf("user_code -> device_code lookup = %q, %v", mappedCode, err)
	}
}

func TestStartDeviceAuthorizationValidation(t *testing.T) {
	deps, _ := deviceDeps(t)
	h := mountRouter(deps)

	for _, tc := range []struct{ name, body string }{
		{"missing client_id", `{"scope":"x"}`},
		{"malformed json", `{`},
		{"unknown field", `{"client_id":"cli","bogus":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/auth/device/code", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestStartDeviceAuthorizationRedisFailure(t *testing.T) {
	deps, fr := deviceDeps(t)
	fr.failSet = true
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/device/code", `{"client_id":"cli"}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// --- token endpoint ---

func TestPollDeviceTokenErrors(t *testing.T) {
	now := time.Now().Unix()
	tests := []struct {
		name     string
		body     string
		seed     func(fr *fakeRedis)
		wantCode string
	}{
		{
			name:     "wrong grant_type",
			body:     `{"client_id":"cli","device_code":"dc","grant_type":"authorization_code"}`,
			wantCode: "unsupported_grant_type",
		},
		{
			name:     "missing device_code",
			body:     fmt.Sprintf(`{"client_id":"cli","device_code":"","grant_type":%q}`, deviceGrantType),
			wantCode: "invalid_request",
		},
		{
			name:     "unknown device_code",
			body:     tokenBody("does-not-exist"),
			wantCode: "expired_token",
		},
		{
			name: "pending",
			body: tokenBody("dc-pending"),
			seed: func(fr *fakeRedis) {
				seedDevice(t, fr, "dc-pending", deviceRecord{
					State: deviceStatePending, UserCode: "AAAA-BBBB", Interval: devicePollInterval, LastPollUnix: 0,
				}, deviceCodeTTL)
			},
			wantCode: "authorization_pending",
		},
		{
			name: "denied",
			body: tokenBody("dc-denied"),
			seed: func(fr *fakeRedis) {
				seedDevice(t, fr, "dc-denied", deviceRecord{
					State: deviceStateDenied, UserCode: "CCCC-DDDD", Interval: devicePollInterval, LastPollUnix: 0,
				}, deviceCodeTTL)
			},
			wantCode: "access_denied",
		},
		{
			name: "slow_down",
			body: tokenBody("dc-fast"),
			seed: func(fr *fakeRedis) {
				seedDevice(t, fr, "dc-fast", deviceRecord{
					State: deviceStatePending, UserCode: "EEEE-FFFF", Interval: devicePollInterval, LastPollUnix: now,
				}, deviceCodeTTL)
			},
			wantCode: "slow_down",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps, fr := deviceDeps(t)
			if tc.seed != nil {
				tc.seed(fr)
			}
			h := mountRouter(deps)
			rec := doJSON(t, h, http.MethodPost, "/auth/device/token", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			var e oauthError
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if e.Error != tc.wantCode {
				t.Errorf("error = %q, want %q", e.Error, tc.wantCode)
			}
		})
	}
}

// TestPollDeviceTokenSlowDownRaisesInterval verifies slow_down bumps the
// effective interval and last_poll so repeated fast polling is progressively
// throttled.
func TestPollDeviceTokenSlowDownRaisesInterval(t *testing.T) {
	deps, fr := deviceDeps(t)
	seedDevice(t, fr, "dc-fast", deviceRecord{
		State: deviceStatePending, UserCode: "EEEE-FFFF", Interval: devicePollInterval, LastPollUnix: time.Now().Unix(),
	}, deviceCodeTTL)
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/device/token", tokenBody("dc-fast"), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	got := fr.getRecord(t, "dc-fast")
	if got.Interval != devicePollInterval+slowDownStep {
		t.Errorf("interval = %d, want %d", got.Interval, devicePollInterval+slowDownStep)
	}
}

// TestPollDeviceTokenApproved proves an approved record mints a working session
// JWT and then deletes both keys so the code is single-use.
func TestPollDeviceTokenApproved(t *testing.T) {
	deps, fr := deviceDeps(t)
	seedDevice(t, fr, "dc-ok", deviceRecord{
		State:        deviceStateApproved,
		UserCode:     "GGGG-HHHH",
		Interval:     devicePollInterval,
		LastPollUnix: 0,
		Subject:      "idp-sub-1",
		MemberID:     "member-1",
		OrgID:        "org-1",
		Roles:        []string{"admin"},
	}, deviceCodeTTL)
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/device/token", tokenBody("dc-ok"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tok.AccessToken == "" || tok.TokenType != "Bearer" {
		t.Fatalf("bad token response: %+v", tok)
	}

	// The minted token must authenticate the protected /auth/whoami and carry
	// the bound identity.
	whoami := doJSON(t, h, http.MethodGet, "/auth/whoami", "",
		http.Header{"Authorization": {"Bearer " + tok.AccessToken}})
	if whoami.Code != http.StatusOK {
		t.Fatalf("whoami status = %d (body %s)", whoami.Code, whoami.Body.String())
	}
	var id identityResponse
	if err := json.Unmarshal(whoami.Body.Bytes(), &id); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if id.Subject != "idp-sub-1" || id.OrganizationID != "org-1" {
		t.Errorf("identity = %+v", id)
	}

	// Single-use: a second poll finds nothing.
	again := doJSON(t, h, http.MethodPost, "/auth/device/token", tokenBody("dc-ok"), nil)
	var e oauthError
	_ = json.Unmarshal(again.Body.Bytes(), &e)
	if again.Code != http.StatusBadRequest || e.Error != "expired_token" {
		t.Errorf("second poll = %d %q, want 400 expired_token", again.Code, e.Error)
	}
	if _, ok := fr.data[userCodeKeyName("GGGG-HHHH")]; ok {
		t.Error("user_code key not deleted after mint")
	}
}

// --- approve endpoint ---

func TestApproveDeviceRequiresAuth(t *testing.T) {
	deps, _ := deviceDeps(t)
	h := mountRouter(deps)
	rec := doJSON(t, h, http.MethodPost, "/auth/device/approve", `{"user_code":"AAAA-BBBB","decision":"approve"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestApproveDeviceValidation(t *testing.T) {
	deps, _ := deviceDeps(t)
	h := mountRouter(deps)
	bearer := sessionToken(t, h)

	for _, tc := range []struct{ name, body string }{
		{"missing user_code", `{"decision":"approve"}`},
		{"bad decision", `{"user_code":"AAAA-BBBB","decision":"maybe"}`},
		{"malformed", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/auth/device/approve", tc.body,
				http.Header{"Authorization": {"Bearer " + bearer}})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestApproveDeviceUnknownUserCode(t *testing.T) {
	deps, _ := deviceDeps(t)
	h := mountRouter(deps)
	bearer := sessionToken(t, h)

	rec := doJSON(t, h, http.MethodPost, "/auth/device/approve",
		`{"user_code":"ZZZZ-ZZZZ","decision":"approve"}`,
		http.Header{"Authorization": {"Bearer " + bearer}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestDeviceFlowEndToEnd walks the whole flow: a public code request, an
// authenticated approval that binds the principal, and a poll that mints the
// session token.
func TestDeviceFlowEndToEnd(t *testing.T) {
	for _, decision := range []string{"approve", "deny"} {
		t.Run(decision, func(t *testing.T) {
			deps, _ := deviceDeps(t)
			h := mountRouter(deps)
			bearer := sessionToken(t, h)

			codeRec := doJSON(t, h, http.MethodPost, "/auth/device/code", `{"client_id":"cli"}`, nil)
			if codeRec.Code != http.StatusOK {
				t.Fatalf("code status = %d", codeRec.Code)
			}
			var code deviceAuthorizationResponse
			if err := json.Unmarshal(codeRec.Body.Bytes(), &code); err != nil {
				t.Fatalf("decode code: %v", err)
			}

			apprRec := doJSON(t, h, http.MethodPost, "/auth/device/approve",
				fmt.Sprintf(`{"user_code":%q,"decision":%q}`, code.UserCode, decision),
				http.Header{"Authorization": {"Bearer " + bearer}})
			if apprRec.Code != http.StatusOK {
				t.Fatalf("approve status = %d (body %s)", apprRec.Code, apprRec.Body.String())
			}

			tokRec := doJSON(t, h, http.MethodPost, "/auth/device/token", tokenBody(code.DeviceCode), nil)
			if decision == "approve" {
				if tokRec.Code != http.StatusOK {
					t.Fatalf("token status = %d (body %s)", tokRec.Code, tokRec.Body.String())
				}
				var tok tokenResponse
				if err := json.Unmarshal(tokRec.Body.Bytes(), &tok); err != nil {
					t.Fatalf("decode token: %v", err)
				}
				if tok.AccessToken == "" {
					t.Error("empty access token after approval")
				}
			} else {
				var e oauthError
				_ = json.Unmarshal(tokRec.Body.Bytes(), &e)
				if tokRec.Code != http.StatusBadRequest || e.Error != "access_denied" {
					t.Errorf("token = %d %q, want 400 access_denied", tokRec.Code, e.Error)
				}
			}
		})
	}
}

// --- helpers ---

// sessionToken performs the OIDC session exchange against h and returns the
// minted bearer token, used to authenticate the protected approve endpoint.
func sessionToken(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/auth/session",
		`{"id_token":"raw","organization_id":"org-1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session exchange status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode session token: %v", err)
	}
	return tok.AccessToken
}

func assertUserCodeFormat(t *testing.T, code string) {
	t.Helper()
	if len(code) != userCodeGroupLen*userCodeGroups+1 {
		t.Fatalf("user_code %q has wrong length", code)
	}
	if code[userCodeGroupLen] != '-' {
		t.Fatalf("user_code %q missing separator", code)
	}
	for i, c := range code {
		if i == userCodeGroupLen {
			continue
		}
		if !strings.ContainsRune(userCodeAlphabet, c) {
			t.Fatalf("user_code %q has out-of-alphabet char %q", code, c)
		}
	}
}

func TestGenerateUserCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := generateUserCode()
		if err != nil {
			t.Fatalf("generateUserCode: %v", err)
		}
		assertUserCodeFormat(t, code)
		seen[code] = true
	}
	// Not a strict uniqueness guarantee, but 200 draws from a 20^8 space should
	// never collide; a tiny set signals a broken generator.
	if len(seen) < 190 {
		t.Errorf("only %d distinct codes from 200 draws", len(seen))
	}
}

// ensure the fake satisfies the interface the handlers use.
var _ DeviceStore = (*fakeRedis)(nil)
