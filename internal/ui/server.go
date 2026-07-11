package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/ui/views"
)

// sessionCookieName is the cookie carrying the control-plane session token. It
// is set HttpOnly (never readable from JavaScript) and, in production, Secure;
// the server attaches its value as an Authorization: Bearer header on every
// control-plane call.
const sessionCookieName = "dd_session"

// spendWindow is how far back the dashboard's spend summary looks. It is fixed
// for this bounded shell; a follow-up can make it operator-selectable.
const spendWindow = 30 * 24 * time.Hour

// spendWindowLabel describes the fixed spend window in the UI.
const spendWindowLabel = "(last 30 days)"

// ctxKey is the private context-key type for values this package stashes on the
// request context.
type ctxKey int

const tokenCtxKey ctxKey = iota

// Server is the admin-ui HTTP handler: a chi router serving the login page, the
// dashboard, the governance pages (sessions, spend, approvals), and a health
// check, backed by a control-plane [Client]. Construct it with [New]; it
// implements [http.Handler].
type Server struct {
	client       *Client
	logger       *slog.Logger
	secureCookie bool
	now          func() time.Time
	router       chi.Router
}

// Option customizes a [Server].
type Option func(*Server)

// WithSecureCookie sets the Secure attribute on the session cookie. Enable it
// in production (HTTPS); leave it off for plain-HTTP local development, where a
// Secure cookie would never be sent back.
func WithSecureCookie(secure bool) Option {
	return func(s *Server) { s.secureCookie = secure }
}

// WithClock overrides the wall clock used to compute the spend window. Tests
// use it for determinism; production uses the default [time.Now].
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// New builds a Server that renders pages from live data fetched through client.
// Pass nil for logger to use [slog.Default]. The returned Server is ready to
// serve; its routes are wired once, here.
func New(client *Client, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		client: client,
		logger: logger,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.router = s.routes()
	return s
}

// ServeHTTP implements http.Handler by delegating to the wired router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// routes builds the chi router. Health and the login endpoints are public; the
// dashboard, the governance pages, and the HTMX fragment/action sit behind
// requireAuth, which redirects to /login when the session cookie is absent.
func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/healthz", s.handleHealthz)
	r.Get("/login", s.handleLoginPage)
	r.Post("/login", s.handleLoginSubmit)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleDashboard)
		r.Get("/fragments/summary", s.handleSummaryFragment)
		r.Get("/sessions", s.handleSessions)
		r.Get("/spend", s.handleSpend)
		r.Get("/approvals", s.handleApprovals)
		r.Post("/approvals/{id}/decision", s.handleApprovalDecision)
	})

	return r
}

// handleHealthz reports liveness with a static 200; it does not consult the
// control plane, so a control-plane blip cannot make an orchestrator kill an
// otherwise-healthy admin-ui process.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

// handleLoginPage renders the sign-in form.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, views.Login(views.LoginData{}))
}

// handleLoginSubmit reads the pasted session token from the form, stores it in
// the session cookie, and redirects to the dashboard. An empty token re-renders
// the form with an error and a 400.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.Login(views.LoginData{Error: "Could not read the submitted form."}))
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" {
		s.render(w, r, http.StatusBadRequest, views.Login(views.LoginData{Error: "A session token is required."}))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout clears the session cookie and returns to the login page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleDashboard renders the authenticated landing page. A token the control
// plane rejects (401) clears the cookie and bounces the operator to /login; any
// other whoami failure renders a 502 error page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	id, err := s.client.Whoami(r.Context(), token)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			s.redirectToLogin(w, r)
			return
		}
		s.logger.ErrorContext(r.Context(), "admin-ui: whoami failed", slog.Any("error", err))
		http.Error(w, "control plane unavailable", http.StatusBadGateway)
		return
	}

	summary, err := s.buildSummary(r.Context(), token)
	if err != nil {
		// Only ErrUnauthorized propagates here; section-level failures are
		// embedded in the summary instead.
		s.redirectToLogin(w, r)
		return
	}

	s.render(w, r, http.StatusOK, views.Dashboard(views.DashboardData{
		Identity: views.IdentityView{
			Subject:        id.Subject,
			OrganizationID: id.OrganizationID,
			Roles:          rolesText(id.Roles),
		},
		Summary: summary,
	}))
}

// handleSummaryFragment renders just the live-overview section for HTMX
// polling. On a rejected token it asks HTMX to redirect the whole page to
// /login via the HX-Redirect header (a 302 would otherwise be swapped into the
// fragment).
func (s *Server) handleSummaryFragment(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	summary, err := s.buildSummary(r.Context(), token)
	if err != nil {
		s.hxRedirectToLogin(w)
		return
	}
	s.render(w, r, http.StatusOK, views.Summary(summary))
}

// handleSessions renders the Sessions page: every governed session with its
// fan-out ceiling. A rejected token redirects to /login; any other
// control-plane error is surfaced inline instead of blanking the page.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	var data views.SessionsData
	list, err := s.client.ListSessions(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list sessions failed", slog.Any("error", err))
		data.Error = "Could not load sessions: " + readableError(err)
	default:
		data.Sessions = sessionDetailRows(list.Items)
	}
	s.render(w, r, http.StatusOK, views.Sessions(data))
}

// handleSpend renders the Spend page: total spend and the per-agent breakdown
// over the fixed window, plus budget utilization. The spend and budgets
// sections fail independently so one degraded endpoint does not hide the other.
func (s *Server) handleSpend(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	data := views.SpendPageData{Window: spendWindowLabel, Total: "—"}

	to := s.now().UTC()
	from := to.Add(-spendWindow)
	report, err := s.client.QuerySpend(r.Context(), token, from.Format(time.RFC3339), to.Format(time.RFC3339), "agent")
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: query spend failed", slog.Any("error", err))
		data.SpendError = "Could not load spend: " + readableError(err)
	default:
		data.Total = amountText(report.Total, report.Currency)
		data.Groups = spendRows(report.Groups, report.Currency)
	}

	budgets, err := s.client.ListBudgets(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list budgets failed", slog.Any("error", err))
		data.BudgetsError = "Could not load budgets: " + readableError(err)
	default:
		data.Budgets = budgetRows(budgets.Items)
	}

	s.render(w, r, http.StatusOK, views.Spend(data))
}

// handleApprovals renders the Approvals page: the escalation queue, listing
// pending items with an approve/deny action and decided items read-only.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	var data views.ApprovalsData
	list, err := s.client.ListApprovals(r.Context(), token, "")
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list approvals failed", slog.Any("error", err))
		data.Error = "Could not load approvals: " + readableError(err)
	default:
		data.Approvals = approvalRows(list.Items)
	}
	s.render(w, r, http.StatusOK, views.Approvals(data))
}

// handleApprovalDecision records an approve/deny decision via the control plane
// and re-renders just the affected row for HTMX to swap in place. A rejected
// token asks HTMX to redirect to /login; any other control-plane error (for
// example a 409 on an already-decided request) is surfaced inside the row.
func (s *Server) handleApprovalDecision(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	id := chi.URLParam(r, "id")

	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.ApprovalRowTr(views.ApprovalRow{ID: id, RowError: "Could not read the decision."}))
		return
	}
	decision := strings.TrimSpace(r.PostFormValue("decision"))
	if decision != "approve" && decision != "deny" {
		s.render(w, r, http.StatusBadRequest, views.ApprovalRowTr(views.ApprovalRow{ID: id, RowError: "Invalid decision."}))
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))

	approval, err := s.client.DecideApproval(r.Context(), token, id, decision, reason)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.hxRedirectToLogin(w)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: decide approval failed", slog.Any("error", err))
		s.render(w, r, http.StatusOK, views.ApprovalRowTr(views.ApprovalRow{ID: id, RowError: "Decision failed: " + readableError(err)}))
		return
	}
	s.render(w, r, http.StatusOK, views.ApprovalRowTr(approvalRow(*approval)))
}

// buildSummary fetches the dashboard's live data. Each section fails
// independently: a control-plane error on one endpoint is recorded as that
// section's error message rather than blanking the page. Only a rejected token
// (ErrUnauthorized) is returned as an error, so the caller can redirect.
func (s *Server) buildSummary(ctx context.Context, token string) (views.SummaryData, error) {
	sd := views.SummaryData{SpendWindow: spendWindowLabel}

	agents, err := s.client.ListAgents(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.SummaryData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list agents failed", slog.Any("error", err))
		sd.AgentsError = "Could not load agents."
		sd.AgentCount = "—"
	default:
		sd.AgentCount = countText(len(agents.Items), agents.NextCursor)
		sd.Agents = agentRows(agents.Items)
	}

	sessions, err := s.client.ListSessions(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.SummaryData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list sessions failed", slog.Any("error", err))
		sd.SessionsError = "Could not load sessions."
		sd.SessionCount = "—"
	default:
		sd.SessionCount = countText(len(sessions.Items), sessions.NextCursor)
		sd.Sessions = sessionRows(sessions.Items)
	}

	to := s.now().UTC()
	from := to.Add(-spendWindow)
	report, err := s.client.QuerySpend(ctx, token, from.Format(time.RFC3339), to.Format(time.RFC3339), "agent")
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.SummaryData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: query spend failed", slog.Any("error", err))
		sd.SpendError = "Could not load spend."
		sd.SpendTotal = "—"
	default:
		sd.SpendTotal = amountText(report.Total, report.Currency)
		sd.Spend = spendRows(report.Groups, report.Currency)
	}

	return sd, nil
}

// requireAuth is middleware that admits a request only when the session cookie
// is present and non-empty, stashing the token on the request context.
// Otherwise it redirects to /login.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || strings.TrimSpace(cookie.Value) == "" {
			s.redirectToLogin(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), tokenCtxKey, cookie.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// redirectToLogin clears any stale cookie and sends the client to /login.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// hxRedirectToLogin clears the stale cookie and tells HTMX to navigate the
// whole page to /login. HTMX honors the HX-Redirect response header on a 200,
// whereas a normal 3xx would be swapped into the target element instead.
func (s *Server) hxRedirectToLogin(w http.ResponseWriter) {
	s.clearSessionCookie(w)
	w.Header().Set("HX-Redirect", "/login")
	w.WriteHeader(http.StatusOK)
}

// clearSessionCookie expires the session cookie on the client.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// render writes c as an HTML response. It renders into a buffer first so a
// mid-render failure yields a clean 500 instead of a half-written body under a
// success status.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: render failed", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// tokenFromContext returns the session token stashed by requireAuth, or "".
func tokenFromContext(ctx context.Context) string {
	tok, _ := ctx.Value(tokenCtxKey).(string)
	return tok
}

// --- view-model mapping helpers ---

// rolesText joins roles for display, rendering an empty list as an em dash.
func rolesText(roles []string) string {
	if len(roles) == 0 {
		return "—"
	}
	return strings.Join(roles, ", ")
}

// countText renders a page size, suffixing "+" when a next cursor indicates
// more items exist beyond the first page.
func countText(n int, nextCursor string) string {
	s := strconv.Itoa(n)
	if nextCursor != "" {
		s += "+"
	}
	return s
}

// amountText formats a monetary amount with its currency.
func amountText(amount float64, currency string) string {
	if currency == "" {
		return strconv.FormatFloat(amount, 'f', 2, 64)
	}
	return strconv.FormatFloat(amount, 'f', 2, 64) + " " + currency
}

// utilizationText renders spent/limit as a percentage, or an em dash when the
// limit is non-positive (and a ratio would be meaningless).
func utilizationText(spent, limit float64) string {
	if limit <= 0 {
		return "—"
	}
	return strconv.FormatFloat(spent/limit*100, 'f', 1, 64) + "%"
}

// readableError renders a control-plane error for display. It prefers the flat
// Error envelope's message when present and falls back to a generic line, so a
// page or row never surfaces a raw Go error or blanks out.
func readableError(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message
	}
	return "control plane unavailable"
}

// agentRows maps catalog agents to display rows.
func agentRows(agents []Agent) []views.AgentRow {
	rows := make([]views.AgentRow, 0, len(agents))
	for _, a := range agents {
		ver := "—"
		if a.LatestVersion != nil {
			ver = strconv.Itoa(*a.LatestVersion)
		}
		rows = append(rows, views.AgentRow{
			Name:        a.Name,
			Version:     ver,
			Description: a.Description,
		})
	}
	return rows
}

// sessionRows maps governed sessions to the compact dashboard-summary rows.
func sessionRows(sessions []Session) []views.SessionRow {
	rows := make([]views.SessionRow, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, views.SessionRow{
			ID:      s.SessionID,
			Agent:   s.AgentID,
			Status:  s.Status,
			Created: s.CreatedAt,
		})
	}
	return rows
}

// sessionDetailRows maps governed sessions to the full Sessions-page rows,
// which additionally carry the fan-out ceiling.
func sessionDetailRows(sessions []Session) []views.SessionDetailRow {
	rows := make([]views.SessionDetailRow, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, views.SessionDetailRow{
			ID:      s.SessionID,
			Agent:   s.AgentID,
			Status:  s.Status,
			Created: s.CreatedAt,
			FanOut:  strconv.Itoa(s.FanOutCeiling),
		})
	}
	return rows
}

// spendRows maps spend groups to display rows.
func spendRows(groups []SpendGroup, currency string) []views.SpendRow {
	rows := make([]views.SpendRow, 0, len(groups))
	for _, g := range groups {
		tokens := "—"
		if g.Tokens != nil {
			tokens = strconv.FormatInt(*g.Tokens, 10)
		}
		rows = append(rows, views.SpendRow{
			Key:    g.Key,
			Amount: amountText(g.Amount, currency),
			Tokens: tokens,
		})
	}
	return rows
}

// budgetRows maps budgets to display rows, computing utilization and flagging
// any budget whose spend has reached or exceeded its limit.
func budgetRows(budgets []Budget) []views.BudgetRow {
	rows := make([]views.BudgetRow, 0, len(budgets))
	for _, b := range budgets {
		rows = append(rows, views.BudgetRow{
			Name:        b.Name,
			Period:      b.Period,
			Spent:       amountText(b.Spent, b.Currency),
			Limit:       amountText(b.Limit, b.Currency),
			Utilization: utilizationText(b.Spent, b.Limit),
			Over:        b.Limit > 0 && b.Spent >= b.Limit,
		})
	}
	return rows
}

// approvalRows maps escalation-queue items to display rows.
func approvalRows(approvals []Approval) []views.ApprovalRow {
	rows := make([]views.ApprovalRow, 0, len(approvals))
	for _, a := range approvals {
		rows = append(rows, approvalRow(a))
	}
	return rows
}

// approvalRow maps a single approval to a display row. A "pending" status is
// the only state that offers the approve/deny action.
func approvalRow(a Approval) views.ApprovalRow {
	return views.ApprovalRow{
		ID:        a.ID,
		Session:   a.SessionID,
		Action:    a.Action,
		Reason:    a.Reason,
		Status:    a.Status,
		DecidedBy: a.DecidedBy,
		Created:   a.CreatedAt,
		Pending:   a.Status == "pending",
	}
}
