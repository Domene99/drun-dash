package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/health"
)

// testRouter builds the router with a logger that discards output so tests do
// not spam stdout, plus any readiness checkers the test wants wired into
// /readyz. Auth is not wired here; the auth vertical is covered end-to-end in
// internal/httpapi.
func testRouter(checkers ...health.Named) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newRouter(logger, nil, checkers...)
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

// TestReadyzReady wires a fake passing checker and expects a 200 ready body.
func TestReadyzReady(t *testing.T) {
	checker := health.CheckerFunc(func(context.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	testRouter(health.Named{Name: "database", Checker: checker}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"ready"}` {
		t.Errorf("body = %q, want %q", body, `{"status":"ready"}`)
	}
}

// TestReadyzNotReady wires a failing checker and expects a 503 whose flat error
// body names the failed dependency.
func TestReadyzNotReady(t *testing.T) {
	checker := health.CheckerFunc(func(context.Context) error { return context.DeadlineExceeded })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	testRouter(health.Named{Name: "database", Checker: checker}).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != health.CodeNotReady {
		t.Errorf("code = %q, want %q", body.Code, health.CodeNotReady)
	}
	if !strings.Contains(body.Message, "database") {
		t.Errorf("message = %q, want it to name the failed dependency", body.Message)
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
