package memberui

import (
	"net/http"
	"os"
	"path/filepath"
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

// TestCommitPersistsNewFilesToWorkingDir covers the issue-#296 follow-up: files
// the agent *created* (which session_commit does not write back) are persisted
// into the member's working directory on commit, including nested paths, while a
// traversal key can never escape that directory.
func TestCommitPersistsNewFilesToWorkingDir(t *testing.T) {
	dir := t.TempDir()
	conn := &fakeDrunConn{
		tools:      []drunmcp.Tool{bashTool()},
		diffText:   "--- a/hello.py\n+++ b/hello.py\n@@ -0,0 +1 @@\n+print(\"hi\")",
		exportKeys: []string{"hello.py", "src/util.py", "../escape.txt"},
		readFiles: map[string]string{
			"hello.py":      "print(\"hi\")\n",
			"src/util.py":   "x = 1\n",
			"../escape.txt": "should not escape\n",
		},
	}
	s := newRunServerRT(t, func(http.ResponseWriter, *http.Request) {}, conn)
	sess := storedSession("http://proxy")
	sess.WorkingDir = dir
	s.storeSession(sess)
	authGet(s, "/sessions/sess-1") // bind

	rr := authPostForm(s, "/sessions/sess-1/commit", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !conn.committed {
		t.Error("session_commit must still run for modified mounted files")
	}
	// New files land in the working directory, nested paths preserved.
	for rel, want := range map[string]string{
		"hello.py":    "print(\"hi\")\n",
		"src/util.py": "x = 1\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("new file %q not written to working dir: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%q = %q, want %q", rel, got, want)
		}
	}
	// The traversal key is neutralized: nothing is written outside the dir.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Error("a ../ export key escaped the working directory")
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
