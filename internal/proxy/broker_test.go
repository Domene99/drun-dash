package proxy

import (
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
)

// roundTripFunc adapts a function to an http.RoundTripper so tests can stand in
// for the upstream without binding a socket, exercising the broker's forward
// path (request construction, header copying, streaming) end to end in-process.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestBroker builds a broker wired to the given RoundTripper as its upstream,
// preserving the same client redirect policy newBroker uses.
func newTestBroker(t *testing.T, upstream string, rt http.RoundTripper) *broker {
	t.Helper()
	u, err := parseUpstream(upstream)
	if err != nil {
		t.Fatalf("parseUpstream(%q): %v", upstream, err)
	}
	return &broker{
		upstream: u,
		client: &http.Client{
			Transport: rt,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: testLogger(),
	}
}

// TestBrokerPassesJSONThrough asserts a non-streamed JSON response is forwarded
// faithfully: method, path, query, and safe request headers/body reach the
// upstream, hop-by-hop headers are dropped, and the upstream status, headers
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

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=false&model=gpt", strings.NewReader(`{"model":"gpt"}`))
	req.Header.Set("Authorization", "Bearer sk-client")
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
	if got.Header.Get("Authorization") != "Bearer sk-client" {
		t.Errorf("upstream Authorization = %q, want the client's header forwarded", got.Header.Get("Authorization"))
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

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
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

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
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
	b := newBroker(testLogger())
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
	b := newBroker(testLogger())
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
	b := newBroker(testLogger())
	if b.upstream == nil {
		t.Fatal("expected a configured upstream")
	}
	if b.upstream.Host != "api.openai.com" {
		t.Errorf("upstream host = %q, want api.openai.com", b.upstream.Host)
	}
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
