package memberui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

func TestTimelineRendersFromHistoryAndTree(t *testing.T) {
	conn := &fakeDrunConn{
		tools: []drunmcp.Tool{bashTool()},
		historyText: `[{"checkpoint_id":0,"file_count":10},` +
			`{"checkpoint_id":1,"label":"baseline","files_added_count":2,"files_modified_count":1,"files_removed_count":0}]`,
		treeText: `[{"session_id":"drun-sess","checkpoints":[{"checkpoint_id":0,"is_current":false},{"checkpoint_id":1,"is_current":true}]},` +
			`{"session_id":"fork-9","parent_session_id":"drun-sess","parent_checkpoint_id":1,"checkpoints":[{"checkpoint_id":0,"is_current":true}]}]`,
	}
	s := newRunServerRT(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy"))
	authGet(s, "/sessions/sess-1") // bind the engine (creates drun-sess)

	rr := authGet(s, "/sessions/sess-1/timeline")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"#0", "#1", "baseline", "current", "+2 ~1 -0", "fork-9"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline missing %q:\n%s", want, body)
		}
	}
}

func TestTimelineRunInProgress(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServerRT(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy"))
	authGet(s, "/sessions/sess-1") // bind

	// Simulate an in-flight turn holding the sequential drun client.
	run := s.runFor(s.lookupSession("sess-1"))
	run.mu.Lock()
	rr := authGet(s, "/sessions/sess-1/timeline")
	run.mu.Unlock()

	if !strings.Contains(rr.Body.String(), "run is in progress") {
		t.Errorf("timeline should report a run in progress while the client is busy: %q", rr.Body.String())
	}
}

func TestTimelineUnknownSessionNotFound(t *testing.T) {
	s := newRunServerRT(t, func(http.ResponseWriter, *http.Request) {}, &fakeDrunConn{})
	rr := authGet(s, "/sessions/nope/timeline")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}
