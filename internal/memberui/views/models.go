package views

import (
	"net/url"
	"strconv"
)

// LoginData is the view model for the device-flow login page. On success it
// carries the user code the member confirms and the verification link they open
// to approve; Error, when non-empty, explains why the flow could not start.
type LoginData struct {
	// UserCode is the short code the member confirms in the admin UI.
	UserCode string
	// VerificationURI is where the member goes to approve the device.
	VerificationURI string
	// VerificationURIComplete is the verification URI with the user code
	// pre-filled, offered as a one-click link when present.
	VerificationURIComplete string
	// IntervalSeconds is how often the approval poller checks for completion.
	IntervalSeconds int
	// Error is shown as an alert when the flow could not be started.
	Error string
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

// SessionRow is one launched session shown on the home page. The secret
// proxy_token is deliberately absent — it never reaches the view.
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
