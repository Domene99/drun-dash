// Package proxy provides the HTTP layer for the drun-dash llm-proxy, the
// OpenAI-compatible LLM call broker. Every model call routes through the proxy
// so the control plane holds master credentials, meters spend, and enforces the
// per-session credential grant at the point of each call.
//
// [NewRouter] exposes a health endpoint and forwards POST /v1/chat/completions
// to a single configured upstream (see [broker]), streaming the response back
// faithfully — including SSE. Every forwarded call is gated by a per-session
// credential grant and metered (ADR-0003 §3/§5, see [broker]); multi-endpoint
// routing layers on in a later batch.
package proxy

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// NewRouter builds the HTTP router for the llm-proxy server, wrapping every
// route in the shared middleware chain: request IDs, panic recovery, and
// structured access logging. pool is the shared database handle the broker's
// per-call grant validation and metering resolve through (any store.Querier,
// so a *db.Pool satisfies it).
//
// The upstream base URL is read and validated once here (see newBroker); an
// unset or invalid LLM_UPSTREAM_BASE_URL is not fatal — the broker route then
// answers 502 upstream_unconfigured while /healthz stays up.
func NewRouter(logger *slog.Logger, pool store.Querier) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer(logger), httpx.Logger(logger))

	b := newBroker(logger, pool)

	r.Get("/healthz", healthz)
	r.Post("/v1/chat/completions", b.chatCompletions)
	return r
}

// healthz reports server liveness.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
