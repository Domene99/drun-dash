package memberui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// matrixScoreFor gives each branch a distinct score by its proxy token, so the
// ranking has a clear winner.
func matrixScoreFor(token string) string {
	switch token {
	case "tok-alpha":
		return "0.9"
	case "tok-beta":
		return "0.3"
	default:
		return "0.1"
	}
}

func TestMatrixRanksBranchesAndTerminatesAll(t *testing.T) {
	// Proxy returns each branch's numeric score (keyed by its proxy token), with
	// no tool calls, so the scorer turn's final answer is the score.
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, matrixScoreFor(tok))
	})

	var mu sync.Mutex
	terminated := map[string]bool{}
	cp := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sessions/launch"):
			var body struct {
				AgentID string `json:"agent_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(SessionLaunch{
				SessionID:     "sess-" + body.AgentID,
				ProxyBaseURL:  "http://proxy",
				ProxyToken:    "tok-" + body.AgentID,
				FanOutCeiling: 2,
				PolicyBundle:  json.RawMessage(`{"stub":"b"}`),
			})
		case strings.HasSuffix(r.URL.Path, "/terminate"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/terminate")
			mu.Lock()
			terminated[id] = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(Session{SessionID: id, Status: "terminated"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}

	// Each branch gets its own fake drun engine (matrix runs branches
	// concurrently; a shared client would race and is sequential by contract).
	s := newRunServerRT(t, cp, nil,
		WithProxyHTTPClient(handlerClient(proxy)),
		WithDrunDialer(func(string) drunConn {
			return &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
		}))

	form := url.Values{
		"directive": {"do the task"},
		"scorer":    {"print a score"},
		"agent":     {"alpha", "beta"},
	}
	rr := authPostForm(s, "/matrix", form)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"alpha", "beta", "0.9", "Winner", "All sessions terminated"} {
		if !strings.Contains(body, want) {
			t.Errorf("matrix result missing %q:\n%s", want, body)
		}
	}
	// The winner (alpha, 0.9) must be ranked ahead of beta (0.3).
	if strings.Index(body, "alpha") > strings.Index(body, "beta") {
		t.Errorf("alpha (higher score) should rank before beta:\n%s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if !terminated["sess-alpha"] || !terminated["sess-beta"] {
		t.Errorf("every launched session must be terminated, got %v", terminated)
	}
	if strings.Contains(body, "dgp_") || strings.Contains(body, "tok-alpha") {
		t.Errorf("matrix result must not leak proxy tokens: %q", body)
	}
}

func TestMatrixViewListsEntitledAgents(t *testing.T) {
	cp := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/me/agents") {
			_ = json.NewEncoder(w).Encode(AgentList{Items: []Agent{{ID: "a1", Name: "Alpha"}}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	s := newRunServerRT(t, cp, &fakeDrunConn{})

	rr := authGet(s, "/matrix")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Alpha", "Run matrix", `name="directive"`, `name="scorer"`, `value="a1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("matrix view missing %q:\n%s", want, body)
		}
	}
}

func TestMatrixRunRequiresInput(t *testing.T) {
	s := newRunServerRT(t, func(http.ResponseWriter, *http.Request) {}, &fakeDrunConn{})
	rr := authPostForm(s, "/matrix", url.Values{"directive": {""}, "scorer": {""}})
	if !strings.Contains(rr.Body.String(), "at least one agent") {
		t.Errorf("empty matrix submission should be rejected: %q", rr.Body.String())
	}
}
