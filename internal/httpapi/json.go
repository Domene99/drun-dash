package httpapi

import (
	"net/http"

	"github.com/Domene99/drun-dash/internal/httpx"
)

// WriteOK writes v as a 200 OK JSON response. It is a thin convenience over
// httpx.WriteJSON so handlers reading the success path stay uniform; the
// encoding-failure fallback (a clean 500 envelope) is handled by httpx.
func WriteOK(w http.ResponseWriter, v any) {
	_ = httpx.WriteJSON(w, http.StatusOK, v)
}

// WriteCreated writes v as a 201 Created JSON response, for create endpoints.
func WriteCreated(w http.ResponseWriter, v any) {
	_ = httpx.WriteJSON(w, http.StatusCreated, v)
}

// WriteAccepted writes v as a 202 Accepted JSON response, for endpoints that
// acknowledge a batch taken in for processing rather than a resource created in
// place — trajectory ingest returns its accepted/rejected tally this way.
func WriteAccepted(w http.ResponseWriter, v any) {
	_ = httpx.WriteJSON(w, http.StatusAccepted, v)
}
