package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// These tests render the admin views against fully-resolved view models and
// assert the design-system markup (ADR-0005): the uikit shell (sidebar + top
// bar), status badges, ID chips, relative times, and empty states. They are
// hermetic — no control plane, no HTTP — so they complement server_test.go
// (which drives the same views through the handlers against a mock control
// plane) and run anywhere.

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in:\n%s", w, body)
		}
	}
}

func sampleChrome(active string) Chrome {
	return Chrome{
		Title:       active,
		Active:      active,
		Environment: "staging",
		Identity:    IdentityView{Subject: "user-1", OrganizationID: "org-1", Email: "a@b.co", Roles: "admin"},
	}
}

func TestRenderOverviewIsSummaryNotClaimsDump(t *testing.T) {
	body := render(t, Dashboard(DashboardData{
		Chrome: sampleChrome("overview"),
		Summary: SummaryData{
			ActiveSessions: "3", PendingApprovals: "1", SpendTotal: "12.50 USD", AgentCount: "5",
			SpendWindow: "(last 30 days)",
			RecentSessions: []SessionRow{{
				ID: "sess-abcdef12", Agent: "Planner", Status: "running",
				Started: TimeView{Relative: "3 minutes ago", Absolute: "2026-07-15T00:00:00Z"},
			}},
		},
	}))
	mustContain(t, body,
		"Overview", "Recent sessions", "Active sessions", "Pending approvals",
		"12.50 USD", "Planner", "running", "3 minutes ago",
		"uk-shell", "uk-sidebar", "uk-topbar",
		"a@b.co",                                        // identity lives in the top-bar user menu
		`data-uk-copy="user-1"`, `data-uk-copy="org-1"`, // subject/org as copyable ID chips
		`data-uk-copy="sess-abcdef12"`, ">sess-abc", // session id as truncated chip
		"staging",                // environment indicator
		`href="/uikit/uikit.css`, // shared stylesheet linked
		`aria-current="page"`,    // active nav item
		"uk-badge--info",         // running -> info status badge
	)
	// The raw subject/org UUIDs must not be the page's primary <dl> content.
	if strings.Contains(body, "<dl>") {
		t.Errorf("overview still renders a claims <dl> dump:\n%s", body)
	}
}

func TestRenderSessionsReadableAndEmpty(t *testing.T) {
	body := render(t, Sessions(SessionsData{
		Chrome: sampleChrome("sessions"),
		Sessions: []SessionDetailRow{{
			ID: "sess-1", Agent: "Planner", Status: "completed",
			Started: TimeView{Relative: "1 hour ago", Absolute: "x"}, FanOut: "4",
		}},
	}))
	mustContain(t, body, "Sessions", "Planner", "completed", "1 hour ago",
		"View trajectory", `href="/sessions/sess-1/trajectory"`, "uk-badge--success")

	empty := render(t, Sessions(SessionsData{Chrome: sampleChrome("sessions")}))
	mustContain(t, empty, "No sessions yet", "uk-empty")
}

func TestRenderSpendFlagsOverBudget(t *testing.T) {
	body := render(t, Spend(SpendPageData{
		Chrome: sampleChrome("spend"), Window: "(last 30 days)", Total: "12.50 USD",
		Groups:  []SpendRow{{Key: "Planner", Amount: "12.50 USD", Tokens: "100"}},
		Budgets: []BudgetRow{{Name: "Cap", Period: "monthly", Spent: "120.00 USD", Limit: "100.00 USD", Utilization: "120.0%", Over: true}},
	}))
	mustContain(t, body, "Spend", "Budgets", "Cap", "120.0%", "uk-badge--danger")
}

func TestRenderApprovalRowPendingVsDecided(t *testing.T) {
	pending := render(t, ApprovalRowTr(ApprovalRow{
		ID: "ap1", Session: "sess-1", Action: "spawn", Reason: "why", Status: "pending",
		Created: TimeView{Relative: "now", Absolute: "x"}, Pending: true,
	}))
	mustContain(t, pending, "spawn", "Approve", "Deny",
		`hx-post="/approvals/ap1/decision"`, "uk-badge--warning", `data-uk-copy="sess-1"`)

	decided := render(t, ApprovalRowTr(ApprovalRow{
		ID: "ap2", Session: "sess-2", Action: "call", Status: "approved",
		DecidedBy: "user-9", Created: TimeView{Relative: "now"}, Pending: false,
	}))
	if strings.Contains(decided, "hx-post") {
		t.Errorf("decided approval must not offer an action:\n%s", decided)
	}
	mustContain(t, decided, "user-9", "uk-badge--success")
}

func TestRenderTrajectoryTree(t *testing.T) {
	body := render(t, Trajectory(TrajectoryData{
		Chrome: sampleChrome("sessions"), Heading: "Trajectory · Planner", SessionID: "sess-1",
		Rows: []TrajectoryRow{
			{Type: "session_start", Summary: "agent (string, 8B)", FieldClass: "structure", Occurred: TimeView{Relative: "now"}},
			{Type: "tool_call", Summary: "tool_name=search", FieldClass: "payload", Occurred: TimeView{Relative: "now"}, Indent: "— "},
		},
	}))
	mustContain(t, body, "Trajectory · Planner", "session_start", "tool_call",
		"raw value captured", "uk-badge--info", "uk-badge--danger",
		`data-uk-copy="sess-1"`, "Back to sessions", "— ")
}

func TestRenderLoginAndDeviceUseMinimalShell(t *testing.T) {
	login := render(t, Login(LoginData{}))
	mustContain(t, login, "Sign in", `name="token"`, "uk-shell", "uk-topbar")
	// Login uses the sidebar-less Minimal shell.
	if strings.Contains(login, "uk-sidebar") {
		t.Errorf("login should use the sidebar-less shell")
	}

	oidc := render(t, Login(LoginData{OIDC: true}))
	mustContain(t, oidc, "identity provider", `href="/login"`)

	device := render(t, Device(DeviceData{UserCode: "WXYZ-1234"}))
	mustContain(t, device, "Approve device login", "WXYZ-1234", "Approve", "Deny")

	done := render(t, Device(DeviceData{UserCode: "WXYZ-1234", Done: true, Approved: true}))
	mustContain(t, done, "Device approved", "uk-alert--success")
}
