package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
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
//
// Per-call routing selects the upstream *credential* from the grant's endpoint
// (see broker.forward and ADR-0003 §3), but the upstream *destination* remains a
// single configured host: multi-endpoint routing is a later batch.
const envUpstreamBaseURL = "LLM_UPSTREAM_BASE_URL"

// Flat error codes emitted by the broker for proxy-transport failures. httpx is
// frozen for this issue, so these live here and are passed to httpx.WriteError
// as plain strings rather than added to the shared httpx.Code… set. The
// grant-validation path (401/403) reuses the shared httpx codes, since those
// map onto the OpenAPI Error contract.
const (
	// upstreamUnconfiguredCode reports that LLM_UPSTREAM_BASE_URL is missing or
	// invalid, so the proxy has no upstream to forward to.
	upstreamUnconfiguredCode = "upstream_unconfigured"
	// upstreamCredentialCode reports that the upstream master credential could
	// not be resolved from the vault (or the grant names no endpoint), so the
	// proxy has no key to inject. The request is not forwarded and the
	// underlying secret is never echoed.
	upstreamCredentialCode = "upstream_credential_unavailable"
	// badGatewayCode reports that the upstream could not be reached or failed
	// at the transport layer.
	badGatewayCode = "bad_gateway"
)

// meterCaptureLimit bounds how many bytes of the upstream response body the
// broker retains to extract the usage/model for metering (ADR-0003 §5). Chat
// completions responses are small; capping the capture keeps a runaway body
// from consuming unbounded memory. A response larger than this is still
// streamed to the client in full — only the metering read is truncated.
const meterCaptureLimit = 1 << 20 // 1 MiB

// grantReader is the broker's per-call token lookup (ADR-0003 §3 step 2).
type grantReader interface {
	GetByTokenHash(ctx context.Context, hash string) (store.CredentialGrant, error)
}

// sessionReader loads the session owning a grant, for the fail-closed status
// check and for usage attribution (ADR-0003 §3 step 3, §5).
type sessionReader interface {
	GetByID(ctx context.Context, id string) (store.Session, error)
}

// endpointReader resolves the grant's endpoint to its vault secret_ref (ADR-0003
// §3 step 5).
type endpointReader interface {
	GetByID(ctx context.Context, id string) (store.ModelEndpoint, error)
}

// usageWriter appends the append-only metering record after a forwarded call
// (ADR-0003 §5).
type usageWriter interface {
	Append(ctx context.Context, p store.AppendUsageEventParams) (store.UsageEvent, error)
}

// broker forwards POST /v1/chat/completions to a single configured upstream and
// streams the response back unbuffered, preserving status codes, headers, and
// streaming (SSE) bodies. Every call is gated by a valid credential grant
// (ADR-0003 §3): the presented bearer token is hashed and looked up, the grant
// is checked fail-closed (revocation, expiry, session status) and against its
// scopes, and only then is the upstream master credential — resolved from the
// grant's endpoint — injected as the Authorization header, overwriting any
// client-supplied value. So the master key lives only in the proxy and is never
// sent, seen, or chosen by the client. Each forwarded call is metered (§5).
type broker struct {
	// upstream is the validated base URL, or nil when the proxy is
	// unconfigured (env unset/invalid). A nil upstream makes chatCompletions
	// answer 502 upstream_unconfigured.
	upstream *url.URL
	// vault resolves the grant endpoint's secret_ref to the upstream master
	// credential on each forwarded request.
	vault vault.Vault
	// grants, sessions, endpoints, and usage are the store seams the per-call
	// validation and metering resolve through. They are interfaces so tests can
	// inject fakes without a database.
	grants    grantReader
	sessions  sessionReader
	endpoints endpointReader
	usage     usageWriter
	client    *http.Client
	logger    *slog.Logger
}

// storeQuerier is the subset of the database handle the broker's stores need.
// A *db.Pool satisfies it, and so does any test double, so newBroker does not
// depend on internal/db directly.
type storeQuerier = store.Querier

// newBroker builds a broker from the environment and the shared database pool.
// The upstream base URL is read and validated exactly once, here at router
// construction, so a bad value is caught at startup instead of on every
// request. An unset or invalid value is not fatal: the broker is returned with
// a nil upstream and its route reports 502 upstream_unconfigured, keeping
// /healthz and the rest of the server alive.
//
// The stores are built over pool so the per-call path (grant lookup, session
// load, endpoint resolve, usage append) runs against the real database. The
// upstream credential is brokered through the EnvVault development backend: the
// secret_ref comes from the grant's endpoint and is resolved per request in
// forward, so the credential is never cached in the process and honors
// rotation.
func newBroker(logger *slog.Logger, pool storeQuerier) *broker {
	if logger == nil {
		logger = slog.Default()
	}
	b := &broker{
		logger:    logger,
		vault:     vault.NewEnvVault(),
		grants:    store.NewCredentialGrantStore(pool),
		sessions:  store.NewSessionStore(pool),
		endpoints: store.NewModelEndpointStore(pool),
		usage:     store.NewUsageEventStore(pool),
		client: &http.Client{
			// Pass upstream redirects through verbatim instead of following
			// them, so the client sees the upstream's real status codes.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

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
// unconfigured it reports 502 upstream_unconfigured; otherwise it authorizes the
// request against its credential grant (ADR-0003 §3) and, only on success,
// forwards to the upstream and streams the response back while metering it.
func (b *broker) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if b.upstream == nil {
		_ = httpx.WriteError(w, http.StatusBadGateway, upstreamUnconfiguredCode,
			"llm upstream is not configured; set "+envUpstreamBaseURL)
		return
	}
	grant, session, ok := b.authorize(w, r)
	if !ok {
		return
	}
	b.forward(w, r, grant, session)
}

// operationChatCompletions is the operation this route requires the grant's
// scopes to authorize (ADR-0003 §3 step 4).
const operationChatCompletions = "chat.completions"

// authorize runs the per-call grant validation of ADR-0003 §3 steps 1–4 and
// returns the resolved grant and its session on success. On any failure it
// writes the response envelope and returns ok=false; the checks fail closed, so
// a missing/unknown/expired/revoked grant or a terminated session yields 401 and
// a scope violation yields 403, never a forwarded call.
func (b *broker) authorize(w http.ResponseWriter, r *http.Request) (store.CredentialGrant, store.Session, bool) {
	// Step 1: the presented bearer token. A missing or malformed header is 401.
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		b.writeUnauthorized(w)
		return store.CredentialGrant{}, store.Session{}, false
	}

	// Step 2: hash and look up the grant. An unknown token is 401; the hash is
	// the lowercase-hex SHA-256 of the full presented token, matching the
	// launch handler's stored form.
	grant, err := b.grants.GetByTokenHash(r.Context(), sha256Hex(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			b.writeUnauthorized(w)
			return store.CredentialGrant{}, store.Session{}, false
		}
		b.logger.LogAttrs(r.Context(), slog.LevelError, "grant lookup failed", slog.Any("error", err))
		b.writeInternal(w)
		return store.CredentialGrant{}, store.Session{}, false
	}

	// Step 3: load the session and reject fail-closed on a revoked/expired grant
	// or a terminated/failed session. A missing session (data-integrity fault)
	// fails closed as 401 rather than forwarding.
	session, err := b.sessions.GetByID(r.Context(), grant.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			b.logger.LogAttrs(r.Context(), slog.LevelWarn, "grant references a missing session; denying",
				slog.String("session_id", grant.SessionID))
			b.writeUnauthorized(w)
			return store.CredentialGrant{}, store.Session{}, false
		}
		b.logger.LogAttrs(r.Context(), slog.LevelError, "session lookup failed", slog.Any("error", err))
		b.writeInternal(w)
		return store.CredentialGrant{}, store.Session{}, false
	}

	now := time.Now().UTC()
	if grant.RevokedAt != nil || !grant.ExpiresAt.After(now) ||
		session.Status == store.SessionStatusTerminated || session.Status == store.SessionStatusFailed {
		b.writeUnauthorized(w)
		return store.CredentialGrant{}, store.Session{}, false
	}

	// Step 4: enforce the grant's scopes. The route requires the
	// chat.completions operation; a grant that does not authorize it is 403.
	if !grantAllows(grant.Scopes, operationChatCompletions) {
		_ = httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden,
			"grant does not authorize this operation")
		return store.CredentialGrant{}, store.Session{}, false
	}

	return grant, session, true
}

// forward proxies r to <upstream>/v1/chat/completions and streams the upstream
// response back to w unbuffered (ADR-0003 §3 steps 5–6). It resolves the master
// credential from the grant's endpoint, injects it as the Authorization header
// (overwriting any client value), copies the method, path, query, and safe
// request headers, and streams the status, headers, and body — flushed
// incrementally so SSE (text/event-stream) chunks reach the client as they
// arrive. On the way back it meters the call (§5). If the credential cannot be
// resolved the request is not forwarded and 502 upstream_credential_unavailable
// is returned; a dial or transport failure yields 502 bad_gateway.
func (b *broker) forward(w http.ResponseWriter, r *http.Request, grant store.CredentialGrant, session store.Session) {
	// Step 5: resolve the upstream credential from the grant's endpoint before
	// building the upstream request, so a resolution failure never forwards the
	// client's call (or its own Authorization) upstream. A grant with no
	// endpoint cannot yield a credential and is treated as unavailable. The
	// Secret value is never logged; only the non-sensitive secret_ref/error are.
	if grant.EndpointID == nil {
		b.logger.LogAttrs(r.Context(), slog.LevelError, "grant has no endpoint; cannot resolve upstream credential",
			slog.String("session_id", session.ID))
		_ = httpx.WriteError(w, http.StatusBadGateway, upstreamCredentialCode, "upstream credential is unavailable")
		return
	}
	endpoint, err := b.endpoints.GetByID(r.Context(), *grant.EndpointID)
	if err != nil {
		b.logger.LogAttrs(r.Context(), slog.LevelError, "resolve grant endpoint failed",
			slog.String("endpoint_id", *grant.EndpointID),
			slog.Any("error", err))
		_ = httpx.WriteError(w, http.StatusBadGateway, upstreamCredentialCode, "upstream credential is unavailable")
		return
	}

	secret, err := b.vault.Resolve(r.Context(), endpoint.SecretRef)
	if err != nil {
		b.logger.LogAttrs(r.Context(), slog.LevelError, "resolve upstream credential failed",
			slog.String("secret_ref", endpoint.SecretRef),
			slog.Any("error", err))
		_ = httpx.WriteError(w, http.StatusBadGateway, upstreamCredentialCode, "upstream credential is unavailable")
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
	// Step 6: stream the passthrough, capturing a bounded copy for metering.
	captured := b.streamBody(w, resp.Body)

	// Step 7: meter the forwarded call (§5).
	b.meter(r, grant, session, resp, captured)
}

// streamBody copies the upstream body to w, flushing after every read so that
// streamed responses (SSE) are delivered chunk by chunk instead of buffered to
// completion, and returns a bounded copy of the bytes written for metering.
// Flushing goes through http.ResponseController, which unwraps the middleware's
// response recorder to reach the underlying http.Flusher. The capture is capped
// at meterCaptureLimit so a large body cannot grow memory without bound; the
// client still receives every byte.
func (b *broker) streamBody(w http.ResponseWriter, body io.Reader) []byte {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	var captured []byte
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// The client is gone; stop streaming but keep what we captured
				// so a call that reached the upstream is still metered.
				return captured
			}
			_ = rc.Flush()
			if len(captured) < meterCaptureLimit {
				if room := meterCaptureLimit - len(captured); n <= room {
					captured = append(captured, buf[:n]...)
				} else {
					captured = append(captured, buf[:room]...)
				}
			}
		}
		if readErr != nil {
			return captured
		}
	}
}

// meter appends an append-only usage_events row attributing the forwarded call
// to the grant's session (ADR-0003 §5). Attribution flows from the grant →
// session (org_id, agent_id) and endpoint; token counts and the model come from
// the upstream response body. It is best-effort and never affects the response
// already streamed to the client: only successful (2xx) upstream responses that
// actually report a usage object are recorded, and a write failure is logged,
// not surfaced. The write uses a cancellation-detached context so a client that
// disconnected after receiving its bytes does not drop the metering record.
func (b *broker) meter(r *http.Request, grant store.CredentialGrant, session store.Session, resp *http.Response, body []byte) {
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return
	}
	model, promptTokens, completionTokens, ok := extractUsage(resp.Header.Get("Content-Type"), body)
	if !ok {
		// No usage reported (e.g. a stream without include_usage): nothing
		// faithful to record.
		return
	}

	params := store.AppendUsageEventParams{
		OrgID:            session.OrgID,
		SessionID:        &session.ID,
		AgentID:          &session.AgentID,
		EndpointID:       grant.EndpointID,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
	}
	if model != "" {
		params.Model = &model
	}

	ctx := context.WithoutCancel(r.Context())
	if _, err := b.usage.Append(ctx, params); err != nil {
		b.logger.LogAttrs(ctx, slog.LevelError, "append usage event failed",
			slog.String("session_id", session.ID),
			slog.Any("error", err))
	}
}

// bearerToken extracts the token from an Authorization header value of the form
// "Bearer <token>". The scheme match is case-insensitive per RFC 7235; a
// missing header, a non-Bearer scheme, or an empty token returns ok=false.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// grantScopes mirrors the JSON structure the launch handler stores in
// credential_grants.scopes: the pinned endpoint and the operations the grant
// authorizes at the proxy (ADR-0003 §3). It is derived from policy, never from
// the client, so a stolen token cannot widen its own scope.
type grantScopes struct {
	EndpointID string   `json:"endpoint_id"`
	Operations []string `json:"operations"`
}

// grantAllows reports whether the grant's scopes authorize the given operation.
// Scopes that cannot be parsed, or that list no operations, deny — the scope
// check fails closed, so a malformed or empty scope never widens access.
func grantAllows(scopes json.RawMessage, operation string) bool {
	var s grantScopes
	if err := json.Unmarshal(scopes, &s); err != nil {
		return false
	}
	for _, op := range s.Operations {
		if op == operation {
			return true
		}
	}
	return false
}

// upstreamUsage is the token-count block of an OpenAI-compatible chat
// completion response.
type upstreamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// upstreamCompletion is the subset of an OpenAI-compatible response the broker
// reads for metering: the model that served the call and its token usage.
type upstreamCompletion struct {
	Model string         `json:"model"`
	Usage *upstreamUsage `json:"usage"`
}

// extractUsage pulls the model and token counts from an upstream response body
// for metering. It handles both the non-streamed JSON body (usage is a
// top-level object) and the SSE stream (usage, when the client requested it,
// arrives in a trailing data: event). ok is false when no usage object is
// present, in which case the caller records nothing.
func extractUsage(contentType string, body []byte) (model string, promptTokens, completionTokens int, ok bool) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return extractUsageSSE(body)
	}
	var c upstreamCompletion
	if err := json.Unmarshal(body, &c); err != nil || c.Usage == nil {
		return "", 0, 0, false
	}
	return c.Model, c.Usage.PromptTokens, c.Usage.CompletionTokens, true
}

// extractUsageSSE scans an SSE body for the last data: event carrying a usage
// object, returning its token counts and the model. Non-data lines, empty
// payloads, the [DONE] sentinel, and unparseable payloads are skipped. The
// model is taken from whichever chunk last reported one, since every chunk of an
// OpenAI stream carries it.
func extractUsageSSE(body []byte) (model string, promptTokens, completionTokens int, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), meterCaptureLimit)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var c upstreamCompletion
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			continue
		}
		if c.Model != "" {
			model = c.Model
		}
		if c.Usage != nil {
			promptTokens = c.Usage.PromptTokens
			completionTokens = c.Usage.CompletionTokens
			ok = true
		}
	}
	return model, promptTokens, completionTokens, ok
}

// writeUnauthorized writes the flat 401 envelope used for every fail-closed
// grant rejection (missing/unknown/expired/revoked grant, terminated session).
// The message is deliberately generic so it never distinguishes which check
// failed.
func (b *broker) writeUnauthorized(w http.ResponseWriter) {
	_ = httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized,
		"missing or invalid proxy token")
}

// writeInternal writes the generic 500 envelope for a server-side fault (e.g. a
// database error during grant/session lookup), never echoing internals.
func (b *broker) writeInternal(w http.ResponseWriter) {
	_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
}

// sha256Hex returns the lowercase-hex SHA-256 of s. This is the proxy's per-call
// token hash and must match the launch handler's stored form exactly (ADR-0003
// §3): the full presented token string, hashed with SHA-256, hex-encoded lower.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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
