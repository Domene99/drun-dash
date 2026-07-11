package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

func decodeBody(t *testing.T, body string) httpx.ErrorBody {
	t.Helper()
	var eb httpx.ErrorBody
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("unmarshal error body %q: %v", body, err)
	}
	return eb
}

func TestWriteStoreError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{
			name:     "not found sentinel",
			err:      store.ErrNotFound,
			wantCode: 404,
			wantBody: httpx.CodeNotFound,
		},
		{
			name:     "not found wrapped",
			err:      fmt.Errorf("store: get agent: %w", store.ErrNotFound),
			wantCode: 404,
			wantBody: httpx.CodeNotFound,
		},
		{
			name:     "conflict sentinel",
			err:      store.ErrConflict,
			wantCode: 409,
			wantBody: httpx.CodeConflict,
		},
		{
			name:     "conflict wrapped with constraint",
			err:      fmt.Errorf("%w on agents_org_id_name_key", store.ErrConflict),
			wantCode: 409,
			wantBody: httpx.CodeConflict,
		},
		{
			name:     "unknown error maps to 500",
			err:      errors.New("connection reset by peer"),
			wantCode: 500,
			wantBody: httpx.CodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteStoreError(rec, tt.err)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			eb := decodeBody(t, rec.Body.String())
			if eb.Code != tt.wantBody {
				t.Errorf("code = %q, want %q", eb.Code, tt.wantBody)
			}
			if eb.Message == "" {
				t.Errorf("message is empty; want a human-readable message")
			}
		})
	}
}

// TestWriteStoreErrorNoLeak verifies the 500 path never echoes the raw error
// string to the client.
func TestWriteStoreErrorNoLeak(t *testing.T) {
	secret := "pq: relation \"secret_internal_table\" does not exist"
	rec := httptest.NewRecorder()
	WriteStoreError(rec, errors.New(secret))
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret_internal_table") {
		t.Errorf("500 body leaked raw error: %s", rec.Body.String())
	}
	eb := decodeBody(t, rec.Body.String())
	if eb.Code != httpx.CodeInternal {
		t.Errorf("code = %q, want %q", eb.Code, httpx.CodeInternal)
	}
}

func TestWriteValidationError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteValidationError(rec, "name", "name is required")
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	eb := decodeBody(t, rec.Body.String())
	if eb.Code != httpx.CodeBadRequest {
		t.Errorf("code = %q, want %q", eb.Code, httpx.CodeBadRequest)
	}
	if eb.Message != "name is required" {
		t.Errorf("message = %q, want %q", eb.Message, "name is required")
	}
	if got := eb.Details["field"]; got != "name" {
		t.Errorf("details.field = %v, want %q", got, "name")
	}
}

func TestWriteForbidden(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteForbidden(rec, "not entitled")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	eb := decodeBody(t, rec.Body.String())
	if eb.Code != httpx.CodeForbidden {
		t.Errorf("code = %q, want %q", eb.Code, httpx.CodeForbidden)
	}
}

func TestWriteNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteNotFound(rec, "resource not found")
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	eb := decodeBody(t, rec.Body.String())
	if eb.Code != httpx.CodeNotFound {
		t.Errorf("code = %q, want %q", eb.Code, httpx.CodeNotFound)
	}
}
