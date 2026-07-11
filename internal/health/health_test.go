package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/httpx"
)

// serve runs Handler(checkers) against GET /readyz and returns the recorder.
func serve(t *testing.T, checkers ...Named) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	Handler(checkers...).ServeHTTP(rec, req)
	return rec
}

func passing() Checker {
	return CheckerFunc(func(context.Context) error { return nil })
}

func failing(err error) Checker {
	return CheckerFunc(func(context.Context) error { return err })
}

func TestHandlerReadyWhenAllPass(t *testing.T) {
	rec := serve(t,
		Named{Name: "database", Checker: passing()},
		Named{Name: "cache", Checker: passing()},
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"ready"}` {
		t.Errorf("body = %q, want %q", body, `{"status":"ready"}`)
	}
}

func TestHandlerReadyWithNoCheckers(t *testing.T) {
	rec := serve(t)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"ready"}` {
		t.Errorf("body = %q, want %q", body, `{"status":"ready"}`)
	}
}

func TestHandlerNotReadyNamesFailedDependency(t *testing.T) {
	rec := serve(t,
		Named{Name: "database", Checker: failing(context.DeadlineExceeded)},
	)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != CodeNotReady {
		t.Errorf("code = %q, want %q", body.Code, CodeNotReady)
	}
	if !strings.Contains(body.Message, "database") {
		t.Errorf("message = %q, want it to name the failed dependency", body.Message)
	}
}

// TestHandlerStopsAtFirstFailure verifies that once a checker fails, later
// checkers are not consulted.
func TestHandlerStopsAtFirstFailure(t *testing.T) {
	ran := false
	rec := serve(t,
		Named{Name: "database", Checker: failing(context.DeadlineExceeded)},
		Named{Name: "cache", Checker: CheckerFunc(func(context.Context) error {
			ran = true
			return nil
		})},
	)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if ran {
		t.Error("second checker ran after the first failed; want short-circuit")
	}
}

// TestHandlerEnforcesPerCheckTimeout verifies that a checker which blocks past
// the deadline observes context cancellation rather than hanging the endpoint.
func TestHandlerEnforcesPerCheckTimeout(t *testing.T) {
	rec := serve(t,
		Named{Name: "slow", Checker: CheckerFunc(func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Minute):
				return nil
			}
		})},
	)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !strings.Contains(body.Message, "slow") {
		t.Errorf("message = %q, want it to name the slow dependency", body.Message)
	}
}
