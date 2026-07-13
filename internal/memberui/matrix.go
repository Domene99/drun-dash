package memberui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Domene99/drun-dash/internal/agentrun"
	"github.com/Domene99/drun-dash/internal/matrix"
	"github.com/Domene99/drun-dash/internal/memberui/views"
)

// This file wires the parallel matrix engine (internal/matrix) into the member
// console (design §6.3 / issue B20 #4): the member enters a directive and a
// scorer, selects N of their entitled agents, and the console fans the run
// across them — each branch its own governed session. It composes internal/matrix
// by implementing its ControlPlane (launch/terminate over the memberui client)
// and its Runner (execute a branch's directive/scorer as a governed agentrun
// turn through that branch's proxy grant), mirroring how internal/cli/matrix.go
// adapts the same two seams for the CLI. The orchestration — launch, run, score,
// rank, terminate-all — stays in internal/matrix.

// handleMatrixView renders the matrix form: the entitled agents to fan across,
// plus the directive and scorer inputs. Results post back into #matrix-results.
func (s *Server) handleMatrixView(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	data := views.MatrixData{}
	list, err := s.client.GetEntitledAgents(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "member-console: matrix get agents", slog.Any("error", err))
		data.AgentsError = "Could not load agents: " + readableError(err)
	default:
		data.Agents = matrixAgentOptions(list.Items)
	}
	s.render(w, r, http.StatusOK, views.MatrixPage(data))
}

// handleMatrixRun fans the submitted directive across the selected entitled
// agents via internal/matrix, then renders the ranked result. Every launched
// session is terminated at the end (matrix.Run with KeepWinner off).
func (s *Server) handleMatrixRun(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.MatrixResults(views.MatrixResultData{Error: "Could not read the submitted form."}))
		return
	}
	directive := strings.TrimSpace(r.PostFormValue("directive"))
	scorer := strings.TrimSpace(r.PostFormValue("scorer"))
	agents := cleanAgentIDs(r.PostForm["agent"])

	switch {
	case s.model == "":
		s.render(w, r, http.StatusOK, views.MatrixResults(views.MatrixResultData{Error: "No model is configured (set MEMBER_CONSOLE_MODEL)."}))
		return
	case directive == "" || scorer == "" || len(agents) == 0:
		s.render(w, r, http.StatusBadRequest, views.MatrixResults(views.MatrixResultData{Error: "Enter a directive, a scorer, and select at least one agent."}))
		return
	}

	cm := &consoleMatrix{s: s, token: token, engines: make(map[string]*boundEngine)}
	res, runErr := matrix.Run(r.Context(), matrix.Config{
		Agents:    agents,
		RunnerCmd: directive,
		ScoreCmd:  scorer,
		Control:   cm,
		Exec:      cm,
	})
	if runErr != nil && res == nil {
		s.logger.ErrorContext(r.Context(), "member-console: matrix run", slog.Any("error", runErr))
	}
	s.render(w, r, http.StatusOK, views.MatrixResults(matrixResultData(res, runErr)))
}

// consoleMatrix adapts the memberui control-plane client and a per-branch
// governed agentrun turn to internal/matrix's ControlPlane and Runner seams. It
// launches one governed session per branch (verifying the bundle and binding a
// local drun engine), runs the directive/scorer through each branch's proxy
// grant, and closes every branch's drun engine on terminate.
type consoleMatrix struct {
	s     *Server
	token string

	mu      sync.Mutex
	engines map[string]*boundEngine // control-plane session id -> bound drun engine
}

// Launch launches one governed session for agentID, verifies its policy bundle
// fail-closed, and binds a local drun engine for the branch. A verify/bind
// failure terminates the just-launched session so no grant leaks, and fails only
// this branch.
func (m *consoleMatrix) Launch(ctx context.Context, agentID string) (matrix.Branch, error) {
	launch, err := m.s.client.LaunchSession(ctx, m.token, agentID)
	if err != nil {
		return matrix.Branch{}, err
	}
	if err := m.s.verify(launch.PolicyBundle, m.s.now()); err != nil {
		_, _ = m.s.client.TerminateSession(ctx, m.token, launch.SessionID)
		return matrix.Branch{}, fmt.Errorf("policy bundle rejected: %w", err)
	}
	eng, err := bindEngine(ctx, m.s.dial, m.s.drunEndpoint, launch.PolicyBundle)
	if err != nil {
		_, _ = m.s.client.TerminateSession(ctx, m.token, launch.SessionID)
		return matrix.Branch{}, err
	}
	m.mu.Lock()
	m.engines[launch.SessionID] = eng
	m.mu.Unlock()
	return matrix.Branch{
		SessionID:     launch.SessionID,
		ProxyBaseURL:  launch.ProxyBaseURL,
		ProxyToken:    launch.ProxyToken,
		FanOutCeiling: launch.FanOutCeiling,
	}, nil
}

// Run executes command (the directive, then the scorer) for one branch as a
// governed agentrun turn through that branch's proxy grant, returning the
// model's final answer. The scorer branch is expected to print a numeric score;
// matrix parses it. A policy-blocked tool is recorded as an approval and denied
// (there is no live per-branch approver), so nothing blocked runs un-approved.
func (m *consoleMatrix) Run(ctx context.Context, command string, env map[string]string) (string, error) {
	sessionID := env["DRUN_SESSION_ID"]
	m.mu.Lock()
	eng := m.engines[sessionID]
	m.mu.Unlock()
	if eng == nil {
		return "", fmt.Errorf("memberui: matrix branch has no bound engine for session %q", sessionID)
	}

	res, err := agentrun.Run(ctx, agentrun.Config{
		ProxyBaseURL: env["DRUN_PROXY_BASE_URL"],
		ProxyToken:   env["DRUN_PROXY_TOKEN"],
		Model:        m.s.model,
		Tools:        eng.tools,
		Executor:     eng.executor,
		HTTPClient:   m.s.proxyHTTP,
		OnBlocked: func(action, reason string) agentrun.Decision {
			m.s.raiseApproval(ctx, m.token, sessionID, action, reason)
			return agentrun.Denied
		},
	}, command)
	if err != nil {
		return "", err
	}
	return res.Final, nil
}

// Terminate closes the branch's bound drun engine and terminates its governed
// session, revoking the grant so nothing outlives the run.
func (m *consoleMatrix) Terminate(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	eng := m.engines[sessionID]
	delete(m.engines, sessionID)
	m.mu.Unlock()
	if eng != nil {
		eng.close(ctx)
	}
	_, err := m.s.client.TerminateSession(ctx, m.token, sessionID)
	return err
}

// cleanAgentIDs trims, drops empties, and de-duplicates the selected agent ids
// (the matrix checkboxes), preserving selection order.
func cleanAgentIDs(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// matrixAgentOptions maps entitled agents to the matrix form's selectable
// options.
func matrixAgentOptions(agents []Agent) []views.MatrixAgentOption {
	opts := make([]views.MatrixAgentOption, 0, len(agents))
	for _, a := range agents {
		ver := ""
		if a.LatestVersion != nil {
			ver = strconv.Itoa(*a.LatestVersion)
		}
		name := a.Name
		if strings.TrimSpace(name) == "" {
			name = a.ID
		}
		opts = append(opts, views.MatrixAgentOption{ID: a.ID, Name: name, Version: ver})
	}
	return opts
}

// matrixResultData maps a matrix.Result (and any run error) to the ranked-result
// view model. ErrAllBranchesFailed is an expected "no winner" outcome, not a
// surfaced error; a nil result means matrix rejected the config outright.
func matrixResultData(res *matrix.Result, runErr error) views.MatrixResultData {
	if res == nil {
		msg := "The matrix run could not start."
		if runErr != nil {
			msg = runErr.Error()
		}
		return views.MatrixResultData{Error: msg}
	}

	data := views.MatrixResultData{AllTerminated: true}
	for i, r := range res.Ranked {
		if r.SessionID != "" && !r.Terminated {
			data.AllTerminated = false
		}
		data.Rows = append(data.Rows, views.MatrixRow{
			Rank:    strconv.Itoa(i + 1),
			Agent:   r.Agent,
			Session: r.SessionID,
			Score:   matrixScoreCell(r),
			Status:  r.Status,
			Detail:  matrixDetail(r),
		})
	}
	if res.Winner != nil {
		data.Winner = res.Winner.Agent
	}
	if runErr != nil && !errors.Is(runErr, matrix.ErrAllBranchesFailed) {
		data.Error = runErr.Error()
	}
	return data
}

// matrixScoreCell renders a branch's score, or empty when it produced none.
func matrixScoreCell(r matrix.BranchResult) string {
	if !r.Scored {
		return ""
	}
	return strconv.FormatFloat(r.Score, 'g', -1, 64)
}

// matrixDetail summarizes a branch's failure/termination detail for display.
func matrixDetail(r matrix.BranchResult) string {
	var parts []string
	if r.Err != nil {
		parts = append(parts, r.Err.Error())
	}
	if r.TermErr != nil {
		parts = append(parts, "terminate failed: "+r.TermErr.Error())
	}
	return strings.Join(parts, "; ")
}
