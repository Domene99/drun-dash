// Package proxy provides the HTTP routing layer for the drun-dash llm-proxy,
// the OpenAI-compatible LLM call broker. Every model call routes through the
// proxy so the control plane can hold master credentials, meter spend, and
// enforce entitlement at the point of each call.
//
// This is a v1 scaffold: [NewRouter] exposes a health endpoint and a
// placeholder broker route that reports 501. The credential-brokering and
// provider-routing logic arrives in a later batch.
package proxy

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/httpx"
)

// NewRouter builds the HTTP router for the llm-proxy server, wrapping every
// route in the shared middleware chain: request IDs, panic recovery, and
// structured access logging.
func NewRouter(logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer(logger), httpx.Logger(logger))

	r.Get("/healthz", healthz)
	r.Post("/v1/chat/completions", chatCompletions)
	return r
}

// healthz reports server liveness.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// chatCompletions is the placeholder broker route. Credential brokering and
// provider routing are not implemented yet, so it reports 501.
//
// TODO(batch N): broker the call to an upstream provider using master
// credentials held by the control plane, metering spend and enforcing
// entitlement.
func chatCompletions(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteError(w, http.StatusNotImplemented, httpx.CodeNotImplemented, "llm brokering is not implemented yet")
}
