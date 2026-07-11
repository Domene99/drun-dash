package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/vault"
)

// envUpstreamBaseURL names the environment variable holding the base URL of the
// single upstream provider this proxy forwards to (scheme + host, optionally a
// path prefix), e.g. "https://api.openai.com". It is read directly here rather
// than through internal/config, which is frozen and has no upstream field.
//
// Operators should set it to their OpenAI-compatible endpoint; the canonical
// value is https://api.openai.com. When it is unset or invalid the broker route
// answers 502 with code upstreamUnconfiguredCode rather than forwarding
// anywhere, so a misconfigured proxy fails loudly instead of leaking calls to a
// surprise default.
const envUpstreamBaseURL = "LLM_UPSTREAM_BASE_URL"

// envUpstreamSecretRef names the environment variable holding the vault
// secret_ref for the upstream master credential the proxy injects on every
// forwarded request (see broker.forward). When it is unset the broker falls
// back to defaultUpstreamSecretRef.
//
// The ref is an opaque handle (e.g. "vault://dev/anthropic"), not the key
// itself: the raw master credential lives only in the vault and is resolved
// per request, so it never reaches — nor can be controlled by — the client.
const envUpstreamSecretRef = "LLM_UPSTREAM_SECRET_REF"

// defaultUpstreamSecretRef is the vault ref used when envUpstreamSecretRef is
// unset, mapping to the anthropic dev secret under EnvVault.
const defaultUpstreamSecretRef = "vault://dev/anthropic"

// Flat error codes emitted by the broker. httpx is frozen for this issue, so
// these live here and are passed to httpx.WriteError as plain strings rather
// than added to the shared httpx.Code… set.
const (
	// upstreamUnconfiguredCode reports that LLM_UPSTREAM_BASE_URL is missing or
	// invalid, so the proxy has no upstream to forward to.
	upstreamUnconfiguredCode = "upstream_unconfigured"
	// upstreamCredentialCode reports that the upstream master credential could
	// not be resolved from the vault, so the proxy has no key to inject. The
	// request is not forwarded and the underlying secret is never echoed.
	upstreamCredentialCode = "upstream_credential_unavailable"
	// badGatewayCode reports that the upstream could not be reached or failed
	// at the transport layer.
	badGatewayCode = "bad_gateway"
)

// broker forwards POST /v1/chat/completions to a single configured upstream and
// streams the response back unbuffered, preserving status codes, headers, and
// streaming (SSE) bodies. On every forwarded request it resolves the upstream
// master credential from the vault and injects it as the Authorization header,
// overwriting any client-supplied value — so the master key lives only in the
// proxy and is never sent, seen, or chosen by the client.
type broker struct {
	// upstream is the validated base URL, or nil when the proxy is
	// unconfigured (env unset/invalid). A nil upstream makes chatCompletions
	// answer 502 upstream_unconfigured.
	upstream *url.URL
	// vault resolves secretRef to the upstream master credential on each
	// forwarded request.
	vault vault.Vault
	// secretRef is the opaque vault handle for the upstream credential. It is
	// safe to log; the resolved Secret value is not.
	secretRef string
	client    *http.Client
	logger    *slog.Logger
}

// newBroker builds a broker from the environment. The upstream base URL is read
// and validated exactly once, here at router construction, so a bad value is
// caught at startup instead of on every request. An unset or invalid value is
// not fatal: the broker is returned with a nil upstream and its route reports
// 502 upstream_unconfigured, keeping /healthz and the rest of the server alive.
//
// The upstream credential is brokered through the EnvVault development backend:
// the secret_ref is captured here, but the credential itself is resolved per
// request in forward so it is never cached in the process and honors rotation.
func newBroker(logger *slog.Logger) *broker {
	if logger == nil {
		logger = slog.Default()
	}
	b := &broker{
		logger:    logger,
		vault:     vault.NewEnvVault(),
		secretRef: upstreamSecretRef(),
		client: &http.Client{
			// Pass upstream redirects through verbatim instead of following
			// them, so the client sees the upstream's real status codes.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	logger.Info("llm upstream credential source configured", slog.String("secret_ref", b.secretRef))

	raw := strings.TrimSpace(os.Getenv(envUpstreamBaseURL))
	if raw == "" {
		logger.Warn("llm upstream not configured; broker route will return 502",
			slog.String("env", envUpstreamBaseURL))
		return b
	}

	u, err := parseUpstream(raw)
	if err != nil {
		logger.Error("invalid llm upstream base URL; broker route will return 502",
			slog.String("env", envUpstreamBaseURL),
			slog.Any("error", err))
		return b
	}

	b.upstream = u
	logger.Info("llm upstream configured", slog.String("upstream", u.Redacted()))
	return b
}

// upstreamSecretRef returns the configured vault secret_ref for the upstream
// credential, falling back to defaultUpstreamSecretRef when the env is unset.
func upstreamSecretRef() string {
	if ref := strings.TrimSpace(os.Getenv(envUpstreamSecretRef)); ref != "" {
		return ref
	}
	return defaultUpstreamSecretRef
}

// parseUpstream parses and validates an upstream base URL, requiring an http or
// https scheme and a non-empty host so that later joins produce a dialable
// absolute URL.
func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, &url.Error{Op: "parse", URL: raw, Err: errUnsupportedScheme}
	}
	if u.Host == "" {
		return nil, &url.Error{Op: "parse", URL: raw, Err: errMissingHost}
	}
	return u, nil
}

// upstreamError is a small sentinel error type for base-URL validation.
type upstreamError string

func (e upstreamError) Error() string { return string(e) }

const (
	errUnsupportedScheme upstreamError = "upstream base URL must use http or https"
	errMissingHost       upstreamError = "upstream base URL has no host"
)

// chatCompletions handles POST /v1/chat/completions. When the proxy is
// unconfigured it reports 502 upstream_unconfigured; otherwise it forwards the
// request to the upstream and streams the response back.
func (b *broker) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if b.upstream == nil {
		_ = httpx.WriteError(w, http.StatusBadGateway, upstreamUnconfiguredCode,
			"llm upstream is not configured; set "+envUpstreamBaseURL)
		return
	}
	b.forward(w, r)
}

// forward proxies r to <upstream>/v1/chat/completions and streams the upstream
// response back to w unbuffered. It copies the method, path, query, and safe
// request headers on the way out, injects the vault-resolved master credential
// as the Authorization header (overwriting any client value), and streams the
// status, headers, and body — flushed incrementally so SSE (text/event-stream)
// chunks reach the client as they arrive — on the way back. If the credential
// cannot be resolved the request is not forwarded and 502
// upstream_credential_unavailable is returned; a dial or transport failure
// yields 502 bad_gateway.
func (b *broker) forward(w http.ResponseWriter, r *http.Request) {
	// Resolve the master credential before building the upstream request so a
	// resolution failure never forwards the client's call (or its own
	// Authorization) upstream. The Secret value is never logged; only the
	// non-sensitive secret_ref and error are.
	secret, err := b.vault.Resolve(r.Context(), b.secretRef)
	if err != nil {
		b.logger.LogAttrs(r.Context(), slog.LevelError, "resolve upstream credential failed",
			slog.String("secret_ref", b.secretRef),
			slog.Any("error", err))
		_ = httpx.WriteError(w, http.StatusBadGateway, upstreamCredentialCode,
			"upstream credential is unavailable")
		return
	}

	target := *b.upstream
	target.Path = singleJoiningSlash(b.upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		b.logger.LogAttrs(r.Context(), slog.LevelError, "build upstream request failed",
			slog.Any("error", err))
		_ = httpx.WriteError(w, http.StatusBadGateway, badGatewayCode, "failed to build upstream request")
		return
	}
	copyProxyHeaders(outReq.Header, r.Header)
	// Inject the master credential, overwriting any client-supplied
	// Authorization. Set replaces every prior value for the key, so the
	// client's own header is both dropped (never reaches the upstream) and
	// unable to influence the key the proxy presents.
	outReq.Header.Set("Authorization", "Bearer "+secret.Value)
	// Preserve the body length (or -1 for an unknown/streamed length) so the
	// upstream sees the same framing the client sent.
	outReq.ContentLength = r.ContentLength

	resp, err := b.client.Do(outReq)
	if err != nil {
		b.logger.LogAttrs(r.Context(), slog.LevelError, "upstream request failed",
			slog.String("upstream", b.upstream.Redacted()),
			slog.Any("error", err))
		_ = httpx.WriteError(w, http.StatusBadGateway, badGatewayCode, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyProxyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	b.streamBody(w, resp.Body)
}

// streamBody copies the upstream body to w, flushing after every read so that
// streamed responses (SSE) are delivered chunk by chunk instead of buffered to
// completion. Flushing goes through http.ResponseController, which unwraps the
// middleware's response recorder to reach the underlying http.Flusher.
func (b *broker) streamBody(w http.ResponseWriter, body io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			_ = rc.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// hopHeaders are the hop-by-hop headers (RFC 7230 §6.1) that apply to a single
// transport connection and must not be forwarded end to end.
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// copyProxyHeaders copies src into dst, dropping hop-by-hop headers — both the
// well-known ones and any additional field names listed in a Connection header.
// End-to-end headers (Content-Type and the rest) pass through verbatim.
//
// On the request path the caller overwrites Authorization after this copy with
// the vault-resolved master credential, so any client-supplied Authorization is
// discarded — the client can neither read nor choose the upstream key.
func copyProxyHeaders(dst, src http.Header) {
	drop := make(map[string]bool, len(hopHeaders))
	for _, h := range hopHeaders {
		drop[http.CanonicalHeaderKey(h)] = true
	}
	for _, field := range src["Connection"] {
		for _, name := range strings.Split(field, ",") {
			if name = strings.TrimSpace(name); name != "" {
				drop[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	for key, values := range src {
		if drop[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// singleJoiningSlash joins a base path and request path with exactly one slash
// between them, mirroring net/http/httputil's helper so an upstream base URL
// with or without a trailing path prefix joins cleanly.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
