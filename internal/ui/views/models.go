// Package views holds the server-rendered templ components for the drun-dash
// admin UI. It composes the shared design system in internal/uikit (ADR-0005):
// the app shell (sidebar + top bar), page headers, cards, tables, status
// badges, ID chips, and relative times.
//
// Views are pure presentation. They receive fully-resolved view models (plain
// strings and slices assembled by internal/ui) and never call the control
// plane or import internal/ui, so the dependency edge runs one way
// (internal/ui -> views -> uikit) and stays cycle-free.
//
// The *.templ files are the source of truth; the committed *_templ.go files
// are generated from them with `templ generate` (see internal/ui/README.md).
// Edit the .templ file and regenerate — never hand-edit the generated Go.
package views

import "github.com/Domene99/drun-dash/internal/uikit"

// Chrome is the shell context for an authenticated admin page: the browser
// title, which sidebar section is active, the environment indicator, and the
// signed-in identity rendered into the top-bar user menu. It is assembled by
// internal/ui and threaded into [Page] so every content page shares one shell.
type Chrome struct {
	// Title is the browser/document title for the page.
	Title string
	// Active is the key of the active sidebar item (see [navGroups]).
	Active string
	// Environment is the optional environment indicator shown in the top bar
	// (e.g. "production"); empty hides it.
	Environment string
	// Identity is the signed-in principal shown in the top-bar user menu.
	Identity IdentityView
}

// IdentityView is the authenticated principal shown in the top-bar user menu.
// Roles is pre-joined into a display string by internal/ui. Per ADR-0005 the
// raw subject/organization UUIDs appear only here, as copyable ID chips, never
// as a page's primary content.
type IdentityView struct {
	Subject        string
	OrganizationID string
	Email          string
	Roles          string
}

// TimeView pairs a human relative time ("3 minutes ago") with the absolute
// timestamp shown on hover. internal/ui does the time math; uikit renders both.
type TimeView struct {
	Relative string
	Absolute string
}

// DashboardData is the view model for the Overview page: the shell chrome plus
// the live summary rendered inside the self-refreshing HTMX fragment.
type DashboardData struct {
	Chrome  Chrome
	Summary SummaryData
}

// SummaryData is the live Overview fragment's view model. The metric values and
// amounts are pre-formatted display strings; a failed metric fetch degrades to
// "—" rather than blanking the page.
type SummaryData struct {
	ActiveSessions   string
	PendingApprovals string
	SpendTotal       string
	SpendWindow      string
	AgentCount       string

	RecentSessions []SessionRow

	// SessionsError, when non-empty, replaces the recent-sessions list with a
	// readable message.
	SessionsError string
}

// SessionRow is one compact session row (Overview recent-activity list): the
// agent name, a status string (rendered as a badge), the relative start time,
// and the session id (rendered as a secondary ID chip).
type SessionRow struct {
	ID      string
	Agent   string
	Status  string
	Started TimeView
}

// SessionDetailRow is one row of the Sessions page: a [SessionRow] plus the
// fan-out ceiling.
type SessionDetailRow struct {
	ID      string
	Agent   string
	Status  string
	Started TimeView
	FanOut  string
}

// SpendRow is one row of the spend breakdown table.
type SpendRow struct {
	Key    string
	Amount string
	Tokens string
}

// SessionsData is the view model for the Sessions page: the shell chrome, the
// governed-session rows, and an optional page-level error.
type SessionsData struct {
	Chrome   Chrome
	Error    string
	Sessions []SessionDetailRow
}

// SpendPageData is the view model for the Spend page: the shell chrome, the
// spend total and per-group breakdown, and budget utilization. The two sections
// fail independently, each carrying its own error string.
type SpendPageData struct {
	Chrome       Chrome
	Window       string
	Total        string
	Groups       []SpendRow
	SpendError   string
	Budgets      []BudgetRow
	BudgetsError string
}

// BudgetRow is one row of the budget-utilization table. Over marks a budget
// whose spend has reached or exceeded its limit so the view can flag it.
type BudgetRow struct {
	Name        string
	Period      string
	Spent       string
	Limit       string
	Utilization string
	Over        bool
}

// ApprovalsData is the view model for the Approvals page: the shell chrome, the
// escalation-queue rows, and an optional page-level error.
type ApprovalsData struct {
	Chrome    Chrome
	Error     string
	Approvals []ApprovalRow
}

// ApprovalRow is one row of the approvals table. Pending controls whether the
// approve/deny action is offered; RowError carries a per-row message surfaced
// after a failed decision POST so a single failure does not blank the page.
type ApprovalRow struct {
	ID        string
	Session   string
	Action    string
	Reason    string
	Status    string
	DecidedBy string
	Created   TimeView
	Pending   bool
	RowError  string
}

// TrajectoryData is the view model for the forensic trajectory replay viewer:
// the shell chrome, a readable heading (agent name when resolvable), the session
// id shown as a secondary ID chip, the reconstructed event rows (already
// flattened depth-first with indentation), and an optional page-level error.
type TrajectoryData struct {
	Chrome    Chrome
	Heading   string
	SessionID string
	Error     string
	Rows      []TrajectoryRow
}

// TrajectoryRow is one node of the trajectory tree, already flattened
// depth-first. Indent carries the nesting; FieldClass is the event's capture
// class (structure or payload) rendered as a status badge; Occurred is the
// relative occurrence time.
type TrajectoryRow struct {
	Type       string
	Summary    string
	FieldClass string
	Occurred   TimeView
	Indent     string
}

// LoginData is the view model for the login page. Error, when non-empty, holds
// a human-readable message explaining why a prior sign-in attempt failed. OIDC
// selects the page's mode: when true the page offers the identity-provider
// button; when false it renders the paste-a-token dev form.
type LoginData struct {
	Error string
	OIDC  bool
}

// DeviceData is the view model for the CLI device-approval page. UserCode is the
// short code the operator confirms against the one shown in their terminal.
// Error explains why the request cannot proceed. Done marks a completed
// decision; Approved distinguishes an approval from a denial.
type DeviceData struct {
	UserCode string
	Error    string
	Done     bool
	Approved bool
}

// navGroups builds the admin sidebar navigation (ADR-0005 §3). Every section
// links to its page; only Audit remains a disabled slot (reserved for a later
// batch) so the IA position is fixed and the nav does not reshuffle. active
// marks the current section.
func navGroups(active string) []uikit.NavGroup {
	item := func(key, label, href string, enabled bool) uikit.NavItem {
		return uikit.NavItem{
			Label:    label,
			Href:     href,
			Active:   enabled && key == active,
			Disabled: !enabled,
		}
	}
	return []uikit.NavGroup{
		{Items: []uikit.NavItem{
			item("overview", "Overview", "/", true),
		}},
		{Label: "Operate", Items: []uikit.NavItem{
			item("agents", "Agents", "/agents", true),
			item("sessions", "Sessions", "/sessions", true),
			item("approvals", "Approvals", "/approvals", true),
		}},
		{Label: "Govern", Items: []uikit.NavItem{
			item("policies", "Policies", "/policies", true),
			item("members", "Members", "/members", true),
			item("roles", "Roles", "/roles", true),
			item("teams", "Teams", "/teams", true),
		}},
		{Label: "Analyze", Items: []uikit.NavItem{
			item("spend", "Spend", "/spend", true),
			item("audit", "Audit", "", false),
		}},
	}
}

// shortID truncates an identifier to its first 8 characters for display inside
// an ID chip, keeping the full value copyable. A short id is returned verbatim.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// orDash renders an em dash for an empty string, keeping optional columns tidy.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// trajectoryPath is the trajectory-viewer URL for a session id.
func trajectoryPath(sessionID string) string {
	return "/sessions/" + sessionID + "/trajectory"
}

// decisionPath is the approve/deny decision URL for an approval id.
func decisionPath(approvalID string) string {
	return "/approvals/" + approvalID + "/decision"
}
