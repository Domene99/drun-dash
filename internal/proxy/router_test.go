package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testRouter builds the router with a logger that discards output so tests do
// not spam stdout.
func testRouter() http.Handler {
	return NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestChatCompletionsNotImplemented(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	testRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != "not_implemented" {
		t.Errorf("code = %q, want %q", body.Code, "not_implemented")
	}
	if body.Message == "" {
		t.Error("message is empty, want a human-readable explanation")
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
