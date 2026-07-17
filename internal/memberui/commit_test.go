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

// TestCommitExportsNewFilesToWorkingDir covers the issue-#296 follow-up: on
// commit, files the agent *created* (which session_commit does not write back)
// are exported through drun (session_export) into the member's working
// directory, with the working dir passed as the export output_dir.
func TestCommitExportsNewFilesToWorkingDir(t *testing.T) {
	dir := t.TempDir()
	conn := &fakeDrunConn{
		tools:    []drunmcp.Tool{bashTool()},
		diffText: "--- a/hello.py\n+++ b/hello.py\n@@ -0,0 +1 @@\n+print(\"hi\")",
		exportFiles: map[string]string{
			"hello.py":    "print(\"hi\")\n",
			"src/util.py": "x = 1\n",
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
	// The working dir is passed as the export output_dir (drun does the write).
	if conn.exportOutputDir != dir {
		t.Errorf("session_export output_dir = %q, want the working dir %q", conn.exportOutputDir, dir)
	}
	// The exported new files (nested paths preserved) land in the working dir.
	for rel, want := range map[string]string{
		"hello.py":    "print(\"hi\")\n",
		"src/util.py": "x = 1\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("new file %q not exported into working dir: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%q = %q, want %q", rel, got, want)
		}
	}
	if !strings.Contains(rr.Body.String(), "new file") {
		t.Errorf("commit result should report the new files: %q", rr.Body.String())
	}
}

// TestCommitWithoutWorkingDirDoesNotExport confirms a scratch session (no working
// directory) commits edits but never calls session_export.
func TestCommitWithoutWorkingDirDoesNotExport(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}, diffText: "--- a/x\n+++ b/x\n+change"}
	s := newRunServerRT(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy")) // no WorkingDir
	authGet(s, "/sessions/sess-1")

	rr := authPostForm(s, "/sessions/sess-1/commit", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !conn.committed {
		t.Error("session_commit must run")
	}
	if conn.exportOutputDir != "" {
		t.Errorf("no working dir → session_export must not be called; got output_dir %q", conn.exportOutputDir)
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
