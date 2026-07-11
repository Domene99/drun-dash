package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseLimit(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    int
		wantErr error
	}{
		{name: "absent defaults to 50", query: "", want: DefaultLimit},
		{name: "explicit in range", query: "limit=25", want: 25},
		{name: "min boundary", query: "limit=1", want: MinLimit},
		{name: "max boundary", query: "limit=200", want: MaxLimit},
		{name: "below min clamps up", query: "limit=0", want: MinLimit},
		{name: "negative clamps up", query: "limit=-5", want: MinLimit},
		{name: "above max clamps down", query: "limit=1000", want: MaxLimit},
		{name: "non-integer errors", query: "limit=abc", wantErr: ErrInvalidLimit},
		{name: "float errors", query: "limit=10.5", wantErr: ErrInvalidLimit},
		{name: "empty value defaults", query: "limit=", want: DefaultLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/agents?"+tt.query, nil)
			got, err := ParseLimit(r)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("limit = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		cursor Cursor
	}{
		{
			name:   "nanosecond precision",
			cursor: Cursor{CreatedAt: time.Date(2026, 7, 11, 10, 30, 0, 123456789, time.UTC), ID: "018f-abc-123"},
		},
		{
			name:   "whole second",
			cursor: Cursor{CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ID: "uuid-1"},
		},
		{
			name:   "uuid id",
			cursor: Cursor{CreatedAt: time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC), ID: "3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc := EncodeCursor(tt.cursor)
			// Cursor must be opaque base64url with no padding.
			if _, err := base64.RawURLEncoding.DecodeString(enc); err != nil {
				t.Fatalf("encoded cursor is not raw base64url: %v", err)
			}
			got, err := DecodeCursor(enc)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got == nil {
				t.Fatal("decoded cursor is nil")
			}
			if !got.CreatedAt.Equal(tt.cursor.CreatedAt) {
				t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, tt.cursor.CreatedAt)
			}
			if got.ID != tt.cursor.ID {
				t.Errorf("ID = %q, want %q", got.ID, tt.cursor.ID)
			}
		})
	}
}

func TestDecodeCursorEmptyIsStart(t *testing.T) {
	got, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("empty cursor = %+v, want nil (start)", got)
	}
}

func TestDecodeCursorMalformed(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "not base64", input: "!!!not-base64!!!"},
		{name: "base64 but no separator", input: base64.RawURLEncoding.EncodeToString([]byte("no-separator-here"))},
		{name: "empty id after separator", input: base64.RawURLEncoding.EncodeToString([]byte("2026-07-11T10:30:00Z\x00"))},
		{name: "bad timestamp", input: base64.RawURLEncoding.EncodeToString([]byte("not-a-time\x00some-id"))},
		{name: "padded base64 rejected", input: base64.URLEncoding.EncodeToString([]byte("2026-07-11T10:30:00Z\x00id-1"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeCursor(tt.input)
			if !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("err = %v, want ErrInvalidCursor", err)
			}
			if got != nil {
				t.Errorf("malformed cursor decoded to %+v, want nil", got)
			}
		})
	}
}

// TestEncodeCursorNoPadding verifies the encoded cursor never contains '='
// padding, per ADR-0002 §3.
func TestEncodeCursorNoPadding(t *testing.T) {
	// An id length that would produce padding under standard base64.
	c := Cursor{CreatedAt: time.Date(2026, 7, 11, 10, 30, 0, 0, time.UTC), ID: "a"}
	enc := EncodeCursor(c)
	for _, r := range enc {
		if r == '=' {
			t.Fatalf("encoded cursor %q contains padding", enc)
		}
	}
}
