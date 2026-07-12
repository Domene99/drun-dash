package memberui

import (
	"errors"
	"sort"
	"strconv"

	"github.com/Domene99/drun-dash/internal/memberui/views"
)

// devicePollFallback is the poll cadence (seconds) the login poller uses when
// the control plane does not suggest one. It matches the control plane's
// default device-flow interval.
const devicePollFallback = 5

// pollInterval clamps a control-plane-suggested interval to a sane floor so a
// zero or negative value never turns into a hot loop in the browser.
func pollInterval(suggested int) int {
	if suggested < 1 {
		return devicePollFallback
	}
	return suggested
}

// readableError renders a control-plane error for display. It prefers the flat
// Error envelope's message when present and falls back to a generic line, so a
// page or section never surfaces a raw Go error or blanks out.
func readableError(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message
	}
	return "control plane unavailable"
}

// orDash returns s, or an em dash when s is empty, so absent values render
// cleanly.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// agentRows maps entitled agents to display rows.
func agentRows(agents []Agent) []views.AgentRow {
	rows := make([]views.AgentRow, 0, len(agents))
	for _, a := range agents {
		ver := "—"
		if a.LatestVersion != nil {
			ver = strconv.Itoa(*a.LatestVersion)
		}
		rows = append(rows, views.AgentRow{
			ID:          a.ID,
			Name:        orDash(a.Name),
			Version:     ver,
			Description: a.Description,
		})
	}
	return rows
}

// sortSessionRows orders launched-session rows by id for stable rendering
// (the underlying store is a map with no inherent order).
func sortSessionRows(rows []views.SessionRow) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
}
