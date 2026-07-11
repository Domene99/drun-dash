package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// mockBase is the base URL used in tests. The in-memory transport ignores the
// host and routes purely by path, so any well-formed URL works.
const mockBase = "http://cp.test"

// handlerTransport serves an http.Handler entirely in memory via
// httptest.NewRecorder — no socket is bound, so tests run in restricted
// sandboxes and never touch the network.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func mockClient(h http.Handler) *http.Client {
	return &http.Client{Transport: handlerTransport{h}}
}

// fakeClock returns a clock whose sleep advances a virtual "now" instead of
// blocking, so poll loops run instantly and deterministically.
func fakeClock(start time.Time) clock {
	cur := start
	return clock{
		now:   func() time.Time { return cur },
		sleep: func(d time.Duration) { cur = cur.Add(d) },
	}
}

// deviceServer is a configurable mock of the control-plane device endpoints.
type deviceServer struct {
	t *testing.T

	// tokenReplies is consumed one entry per /auth/device/token poll. Each
	// entry is either an OAuth pending/error (status 400) or, when accessToken
	// is set, a success (status 200). Once exhausted, the last entry repeats.
	tokenReplies []tokenReply

	pollCount int
}

type tokenReply struct {
	accessToken string // non-empty => 200 TokenResponse
	oauthErr    string // used when accessToken == "" => 400 OAuthError
	expiresIn   int    // for the success token
	refresh     string // optional refresh token
}

func newDeviceServer(t *testing.T, replies ...tokenReply) (*http.Client, *deviceServer) {
	ds := &deviceServer{t: t, tokenReplies: replies}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/device/code", ds.handleCode)
	mux.HandleFunc("/auth/device/token", ds.handleToken)
	return mockClient(mux), ds
}

func (ds *deviceServer) handleCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		ds.t.Errorf("device/code method = %s, want POST", r.Method)
	}
	var req deviceAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ds.t.Errorf("device/code decode: %v", err)
	}
	if req.ClientID == "" {
		ds.t.Errorf("device/code missing client_id")
	}
	writeJSON(w, http.StatusOK, deviceAuthResponse{
		DeviceCode:              "dev-code-123",
		UserCode:                "BCDF-GHJK",
		VerificationURI:         "https://example.com/device",
		VerificationURIComplete: "https://example.com/device?code=BCDF-GHJK",
		ExpiresIn:               600,
		Interval:                5,
	})
}

func (ds *deviceServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		ds.t.Errorf("device/token method = %s, want POST", r.Method)
	}
	var req deviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ds.t.Errorf("device/token decode: %v", err)
	}
	if req.GrantType != deviceCodeGrantType {
		ds.t.Errorf("device/token grant_type = %q, want %q", req.GrantType, deviceCodeGrantType)
	}
	if req.DeviceCode != "dev-code-123" {
		ds.t.Errorf("device/token device_code = %q, want dev-code-123", req.DeviceCode)
	}

	idx := ds.pollCount
	ds.pollCount++
	if idx >= len(ds.tokenReplies) {
		idx = len(ds.tokenReplies) - 1
	}
	reply := ds.tokenReplies[idx]

	if reply.accessToken != "" {
		expires := reply.expiresIn
		if expires == 0 {
			expires = 900
		}
		writeJSON(w, http.StatusOK, tokenResponse{
			AccessToken:  reply.accessToken,
			TokenType:    "Bearer",
			ExpiresIn:    expires,
			RefreshToken: reply.refresh,
		})
		return
	}
	writeJSON(w, http.StatusBadRequest, oauthError{ErrorCode: reply.oauthErr})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestDeviceLoginPendingThenSuccess(t *testing.T) {
	hc, ds := newDeviceServer(t,
		tokenReply{oauthErr: "authorization_pending"},
		tokenReply{oauthErr: "authorization_pending"},
		tokenReply{accessToken: "tok-abc", expiresIn: 900, refresh: "ref-1"},
	)

	var out bytes.Buffer
	tok, err := deviceLogin(hc, mockBase, "drun-dash-cli", "", &out, fakeClock(time.Unix(0, 0)))
	if err != nil {
		t.Fatalf("deviceLogin: %v", err)
	}
	if tok.AccessToken != "tok-abc" || tok.RefreshToken != "ref-1" {
		t.Errorf("token = %+v, want access tok-abc / refresh ref-1", tok)
	}
	if ds.pollCount != 3 {
		t.Errorf("pollCount = %d, want 3", ds.pollCount)
	}
	// The verification URL and user code must be surfaced to the operator.
	if !strings.Contains(out.String(), "BCDF-GHJK") || !strings.Contains(out.String(), "https://example.com/device") {
		t.Errorf("prompt missing user code or verification URL:\n%s", out.String())
	}
}

func TestDeviceLoginSlowDown(t *testing.T) {
	hc, _ := newDeviceServer(t,
		tokenReply{oauthErr: "authorization_pending"},
		tokenReply{oauthErr: "slow_down"},
		tokenReply{oauthErr: "authorization_pending"},
		tokenReply{accessToken: "tok-slow"},
	)

	// Track how long each sleep was to confirm slow_down increased the wait.
	var sleeps []time.Duration
	cur := time.Unix(0, 0)
	clk := clock{
		now: func() time.Time { return cur },
		sleep: func(d time.Duration) {
			sleeps = append(sleeps, d)
			cur = cur.Add(d)
		},
	}

	var out bytes.Buffer
	tok, err := deviceLogin(hc, mockBase, "drun-dash-cli", "", &out, clk)
	if err != nil {
		t.Fatalf("deviceLogin: %v", err)
	}
	if tok.AccessToken != "tok-slow" {
		t.Errorf("access token = %q, want tok-slow", tok.AccessToken)
	}
	if len(sleeps) < 2 {
		t.Fatalf("expected at least 2 sleeps, got %d", len(sleeps))
	}
	// interval is 5s; after slow_down it must grow by the backoff.
	if sleeps[0] != 5*time.Second {
		t.Errorf("first sleep = %v, want 5s", sleeps[0])
	}
	if sleeps[1] <= sleeps[0] {
		t.Errorf("sleep after slow_down = %v, want > %v", sleeps[1], sleeps[0])
	}
}

func TestDeviceLoginExpiry(t *testing.T) {
	// Server never approves; the code's 600s lifetime lapses under the fake
	// clock as sleeps accumulate.
	hc, _ := newDeviceServer(t, tokenReply{oauthErr: "authorization_pending"})

	var out bytes.Buffer
	_, err := deviceLogin(hc, mockBase, "drun-dash-cli", "", &out, fakeClock(time.Unix(0, 0)))
	if !errors.Is(err, errDeviceCodeExpired) {
		t.Fatalf("deviceLogin err = %v, want errDeviceCodeExpired", err)
	}
}

func TestDeviceLoginServerExpiredToken(t *testing.T) {
	hc, _ := newDeviceServer(t,
		tokenReply{oauthErr: "authorization_pending"},
		tokenReply{oauthErr: "expired_token"},
	)

	var out bytes.Buffer
	_, err := deviceLogin(hc, mockBase, "drun-dash-cli", "", &out, fakeClock(time.Unix(0, 0)))
	if !errors.Is(err, errDeviceCodeExpired) {
		t.Fatalf("deviceLogin err = %v, want errDeviceCodeExpired", err)
	}
}

func TestDeviceLoginAccessDenied(t *testing.T) {
	hc, _ := newDeviceServer(t, tokenReply{oauthErr: "access_denied"})

	var out bytes.Buffer
	_, err := deviceLogin(hc, mockBase, "drun-dash-cli", "", &out, fakeClock(time.Unix(0, 0)))
	if !errors.Is(err, errAccessDenied) {
		t.Fatalf("deviceLogin err = %v, want errAccessDenied", err)
	}
}

// TestLoginCommandEndToEnd drives the whole `login` command through Run,
// including token storage, against a mock that approves on the first poll.
func TestLoginCommandEndToEnd(t *testing.T) {
	hc, _ := newDeviceServer(t, tokenReply{accessToken: "stored-tok", expiresIn: 900, refresh: "stored-ref"})
	withAuthClient(t, hc)

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)

	var out, errOut bytes.Buffer
	code := Run([]string{"login", "--server", mockBase}, &out, &errOut)
	if code != 0 {
		t.Fatalf("login exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Login successful") {
		t.Errorf("stdout missing success message:\n%s", out.String())
	}

	// The token file must exist with 0600 perms and the right contents.
	path := filepath.Join(cfgDir, "credentials.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perm = %o, want 600", perm)
	}

	tok, err := clitoken.New(cfgDir).Load()
	if err != nil {
		t.Fatalf("load stored token: %v", err)
	}
	if tok.AccessToken != "stored-tok" || tok.RefreshToken != "stored-ref" {
		t.Errorf("stored token = %+v, want access stored-tok / refresh stored-ref", tok)
	}
	if tok.ServerURL != mockBase {
		t.Errorf("stored ServerURL = %q, want %q", tok.ServerURL, mockBase)
	}
	if tok.ExpiresAt.IsZero() || tok.Expired() {
		t.Errorf("stored ExpiresAt = %v, want a future time", tok.ExpiresAt)
	}
}

// withAuthClient temporarily points the auth commands at the given HTTP client
// (an in-memory mock), restoring the default when the test ends.
func withAuthClient(t *testing.T, hc *http.Client) {
	t.Helper()
	old := newAuthHTTPClient
	newAuthHTTPClient = func() *http.Client { return hc }
	t.Cleanup(func() { newAuthHTTPClient = old })
}

func TestResolveServer(t *testing.T) {
	t.Run("flag wins and trailing slash trimmed", func(t *testing.T) {
		t.Setenv(serverEnv, "https://env.example.com")
		if got := resolveServer("https://flag.example.com/"); got != "https://flag.example.com" {
			t.Errorf("resolveServer = %q, want https://flag.example.com", got)
		}
	})
	t.Run("env used when flag empty", func(t *testing.T) {
		t.Setenv(serverEnv, "https://env.example.com/")
		if got := resolveServer(""); got != "https://env.example.com" {
			t.Errorf("resolveServer = %q, want https://env.example.com", got)
		}
	})
	t.Run("default when both empty", func(t *testing.T) {
		t.Setenv(serverEnv, "")
		if got := resolveServer(""); got != defaultServer {
			t.Errorf("resolveServer = %q, want %q", got, defaultServer)
		}
	})
}
