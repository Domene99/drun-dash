package ui

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/ui/views"
	"github.com/Domene99/drun-dash/internal/uikit"
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

// recentSessionsLimit caps how many sessions the Overview recent-activity list
// shows; the full list lives on the Sessions page.
const recentSessionsLimit = 8

// ctxKey is the private context-key type for values this package stashes on the
// request context.
type ctxKey int

const tokenCtxKey ctxKey = iota

// Server is the admin-ui HTTP handler: a chi router serving the login and OIDC
// callback endpoints, the dashboard, the governance pages (sessions, spend,
// approvals), the CLI device-approval page, the shared uikit assets, and a
// health check, backed by a control-plane [Client]. Construct it with [New]; it
// implements [http.Handler].
type Server struct {
	client       *Client
	logger       *slog.Logger
	oidc         *OIDCAuthenticator
	secureCookie bool
	environment  string
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

// WithEnvironment sets the environment indicator shown in the top bar (e.g.
// "production"). An empty value hides the indicator.
func WithEnvironment(env string) Option {
	return func(s *Server) { s.environment = env }
}

// WithOIDC enables the OIDC authorization-code browser login. When set, GET
// /login redirects to the identity provider and GET /callback completes the
// exchange; when nil, the server keeps the paste-a-token dev login.
func WithOIDC(a *OIDCAuthenticator) Option {
	return func(s *Server) { s.oidc = a }
}

// WithClock overrides the wall clock used to compute the spend window and
// relative times. Tests use it for determinism; production uses [time.Now].
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

// routes builds the chi router. Health, the shared uikit assets, the login
// endpoints, and the OIDC callback are public; the dashboard, the governance
// pages, the device-approval page, and the HTMX fragment/action sit behind
// requireAuth, which redirects to /login when the session cookie is absent.
func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/healthz", s.handleHealthz)
	// The shared design-system stylesheet and theme script (embedded in uikit)
	// are served under uikit.MountPrefix and linked by uikit.Head.
	r.Handle(uikit.MountPrefix+"*", uikit.Handler())
	r.Get("/login", s.handleLogin)
	r.Post("/login", s.handleLoginSubmit)
	r.Post("/login/token", s.handleTokenLogin)
	r.Get("/login/sso", s.handleSSOStart)
	r.Get("/signup", s.handleSignup)
	r.Post("/signup", s.handleSignupSubmit)
	r.Get("/accept-invite", s.handleAcceptInvite)
	r.Post("/accept-invite", s.handleAcceptInviteSubmit)
	r.Get("/callback", s.handleCallback)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleDashboard)
		r.Get("/fragments/summary", s.handleSummaryFragment)
		r.Get("/sessions", s.handleSessions)
		r.Get("/sessions/{sessionId}/trajectory", s.handleTrajectory)
		r.Get("/spend", s.handleSpend)
		r.Get("/approvals", s.handleApprovals)
		r.Post("/approvals/{id}/decision", s.handleApprovalDecision)
		r.Get("/device", s.handleDevicePage)
		r.Post("/device", s.handleDeviceSubmit)

		// RBAC governance pages (ADR-0006 §7) and the read-only catalog detail
		// views. Management POSTs sit behind the same auth-cookie middleware;
		// the control plane still enforces the per-endpoint permission, this is
		// the UX layer that also hides actions the caller cannot perform.
		r.Get("/roles", s.handleRoles)
		r.Post("/roles", s.handleRoleCreate)
		r.Post("/roles/{roleId}/update", s.handleRoleUpdate)
		r.Post("/roles/{roleId}/delete", s.handleRoleDelete)
		r.Get("/members", s.handleMembers)
		r.Post("/members/invite", s.handleMemberInvite)
		r.Post("/members/{memberId}/roles", s.handleMemberRoles)
		r.Get("/teams", s.handleTeams)
		r.Get("/agents", s.handleAgents)
		r.Get("/agents/new", s.handleAgentNew)
		r.Post("/agents", s.handleAgentCreate)
		r.Get("/agents/{agentId}", s.handleAgentDetail)
		r.Get("/policies", s.handlePolicies)
		r.Get("/policies/{policyId}", s.handlePolicyDetail)
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

// handleLogin renders the sign-in page. The primary form is the local password
// login (org slug + email + password); a "Sign in with SSO" button (when OIDC is
// configured) and a collapsed paste-a-token dev section are offered alongside it.
// Unlike before, it never auto-redirects to the identity provider — the SSO flow
// starts from handleSSOStart via the button.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, views.Login(views.LoginData{OIDC: s.oidc != nil}))
}

// handleSSOStart begins the OIDC authorization-code browser flow: it mints a
// state token and PKCE verifier, stores them in transient cookies, and redirects
// the browser to the identity provider's authorize URL. The login page's "Sign
// in with SSO" button links here (shown only when OIDC is configured); without
// OIDC it returns to the login page.
func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		s.redirectToLogin(w, r)
		return
	}
	authURL, state, verifier, err := s.oidc.StartLogin()
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: start oidc login", slog.Any("error", err))
		s.renderLoginError(w, r, "Could not start sign-in. Please try again.")
		return
	}
	s.setTransientCookie(w, oidcStateCookie, state)
	s.setTransientCookie(w, oidcVerifierCookie, verifier)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleLoginSubmit is the primary local sign-in: it reads the org slug, email,
// and password from the form, exchanges them for a session token via POST
// /auth/login, stores the token in the session cookie, and redirects to the
// dashboard. Invalid credentials (the generic 401) and missing fields re-render
// the form with a readable message, preserving the submitted org slug and email.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.Login(views.LoginData{OIDC: s.oidc != nil, Error: "Could not read the submitted form."}))
		return
	}
	orgSlug := strings.TrimSpace(r.PostFormValue("org_slug"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	if orgSlug == "" || email == "" || password == "" {
		s.renderLoginFormError(w, r, http.StatusBadRequest, orgSlug, email, "Enter your organization slug, email, and password.")
		return
	}

	token, err := s.client.Login(r.Context(), orgSlug, email, password)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.renderLoginFormError(w, r, http.StatusUnauthorized, orgSlug, email, "Invalid organization, email, or password.")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: password login failed", slog.Any("error", err))
		s.renderLoginFormError(w, r, http.StatusBadGateway, orgSlug, email, "Could not sign in: "+readableError(err))
		return
	}

	s.setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderLoginFormError re-renders the login page with a message after a failed
// password sign-in, preserving the submitted org slug and email and reflecting
// whether the SSO button should be shown.
func (s *Server) renderLoginFormError(w http.ResponseWriter, r *http.Request, status int, orgSlug, email, msg string) {
	s.render(w, r, status, views.Login(views.LoginData{
		OIDC:    s.oidc != nil,
		OrgSlug: orgSlug,
		Email:   email,
		Error:   msg,
	}))
}

// handleTokenLogin is the paste-a-token dev fallback (the collapsed "Developer
// sign-in" section): it reads a control-plane session token from the form,
// stores it in the session cookie, and redirects to the dashboard. An empty
// token re-renders the login page with an error and a 400. It stays available so
// local development works without an identity provider.
func (s *Server) handleTokenLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.Login(views.LoginData{OIDC: s.oidc != nil, Error: "Could not read the submitted form."}))
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" {
		s.render(w, r, http.StatusBadRequest, views.Login(views.LoginData{OIDC: s.oidc != nil, Error: "A session token is required."}))
		return
	}

	s.setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleCallback completes the OIDC authorization-code flow: it validates the
// CSRF state against the transient cookie, exchanges the code (with the PKCE
// verifier) for the ID token, hands the ID token to the control plane's
// /auth/session for a session JWT, stores that JWT in the session cookie, and
// lands on the dashboard. Any failure clears the transient cookies and renders
// the login page with a readable message.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		s.redirectToLogin(w, r)
		return
	}

	q := r.URL.Query()

	// The transient cookies are single-use; clear them regardless of outcome.
	stateCookie, stateErr := r.Cookie(oidcStateCookie)
	verifierCookie, verifierErr := r.Cookie(oidcVerifierCookie)
	s.clearTransientCookie(w, oidcStateCookie)
	s.clearTransientCookie(w, oidcVerifierCookie)

	if errCode := q.Get("error"); errCode != "" {
		s.renderLoginError(w, r, "The identity provider reported an error: "+errCode)
		return
	}

	state := q.Get("state")
	if stateErr != nil || state == "" || stateCookie.Value == "" ||
		subtle.ConstantTimeCompare([]byte(state), []byte(stateCookie.Value)) != 1 {
		s.renderLoginError(w, r, "Sign-in could not be verified (state mismatch). Please try again.")
		return
	}
	if verifierErr != nil || verifierCookie.Value == "" {
		s.renderLoginError(w, r, "Sign-in could not be verified (missing verifier). Please try again.")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.renderLoginError(w, r, "The identity provider did not return an authorization code.")
		return
	}

	idToken, err := s.oidc.Exchange(r.Context(), code, verifierCookie.Value)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: oidc code exchange failed", slog.Any("error", err))
		s.renderLoginError(w, r, "Could not complete sign-in with the identity provider.")
		return
	}

	sessionJWT, err := s.client.ExchangeSession(r.Context(), idToken, s.oidc.orgID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: session exchange failed", slog.Any("error", err))
		s.renderLoginError(w, r, "The control plane rejected the sign-in: "+readableError(err))
		return
	}

	s.setSessionCookie(w, sessionJWT)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout clears the session cookie and returns to the login page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleDevicePage renders the CLI device-approval confirmation page. It shows
// the user_code from the query so the operator can confirm it matches the one
// in their terminal before approving. It sits behind requireAuth so only a
// signed-in member can approve a device. A missing code renders an inline
// message instead of an actionable form.
func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	userCode := strings.TrimSpace(r.URL.Query().Get("user_code"))
	data := views.DeviceData{UserCode: userCode}
	if userCode == "" {
		data.Error = "Missing device code. Return to your terminal and start the login again."
	}
	s.render(w, r, http.StatusOK, views.Device(data))
}

// handleDeviceSubmit records the operator's approve/deny decision for a CLI
// device-authorization request via the control plane's /auth/device/approve,
// then renders a confirmation. A rejected session token redirects to /login;
// any other control-plane error is surfaced on the page.
func (s *Server) handleDeviceSubmit(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.Device(views.DeviceData{Error: "Could not read the submitted form."}))
		return
	}
	userCode := strings.TrimSpace(r.PostFormValue("user_code"))
	decision := strings.TrimSpace(r.PostFormValue("decision"))
	if userCode == "" {
		s.render(w, r, http.StatusBadRequest, views.Device(views.DeviceData{Error: "Missing device code."}))
		return
	}
	if decision != "approve" && decision != "deny" {
		s.render(w, r, http.StatusBadRequest, views.Device(views.DeviceData{UserCode: userCode, Error: "Invalid decision."}))
		return
	}

	if err := s.client.ApproveDevice(r.Context(), token, userCode, decision); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			s.redirectToLogin(w, r)
			return
		}
		s.logger.ErrorContext(r.Context(), "admin-ui: device approve failed", slog.Any("error", err))
		s.render(w, r, http.StatusOK, views.Device(views.DeviceData{
			UserCode: userCode,
			Error:    "Could not record the decision: " + readableError(err),
		}))
		return
	}

	s.render(w, r, http.StatusOK, views.Device(views.DeviceData{
		UserCode: userCode,
		Done:     true,
		Approved: decision == "approve",
	}))
}

// handleDashboard renders the authenticated Overview page. It builds the shell
// chrome (identity for the top-bar user menu) and the live summary. A token the
// control plane rejects (401) clears the cookie and bounces to /login.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	chrome, _, err := s.buildChrome(r.Context(), token, "overview", "Overview")
	if err != nil {
		s.redirectToLogin(w, r)
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
		Chrome:  chrome,
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

// handleSessions renders the Sessions page: every governed session by agent
// name, status badge, relative start, and fan-out ceiling (the session UUID is
// a secondary ID chip). A rejected token redirects to /login; any other
// control-plane error is surfaced inline instead of blanking the page.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	chrome, _, err := s.buildChrome(r.Context(), token, "sessions", "Sessions")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}

	data := views.SessionsData{Chrome: chrome}
	names := s.agentNames(r.Context(), token)
	list, err := s.client.ListSessions(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list sessions failed", slog.Any("error", err))
		data.Error = "Could not load sessions: " + readableError(err)
	default:
		data.Sessions = sessionDetailRows(list.Items, names, s.now())
	}
	s.render(w, r, http.StatusOK, views.Sessions(data))
}

// handleTrajectory renders the forensic trajectory replay viewer: the session's
// captured decision graph (its fork/rollback event tree) reconstructed from the
// flat event list and rendered depth-first with indentation, under a readable
// heading (agent name when resolvable). A rejected token redirects to /login;
// any other control-plane error (for example a 404 for an unknown session) is
// surfaced inline rather than blanking the page.
func (s *Server) handleTrajectory(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	sessionID := chi.URLParam(r, "sessionId")

	chrome, _, err := s.buildChrome(r.Context(), token, "sessions", "Trajectory")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}

	data := views.TrajectoryData{
		Chrome:    chrome,
		SessionID: sessionID,
		Heading:   s.trajectoryHeading(r.Context(), token, sessionID),
	}
	list, err := s.client.GetSessionTrajectory(r.Context(), token, sessionID)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: get trajectory failed", slog.Any("error", err))
		data.Error = "Could not load trajectory: " + readableError(err)
	default:
		data.Rows = trajectoryRows(list.Items, s.now())
	}
	s.render(w, r, http.StatusOK, views.Trajectory(data))
}

// handleSpend renders the Spend page: total spend and the per-agent breakdown
// over the fixed window, plus budget utilization. The spend and budgets
// sections fail independently so one degraded endpoint does not hide the other.
func (s *Server) handleSpend(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	chrome, _, err := s.buildChrome(r.Context(), token, "spend", "Spend")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}

	data := views.SpendPageData{Chrome: chrome, Window: spendWindowLabel, Total: "—"}

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

	chrome, _, err := s.buildChrome(r.Context(), token, "approvals", "Approvals")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}

	data := views.ApprovalsData{Chrome: chrome}
	list, err := s.client.ListApprovals(r.Context(), token, "")
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list approvals failed", slog.Any("error", err))
		data.Error = "Could not load approvals: " + readableError(err)
	default:
		data.Approvals = approvalRows(list.Items, s.now())
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
	s.render(w, r, http.StatusOK, views.ApprovalRowTr(approvalRow(*approval, s.now())))
}

// buildChrome assembles the shell chrome for an authenticated page: the active
// nav key, the environment indicator, and the signed-in identity (for the
// top-bar user menu) from GET /auth/whoami. It also returns the caller's
// effective permission set so a handler can show or hide management actions. A
// rejected token returns ErrUnauthorized so the caller can redirect; any other
// whoami failure is logged and the page still renders (with an empty identity
// and no permissions) rather than blanking on a control-plane blip.
func (s *Server) buildChrome(ctx context.Context, token, active, title string) (views.Chrome, []string, error) {
	chrome := views.Chrome{Title: title, Active: active, Environment: s.environment}
	id, err := s.client.Whoami(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.Chrome{}, nil, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: whoami failed", slog.Any("error", err))
	default:
		chrome.Identity = views.IdentityView{
			Subject:        id.Subject,
			OrganizationID: id.OrganizationID,
			Email:          id.Email,
			Roles:          rolesText(id.Roles),
		}
		return chrome, id.Permissions, nil
	}
	return chrome, nil, nil
}

// buildSummary fetches the Overview's live data: the metric figures (active
// sessions, pending approvals, spend this period, agents) and a recent-sessions
// list with readable agent names. Each section fails independently — a metric
// that cannot be loaded degrades to "—" and the recent-sessions failure is
// surfaced inline. Only a rejected token (ErrUnauthorized) is returned as an
// error, so the caller can redirect.
func (s *Server) buildSummary(ctx context.Context, token string) (views.SummaryData, error) {
	now := s.now()
	sd := views.SummaryData{
		SpendWindow:      spendWindowLabel,
		ActiveSessions:   "—",
		PendingApprovals: "—",
		SpendTotal:       "—",
		AgentCount:       "—",
	}

	names := map[string]string{}
	agents, err := s.client.ListAgents(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.SummaryData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list agents failed", slog.Any("error", err))
	default:
		names = agentNameMap(agents.Items)
		sd.AgentCount = countText(len(agents.Items), agents.NextCursor)
	}

	sessions, err := s.client.ListSessions(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.SummaryData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list sessions failed", slog.Any("error", err))
		sd.SessionsError = "Could not load sessions."
	default:
		sd.ActiveSessions = strconv.Itoa(activeSessionCount(sessions.Items))
		sd.RecentSessions = recentSessionRows(sessions.Items, names, now)
	}

	to := now.UTC()
	from := to.Add(-spendWindow)
	report, err := s.client.QuerySpend(ctx, token, from.Format(time.RFC3339), to.Format(time.RFC3339), "agent")
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.SummaryData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: query spend failed", slog.Any("error", err))
	default:
		sd.SpendTotal = amountText(report.Total, report.Currency)
	}

	approvals, err := s.client.ListApprovals(ctx, token, "pending")
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.SummaryData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list approvals failed", slog.Any("error", err))
	default:
		sd.PendingApprovals = countText(len(approvals.Items), approvals.NextCursor)
	}

	return sd, nil
}

// agentNames fetches the agent catalog and returns an agent-id -> name map for
// resolving readable session labels. It is best-effort: a control-plane error
// yields an empty map (rows then fall back to a short id) rather than failing
// the whole page. A rejected token surfaces on the caller's own data fetch.
func (s *Server) agentNames(ctx context.Context, token string) map[string]string {
	agents, err := s.client.ListAgents(ctx, token)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			s.logger.ErrorContext(ctx, "admin-ui: list agents for names failed", slog.Any("error", err))
		}
		return map[string]string{}
	}
	return agentNameMap(agents.Items)
}

// trajectoryHeading resolves a readable heading for the trajectory viewer by
// looking up the session's agent name. It is best-effort: any failure (or an
// unknown session) falls back to the plain "Trajectory" heading.
func (s *Server) trajectoryHeading(ctx context.Context, token, sessionID string) string {
	const fallback = "Trajectory"
	sessions, err := s.client.ListSessions(ctx, token)
	if err != nil {
		return fallback
	}
	agentID := ""
	for _, sess := range sessions.Items {
		if sess.SessionID == sessionID {
			agentID = sess.AgentID
			break
		}
	}
	if agentID == "" {
		return fallback
	}
	label := agentLabel(s.agentNames(ctx, token), agentID)
	if label == "" || label == "—" {
		return fallback
	}
	return fallback + " · " + label
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

// renderLoginError renders the login page with a message. It reflects the OIDC
// mode so the page offers the identity-provider button (rather than the dev
// form) when the browser flow is configured.
func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, msg string) {
	s.render(w, r, http.StatusOK, views.Login(views.LoginData{OIDC: s.oidc != nil, Error: msg}))
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

// setSessionCookie stores token in the HttpOnly session cookie.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
	})
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

// setTransientCookie stores a short-lived, HttpOnly value used only across the
// IdP redirect (the CSRF state or the PKCE verifier). SameSite=Lax lets it ride
// the top-level GET navigation back from the identity provider.
func (s *Server) setTransientCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oidcLoginTTL / time.Second),
	})
}

// clearTransientCookie expires a transient login cookie on the client.
func (s *Server) clearTransientCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
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

// agentNameMap builds an agent-id -> name lookup from the catalog page.
func agentNameMap(agents []Agent) map[string]string {
	m := make(map[string]string, len(agents))
	for _, a := range agents {
		m[a.ID] = a.Name
	}
	return m
}

// agentLabel resolves an agent id to its readable name, degrading gracefully to
// a short id (never the full raw UUID as a headline) when the name is unknown.
func agentLabel(names map[string]string, id string) string {
	if n := strings.TrimSpace(names[id]); n != "" {
		return n
	}
	if id == "" {
		return "—"
	}
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// activeSessionCount counts sessions that have not yet terminated.
func activeSessionCount(sessions []Session) int {
	n := 0
	for _, sess := range sessions {
		if sess.TerminatedAt == "" {
			n++
		}
	}
	return n
}

// relativeTime converts an RFC-3339 timestamp into a [views.TimeView] pairing a
// human relative string with the absolute stamp (kept for the hover title).
// An empty stamp renders an em dash; an unparseable one is shown verbatim.
func relativeTime(now time.Time, ts string) views.TimeView {
	if ts == "" {
		return views.TimeView{Relative: "—"}
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return views.TimeView{Relative: ts, Absolute: ts}
	}
	return views.TimeView{Relative: humanizeSince(now, t), Absolute: ts}
}

// humanizeSince renders how long ago t was relative to now, at minute/hour/day/
// month granularity. A future timestamp (clock skew) renders "just now".
func humanizeSince(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	default:
		return plural(int(d.Hours()/(24*30)), "month")
	}
}

// plural renders "<n> <unit>[s] ago", using at least 1 so a sub-unit rounding to
// zero still reads sensibly.
func plural(n int, unit string) string {
	if n < 1 {
		n = 1
	}
	s := strconv.Itoa(n) + " " + unit
	if n != 1 {
		s += "s"
	}
	return s + " ago"
}

// recentSessionRows maps the first few governed sessions to compact Overview
// rows with readable agent names and relative start times.
func recentSessionRows(sessions []Session, names map[string]string, now time.Time) []views.SessionRow {
	if len(sessions) > recentSessionsLimit {
		sessions = sessions[:recentSessionsLimit]
	}
	rows := make([]views.SessionRow, 0, len(sessions))
	for _, sess := range sessions {
		rows = append(rows, views.SessionRow{
			ID:      sess.SessionID,
			Agent:   agentLabel(names, sess.AgentID),
			Status:  sess.Status,
			Started: relativeTime(now, sess.CreatedAt),
		})
	}
	return rows
}

// sessionDetailRows maps governed sessions to the full Sessions-page rows:
// readable agent name, status, relative start, fan-out ceiling, and the session
// id (rendered as a secondary ID chip by the view).
func sessionDetailRows(sessions []Session, names map[string]string, now time.Time) []views.SessionDetailRow {
	rows := make([]views.SessionDetailRow, 0, len(sessions))
	for _, sess := range sessions {
		rows = append(rows, views.SessionDetailRow{
			ID:      sess.SessionID,
			Agent:   agentLabel(names, sess.AgentID),
			Status:  sess.Status,
			Started: relativeTime(now, sess.CreatedAt),
			FanOut:  strconv.Itoa(sess.FanOutCeiling),
		})
	}
	return rows
}

// trajectoryRows reconstructs the event tree from the flat list and flattens it
// depth-first into display rows carrying an indentation string. Roots are events
// with no parent (or a parent absent from the page); siblings are ordered by
// occurrence time (then event id for stability). A visited set guards against a
// malformed list whose parent links form a cycle.
func trajectoryRows(events []TrajectoryEvent, now time.Time) []views.TrajectoryRow {
	known := make(map[string]bool, len(events))
	for _, e := range events {
		known[e.EventID] = true
	}

	children := make(map[string][]TrajectoryEvent)
	var roots []TrajectoryEvent
	for _, e := range events {
		if e.ParentEventID == "" || !known[e.ParentEventID] {
			roots = append(roots, e)
			continue
		}
		children[e.ParentEventID] = append(children[e.ParentEventID], e)
	}

	byOccurrence := func(es []TrajectoryEvent) {
		sort.SliceStable(es, func(i, j int) bool {
			if es[i].OccurredAt != es[j].OccurredAt {
				return es[i].OccurredAt < es[j].OccurredAt
			}
			return es[i].EventID < es[j].EventID
		})
	}
	byOccurrence(roots)
	for k := range children {
		byOccurrence(children[k])
	}

	rows := make([]views.TrajectoryRow, 0, len(events))
	visited := make(map[string]bool, len(events))
	var walk func(e TrajectoryEvent, depth int)
	walk = func(e TrajectoryEvent, depth int) {
		if visited[e.EventID] {
			return
		}
		visited[e.EventID] = true
		rows = append(rows, views.TrajectoryRow{
			Type:       e.Type,
			Summary:    trajectorySummary(e.Fields),
			FieldClass: e.FieldClass,
			Occurred:   relativeTime(now, e.OccurredAt),
			Indent:     strings.Repeat("— ", depth),
		})
		for _, c := range children[e.EventID] {
			walk(c, depth+1)
		}
	}
	for _, e := range roots {
		walk(e, 0)
	}
	return rows
}

// trajectorySummary renders a compact field/tool summary for a trajectory
// event. Payload fields show their captured value; structure fields show their
// type and byte size, so an operator can see the shape without the raw data.
func trajectorySummary(fields []TrajectoryField) string {
	if len(fields) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		part := f.Key
		switch {
		case f.FieldClass == "payload" && len(f.Value) > 0:
			part += "=" + summarizeValue(f.Value)
		case f.ValueType != "" && f.ByteSize != nil:
			part += fmt.Sprintf(" (%s, %dB)", f.ValueType, *f.ByteSize)
		case f.ValueType != "":
			part += " (" + f.ValueType + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// summarizeValue renders a captured payload value for display: a JSON string is
// unquoted, anything else is shown as its compact JSON. Long values are
// truncated so one field cannot dominate the row.
func summarizeValue(raw json.RawMessage) string {
	const max = 80
	s := strings.TrimSpace(string(raw))
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		s = str
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
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
func approvalRows(approvals []Approval, now time.Time) []views.ApprovalRow {
	rows := make([]views.ApprovalRow, 0, len(approvals))
	for _, a := range approvals {
		rows = append(rows, approvalRow(a, now))
	}
	return rows
}

// approvalRow maps a single approval to a display row. A "pending" status is
// the only state that offers the approve/deny action.
func approvalRow(a Approval, now time.Time) views.ApprovalRow {
	return views.ApprovalRow{
		ID:        a.ID,
		Session:   a.SessionID,
		Action:    a.Action,
		Reason:    a.Reason,
		Status:    a.Status,
		DecidedBy: a.DecidedBy,
		Created:   relativeTime(now, a.CreatedAt),
		Pending:   a.Status == "pending",
	}
}
