package memberui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// bindForCommit stores a session and binds the engine, returning the fake conn.
func bindForCommit(t *testing.T, diff string) (*Server, *fakeDrunConn) {
	t.Helper()
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}, diffText: diff}
	s := newRunServerRT(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy"))
	authGet(s, "/sessions/sess-1") // bind
	return s, conn
}

func TestDiffShowsPendingChangesWithoutCommitting(t *testing.T) {
	s, conn := bindForCommit(t, "--- a/x\n+++ b/x\n+a new line")

	rr := authGet(s, "/sessions/sess-1/diff")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"a new line", "commit", "Reject"} {
		if !strings.Contains(body, want) {
			t.Errorf("diff review missing %q:\n%s", want, body)
		}
	}
	if conn.committed {
		t.Error("viewing the diff must not commit anything to host files")
	}
}

func TestCommitOnlyOnExplicitMemberApproval(t *testing.T) {
	s, conn := bindForCommit(t, "--- a/x\n+++ b/x\n+change")

	// Nothing has committed yet just from binding / viewing.
	if conn.committed {
		t.Fatal("no commit should have happened before the member approves")
	}

	rr := authPostForm(s, "/sessions/sess-1/commit", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !conn.committed {
		t.Error("the approve action must call session_commit")
	}
	if !strings.Contains(rr.Body.String(), "Committed") {
		t.Errorf("commit result should confirm the write: %q", rr.Body.String())
	}
}

func TestRejectCommitsNothing(t *testing.T) {
	s, conn := bindForCommit(t, "--- a/x\n+++ b/x\n+change")

	rr := authPostForm(s, "/sessions/sess-1/reject", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if conn.committed {
		t.Error("reject must not commit anything")
	}
	if !strings.Contains(rr.Body.String(), "Rejected") {
		t.Errorf("reject result should say nothing was committed: %q", rr.Body.String())
	}
}
