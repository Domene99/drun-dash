// Package httpx provides shared HTTP building blocks for drun-dash binaries
// (control-plane, llm-proxy): request-scoped IDs, structured request logging,
// panic recovery, and JSON request/response helpers. Centralizing them keeps
// handlers thin and their behavior consistent across services.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Machine-readable error codes. These mirror the stable codes used by the
// OpenAPI contract (openapi/v1.yaml #/components/schemas/Error) so error
// bodies emitted here match the documented API surface.
const (
	CodeBadRequest     = "bad_request"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeInternal       = "internal_error"
	CodeNotImplemented = "not_implemented"
)

// DefaultMaxBodyBytes is the maximum request body size accepted by DecodeJSON
// (1 MiB). Callers needing a different limit can use DecodeJSONLimit.
const DefaultMaxBodyBytes int64 = 1 << 20

// contentTypeJSON is the Content-Type set on every JSON response body.
const contentTypeJSON = "application/json; charset=utf-8"

// ErrorBody is the JSON error envelope. It matches the OpenAPI Error schema
// (openapi/v1.yaml #/components/schemas/Error): a flat object with a stable
// machine-readable code, a human-readable message, and optional structured
// details. It is deliberately NOT wrapped in an outer {"error": ...} object,
// to stay conformant with the merged contract that batch 3 handlers serve.
type ErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WriteJSON marshals v and writes it as the response body with the given
// status code and a JSON Content-Type.
//
// v is marshalled into a buffer before any header is written, so an encoding
// failure yields a clean 500 error envelope instead of a half-written body
// with a success status. The returned error (if any) is the original marshal
// or write error, for logging by the caller.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", contentTypeJSON)
		w.WriteHeader(http.StatusInternalServerError)
		// A hand-built envelope avoids recursing through json.Marshal, which
		// just failed, while still returning valid JSON in the flat shape.
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"failed to encode response"}`))
		return err
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_, err = w.Write(buf)
	return err
}

// WriteError writes a flat JSON error envelope with the given HTTP status,
// machine-readable code, and human-readable message.
func WriteError(w http.ResponseWriter, status int, code, message string) error {
	return WriteJSON(w, status, ErrorBody{Code: code, Message: message})
}

// WriteErrorDetails is like WriteError but attaches a structured details
// object to the envelope.
func WriteErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) error {
	return WriteJSON(w, status, ErrorBody{Code: code, Message: message, Details: details})
}

// ErrEmptyBody indicates the request body was empty when a JSON value was
// expected. Callers can match it with errors.Is to distinguish a missing body
// from malformed JSON.
var ErrEmptyBody = errors.New("httpx: request body is empty")

// ErrTrailingData indicates the request body contained more than one JSON
// value. Callers can match it with errors.Is.
var ErrTrailingData = errors.New("httpx: request body must contain a single JSON value")

// DecodeJSON decodes exactly one JSON value from the request body into dst,
// using DefaultMaxBodyBytes as the size limit.
//
// It enforces a maximum body size, rejects unknown fields, and rejects
// trailing data after the first JSON value. Its error is suitable for a 400
// response; map it with WriteError and CodeBadRequest.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return DecodeJSONLimit(w, r, dst, DefaultMaxBodyBytes)
}

// DecodeJSONLimit is like DecodeJSON but caps the body at maxBytes.
func DecodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if r.Body == nil {
		return ErrEmptyBody
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return ErrEmptyBody
		}
		return err
	}
	if dec.More() {
		return ErrTrailingData
	}
	return nil
}
