package memberui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/agentrun"
	"github.com/Domene99/drun-dash/internal/memberui/views"
)

// trajectoryFlushTimeout bounds the detached, best-effort trajectory POST that
// runs after a turn. It is generous — telemetry is off the member's critical
// path (ADR-0004 §5) — but bounded so a wedged control plane cannot leak
// goroutines.
const trajectoryFlushTimeout = 15 * time.Second

// runState is the per-session run record for the conversational view. It holds
// the launched session (with its secret proxy_token, never rendered), the bound
// drun engine, and the directive awaiting the SSE stream. A drunmcp.Client is
// sequential, so mu serializes every turn and lifecycle action against the one
// engine (ADR-0004 §4).
type runState struct {
	launch *launchedSession

	mu      sync.Mutex
	engine  *boundEngine
	bindErr string
	pending string // directive submitted, awaiting the stream to run it
}

// runFor returns the run record for a launched session, creating it on first
// use. Records live for the life of the session; handleTerminate drops them.
func (s *Server) runFor(ls *launchedSession) *runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[ls.SessionID]; ok {
		return r
	}
	r := &runState{launch: ls}
	s.runs[ls.SessionID] = r
	return r
}

// lookupSession returns the launched session by id, or nil when this console
// did not launch it (or it was terminated).
func (s *Server) lookupSession(sessionID string) *launchedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[sessionID]
}

// handleRunView renders the conversational run view for a launched session. On
// first entry it binds the local drun engine (probe, create session, discover
// the policy-bound tool set) after verifying the policy bundle fail-closed; a
// bind failure renders a clear error and withholds the chat (ADR-0004 §3).
func (s *Server) handleRunView(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	ls := s.lookupSession(sessionID)
	if ls == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	run := s.runFor(ls)

	run.mu.Lock()
	if run.engine == nil && run.bindErr == "" {
		if err := s.bindRun(r.Context(), run); err != nil {
			run.bindErr = readableEngineError(err)
			s.logger.ErrorContext(r.Context(), "member-console: bind run engine",
				slog.String("session_id", sessionID), slog.Any("error", err))
		}
	}
	data := s.runViewData(run)
	run.mu.Unlock()

	s.render(w, r, http.StatusOK, views.RunPage(data))
}

// bindRun verifies the session's policy bundle and binds the drun engine. It is
// fail-closed: no verified bundle → no bind → no run (ADR-0004 §3). The caller
// holds run.mu.
func (s *Server) bindRun(ctx context.Context, run *runState) error {
	if err := s.verify(run.launch.policyBundle, s.now()); err != nil {
		return fmt.Errorf("policy bundle rejected: %w", err)
	}
	eng, err := bindEngine(ctx, s.dial, s.drunEndpoint, run.launch.policyBundle)
	if err != nil {
		return err
	}
	run.engine = eng
	return nil
}

// runViewData assembles the run view model from the run state. The caller holds
// run.mu. The proxy token is deliberately never copied into the view.
func (s *Server) runViewData(run *runState) views.RunData {
	ls := run.launch
	data := views.RunData{
		SessionID:    ls.SessionID,
		Agent:        ls.AgentID,
		ProxyBaseURL: ls.ProxyBaseURL,
		FanOut:       fmt.Sprintf("%d", ls.FanOut),
		Expires:      orDash(ls.ExpiresAt),
		Model:        s.model,
		ModelMissing: s.model == "",
		EngineError:  run.bindErr,
	}
	if run.engine != nil {
		data.EngineReady = true
		data.PolicyScoped = run.engine.policyScoped
		for _, t := range run.engine.tools {
			data.Tools = append(data.Tools, t.Name)
		}
	}
	return data
}

// handleTurn accepts a directive and stages it for the SSE stream to run. It
// returns the transcript fragment (the member's message plus a live assistant
// bubble that connects to the stream). The heavy lifting — the governed turn
// loop — happens in handleStream so the browser can render steps as they arrive.
func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	ls := s.lookupSession(sessionID)
	if ls == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	run := s.runFor(ls)

	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.TurnStarted(sessionID, "Could not read the submitted directive."))
		return
	}
	directive := strings.TrimSpace(r.PostFormValue("directive"))
	if directive == "" {
		s.render(w, r, http.StatusBadRequest, views.TurnStarted(sessionID, "Type a directive to run."))
		return
	}

	run.mu.Lock()
	ready := run.engine != nil
	if ready && s.model != "" {
		run.pending = directive
	}
	run.mu.Unlock()

	if !ready {
		s.render(w, r, http.StatusOK, views.TurnStarted(sessionID, "The drun engine is not bound; reload the run view."))
		return
	}
	if s.model == "" {
		s.render(w, r, http.StatusOK, views.TurnStarted(sessionID, "No model is configured (set MEMBER_CONSOLE_MODEL)."))
		return
	}

	s.render(w, r, http.StatusOK, views.TurnStarted(sessionID, directive))
}

// handleStream runs the staged directive as a governed turn and streams each
// step to the browser as Server-Sent Events (ADR-0004 §4). It calls
// [agentrun.Run] with the session's proxy grant and the bound drun tool set,
// emits structure-only trajectory as it goes (§5), and escalates policy-blocked
// tools to approvals (§6). An EventSource reconnect after the turn finds no
// staged directive and closes immediately, so a turn runs at most once.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	token := tokenFromContext(r.Context())
	ls := s.lookupSession(sessionID)
	if ls == nil {
		http.NotFound(w, r)
		return
	}
	run := s.runFor(ls)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	setSSEHeaders(w)

	run.mu.Lock()
	defer run.mu.Unlock()

	directive := run.pending
	run.pending = ""
	engine := run.engine
	if directive == "" || engine == nil {
		// Nothing staged (e.g. a reconnect): close the stream cleanly.
		writeSSEEvent(w, flusher, "done", "")
		return
	}

	emitter := newTrajectoryEmitter(s.now, s.newEventID)
	emitter.begin()

	cfg := agentrun.Config{
		ProxyBaseURL: ls.ProxyBaseURL,
		ProxyToken:   ls.proxyToken,
		Model:        s.model,
		Tools:        engine.tools,
		Executor:     engine.executor,
		HTTPClient:   s.proxyHTTP,
		OnStep: func(step agentrun.Step) {
			emitter.onStep(step)
			writeSSEEvent(w, flusher, "step", views.RenderStepFragment(toStepView(step)))
		},
		OnBlocked: s.escalation(r.Context(), token, sessionID),
	}

	_, runErr := agentrun.Run(r.Context(), cfg, directive)
	if runErr != nil {
		if errors.Is(runErr, agentrun.ErrProxyBlocked) {
			// A fail-closed proxy rejection is an escalation, not a retry
			// (ADR-0004 §6a): raise a visible approval for an admin to decide.
			s.raiseApproval(r.Context(), token, sessionID, "model_call", runErr.Error())
		}
		writeSSEEvent(w, flusher, "step", views.RenderErrorFragment(readableRunError(runErr)))
	}

	// Flush trajectory off the critical path: the stream closes now; the batch
	// posts in the background (idempotent by event_id).
	if events := emitter.drain(); len(events) > 0 {
		go s.flushTrajectory(token, sessionID, events)
	}

	writeSSEEvent(w, flusher, "done", "")
}

// escalation returns the turn loop's OnBlocked hook: it raises a visible
// approval for a policy-blocked tool call and fails closed (Denied), so the
// blocked action never runs and the model gets a governed denial to adapt to
// (ADR-0004 §6). Live poll-to-approve-then-resume is future work; this batch
// records the escalation and denies the current turn.
func (s *Server) escalation(ctx context.Context, token, sessionID string) func(action, reason string) agentrun.Decision {
	return func(action, reason string) agentrun.Decision {
		s.raiseApproval(ctx, token, sessionID, action, reason)
		return agentrun.Denied
	}
}

// raiseApproval posts a structure-only approval request, logging (but not
// failing on) an error — escalation is best-effort observability plus a human
// decision, not a governance gate (the gate already blocked the action).
func (s *Server) raiseApproval(ctx context.Context, token, sessionID, action, reason string) {
	_, err := s.client.CreateApproval(ctx, token, sessionID, action, reason, map[string]any{
		"tool":   action,
		"source": "member-console",
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "member-console: raise approval",
			slog.String("session_id", sessionID), slog.String("action", action), slog.Any("error", err))
	}
}

// flushTrajectory posts a turn's structure-only events to the control plane on a
// detached, bounded context. A failure is logged and dropped; ingest is
// idempotent by event_id, and telemetry never blocks a run (ADR-0004 §5).
func (s *Server) flushTrajectory(token, sessionID string, events []trajectoryEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), trajectoryFlushTimeout)
	defer cancel()
	if _, err := s.client.IngestTrajectory(ctx, token, sessionID, events); err != nil {
		s.logger.ErrorContext(ctx, "member-console: ingest trajectory",
			slog.String("session_id", sessionID), slog.Any("error", err))
	}
}

// toStepView maps a turn-loop step to its render model, carrying only structure
// (tool name, arg/result shape and size) plus any model-authored text.
func toStepView(s agentrun.Step) views.StepView {
	v := views.StepView{
		Kind:     s.Kind.String(),
		ToolName: s.ToolName,
		Text:     s.Text,
	}
	if meta := s.ArgsMeta; meta != nil {
		v.HasMeta = true
		v.ValueType = meta.ValueType
		v.ByteSize = meta.ByteSize
	}
	if meta := s.ResultMeta; meta != nil {
		v.HasMeta = true
		v.ValueType = meta.ValueType
		v.ByteSize = meta.ByteSize
	}
	if s.Kind == agentrun.StepEscalated {
		if s.Decision == agentrun.Approved {
			v.Decision = "approved"
		} else {
			v.Decision = "denied"
		}
	}
	return v
}

// setSSEHeaders writes the headers for a text/event-stream response.
func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Ask any reverse proxy not to buffer, so events reach the browser live.
	h.Set("X-Accel-Buffering", "no")
}

// writeSSEEvent writes one named SSE event with a single-line data payload and
// flushes it. data must contain no raw newlines (the fragment renderers ensure
// this); an empty data still yields a valid, if empty, event line.
func writeSSEEvent(w io.Writer, flusher http.Flusher, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	flusher.Flush()
}

// readableEngineError renders a bind failure for the run view. It prefers the
// wrapped error's message but never leaks internals beyond what the member needs
// to act (start the engine, check the bundle).
func readableEngineError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// readableRunError renders a turn failure for the stream. Proxy grant rejections
// and iteration bounds get a friendlier line; everything else surfaces its
// message.
func readableRunError(err error) string {
	switch {
	case errors.Is(err, agentrun.ErrProxyBlocked):
		return "The model call was blocked by the governed proxy; an approval has been raised."
	case errors.Is(err, agentrun.ErrMaxIterations):
		return "The run hit its step limit before finishing. Try a narrower directive."
	default:
		return "The run could not complete: " + err.Error()
	}
}
