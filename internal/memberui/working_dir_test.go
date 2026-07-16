package memberui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// These tests cover the chosen-working-directory mount (issue #296): a member
// picks a host folder when starting a chat, drun-dash mounts it into the sandbox
// session so the agent works on real files, and Commit writes back to that
// folder. The launch form captures the path, handleLaunch validates + stores it,
// and bindEngine mounts it via SessionMount after CreateSession.

// TestLaunchStoresAbsoluteWorkingDir verifies the launch form's optional path is
// captured onto the launched-session state when it is an absolute host path.
func TestLaunchStoresAbsoluteWorkingDir(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sessions/launch") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(SessionLaunch{
				SessionID:     "sess-1",
				ProxyBaseURL:  "http://proxy:8081",
				ProxyToken:    "dgp_secret",
				FanOutCeiling: 4,
				ExpiresAt:     "2026-07-11T12:00:00Z",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(AgentList{})
	})

	form := url.Values{"agent_id": {"agent-9"}, "agent_name": {"Alpha"}, "working_dir": {"/Users/me/project"}}
	rr := authPostForm(s, "/sessions", form)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	ls, ok := s.sessions["sess-1"]
	if !ok {
		t.Fatal("session was not stored server-side")
	}
	if ls.WorkingDir != "/Users/me/project" {
		t.Errorf("stored WorkingDir = %q, want /Users/me/project", ls.WorkingDir)
	}
}

// TestLaunchBlankWorkingDirIsScratch verifies a blank path stores no working
// directory — the prior scratch-sandbox behavior.
func TestLaunchBlankWorkingDirIsScratch(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sessions/launch") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(SessionLaunch{SessionID: "sess-1", ProxyBaseURL: "http://proxy", ProxyToken: "t"})
			return
		}
		_ = json.NewEncoder(w).Encode(AgentList{})
	})

	form := url.Values{"agent_id": {"agent-9"}, "working_dir": {"   "}}
	rr := authPostForm(s, "/sessions", form)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ls := s.sessions["sess-1"]; ls == nil || ls.WorkingDir != "" {
		t.Errorf("blank working dir should store no path, got %+v", ls)
	}
}

// TestLaunchRejectsRelativeWorkingDir verifies a non-absolute path is rejected
// before the session is launched, with a readable message.
func TestLaunchRejectsRelativeWorkingDir(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("control plane should not be called for an invalid working dir")
	})

	form := url.Values{"agent_id": {"agent-9"}, "working_dir": {"relative/dir"}}
	rr := authPostForm(s, "/sessions", form)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "absolute path") {
		t.Errorf("expected an absolute-path validation message: %q", rr.Body.String())
	}
	if _, ok := s.sessions["sess-1"]; ok {
		t.Error("a session must not be stored when the working dir is invalid")
	}
}

// TestHomeRendersWorkingDirField verifies the launch form exposes the optional
// working-directory field and its help text.
func TestHomeRendersWorkingDirField(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(AgentList{Items: []Agent{{ID: "a1", Name: "Alpha", Description: "the alpha agent"}}})
	})
	rr := authGet(s, "/")
	body := rr.Body.String()
	if !strings.Contains(body, `name="working_dir"`) {
		t.Errorf("home page missing the working-directory field: %q", body)
	}
	if !strings.Contains(body, "mounted into the sandbox") {
		t.Errorf("home page missing the working-directory help text: %q", body)
	}
}

// TestRunViewMountsWorkingDir verifies bindEngine mounts the session's chosen
// working directory (SessionMount) after creating the session, binds normally,
// and the run view shows where Commit will write.
func TestRunViewMountsWorkingDir(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn)
	ls := storedSession("http://proxy")
	ls.WorkingDir = "/Users/me/project"
	s.storeSession(ls)

	rr := authGet(s, "/sessions/sess-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(conn.mounted) != 1 || conn.mounted[0] != "/Users/me/project" {
		t.Errorf("expected one SessionMount of the working dir, got %v", conn.mounted)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/Users/me/project") {
		t.Errorf("run view should show the mounted working directory: %q", body)
	}
	if !strings.Contains(body, "session_bash") {
		t.Error("engine should still bind and offer tools after mounting")
	}
}

// TestRunViewNoWorkingDirDoesNotMount verifies a session with no working dir
// binds without mounting and the run view names it a scratch sandbox.
func TestRunViewNoWorkingDirDoesNotMount(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn)
	s.storeSession(storedSession("http://proxy")) // storedSession sets no WorkingDir

	rr := authGet(s, "/sessions/sess-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(conn.mounted) != 0 {
		t.Errorf("no working dir should mean no mount, got %v", conn.mounted)
	}
	if !strings.Contains(rr.Body.String(), "scratch sandbox") {
		t.Errorf("run view should note a scratch sandbox with no host folder: %q", rr.Body.String())
	}
}

// TestRunViewMountErrorSurfaces verifies a mount failure aborts the bind with a
// readable error and withholds the chat (fail-closed).
func TestRunViewMountErrorSurfaces(t *testing.T) {
	conn := &fakeDrunConn{tools: []drunmcp.Tool{bashTool()}, mountErr: errWrap("no such directory")}
	s := newRunServer(t, func(http.ResponseWriter, *http.Request) {}, conn)
	ls := storedSession("http://proxy")
	ls.WorkingDir = "/Users/me/missing"
	s.storeSession(ls)

	rr := authGet(s, "/sessions/sess-1")
	body := rr.Body.String()
	if !strings.Contains(body, "could not mount") || !strings.Contains(body, "/Users/me/missing") {
		t.Errorf("expected a readable mount error naming the path, got: %q", body)
	}
	if strings.Contains(body, `name="directive"`) {
		t.Error("chat input should be withheld when the mount fails")
	}
}
