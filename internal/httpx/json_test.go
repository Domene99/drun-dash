package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := WriteJSON(rec, http.StatusCreated, map[string]string{"hello": "world"}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got["hello"] != "world" {
		t.Errorf("body = %v, want hello=world", got)
	}
}

func TestWriteJSON_MarshalFailureWritesEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	// A channel cannot be marshalled, forcing the encode-failure path.
	err := WriteJSON(rec, http.StatusOK, make(chan int))
	if err == nil {
		t.Fatal("expected a marshal error")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("fallback body is not valid JSON: %v", err)
	}
	if body.Code != CodeInternal {
		t.Errorf("code = %q, want %q", body.Code, CodeInternal)
	}
}

func TestWriteError_FlatShape(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := WriteError(rec, http.StatusNotFound, CodeNotFound, "session does not exist"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	// The envelope must be flat (matching openapi Error), not wrapped in
	// an outer "error" object.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, wrapped := raw["error"]; wrapped {
		t.Errorf("body must not be wrapped in an \"error\" object: %s", rec.Body.String())
	}
	if _, ok := raw["code"]; !ok {
		t.Errorf("body missing top-level \"code\": %s", rec.Body.String())
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if body.Code != CodeNotFound || body.Message != "session does not exist" {
		t.Errorf("body = %+v", body)
	}
	if body.Details != nil {
		t.Errorf("details should be omitted, got %v", body.Details)
	}
}

func TestWriteErrorDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	details := map[string]any{"field": "name"}
	if err := WriteErrorDetails(rec, http.StatusBadRequest, CodeBadRequest, "invalid", details); err != nil {
		t.Fatalf("WriteErrorDetails: %v", err)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Details["field"] != "name" {
		t.Errorf("details = %v, want field=name", body.Details)
	}
}

type payload struct {
	Name string `json:"name"`
}

func TestDecodeJSON_Valid(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ada"}`))
	rec := httptest.NewRecorder()
	var p payload
	if err := DecodeJSON(rec, req, &p); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if p.Name != "ada" {
		t.Errorf("name = %q, want ada", p.Name)
	}
}

func TestDecodeJSON_UnknownField(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ada","extra":1}`))
	rec := httptest.NewRecorder()
	var p payload
	err := DecodeJSON(rec, req, &p)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error = %v, want unknown field", err)
	}
}

func TestDecodeJSON_TrailingData(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ada"}{"name":"bob"}`))
	rec := httptest.NewRecorder()
	var p payload
	err := DecodeJSON(rec, req, &p)
	if !errors.Is(err, ErrTrailingData) {
		t.Errorf("error = %v, want ErrTrailingData", err)
	}
}

func TestDecodeJSON_EmptyBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	rec := httptest.NewRecorder()
	var p payload
	err := DecodeJSON(rec, req, &p)
	if !errors.Is(err, ErrEmptyBody) {
		t.Errorf("error = %v, want ErrEmptyBody", err)
	}
}

func TestDecodeJSONLimit_TooLarge(t *testing.T) {
	big := `{"name":"` + strings.Repeat("x", 100) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
	rec := httptest.NewRecorder()
	var p payload
	err := DecodeJSONLimit(rec, req, &p, 16)
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want body too large", err)
	}
}
