package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteOK(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteOK(rec, map[string]string{"status": "ok"})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("body = %v", got)
	}
}

func TestWriteCreated(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteCreated(rec, map[string]int{"id": 1})
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
}
