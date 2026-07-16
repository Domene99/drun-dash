package views

import (
	"net/url"
	"strconv"
)

// LoginData is the view model for the login page, which offers two ways in:
// email/password (the primary local path, ADR-0008 §5) and the device flow (for
// OIDC/SSO orgs). The password fields drive the sign-in form; the device fields
// carry the user code and verification link the member approves.
type LoginData struct {
	// --- email/password sign-in (primary) ---

	// OrgSlug and Email are echoed back to repopulate the form after a failed
	// attempt so the member need not retype them; the password is never echoed.
	OrgSlug string
	Email   string
	// PasswordError is shown as an alert above the password form when a sign-in
	// attempt fails. It is deliberately generic ("invalid credentials") and
	// never reveals which field was wrong.
	PasswordError string

	// --- device flow (secondary; OIDC/SSO orgs) ---

	// UserCode is the short code the member confirms in the admin UI.
	UserCode string
	// VerificationURI is where the member goes to approve the device.
	VerificationURI string
	// VerificationURIComplete is the verification URI with the user code
	// pre-filled, offered as a one-click link when present.
	VerificationURIComplete string
	// IntervalSeconds is how often the approval poller checks for completion.
	IntervalSeconds int
	// DeviceError is shown in the device-flow section when that flow could not
	// be started; the password form still works.
	DeviceError string
}

// PollData is the view model for the login approval poller fragment. Pending
// keeps the HTMX poll running (the request is not yet approved); a false
// Pending renders a terminal message and stops polling. Message is the status
// or error line shown to the member.
type PollData struct {
	// Pending keeps the poller live when true.
	Pending bool
	// IntervalSeconds is the poll cadence while Pending.
	IntervalSeconds int
	// Message is the status/error line shown to the member.
	Message string
}

// HomeData is the view model for the authenticated home page: the agents the
// member may launch and the sessions this console has launched. AgentsError, if
// set, replaces the agent list with an inline message.
type HomeData struct {
	Agents      []AgentRow
	AgentsError string
	Sessions    []SessionRow
}

// AgentRow is one entitled agent shown on the home page, with the hidden ID the
// launch form submits.
type AgentRow struct {
	ID          string
	Name        string
	Version     string
	Description string
}

// SessionsData is the view model for the launched-sessions section, rendered
// standalone so HTMX can swap it after a launch or terminate. Error, if set, is
// shown above the table.
type SessionsData struct {
	Sessions []SessionRow
	Error    string
}

// SessionRow is one launched session shown on the home page as a reopenable
// conversation. Agent is the human agent name (the readable primary label, per
// ADR-0005 §4); the raw session UUID (ID) appears only as a copyable ID chip,
// never as the headline. The secret proxy_token is deliberately absent — it
// never reaches the view.
type SessionRow struct {
	ID           string
	Agent        string
	ProxyBaseURL string
	FanOut       string
	Expires      string
}

// triggerEvery renders the HTMX hx-trigger value for the login poller. It
// clamps to a 1-second floor so a zero interval never becomes a hot loop.
func triggerEvery(seconds int) string {
	if seconds < 1 {
		seconds = 1
	}
	return "every " + strconv.Itoa(seconds) + "s"
}

// terminatePath builds the HTMX hx-post target for terminating a session,
// escaping the id so an unusual value cannot break out of the path.
func terminatePath(sessionID string) string {
	return "/sessions/" + url.PathEscape(sessionID) + "/terminate"
}

// runPath builds the link target for opening a session's conversation view,
// escaping the id so an unusual value cannot break out of the path.
func runPath(sessionID string) string {
	return "/sessions/" + url.PathEscape(sessionID)
}

// shortID returns a truncated form of an identifier for the ID chip's visible
// text (the full value is still copied on click). It keeps the first eight
// characters, which is enough to disambiguate a session at a glance.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}
