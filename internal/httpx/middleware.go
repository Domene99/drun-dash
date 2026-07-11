package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"
)

// Middleware is a chi-compatible middleware constructor:
// func(http.Handler) http.Handler. All middleware in this package are of this
// type, so they can be passed directly to chi's Router.Use.
type Middleware = func(http.Handler) http.Handler

// HeaderRequestID is the HTTP header carrying the request ID, both inbound
// (client-supplied) and outbound (echoed on the response).
const HeaderRequestID = "X-Request-ID"

// maxRequestIDLen bounds an accepted client-supplied request ID.
const maxRequestIDLen = 128

type contextKey int

const requestIDKey contextKey = iota

// RequestID is middleware that ensures every request carries a request ID. If
// the inbound request has a valid X-Request-ID header it is reused; otherwise a
// new random 128-bit ID is generated. The ID is stored in the request context
// (see RequestIDFromContext) and echoed on the response X-Request-ID header so
// clients and logs can correlate a request end to end.
//
// It has the Middleware signature, so it can be passed directly to chi's Use.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !isValidRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the request ID stored by the RequestID
// middleware, or "" if none is present.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// isValidRequestID reports whether a client-supplied request ID is safe to echo
// and log: non-empty, at most maxRequestIDLen bytes, and limited to unreserved
// URL/token characters. Rejecting anything else prevents header- and
// log-injection through the X-Request-ID header.
func isValidRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == '+', c == '=', c == '/', c == ':':
		default:
			return false
		}
	}
	return true
}

// newRequestID returns a random 128-bit hex-encoded identifier. It falls back
// to a nanosecond timestamp only if the system CSPRNG is unavailable, which
// should not happen in practice.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// RecovererOptions configures Recoverer behavior.
type RecovererOptions struct {
	// Logger receives the panic value and stack trace. If nil, slog.Default().
	Logger *slog.Logger
	// Debug, when true, includes the recovered panic value in the response
	// body's details. Leave false in production so internal details do not
	// leak to clients.
	Debug bool
}

// Recoverer returns middleware that recovers from panics in downstream
// handlers, logs them with a stack trace, and writes a generic 500 JSON error
// envelope. The panic value is never written to the response body, so internal
// details do not leak to clients. Pass nil to use slog.Default().
func Recoverer(logger *slog.Logger) Middleware {
	return RecovererWithOptions(RecovererOptions{Logger: logger})
}

// RecovererWithOptions is Recoverer with explicit options, including a Debug
// mode that echoes the panic value in the response details for local
// development. Do not enable Debug in production.
func RecovererWithOptions(opts RecovererOptions) Middleware {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is a deliberate signal to abort the
				// response; re-panic so the server handles it as intended.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("request_id", RequestIDFromContext(r.Context())),
					slog.String("stack", string(debug.Stack())),
				)
				if opts.Debug {
					_ = WriteErrorDetails(w, http.StatusInternalServerError, CodeInternal,
						"internal server error", map[string]any{"panic": panicString(rec)})
					return
				}
				_ = WriteError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logger returns middleware that logs one structured slog line per request
// after it completes, with method, path, status, byte count, duration, and the
// request ID (populated when RequestID ran earlier in the chain). Pass nil to
// use slog.Default().
func Logger(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// panicString renders a recovered panic value for debug output.
func panicString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// statusRecorder wraps an http.ResponseWriter to capture the status code and
// number of bytes written, so Logger can report them after the handler runs.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// Flush implements http.Flusher when the wrapped writer supports it, so
// streaming handlers keep working through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to net/http's ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
