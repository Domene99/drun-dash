package memberui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// rtFunc adapts a function to http.RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// handlerClient returns an *http.Client that serves every request in-memory
// through h (via httptest.NewRecorder — no socket bind), so tests exercise the
// full control-plane / proxy round trip without binding a port. This keeps the
// suite runnable inside the network-isolated drun sandbox as well as on a host.
func handlerClient(h http.Handler) *http.Client {
	return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Result(), nil
	})}
}

// newRunServerRT builds a Server whose control-plane client is served in-memory
// by cpHandler and whose drun engine is the fake conn. It mirrors newRunServer
// but binds no sockets; run-view seams are stubbed for deterministic tests.
func newRunServerRT(t *testing.T, cpHandler http.HandlerFunc, conn drunConn, opts ...Option) *Server {
	t.Helper()
	client := NewClient("http://cp", handlerClient(cpHandler))
	base := []Option{
		WithModel("test-model"),
		WithDrunDialer(func(string) drunConn { return conn }),
		WithBundleVerifier(func(json.RawMessage, time.Time) error { return nil }),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		WithEventIDFunc(seqIDs()),
	}
	base = append(base, opts...)
	return New(client, nil, base...)
}
