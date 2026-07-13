package memberui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/memberui/views"
)

// This file renders the run's checkpoint timeline (design §6.3): the
// checkpoint/fork tree of the bound drun session, shown as an HTMX-refreshed
// fragment in the run view. drun creates a checkpoint on every write/bash step,
// so the timeline surfaces the checkpoints agentrun creates as the loop runs —
// each an inspectable entry (id, label, parent, file delta), with the current
// head and any forks marked.
//
// The timeline is read through the bound session's sequential drunmcp.Client, so
// it shares run.mu with the turn loop. A poll that arrives mid-turn cannot grab
// the client (the turn holds it), so it renders a "run in progress" note and
// keeps polling rather than blocking the request — the timeline refreshes as
// soon as the turn settles.

// historyCheckpoint mirrors one entry of the session_history JSON array: a
// checkpoint with its file delta relative to the previous one. Only the fields
// the timeline renders are decoded.
type historyCheckpoint struct {
	CheckpointID  int    `json:"checkpoint_id"`
	Label         string `json:"label,omitempty"`
	FileCount     int    `json:"file_count"`
	FilesAdded    int    `json:"files_added_count"`
	FilesModified int    `json:"files_modified_count"`
	FilesRemoved  int    `json:"files_removed_count"`
}

// treeSession mirrors one node of the session_tree JSON array: a session, the
// checkpoint it forked from (if any), and its checkpoints with the current-head
// flag. The tree is the only source of is_current and of fork relationships,
// which session_history does not carry.
type treeSession struct {
	SessionID          string `json:"session_id"`
	ParentSessionID    string `json:"parent_session_id"`
	ParentCheckpointID *int   `json:"parent_checkpoint_id"`
	Checkpoints        []struct {
		CheckpointID int  `json:"checkpoint_id"`
		IsCurrent    bool `json:"is_current"`
	} `json:"checkpoints"`
}

// handleTimeline renders the checkpoint-timeline fragment for a bound session.
// It is polled by the run view (HTMX hx-trigger) and returns just the timeline
// markup so each poll swaps #timeline in place.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	ls := s.lookupSession(sessionID)
	if ls == nil {
		http.NotFound(w, r)
		return
	}
	run := s.runFor(ls)

	// TryLock so a poll never blocks behind an in-flight turn (which holds
	// run.mu for its whole duration) and never piles up: if the turn owns the
	// sequential client, show a live note and let HTMX poll again.
	if !run.mu.TryLock() {
		s.render(w, r, http.StatusOK, views.TimelineFragment(views.TimelineData{RunInProgress: true}))
		return
	}
	engine := run.engine
	bindErr := run.bindErr
	run.mu.Unlock()

	data := views.TimelineData{}
	switch {
	case engine == nil && bindErr != "":
		data.Error = bindErr
	case engine == nil:
		data.Error = "The drun engine is not bound yet."
	default:
		// Re-acquire for the duration of the (sequential) drun reads.
		if !run.mu.TryLock() {
			s.render(w, r, http.StatusOK, views.TimelineFragment(views.TimelineData{RunInProgress: true}))
			return
		}
		entries, err := s.fetchTimeline(r.Context(), engine)
		run.mu.Unlock()
		if err != nil {
			s.logger.ErrorContext(r.Context(), "member-console: fetch timeline",
				slog.String("session_id", sessionID), slog.Any("error", err))
			data.Error = "Could not read the checkpoint timeline from the drun engine."
		} else {
			data.Entries = entries
		}
	}

	s.render(w, r, http.StatusOK, views.TimelineFragment(data))
}

// fetchTimeline reads session_history and session_tree for the bound session and
// merges them into the ordered timeline view model: history supplies the ordered
// checkpoints and their file deltas; the tree supplies the current-head marker
// and any child sessions forked from a checkpoint. The caller holds run.mu (the
// drun client is sequential).
func (s *Server) fetchTimeline(ctx context.Context, eng *boundEngine) ([]views.TimelineEntry, error) {
	histRes, err := eng.conn.SessionHistory(ctx, eng.sessionID)
	if err != nil {
		return nil, fmt.Errorf("session_history: %w", err)
	}
	var history []historyCheckpoint
	if err := json.Unmarshal([]byte(histRes.Text()), &history); err != nil {
		return nil, fmt.Errorf("decode session_history: %w", err)
	}

	// The tree is best-effort enrichment: without it the timeline still renders
	// from history, just without the current-head marker or fork notes.
	current := map[int]bool{}
	forksAt := map[int][]string{}
	if treeRes, err := eng.conn.SessionTree(ctx, eng.sessionID); err == nil {
		var tree []treeSession
		if json.Unmarshal([]byte(treeRes.Text()), &tree) == nil {
			for _, node := range tree {
				if node.SessionID == eng.sessionID {
					for _, cp := range node.Checkpoints {
						if cp.IsCurrent {
							current[cp.CheckpointID] = true
						}
					}
					continue
				}
				if node.ParentSessionID == eng.sessionID && node.ParentCheckpointID != nil {
					forksAt[*node.ParentCheckpointID] = append(forksAt[*node.ParentCheckpointID], node.SessionID)
				}
			}
		}
	}

	sort.Slice(history, func(i, j int) bool { return history[i].CheckpointID < history[j].CheckpointID })
	entries := make([]views.TimelineEntry, 0, len(history))
	for i, cp := range history {
		parent := "—"
		if i > 0 {
			parent = strconv.Itoa(history[i-1].CheckpointID)
		}
		entries = append(entries, views.TimelineEntry{
			ID:        strconv.Itoa(cp.CheckpointID),
			Label:     cp.Label,
			Parent:    parent,
			IsCurrent: current[cp.CheckpointID],
			Delta:     fmt.Sprintf("+%d ~%d -%d", cp.FilesAdded, cp.FilesModified, cp.FilesRemoved),
			Forks:     forksAt[cp.CheckpointID],
		})
	}
	return entries, nil
}
