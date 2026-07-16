package memberui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/memberui/views"
	"github.com/Domene99/drun-dash/internal/uikit"
	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// sessionCookieName carries the control-plane session token. It is set HttpOnly
// (never readable from JavaScript) and, in production, Secure; the server
// attaches its value as an Authorization: Bearer header on every control-plane
// call.
const sessionCookieName = "dd_member_session"

// deviceCookieName carries the in-flight device_code across the login poll. It
// is transient and HttpOnly: it lets the stateless /login/poll fragment resume
// polling the control plane without server-side login state. It is cleared once
// login completes or fails.
const deviceCookieName = "dd_member_device"

// defaultClientID is the device-flow client_id the console identifies itself
// with when none is configured. The control plane only requires it be present
// and echoes it back; it is not a confidential credential.
const defaultClientID = "drun-dash-member-console"

// deviceLoginTTL bounds the transient device cookie's lifetime, matching the
// control plane's device-code TTL (RFC 8628; 10 minutes).
const deviceLoginTTL = 10 * time.Minute

// proxyRequestTimeout bounds each governed model call the turn loop makes
// through the proxy. Turns can take several model calls; the per-call bound
// keeps a hung upstream from wedging a run without cutting healthy calls short.
const proxyRequestTimeout = 2 * time.Minute

// defaultApprovalPollInterval and defaultApprovalWaitTimeout bound the
// escalation approve/resume poll (ADR-0004 §6): a policy-blocked step raises an
// approval and pauses while the console polls the escalation queue this often,
// for at most this long before it falls closed (treated as denied).
const (
	defaultApprovalPollInterval = 3 * time.Second
	defaultApprovalWaitTimeout  = 5 * time.Minute
)

// ctxKey is the private context-key type for values this package stashes on the
// request context.
type ctxKey int

const tokenCtxKey ctxKey = iota

// launchedSession is the server-side record of a session this console launched.
// It holds the secret proxy_token so it is never rendered to the page or
// written to logs; only the non-secret fields reach the view. The policy bundle
// is held so the run view can verify it fail-closed and derive the tool set
// before binding the drun engine (ADR-0004 §3). AgentName is the human agent
// name captured at launch so session history reads by name, not UUID (ADR-0005
// §4). WorkingDir is the member's chosen host working directory (issue #296):
// an absolute path that bindRun mounts into the drun session so the agent works
// on real files and Commit writes back to that folder; empty means a scratch
// sandbox (no host folder).
type launchedSession struct {
	SessionID    string
	AgentID      string
	AgentName    string
	ProxyBaseURL string
	proxyToken   string
	FanOut       int
	ExpiresAt    string
	WorkingDir   string
	policyBundle json.RawMessage
}

// Server is the member-console HTTP handler: a chi router serving the
// device-flow login, the entitled-agent home page, session launch/terminate,
// the conversational run view (ADR-0004), and a health check, backed by a
// control-plane [Client]. It holds the proxy_token of each launched session in
// memory (never on the page) and, per active run, a bound local drun engine.
// Construct it with [New]; it implements [http.Handler].
type Server struct {
	client       *Client
	logger       *slog.Logger
	clientID     string
	secureCookie bool
	router       chi.Router

	// Run-view configuration (ADR-0004). model is the id passed to the governed
	// proxy; drunEndpoint locates the local drun engine; dial opens a drun
	// client (swappable in tests); proxyHTTP calls the proxy; verify checks the
	// policy bundle fail-closed; now/newEventID are injectable clocks/ids.
	model        string
	drunEndpoint string
	dial         drunDialer
	proxyHTTP    *http.Client
	verify       bundleVerifier
	now          func() time.Time
	newEventID   func() string

	// Escalation approve/resume timing (ADR-0004 §6): how often to poll the
	// control-plane escalation queue for a blocked step's decision, and how long
	// to wait before the bounded timeout falls closed (treated as denied).
	approvalPollInterval time.Duration
	approvalWaitTimeout  time.Duration

	mu       sync.Mutex
	sessions map[string]*launchedSession
	runs     map[string]*runState
}

// Option customizes a [Server].
type Option func(*Server)

// WithSecureCookie sets the Secure attribute on the session and device cookies.
// Enable it in production (HTTPS); leave it off for plain-HTTP localhost, where
// a Secure cookie would never be sent back.
func WithSecureCookie(secure bool) Option {
	return func(s *Server) { s.secureCookie = secure }
}

// WithClientID overrides the device-flow client_id the console sends. Defaults
// to [defaultClientID].
func WithClientID(id string) Option {
	return func(s *Server) {
		if strings.TrimSpace(id) != "" {
			s.clientID = id
		}
	}
}

// WithModel sets the model id the run loop passes to the governed proxy
// (ADR-0004 §4). When empty, the run view withholds the chat and explains that
// a model must be configured (MEMBER_CONSOLE_MODEL).
func WithModel(model string) Option {
	return func(s *Server) { s.model = strings.TrimSpace(model) }
}

// WithDrunEndpoint overrides the local drun engine endpoint. Defaults to
// [drunmcp.DefaultEndpoint].
func WithDrunEndpoint(endpoint string) Option {
	return func(s *Server) {
		if strings.TrimSpace(endpoint) != "" {
			s.drunEndpoint = endpoint
		}
	}
}

// WithDrunDialer overrides how the console opens a drun client. Production uses
// the default (a live *drunmcp.Client); tests inject a fake engine.
func WithDrunDialer(dial drunDialer) Option {
	return func(s *Server) {
		if dial != nil {
			s.dial = dial
		}
	}
}

// WithProxyHTTPClient overrides the HTTP client the turn loop uses for governed
// proxy calls. Tests route it at a mock proxy.
func WithProxyHTTPClient(hc *http.Client) Option {
	return func(s *Server) {
		if hc != nil {
			s.proxyHTTP = hc
		}
	}
}

// WithBundleVerifier overrides the fail-closed policy-bundle check run before a
// run binds its engine (ADR-0004 §3). Tests inject a permissive or failing
// verifier; production uses [defaultBundleVerifier].
func WithBundleVerifier(v bundleVerifier) Option {
	return func(s *Server) {
		if v != nil {
			s.verify = v
		}
	}
}

// WithClock overrides the time source used for trajectory timestamps and bundle
// freshness. Tests inject a fixed clock.
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

// WithEventIDFunc overrides the trajectory event-id generator. Tests inject a
// deterministic sequence.
func WithEventIDFunc(newID func() string) Option {
	return func(s *Server) {
		if newID != nil {
			s.newEventID = newID
		}
	}
}

// WithApprovalPollInterval overrides how often the escalation approve/resume
// loop polls the control-plane escalation queue for a decision. Tests set a
// tiny interval so a blocked-then-approved poll resolves without real waiting.
func WithApprovalPollInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.approvalPollInterval = d
		}
	}
}

// WithApprovalWaitTimeout overrides how long a blocked step waits for a decision
// before it falls closed (treated as denied). Tests set a small bound.
func WithApprovalWaitTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.approvalWaitTimeout = d
		}
	}
}

// New builds a Server that renders pages from live data fetched through client.
// Pass nil for logger to use [slog.Default]. The returned Server is ready to
// serve; its routes are wired once, here.
func New(client *Client, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		client:       client,
		logger:       logger,
		clientID:     defaultClientID,
		drunEndpoint: drunmcp.DefaultEndpoint,
		dial:         defaultDrunDialer,
		proxyHTTP:    &http.Client{Timeout: proxyRequestTimeout},
		verify:       defaultBundleVerifier,
		now:          time.Now,
		newEventID:   randomEventID,

		approvalPollInterval: defaultApprovalPollInterval,
		approvalWaitTimeout:  defaultApprovalWaitTimeout,

		sessions: make(map[string]*launchedSession),
		runs:     make(map[string]*runState),
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

// routes builds the chi router. The shared design-system assets (internal/uikit)
// and the login endpoints are public; the home page, the session actions, and
// the run view sit behind requireAuth, which redirects to /login when the
// session cookie is absent.
func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	// Serve the shared uikit stylesheet + theme script under /uikit/ so both the
	// web binary and the desktop app render on the same design system (ADR-0005).
	// The assets are public and content-hashed for cache-busting.
	r.Handle(uikit.MountPrefix+"*", uikit.Handler())

	r.Get("/healthz", s.handleHealthz)
	r.Get("/login", s.handleLogin)
	r.Post("/login", s.handlePasswordLogin)
	r.Get("/login/poll", s.handleLoginPoll)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleHome)
		r.Post("/sessions", s.handleLaunch)
		r.Post("/sessions/{sessionId}/terminate", s.handleTerminate)
		r.Get("/sessions/{sessionId}", s.handleRunView)
		r.Post("/sessions/{sessionId}/turn", s.handleTurn)
		r.Get("/sessions/{sessionId}/stream", s.handleStream)
		r.Get("/sessions/{sessionId}/timeline", s.handleTimeline)
		r.Get("/sessions/{sessionId}/diff", s.handleDiff)
		r.Post("/sessions/{sessionId}/commit", s.handleCommit)
		r.Post("/sessions/{sessionId}/reject", s.handleReject)
	})

	return r
}

// handleHealthz reports liveness with a static 200; it does not consult the
// control plane, so a control-plane blip cannot make an orchestrator kill an
// otherwise-healthy console process.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

// handleLogin renders the sign-in page. It leads with the email/password form
// (the primary local path) and starts the device-authorization flow for the
// secondary SSO section: it asks the control plane for a device+user code,
// stashes the device_code in the transient cookie, and shows the user code,
// verification link, and the HTMX poller that watches for approval. If the
// member is already signed in it goes straight to the home page.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLoginPage(w, r, http.StatusOK, views.LoginData{})
}

// handlePasswordLogin implements email/password sign-in (ADR-0008 §5): it trades
// (org slug, email, password) for a control-plane session JWT via POST
// /auth/login, stores it in the HttpOnly dd_member_session cookie, and redirects
// to the home/agent picker. A missing field or a rejected credential re-renders
// the login page with a single generic "invalid credentials" message (echoing
// the org/email but never the password) that reveals nothing about which field
// failed; a control-plane outage is surfaced as a transient error.
func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLoginPage(w, r, http.StatusBadRequest, views.LoginData{
			PasswordError: "Could not read the submitted form.",
		})
		return
	}
	orgSlug := strings.TrimSpace(r.PostFormValue("org_slug"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")

	if orgSlug == "" || email == "" || password == "" {
		s.renderLoginPage(w, r, http.StatusOK, views.LoginData{
			OrgSlug:       orgSlug,
			Email:         email,
			PasswordError: "Enter your organization, email, and password.",
		})
		return
	}

	token, err := s.client.Login(r.Context(), orgSlug, email, password)
	switch {
	case err == nil:
		s.setSessionCookie(w, token)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case errors.Is(err, ErrUnauthorized):
		// The control plane returns the same generic 401 for every failure mode
		// (unknown org/email, wrong password, inactive); mirror that here.
		s.renderLoginPage(w, r, http.StatusUnauthorized, views.LoginData{
			OrgSlug:       orgSlug,
			Email:         email,
			PasswordError: "Invalid credentials. Check your organization, email, and password.",
		})
	default:
		s.logger.ErrorContext(r.Context(), "member-console: password login", slog.Any("error", err))
		s.renderLoginPage(w, r, http.StatusBadGateway, views.LoginData{
			OrgSlug:       orgSlug,
			Email:         email,
			PasswordError: "Could not sign in right now: " + readableError(err),
		})
	}
}

// renderLoginPage starts (or restarts) the device-authorization flow and renders
// the login page around the supplied form state (a password error plus the
// echoed org/email). Starting the device flow here — on the initial GET and on a
// failed password POST — keeps the secondary SSO section live without separate
// wiring and sets the transient device cookie so the HTMX poller can resume. A
// device-start failure is surfaced in that section (DeviceError) and never
// blocks the primary password form.
func (s *Server) renderLoginPage(w http.ResponseWriter, r *http.Request, status int, form views.LoginData) {
	da, err := s.client.StartDeviceAuthorization(r.Context(), s.clientID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "member-console: start device authorization", slog.Any("error", err))
		form.DeviceError = "Could not start device sign-in: " + readableError(err)
	} else {
		s.setTransientCookie(w, deviceCookieName, da.DeviceCode)
		form.UserCode = da.UserCode
		form.VerificationURI = da.VerificationURI
		form.VerificationURIComplete = da.VerificationURIComplete
		form.IntervalSeconds = pollInterval(da.Interval)
	}
	s.render(w, r, status, views.Login(form))
}

// handleLoginPoll is the HTMX poll fragment. It reads the in-flight device_code
// from the transient cookie and asks the control plane whether the request has
// been approved. While pending it returns the poller so HTMX keeps polling; on
// approval it stores the minted session JWT in the session cookie and asks HTMX
// to navigate to the home page; on denial or expiry it clears the device cookie
// and returns a terminal message (which stops the poll).
func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(deviceCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		s.render(w, r, http.StatusOK, views.LoginPoll(views.PollData{
			Message: "Sign-in expired. Reload the page to start again.",
		}))
		return
	}

	token, err := s.client.PollDeviceToken(r.Context(), cookie.Value)
	switch {
	case err == nil:
		s.clearCookie(w, deviceCookieName)
		s.setSessionCookie(w, token)
		// HTMX honors HX-Redirect on a 200 and navigates the whole page; a 3xx
		// would be swapped into the poller element instead.
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, ErrAuthorizationPending), errors.Is(err, ErrSlowDown):
		s.render(w, r, http.StatusOK, views.LoginPoll(views.PollData{
			Pending:         true,
			IntervalSeconds: devicePollFallback,
			Message:         "Waiting for approval…",
		}))
	case errors.Is(err, ErrAccessDenied):
		s.clearCookie(w, deviceCookieName)
		s.render(w, r, http.StatusOK, views.LoginPoll(views.PollData{
			Message: "The request was denied. Reload the page to try again.",
		}))
	case errors.Is(err, ErrExpiredToken):
		s.clearCookie(w, deviceCookieName)
		s.render(w, r, http.StatusOK, views.LoginPoll(views.PollData{
			Message: "Sign-in expired. Reload the page to start again.",
		}))
	default:
		s.logger.ErrorContext(r.Context(), "member-console: poll device token", slog.Any("error", err))
		s.render(w, r, http.StatusOK, views.LoginPoll(views.PollData{
			Pending:         true,
			IntervalSeconds: devicePollFallback,
			Message:         "Temporary error contacting the control plane; retrying…",
		}))
	}
}

// handleLogout clears the session cookie and returns to the login page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearCookie(w, sessionCookieName)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleHome renders the authenticated landing page: the agents the member is
// entitled to launch, plus the sessions this console has launched. A rejected
// token bounces to /login; any other agent-list failure is surfaced inline
// rather than blanking the page.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	data := views.HomeData{Sessions: s.sessionRows()}
	list, err := s.client.GetEntitledAgents(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "member-console: get entitled agents", slog.Any("error", err))
		data.AgentsError = "Could not load agents: " + readableError(err)
	default:
		data.Agents = agentRows(list.Items)
	}

	s.render(w, r, http.StatusOK, views.Home(data))
}

// handleLaunch launches the chosen agent's governed session, records it (with
// its secret proxy_token and policy bundle) server-side, and drops the member
// straight into the conversation via HTMX (HX-Redirect to the run view) —
// chat-first (ADR-0007 §4). The optional working_dir is the member's chosen host
// folder (issue #296): when present it must be an absolute path, captured here
// and mounted into the drun session at bind time so the agent works on real
// files and Commit writes back to it; blank keeps the prior scratch-sandbox
// behavior. A rejected token asks HTMX to redirect to /login; any other
// control-plane error is surfaced inside the sessions section.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())

	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.SessionsSection(views.SessionsData{
			Sessions: s.sessionRows(),
			Error:    "Could not read the submitted form.",
		}))
		return
	}
	agentID := strings.TrimSpace(r.PostFormValue("agent_id"))
	agentName := strings.TrimSpace(r.PostFormValue("agent_name"))
	if agentID == "" {
		s.render(w, r, http.StatusBadRequest, views.SessionsSection(views.SessionsData{
			Sessions: s.sessionRows(),
			Error:    "Choose an agent to launch.",
		}))
		return
	}

	// Optional host working directory (issue #296). A blank value means a
	// scratch sandbox (no mount); a provided value must be an absolute host path
	// so drun can mount it — a relative path is rejected up front rather than
	// silently mounting something ambiguous or falling back to a temp dir.
	workingDir := strings.TrimSpace(r.PostFormValue("working_dir"))
	if workingDir != "" && !filepath.IsAbs(workingDir) {
		s.render(w, r, http.StatusBadRequest, views.SessionsSection(views.SessionsData{
			Sessions: s.sessionRows(),
			Error:    "The working directory must be an absolute path (e.g. /Users/you/project). Leave it blank for a scratch sandbox.",
		}))
		return
	}

	launch, err := s.client.LaunchSession(r.Context(), token, agentID)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.hxRedirectToLogin(w)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "member-console: launch session", slog.Any("error", err))
		s.render(w, r, http.StatusOK, views.SessionsSection(views.SessionsData{
			Sessions: s.sessionRows(),
			Error:    "Could not launch session: " + readableError(err),
		}))
		return
	}

	s.storeSession(&launchedSession{
		SessionID:    launch.SessionID,
		AgentID:      agentID,
		AgentName:    agentName,
		ProxyBaseURL: launch.ProxyBaseURL,
		proxyToken:   launch.ProxyToken,
		FanOut:       launch.FanOutCeiling,
		ExpiresAt:    launch.ExpiresAt,
		WorkingDir:   workingDir,
		policyBundle: launch.PolicyBundle,
	})

	// Chat-first: navigate the whole page straight into the new conversation.
	// HTMX honors HX-Redirect on a 200; a 3xx would be swapped into the target.
	w.Header().Set("HX-Redirect", "/sessions/"+launch.SessionID)
	w.WriteHeader(http.StatusOK)
}

// handleTerminate terminates a launched session, closes its bound drun session,
// drops its server-side record (and its proxy_token), and re-renders the
// sessions section. A rejected token asks HTMX to redirect to /login; any other
// control-plane error is surfaced inside the section (the record is kept so the
// member can retry).
func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	sessionID := chi.URLParam(r, "sessionId")

	_, err := s.client.TerminateSession(r.Context(), token, sessionID)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.hxRedirectToLogin(w)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "member-console: terminate session", slog.Any("error", err))
		s.render(w, r, http.StatusOK, views.SessionsSection(views.SessionsData{
			Sessions: s.sessionRows(),
			Error:    "Could not terminate session: " + readableError(err),
		}))
		return
	}

	s.dropRun(sessionID)
	s.dropSession(sessionID)
	s.render(w, r, http.StatusOK, views.SessionsSection(views.SessionsData{Sessions: s.sessionRows()}))
}

// --- server-side session store ---

// storeSession records a launched session under its id.
func (s *Server) storeSession(ls *launchedSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[ls.SessionID] = ls
}

// dropSession removes a session record (and its proxy_token) from memory.
func (s *Server) dropSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// dropRun removes a run record and closes its bound drun session, freeing the
// sandbox subprocess. It waits for any in-flight turn (which holds run.mu) to
// finish before closing, so a terminate never yanks the engine mid-turn.
func (s *Server) dropRun(sessionID string) {
	s.mu.Lock()
	run := s.runs[sessionID]
	delete(s.runs, sessionID)
	s.mu.Unlock()
	if run == nil {
		return
	}
	run.mu.Lock()
	eng := run.engine
	run.engine = nil
	run.mu.Unlock()
	if eng != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		eng.close(ctx)
	}
}

// sessionRows snapshots the launched sessions as display rows, ordered by id
// for stable rendering. The secret proxy_token is deliberately never copied
// into the view model; the agent name is the readable label (ADR-0005 §4).
func (s *Server) sessionRows() []views.SessionRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]views.SessionRow, 0, len(s.sessions))
	for _, ls := range s.sessions {
		rows = append(rows, views.SessionRow{
			ID:           ls.SessionID,
			Agent:        sessionAgentLabel(ls),
			ProxyBaseURL: ls.ProxyBaseURL,
			FanOut:       strconv.Itoa(ls.FanOut),
			Expires:      orDash(ls.ExpiresAt),
		})
	}
	sortSessionRows(rows)
	return rows
}

// sessionAgentLabel returns the readable agent name for a session row, falling
// back to the agent id when the name was not captured at launch.
func sessionAgentLabel(ls *launchedSession) string {
	if strings.TrimSpace(ls.AgentName) != "" {
		return ls.AgentName
	}
	return ls.AgentID
}

// requireAuth admits a request only when the session cookie is present and
// non-empty, stashing the token on the request context. Otherwise it redirects
// to /login.
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

// redirectToLogin clears any stale session cookie and sends the client to
// /login.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	s.clearCookie(w, sessionCookieName)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// hxRedirectToLogin clears the stale cookie and tells HTMX to navigate the
// whole page to /login (HTMX honors HX-Redirect on a 200, whereas a 3xx would
// be swapped into the target element instead).
func (s *Server) hxRedirectToLogin(w http.ResponseWriter) {
	s.clearCookie(w, sessionCookieName)
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

// setTransientCookie stores a short-lived, HttpOnly value (the in-flight
// device_code) used only across the login poll.
func (s *Server) setTransientCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(deviceLoginTTL / time.Second),
	})
}

// clearCookie expires the named cookie on the client.
func (s *Server) clearCookie(w http.ResponseWriter, name string) {
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
		s.logger.ErrorContext(r.Context(), "member-console: render failed", slog.Any("error", err))
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
