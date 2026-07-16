package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testRouter builds the router with a logger that discards output so tests do
// not spam stdout. The health and middleware tests never reach the broker
// route, so a nil pool is sufficient: newBroker builds its stores over it but
// the grant lookup only runs on POST /v1/chat/completions. Callers that
// exercise the broker route should set LLM_UPSTREAM_BASE_URL (via t.Setenv)
// before calling this, since the upstream is read once at construction.
func testRouter() http.Handler {
	return NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	testRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if body := rec.Body.String(); body != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", body, `{"status":"ok"}`)
	}
}

// TestRequestIDMiddleware verifies the shared middleware chain is wired in by
// checking that responses carry the X-Request-ID header.
func TestRequestIDMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	testRouter().ServeHTTP(rec, req)

	if got := rec.Header().Get(httpxHeaderRequestID); got == "" {
		t.Errorf("X-Request-ID header = %q, want a generated ID", got)
	}
}

// httpxHeaderRequestID mirrors httpx.HeaderRequestID; duplicated here to keep
// the test independent of the internal constant's exported surface.
const httpxHeaderRequestID = "X-Request-ID"
