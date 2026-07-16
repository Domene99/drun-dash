package memberui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/uikit"
	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// storedSession seeds a launched session (as if /sessions had launched it) so
// the run-view tests can exercise binding and turns without the launch round
// trip. proxyBaseURL points the turn loop at a mock proxy.
func storedSession(proxyBaseURL string) *launchedSession {
	return &launchedSession{
		SessionID:    "sess-1",
		AgentID:      "agent-9",
		AgentName:    "Alpha",
		ProxyBaseURL: proxyBaseURL,
		proxyToken:   "dgp_super_secret",
		FanOut:       2,
		ExpiresAt:    "2026-07-11T13:00:00Z",
		policyBundle: json.RawMessage(`{"stub":"bundle"}`),
	}
}

// newRunServer builds a Server with a mock control plane, a fake drun engine,
// and the run-view seams stubbed for deterministic tests. It delegates to
// newRunServerRT so the control plane is served in-memory (no socket bind) and
// the suite runs inside the network-isolated drun sandbox as well as on a host.
func newRunServer(t *testing.T, cpHandler http.HandlerFunc, conn drunConn, opts ...Option) *Server {
	t.Helper()
	return newRunServerRT(t, cpHandler, conn, opts...)
}

// authGet issues an authenticated GET against the server.
func authGet(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

// authPostForm issues an authenticated form POST against the server.
func authPostForm(s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

func TestRunViewBindsEngineAndShowsTools(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy"))

	rr := authGet(s, "/sessions/sess-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "session_bash") {
		t.Errorf("run view missing tool: %q", body)
	}
	if !strings.Contains(body, `name="directive"`) {
		t.Errorf("run view missing chat input: %q", body)
	}
	if strings.Contains(body, "dgp_super_secret") {
		t.Errorf("run view leaked proxy token: %q", body)
	}
	// The htmx SSE extension must load from the local, content-hashed uikit
	// path — never a CDN. The old jsdelivr URL 404'd, silently breaking streaming
	// (issue #292).
	if !strings.Contains(body, uikit.SSEExtensionPath) {
		t.Errorf("run view should load the vendored SSE extension from %q: %q", uikit.SSEExtensionPath, body)
	}
	if strings.Contains(body, "jsdelivr") || strings.Contains(body, "cdn.") {
		t.Errorf("run view must not load the SSE extension from a CDN: %q", body)
	}
}

func TestRunViewUnknownSessionRedirectsHome(t *testing.T) {
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, &fakeDrunConn{})
	rr := authGet(s, "/sessions/does-not-exist")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Errorf("status=%d loc=%q, want 303 /", rr.Code, rr.Header().Get("Location"))
	}
}

func TestRunViewEngineUnreachable(t *testing.T) {
	conn := &fakeDrunConn{initErr: errWrap("connection refused")}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy"))

	rr := authGet(s, "/sessions/sess-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "unreachable") {
		t.Errorf("expected an engine-unreachable error, got: %q", body)
	}
	if strings.Contains(body, `name="directive"`) {
		t.Error("chat input should be withheld when the engine is unreachable")
	}
}

func TestRunViewFailsClosedOnBundleRejection(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn,
		WithBundleVerifier(func(json.RawMessage, time.Time) error { return errWrap("bundle not fresh") }))
	s.storeSession(storedSession("http://proxy"))

	rr := authGet(s, "/sessions/sess-1")
	body := rr.Body.String()
	if !strings.Contains(body, "policy bundle rejected") {
		t.Errorf("expected a policy-rejection error, got: %q", body)
	}
	if conn.closed {
		t.Error("engine should not have been bound when the bundle is rejected")
	}
	if strings.Contains(body, `name="directive"`) {
		t.Error("chat input should be withheld when the bundle is rejected")
	}
}

func TestTurnStagesDirective(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy"))

	// Bind the engine by entering the run view first.
	authGet(s, "/sessions/sess-1")

	rr := authPostForm(s, "/sessions/sess-1/turn", url.Values{"directive": {"upgrade to go 1.22"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "upgrade to go 1.22") {
		t.Errorf("turn fragment missing the directive: %q", body)
	}
	if !strings.Contains(body, `sse-connect="/sessions/sess-1/stream"`) {
		t.Errorf("turn fragment missing the SSE connection: %q", body)
	}
}

func TestStreamRunsGovernedTurn(t *testing.T) {
	// Fake governed proxy (served in-memory): first call proposes a tool, second
	// returns the final answer.
	var proxyCalls int
	proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer dgp_super_secret" {
			t.Errorf("proxy Authorization = %q, want the scoped proxy token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		proxyCalls++
		if proxyCalls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"session_bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"all done"}}]}`))
	})

	conn := &fakeDrunConn{
		tools: []drunmcp.Tool{bashTool()},
		callFn: func(name string, args any) (*drunmcp.CallToolResult, error) {
			return &drunmcp.CallToolResult{Content: []drunmcp.ContentBlock{{Type: "text", Text: "listed"}}}, nil
		},
	}

	trajCh := make(chan []trajectoryEvent, 1)
	cp := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/trajectory") {
			var body struct {
				Events []trajectoryEvent `json:"events"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(IngestAck{Accepted: len(body.Events)})
			trajCh <- body.Events
			return
		}
		w.WriteHeader(http.StatusOK)
	}

	s := newRunServer(t, cp, conn, WithProxyHTTPClient(handlerClient(proxyHandler)))
	s.storeSession(storedSession("http://proxy"))

	authGet(s, "/sessions/sess-1")                                                    // bind
	authPostForm(s, "/sessions/sess-1/turn", url.Values{"directive": {"list files"}}) // stage
	rr := authGet(s, "/sessions/sess-1/stream")                                       // run + stream

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{"event: step", "session_bash", "result", "all done", "event: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "dgp_super_secret") {
		t.Errorf("stream leaked the proxy token: %q", body)
	}
	if len(conn.calls) != 1 || conn.calls[0].name != "session_bash" {
		t.Errorf("expected one session_bash tool call, got %+v", conn.calls)
	}

	select {
	case events := <-trajCh:
		if len(events) == 0 || events[0].Type != trajTypeTurn {
			t.Errorf("trajectory batch not structure-first: %+v", events)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("trajectory was not posted")
	}
}

func TestStreamReconnectWithoutPendingCloses(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy"))
	authGet(s, "/sessions/sess-1") // bind, but no directive staged

	rr := authGet(s, "/sessions/sess-1/stream")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: done") {
		t.Errorf("a stream with nothing staged should close with done: %q", body)
	}
	if strings.Contains(body, "event: step") {
		t.Errorf("no steps should be streamed without a staged directive: %q", body)
	}
}

func TestTerminateClosesEngine(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/terminate") {
			_ = json.NewEncoder(w).Encode(Session{SessionID: "sess-1", Status: "terminated"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}, conn)
	s.storeSession(storedSession("http://proxy"))
	authGet(s, "/sessions/sess-1") // bind the engine

	rr := authPostForm(s, "/sessions/sess-1/terminate", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !conn.closed {
		t.Error("terminate should close the bound drun session")
	}
	if s.lookupSession("sess-1") != nil {
		t.Error("terminate should drop the session record")
	}
}

// errWrap is a tiny error for tests.
func errWrap(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
