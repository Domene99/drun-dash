package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	var got string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got == "" {
		t.Fatal("request ID not stored in context")
	}
	if hdr := rec.Header().Get(HeaderRequestID); hdr != got {
		t.Errorf("response header %q = %q, want %q", HeaderRequestID, hdr, got)
	}
	if len(got) != 32 { // 16 random bytes hex-encoded
		t.Errorf("generated ID %q has length %d, want 32", got, len(got))
	}
}

func TestRequestID_ReusesValidInbound(t *testing.T) {
	const supplied = "client-abc.123"
	var got string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, supplied)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got != supplied {
		t.Errorf("context ID = %q, want %q", got, supplied)
	}
	if hdr := rec.Header().Get(HeaderRequestID); hdr != supplied {
		t.Errorf("response header = %q, want %q", hdr, supplied)
	}
}

func TestRequestID_RejectsUnsafeInbound(t *testing.T) {
	// A value containing a CRLF (log/header injection) must be discarded and
	// replaced with a freshly generated ID.
	var got string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "bad\r\nInjected: 1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if strings.ContainsAny(got, "\r\n ") {
		t.Errorf("unsafe inbound ID was not rejected: %q", got)
	}
	if len(got) != 32 {
		t.Errorf("expected a freshly generated ID, got %q", got)
	}
}

func TestRecoverer_Returns500JSONWithoutLeaking(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	const secret = "super-secret-panic-detail"
	h := Recoverer(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(secret)
	}))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if body.Code != CodeInternal {
		t.Errorf("code = %q, want %q", body.Code, CodeInternal)
	}
	// Production mode: the panic detail must not reach the client.
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("panic detail leaked into response body: %s", rec.Body.String())
	}
	// But it must be logged for operators.
	if !strings.Contains(logBuf.String(), secret) {
		t.Errorf("panic detail was not logged: %s", logBuf.String())
	}
}

func TestRecoverer_DebugIncludesPanic(t *testing.T) {
	const secret = "debug-visible-panic"
	mw := RecovererWithOptions(RecovererOptions{
		Logger: slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)),
		Debug:  true,
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(secret)
	}))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), secret) {
		t.Errorf("debug mode should include the panic value: %s", rec.Body.String())
	}
}

func TestRecoverer_NoPanicPassesThrough(t *testing.T) {
	h := Recoverer(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestLogger_EmitsStructuredLine(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	// Chain RequestID -> Logger so the log line carries the request ID.
	h := RequestID(Logger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	})))

	req := httptest.NewRequest(http.MethodGet, "/tea", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logBuf.Bytes()), &line); err != nil {
		t.Fatalf("log line not valid JSON: %v (%q)", err, logBuf.String())
	}
	if line["method"] != http.MethodGet {
		t.Errorf("method = %v, want GET", line["method"])
	}
	if line["path"] != "/tea" {
		t.Errorf("path = %v, want /tea", line["path"])
	}
	if status, _ := line["status"].(float64); int(status) != http.StatusTeapot {
		t.Errorf("status = %v, want %d", line["status"], http.StatusTeapot)
	}
	if b, _ := line["bytes"].(float64); int(b) != len("short and stout") {
		t.Errorf("bytes = %v, want %d", line["bytes"], len("short and stout"))
	}
	if id, _ := line["request_id"].(string); id == "" {
		t.Error("request_id missing from log line")
	}
}

func TestStatusRecorder_DefaultStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	if _, err := sr.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if sr.status != http.StatusOK {
		t.Errorf("status = %d, want 200 when WriteHeader omitted", sr.status)
	}
	if sr.written != 2 {
		t.Errorf("written = %d, want 2", sr.written)
	}
}
