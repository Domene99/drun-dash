package memberui

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/memberui/views"
	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// This file implements diff-before-commit — the core host-file-safety guarantee
// (design §6.3): nothing the agent did in the sandbox reaches the member's host
// files without an explicit member approval. The run view shows the pending
// session_diff for review; only an explicit member "Commit" action calls
// session_commit; "Reject" commits nothing. The commit path is reachable ONLY
// through handleCommit — never from the agent loop, which is offered neither
// session_commit nor session_export/snapshot (see managementTools in engine.go).

// handleDiff renders the pending host-file diff for review: session_diff from
// the mounted baseline to the current checkpoint, with Commit / Reject actions.
// It is a fragment that swaps the run view's #hostchanges panel.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	ls := s.lookupSession(sessionID)
	if ls == nil {
		http.NotFound(w, r)
		return
	}
	run := s.runFor(ls)

	// TryLock: a diff review requested mid-turn can't use the sequential client
	// (the turn holds it), so ask the member to retry once the run settles
	// rather than blocking the request.
	if !run.mu.TryLock() {
		s.render(w, r, http.StatusOK, views.DiffReview(views.DiffData{RunInProgress: true}))
		return
	}
	engine := run.engine
	bindErr := run.bindErr
	if engine == nil {
		run.mu.Unlock()
		msg := bindErr
		if msg == "" {
			msg = "The drun engine is not bound yet."
		}
		s.render(w, r, http.StatusOK, views.DiffReview(views.DiffData{Error: msg}))
		return
	}
	// Diff the mounted baseline (checkpoint 0) against the current head — the
	// exact set session_commit would write back.
	diff, err := engine.conn.SessionDiff(r.Context(), engine.sessionID, drunmcp.CheckpointRef{}, drunmcp.CheckpointRef{})
	run.mu.Unlock()

	data := views.DiffData{SessionID: sessionID}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "member-console: session diff",
			slog.String("session_id", sessionID), slog.Any("error", err))
		data.Error = "Could not read the pending diff from the drun engine."
	} else {
		data.Diff = diff
		data.Empty = strings.TrimSpace(diff) == ""
	}
	s.render(w, r, http.StatusOK, views.DiffReview(data))
}

// handleCommit performs the member-approved commit: it writes the sandbox's
// changed files back to their host paths via session_commit. This is the ONLY
// path that ever calls session_commit, and it runs only on this explicit member
// POST — never automatically from the agent loop (design §6.3).
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	ls := s.lookupSession(sessionID)
	if ls == nil {
		http.NotFound(w, r)
		return
	}
	run := s.runFor(ls)

	run.mu.Lock()
	engine := run.engine
	if engine == nil {
		run.mu.Unlock()
		s.render(w, r, http.StatusOK, views.CommitResult(views.CommitData{SessionID: sessionID, Error: "The drun engine is not bound; nothing was committed."}))
		return
	}
	res, err := engine.conn.SessionCommit(r.Context(), engine.sessionID)
	run.mu.Unlock()

	if err != nil {
		s.logger.ErrorContext(r.Context(), "member-console: session commit",
			slog.String("session_id", sessionID), slog.Any("error", err))
		s.render(w, r, http.StatusOK, views.CommitResult(views.CommitData{SessionID: sessionID, Error: "The commit failed; no changes were written to your host files."}))
		return
	}
	s.render(w, r, http.StatusOK, views.CommitResult(views.CommitData{SessionID: sessionID, Committed: true, Detail: strings.TrimSpace(res.Text())}))
}

// handleReject records the member's rejection of the pending diff. It commits
// nothing — deliberately it makes no drun call at all — so the sandbox changes
// stay on-box, uncommitted (design §6.3).
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	if s.lookupSession(sessionID) == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.CommitResult(views.CommitData{SessionID: sessionID, Rejected: true}))
}
