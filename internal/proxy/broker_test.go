package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
	"github.com/Domene99/drun-dash/internal/vault"
)

// roundTripFunc adapts a function to an http.RoundTripper so tests can stand in
// for the upstream without binding a socket, exercising the broker's forward
// path (request construction, header copying, streaming) end to end in-process.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubVault is a Vault whose Resolve returns a fixed secret or error, letting
// tests drive the broker's credential-injection and failure paths without any
// environment state.
type stubVault struct {
	secret vault.Secret
	err    error
}

func (s stubVault) Resolve(context.Context, string) (vault.Secret, error) {
	return s.secret, s.err
}

// Fake store seams. The broker holds interfaces, so these fixed-value fakes let
// each test drive one path (a valid grant, an unknown token, a revoked grant, a
// resolve failure) without a database.

type fakeGrants struct {
	grant store.CredentialGrant
	err   error
}

func (f fakeGrants) GetByTokenHash(context.Context, string) (store.CredentialGrant, error) {
	return f.grant, f.err
}

type fakeSessions struct {
	session store.Session
	err     error
}

func (f fakeSessions) GetByID(context.Context, string) (store.Session, error) {
	return f.session, f.err
}

type fakeEndpoints struct {
	endpoint store.ModelEndpoint
	err      error
}

func (f fakeEndpoints) GetByID(context.Context, string) (store.ModelEndpoint, error) {
	return f.endpoint, f.err
}

// fakeUsage records every Append so a test can assert the metered attribution.
type fakeUsage struct {
	mu     sync.Mutex
	events []store.AppendUsageEventParams
	err    error
}

func (f *fakeUsage) Append(_ context.Context, p store.AppendUsageEventParams) (store.UsageEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, p)
	return store.UsageEvent{}, f.err
}

func (f *fakeUsage) recorded() []store.AppendUsageEventParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.AppendUsageEventParams(nil), f.events...)
}

// fakeBudgets stands in for the AnalyticsStore's BudgetUtilization: it returns a
// fixed set of budget rows (or an error) so a test can drive the under-cap,
// at/over-cap, and lookup-failure paths without a database. The zero value
// returns no rows, i.e. no budgets configured -> always under cap.
type fakeBudgets struct {
	rows []store.BudgetUsageRow
	err  error
}

func (f fakeBudgets) BudgetUtilization(context.Context, string, time.Time) ([]store.BudgetUsageRow, error) {
	return f.rows, f.err
}

// testSecret is the master credential the default test vault injects.
const testSecret = "sk-master-test"

// Identifiers the default valid fixtures share, so a test can assert usage
// attribution flows from the grant → session → endpoint.
const (
	testProxyToken = "dgp_testtoken"
	testSessionID  = "sess-1"
	testOrgID      = "org-1"
	testAgentID    = "agent-1"
	testEndpointID = "ep-1"
	testSecretRef  = "vault://dev/anthropic"
)

// scopesJSON marshals a grant scope authorizing the given operations for the
// test endpoint, mirroring what the launch handler stores.
func scopesJSON(operations ...string) json.RawMessage {
	raw, _ := json.Marshal(grantScopes{EndpointID: testEndpointID, Operations: operations})
	return raw
}

// validGrant is a live, unexpired, unrevoked grant scoped to chat.completions on
// the test endpoint.
func validGrant() store.CredentialGrant {
	ep := testEndpointID
	return store.CredentialGrant{
		ID:             "grant-1",
		SessionID:      testSessionID,
		ProxyTokenHash: sha256Hex(testProxyToken),
		EndpointID:     &ep,
		Scopes:         scopesJSON(operationChatCompletions),
		IssuedAt:       time.Now().Add(-time.Minute),
		ExpiresAt:      time.Now().Add(time.Hour),
	}
}

// activeSession is the session owning validGrant, in the active state.
func activeSession() store.Session {
	return store.Session{
		ID:      testSessionID,
		OrgID:   testOrgID,
		AgentID: testAgentID,
		Status:  store.SessionStatusActive,
	}
}

// testEndpoint is the endpoint validGrant pins, carrying the vault ref for the
// upstream master credential.
func testEndpoint() store.ModelEndpoint {
	return store.ModelEndpoint{ID: testEndpointID, OrgID: testOrgID, SecretRef: testSecretRef}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestBroker builds a broker wired to the given RoundTripper as its upstream,
// a stub vault that resolves to testSecret, and the default valid store
// fixtures (a live grant, its active session, its endpoint, and a usage
// recorder). Tests exercising a specific failure override the relevant field
// (b.grants, b.sessions, b.endpoints, b.vault) before calling chatCompletions.
func newTestBroker(t *testing.T, upstream string, rt http.RoundTripper) *broker {
	t.Helper()
	u, err := parseUpstream(upstream)
	if err != nil {
		t.Fatalf("parseUpstream(%q): %v", upstream, err)
	}
	return &broker{
		upstream:  u,
		vault:     stubVault{secret: vault.Secret{Value: testSecret}},
		grants:    fakeGrants{grant: validGrant()},
		sessions:  fakeSessions{session: activeSession()},
		endpoints: fakeEndpoints{endpoint: testEndpoint()},
		budgets:   fakeBudgets{},
		usage:     &fakeUsage{},
		client: &http.Client{
			Transport: rt,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: testLogger(),
	}
}

// authorizedRequest builds a POST to the broker route carrying the default
// valid proxy token.
func authorizedRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testProxyToken)
	return req
}

// TestBrokerPassesJSONThrough asserts a non-streamed JSON response is forwarded
// faithfully: method, path, query, and safe request headers/body reach the
// upstream, hop-by-hop headers are dropped, the client's proxy token is replaced
// by the injected master credential, and the upstream status, headers
// (including Content-Type), and body reach the client unchanged.
func TestBrokerPassesJSONThrough(t *testing.T) {
	var got *http.Request
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return &http.Response{
			StatusCode: http.StatusCreated, // non-200 to prove status propagation
			Header: http.Header{
				"Content-Type":      {"application/json"},
				"X-Upstream-Marker": {"yes"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions?stream=false&model=gpt", `{"model":"gpt"}`)
	req.Header.Set("Content-Type", "application/json")
	// Hop-by-hop headers that must not be forwarded end to end.
	req.Header.Set("Connection", "X-Secret")
	req.Header.Set("X-Secret", "leak")
	req.Header.Set("Proxy-Authorization", "creds")

	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	// Request reached the upstream verbatim.
	if got == nil {
		t.Fatal("upstream never received a request")
	}
	if got.Method != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", got.Method)
	}
	if got.URL.Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", got.URL.Path)
	}
	if got.URL.RawQuery != "stream=false&model=gpt" {
		t.Errorf("upstream query = %q, want stream=false&model=gpt", got.URL.RawQuery)
	}
	if got.URL.Host != "upstream.test" {
		t.Errorf("upstream host = %q, want upstream.test", got.URL.Host)
	}
	// The master credential is injected; the client's own proxy token is gone.
	if got.Header.Get("Authorization") != "Bearer "+testSecret {
		t.Errorf("upstream Authorization = %q, want the injected master credential", got.Header.Get("Authorization"))
	}
	if strings.Contains(got.Header.Get("Authorization"), testProxyToken) {
		t.Errorf("upstream Authorization = %q, must not carry the client's proxy token", got.Header.Get("Authorization"))
	}
	if gotBody != `{"model":"gpt"}` {
		t.Errorf("upstream body = %q, want the client's body forwarded", gotBody)
	}
	if v := got.Header.Get("X-Secret"); v != "" {
		t.Errorf("X-Secret = %q, want it dropped (named in Connection)", v)
	}
	if v := got.Header.Get("Proxy-Authorization"); v != "" {
		t.Errorf("Proxy-Authorization = %q, want it dropped (hop-by-hop)", v)
	}

	// Response reached the client verbatim.
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("X-Upstream-Marker"); got != "yes" {
		t.Errorf("X-Upstream-Marker = %q, want yes", got)
	}
	if body := rec.Body.String(); body != `{"ok":true}` {
		t.Errorf("body = %q, want %q", body, `{"ok":true}`)
	}
}

// TestBrokerInjectsVaultCredential asserts the broker resolves the upstream
// credential from a real EnvVault using the grant endpoint's secret_ref and
// injects it as the Authorization header, overwriting the client's proxy token
// so the master key is never client-chosen and the client's own header never
// reaches the upstream.
func TestBrokerInjectsVaultCredential(t *testing.T) {
	t.Setenv("DRUNDASH_SECRET_ANTHROPIC", "sk-master-env")

	var gotAuth string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	b.vault = vault.NewEnvVault() // endpoint.SecretRef defaults to vault://dev/anthropic

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotAuth != "Bearer sk-master-env" {
		t.Errorf("upstream Authorization = %q, want the vault-resolved master credential", gotAuth)
	}
	if strings.Contains(gotAuth, testProxyToken) {
		t.Errorf("upstream Authorization = %q, must not carry the client-supplied value", gotAuth)
	}
}

// TestBrokerUnresolvableSecretIs502 asserts that when the upstream credential
// cannot be resolved (secret not registered) the request is not forwarded, the
// client receives a flat 502 upstream_credential_unavailable envelope, and the
// missing-secret ref/value never leaks into the response body.
func TestBrokerUnresolvableSecretIs502(t *testing.T) {
	// DRUNDASH_SECRET_ANTHROPIC intentionally left unset -> ErrNotFound.
	forwarded := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return nil, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	b.vault = vault.NewEnvVault()

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if forwarded {
		t.Fatal("request was forwarded upstream without a credential")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, upstreamCredentialCode)
	if strings.Contains(rec.Body.String(), "DRUNDASH_SECRET") {
		t.Errorf("error body leaks the credential env mapping: %q", rec.Body.String())
	}
}

// TestBrokerInvalidSecretRefIs502 asserts a malformed endpoint secret ref
// surfaces as a flat 502 upstream_credential_unavailable rather than forwarding.
func TestBrokerInvalidSecretRefIs502(t *testing.T) {
	forwarded := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return nil, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	b.vault = vault.NewEnvVault()
	b.endpoints = fakeEndpoints{endpoint: store.ModelEndpoint{ID: testEndpointID, SecretRef: "not-a-vault-ref"}}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if forwarded {
		t.Fatal("request was forwarded upstream with an invalid secret ref")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, upstreamCredentialCode)
}

// TestBrokerEndpointLookupFailureIs502 asserts that a grant whose endpoint
// cannot be loaded yields 502 upstream_credential_unavailable without
// forwarding — the proxy has no key to inject.
func TestBrokerEndpointLookupFailureIs502(t *testing.T) {
	forwarded := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return nil, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	b.endpoints = fakeEndpoints{err: store.ErrNotFound}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if forwarded {
		t.Fatal("request was forwarded upstream with an unresolvable endpoint")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, upstreamCredentialCode)
}

// TestBrokerGrantWithoutEndpointIs502 asserts a grant with no endpoint (nothing
// to resolve a credential from) yields 502 upstream_credential_unavailable.
func TestBrokerGrantWithoutEndpointIs502(t *testing.T) {
	forwarded := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return nil, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	g := validGrant()
	g.EndpointID = nil
	b.grants = fakeGrants{grant: g}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if forwarded {
		t.Fatal("request was forwarded upstream despite a grant with no endpoint")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, upstreamCredentialCode)
}

// --- Grant validation (ADR-0003 §3) -----------------------------------------

// noForwardBroker builds a broker whose upstream RoundTripper fails the test if
// it is ever called, so a rejected request that nonetheless forwards is caught.
func noForwardBroker(t *testing.T) *broker {
	t.Helper()
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request was forwarded upstream despite a rejected grant")
		return nil, nil
	})
	return newTestBroker(t, "https://upstream.test", rt)
}

func TestBrokerMissingTokenIs401(t *testing.T) {
	b := noForwardBroker(t)
	// No Authorization header at all.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerMalformedAuthorizationIs401(t *testing.T) {
	b := noForwardBroker(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Basic Zm9vOmJhcg==") // wrong scheme
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerUnknownTokenIs401(t *testing.T) {
	b := noForwardBroker(t)
	b.grants = fakeGrants{err: store.ErrNotFound}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerGrantLookupErrorIs500(t *testing.T) {
	b := noForwardBroker(t)
	b.grants = fakeGrants{err: context.DeadlineExceeded} // a non-NotFound store error

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertErrorCode(t, rec, "internal_error")
}

func TestBrokerRevokedGrantIs401(t *testing.T) {
	b := noForwardBroker(t)
	g := validGrant()
	revoked := time.Now().Add(-time.Minute)
	g.RevokedAt = &revoked
	b.grants = fakeGrants{grant: g}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerExpiredGrantIs401(t *testing.T) {
	b := noForwardBroker(t)
	g := validGrant()
	g.ExpiresAt = time.Now().Add(-time.Second)
	b.grants = fakeGrants{grant: g}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerTerminatedSessionIs401(t *testing.T) {
	b := noForwardBroker(t)
	s := activeSession()
	s.Status = store.SessionStatusTerminated
	b.sessions = fakeSessions{session: s}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerFailedSessionIs401(t *testing.T) {
	b := noForwardBroker(t)
	s := activeSession()
	s.Status = store.SessionStatusFailed
	b.sessions = fakeSessions{session: s}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerMissingSessionIs401(t *testing.T) {
	b := noForwardBroker(t)
	b.sessions = fakeSessions{err: store.ErrNotFound}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "unauthorized")
}

func TestBrokerScopeViolationIs403(t *testing.T) {
	b := noForwardBroker(t)
	g := validGrant()
	g.Scopes = scopesJSON("embeddings") // does not authorize chat.completions
	b.grants = fakeGrants{grant: g}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	assertErrorCode(t, rec, "forbidden")
}

func TestBrokerEmptyScopesIs403(t *testing.T) {
	b := noForwardBroker(t)
	g := validGrant()
	g.Scopes = json.RawMessage(`[]`) // the column default: no operations
	b.grants = fakeGrants{grant: g}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	assertErrorCode(t, rec, "forbidden")
}

// --- Metering (ADR-0003 §5) --------------------------------------------------

// TestBrokerMetersJSONResponse asserts a successful non-streamed call records a
// usage_events row attributed through the grant's session and endpoint, with the
// token counts and model read from the upstream body.
func TestBrokerMetersJSONResponse(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"model":"gpt-4o","usage":{"prompt_tokens":11,"completion_tokens":22}}`)),
		}, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	usage := &fakeUsage{}
	b.usage = usage

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4o"}`)
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	events := usage.recorded()
	if len(events) != 1 {
		t.Fatalf("recorded %d usage events, want 1", len(events))
	}
	e := events[0]
	if e.OrgID != testOrgID {
		t.Errorf("org_id = %q, want %q", e.OrgID, testOrgID)
	}
	if e.SessionID == nil || *e.SessionID != testSessionID {
		t.Errorf("session_id = %v, want %q", e.SessionID, testSessionID)
	}
	if e.AgentID == nil || *e.AgentID != testAgentID {
		t.Errorf("agent_id = %v, want %q", e.AgentID, testAgentID)
	}
	if e.EndpointID == nil || *e.EndpointID != testEndpointID {
		t.Errorf("endpoint_id = %v, want %q", e.EndpointID, testEndpointID)
	}
	if e.Model == nil || *e.Model != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", e.Model)
	}
	if e.PromptTokens != 11 || e.CompletionTokens != 22 {
		t.Errorf("tokens = (%d, %d), want (11, 22)", e.PromptTokens, e.CompletionTokens)
	}
}

// TestBrokerMetersSSEResponse asserts token counts are read from the trailing
// usage event of an SSE stream.
func TestBrokerMetersSSEResponse(t *testing.T) {
	body := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	usage := &fakeUsage{}
	b.usage = usage

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", `{"stream":true}`)
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	events := usage.recorded()
	if len(events) != 1 {
		t.Fatalf("recorded %d usage events, want 1", len(events))
	}
	e := events[0]
	if e.PromptTokens != 5 || e.CompletionTokens != 7 {
		t.Errorf("tokens = (%d, %d), want (5, 7)", e.PromptTokens, e.CompletionTokens)
	}
	if e.Model == nil || *e.Model != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", e.Model)
	}
}

// TestBrokerSkipsMeteringWithoutUsage asserts a response that reports no usage
// object (e.g. an SSE stream the client did not opt into usage on) records
// nothing rather than a zero-token row.
func TestBrokerSkipsMeteringWithoutUsage(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"model":"gpt-4o","choices":[]}`)),
		}, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	usage := &fakeUsage{}
	b.usage = usage

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if events := usage.recorded(); len(events) != 0 {
		t.Fatalf("recorded %d usage events, want 0 when no usage is reported", len(events))
	}
}

// TestBrokerSkipsMeteringOnUpstreamError asserts a non-2xx upstream response is
// not metered even if its body happens to contain a usage-shaped object.
func TestBrokerSkipsMeteringOnUpstreamError(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)),
		}, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	usage := &fakeUsage{}
	b.usage = usage

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if events := usage.recorded(); len(events) != 0 {
		t.Fatalf("recorded %d usage events, want 0 for a non-2xx upstream response", len(events))
	}
}

// streamRecorder is an http.ResponseWriter that captures each Flush as a
// discrete chunk, so a test can observe that streamed bytes are delivered
// incrementally rather than buffered until the handler returns.
type streamRecorder struct {
	header http.Header
	code   int
	chunks chan string
	mu     sync.Mutex
	buf    []byte
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: http.Header{}, chunks: make(chan string, 8)}
}

func (s *streamRecorder) Header() http.Header  { return s.header }
func (s *streamRecorder) WriteHeader(code int) { s.code = code }

func (s *streamRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	s.buf = append(s.buf, b...)
	s.mu.Unlock()
	return len(b), nil
}

func (s *streamRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		s.chunks <- string(s.buf)
		s.buf = nil
	}
}

// TestBrokerStreamsSSEIncrementally asserts SSE chunks are flushed to the client
// as they arrive rather than buffered until the upstream body completes. The
// upstream body is an io.Pipe the test feeds one event at a time; a buffering
// proxy would deliver nothing until the pipe closes, so recvChunk would time
// out.
func TestBrokerStreamsSSEIncrementally(t *testing.T) {
	pr, pw := io.Pipe()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"text/event-stream"}},
			Body:          pr,
			ContentLength: -1,
		}, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := newStreamRecorder()

	done := make(chan struct{})
	go func() {
		b.chatCompletions(rec, req)
		close(done)
	}()

	if _, err := io.WriteString(pw, "data: one\n\n"); err != nil {
		t.Fatalf("write first event: %v", err)
	}
	if got := recvChunk(t, rec.chunks); got != "data: one\n\n" {
		t.Fatalf("first streamed chunk = %q, want %q", got, "data: one\n\n")
	}

	if _, err := io.WriteString(pw, "data: two\n\n"); err != nil {
		t.Fatalf("write second event: %v", err)
	}
	if got := recvChunk(t, rec.chunks); got != "data: two\n\n" {
		t.Fatalf("second streamed chunk = %q, want %q", got, "data: two\n\n")
	}

	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	<-done

	if rec.code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.code, http.StatusOK)
	}
	if got := rec.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
}

// recvChunk receives one flushed chunk with a timeout, failing the test if none
// arrives — which is what a broken (buffered) flush looks like.
func recvChunk(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a flushed chunk; flush is not incremental")
		return ""
	}
}

// TestBrokerUpstreamErrorIs502 asserts a transport failure yields a flat 502
// bad_gateway envelope rather than a panic or a hung request.
func TestBrokerUpstreamErrorIs502(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "https://upstream.test", Err: io.ErrUnexpectedEOF}
	})
	b := newTestBroker(t, "https://upstream.test", rt)

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, badGatewayCode)
}

// TestBrokerUnconfiguredIs502 asserts that with LLM_UPSTREAM_BASE_URL unset the
// broker route reports 502 upstream_unconfigured instead of forwarding.
func TestBrokerUnconfiguredIs502(t *testing.T) {
	t.Setenv(envUpstreamBaseURL, "") // empty == unset
	b := newBroker(testLogger(), nil)
	if b.upstream != nil {
		t.Fatal("expected nil upstream when unconfigured")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, upstreamUnconfiguredCode)
}

// TestBrokerInvalidUpstreamIs502 asserts an invalid base URL is caught at
// construction and surfaces as 502 upstream_unconfigured.
func TestBrokerInvalidUpstreamIs502(t *testing.T) {
	t.Setenv(envUpstreamBaseURL, "ftp://example.com") // unsupported scheme
	b := newBroker(testLogger(), nil)
	if b.upstream != nil {
		t.Fatal("expected nil upstream for an invalid base URL")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, upstreamUnconfiguredCode)
}

// TestBrokerConfiguredFromEnv asserts a valid LLM_UPSTREAM_BASE_URL is accepted
// at construction.
func TestBrokerConfiguredFromEnv(t *testing.T) {
	t.Setenv(envUpstreamBaseURL, "https://api.openai.com")
	b := newBroker(testLogger(), nil)
	if b.upstream == nil {
		t.Fatal("expected a configured upstream")
	}
	if b.upstream.Host != "api.openai.com" {
		t.Errorf("upstream host = %q, want api.openai.com", b.upstream.Host)
	}
}

// --- Unit tests for the helpers ---------------------------------------------

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{"Bearer dgp_abc", "dgp_abc", true},
		{"bearer dgp_abc", "dgp_abc", true}, // scheme is case-insensitive
		{"Bearer   dgp_abc  ", "dgp_abc", true},
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic dgp_abc", "", false},
		{"dgp_abc", "", false},
	}
	for _, c := range cases {
		gotToken, gotOK := bearerToken(c.header)
		if gotToken != c.wantToken || gotOK != c.wantOK {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", c.header, gotToken, gotOK, c.wantToken, c.wantOK)
		}
	}
}

func TestGrantAllows(t *testing.T) {
	if !grantAllows(scopesJSON("chat.completions", "embeddings"), "chat.completions") {
		t.Error("grantAllows = false, want true when the operation is listed")
	}
	if grantAllows(scopesJSON("embeddings"), "chat.completions") {
		t.Error("grantAllows = true, want false when the operation is absent")
	}
	if grantAllows(json.RawMessage(`[]`), "chat.completions") {
		t.Error("grantAllows = true, want false for the empty [] default")
	}
	if grantAllows(json.RawMessage(`not json`), "chat.completions") {
		t.Error("grantAllows = true, want false (fail closed) for unparseable scopes")
	}
	if grantAllows(nil, "chat.completions") {
		t.Error("grantAllows = true, want false for nil scopes")
	}
}

func TestExtractUsage(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		model, pt, ct, ok := extractUsage("application/json", []byte(`{"model":"m","usage":{"prompt_tokens":3,"completion_tokens":4}}`))
		if !ok || model != "m" || pt != 3 || ct != 4 {
			t.Errorf("extractUsage json = (%q, %d, %d, %v), want (m, 3, 4, true)", model, pt, ct, ok)
		}
	})
	t.Run("json without usage", func(t *testing.T) {
		if _, _, _, ok := extractUsage("application/json", []byte(`{"model":"m"}`)); ok {
			t.Error("extractUsage = ok, want false when usage is absent")
		}
	})
	t.Run("sse", func(t *testing.T) {
		body := []byte("data: {\"model\":\"m\"}\n\ndata: {\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n")
		model, pt, ct, ok := extractUsage("text/event-stream; charset=utf-8", body)
		if !ok || model != "m" || pt != 8 || ct != 9 {
			t.Errorf("extractUsage sse = (%q, %d, %d, %v), want (m, 8, 9, true)", model, pt, ct, ok)
		}
	})
	t.Run("sse without usage", func(t *testing.T) {
		if _, _, _, ok := extractUsage("text/event-stream", []byte("data: {\"model\":\"m\"}\n\ndata: [DONE]\n\n")); ok {
			t.Error("extractUsage = ok, want false when the stream reports no usage")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, _, _, ok := extractUsage("application/json", []byte("not json")); ok {
			t.Error("extractUsage = ok, want false for an unparseable body")
		}
	})
}

// assertErrorCode decodes a flat httpx error envelope from a recorder and checks
// its code and Content-Type.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Code != wantCode {
		t.Errorf("error code = %q, want %q", body.Code, wantCode)
	}
	if body.Message == "" {
		t.Error("error message is empty, want a human-readable explanation")
	}
}

// TestParseUpstream covers base-URL validation directly.
func TestParseUpstream(t *testing.T) {
	valid := []string{"https://api.openai.com", "http://localhost:8080", "https://host/prefix"}
	for _, in := range valid {
		if _, err := parseUpstream(in); err != nil {
			t.Errorf("parseUpstream(%q) = %v, want no error", in, err)
		}
	}
	invalid := []string{"", "api.openai.com", "ftp://example.com", "https://"}
	for _, in := range invalid {
		if _, err := parseUpstream(in); err == nil {
			t.Errorf("parseUpstream(%q) = nil error, want an error", in)
		}
	}
}

// TestSingleJoiningSlash covers the path-join helper's slash handling.
func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "/v1/chat/completions", "/v1/chat/completions"},
		{"/", "/v1/chat/completions", "/v1/chat/completions"},
		{"/prefix", "/v1/chat/completions", "/prefix/v1/chat/completions"},
		{"/prefix/", "/v1/chat/completions", "/prefix/v1/chat/completions"},
		{"prefix", "v1", "prefix/v1"},
	}
	for _, c := range cases {
		if got := singleJoiningSlash(c.a, c.b); got != c.want {
			t.Errorf("singleJoiningSlash(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}
