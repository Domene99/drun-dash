// Package proxy provides the HTTP layer for the drun-dash llm-proxy, the
// OpenAI-compatible LLM call broker. Every model call routes through the proxy
// so the control plane can later hold master credentials, meter spend, and
// enforce entitlement at the point of each call.
//
// This batch implements the transport spine: [NewRouter] exposes a health
// endpoint and forwards POST /v1/chat/completions to a single configured
// upstream (see [broker]), streaming the response back faithfully — including
// SSE. Credential brokering, metering, entitlement, and multi-endpoint routing
// layer on in later batches.
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
//
// The upstream base URL is read and validated once here (see newBroker); an
// unset or invalid LLM_UPSTREAM_BASE_URL is not fatal — the broker route then
// answers 502 upstream_unconfigured while /healthz stays up.
func NewRouter(logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer(logger), httpx.Logger(logger))

	b := newBroker(logger)

	r.Get("/healthz", healthz)
	r.Post("/v1/chat/completions", b.chatCompletions)
	return r
}

// healthz reports server liveness.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
