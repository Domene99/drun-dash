package views

import (
	"context"
	"io"
	"strings"

	"github.com/a-h/templ"
)

// This file holds the run view's dynamic panels — the checkpoint timeline, the
// diff-before-commit review, and the matrix fan-out view (design §6.3). Like
// run.go they are authored directly as Go [templ.Component]s rather than
// generated from .templ files: they are HTMX fragments swapped into the run
// page, and the console's build/test runs in a network-isolated sandbox where
// regenerating templ is avoided. They compose with the generated [Layout]
// exactly as the generated views do.

// --- Checkpoint timeline (design §6.3) ----------------------------------------

// TimelineEntry is one checkpoint in the run's timeline: its id, optional label,
// the checkpoint it descends from, the file delta it introduced, whether it is
// the current head, and any child sessions forked from it.
type TimelineEntry struct {
	ID        string
	Label     string
	Parent    string
	IsCurrent bool
	Delta     string
	Forks     []string
}

// TimelineData is the view model for the checkpoint-timeline fragment. Exactly
// one of RunInProgress / Error / Entries drives the render.
type TimelineData struct {
	Entries       []TimelineEntry
	Error         string
	RunInProgress bool
}

// TimelineFragment renders the checkpoint timeline as the inner markup of the
// run view's #timeline panel, refreshed by HTMX polling.
func TimelineFragment(data TimelineData) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		switch {
		case data.RunInProgress:
			b.WriteString(`<p class="muted">A run is in progress; the timeline refreshes when it settles.</p>`)
		case data.Error != "":
			b.WriteString(`<p class="error" role="alert">` + templ.EscapeString(data.Error) + `</p>`)
		case len(data.Entries) == 0:
			b.WriteString(`<p class="muted">No checkpoints yet.</p>`)
		default:
			b.WriteString(`<ol class="timeline">`)
			for _, e := range data.Entries {
				b.WriteString(`<li><code>#` + templ.EscapeString(e.ID) + `</code>`)
				if e.IsCurrent {
					b.WriteString(` <span class="badge">current</span>`)
				}
				if e.Label != "" {
					b.WriteString(` <strong>` + templ.EscapeString(e.Label) + `</strong>`)
				}
				b.WriteString(` <span class="meta">parent ` + templ.EscapeString(e.Parent) + ` · ` + templ.EscapeString(e.Delta) + ` files</span>`)
				for _, f := range e.Forks {
					b.WriteString(` <span class="meta">forked &rarr; <code>` + templ.EscapeString(f) + `</code></span>`)
				}
				b.WriteString(`</li>`)
			}
			b.WriteString(`</ol>`)
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}

// --- Diff-before-commit (design §6.3) -----------------------------------------

// DiffData is the view model for the pending host-file diff review. SessionID
// targets the Commit / Reject / re-review actions. Exactly one of RunInProgress
// / Error / Empty / Diff drives the render.
type DiffData struct {
	SessionID     string
	Diff          string
	Empty         bool
	Error         string
	RunInProgress bool
}

// DiffReview renders the diff-before-commit panel: the pending session_diff plus
// the explicit member Commit / Reject actions. Committing is reachable only
// through the Commit form here — never automatically from the agent loop.
func DiffReview(data DiffData) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		switch {
		case data.RunInProgress:
			b.WriteString(`<p class="muted">A run is in progress; review host changes once it settles.</p>`)
			b.WriteString(reviewButton(data.SessionID))
		case data.Error != "":
			b.WriteString(`<p class="error" role="alert">` + templ.EscapeString(data.Error) + `</p>`)
			b.WriteString(reviewButton(data.SessionID))
		case data.Empty:
			b.WriteString(`<p class="muted">No pending host changes. Nothing to commit.</p>`)
			b.WriteString(reviewButton(data.SessionID))
		default:
			b.WriteString(`<p class="muted">Review the changes the agent made in the sandbox. Nothing is written to your host files until you approve.</p>`)
			b.WriteString(`<pre class="diff">` + templ.EscapeString(data.Diff) + `</pre>`)
			commit := "/sessions/" + templ.EscapeString(data.SessionID) + "/commit"
			reject := "/sessions/" + templ.EscapeString(data.SessionID) + "/reject"
			b.WriteString(`<div class="actions">`)
			b.WriteString(`<form hx-post="` + commit + `" hx-target="#hostchanges" hx-swap="innerHTML" style="display:inline">` +
				`<button type="submit" class="primary">Approve &amp; commit to host</button></form> `)
			b.WriteString(`<form hx-post="` + reject + `" hx-target="#hostchanges" hx-swap="innerHTML" style="display:inline">` +
				`<button type="submit" class="deny">Reject</button></form>`)
			b.WriteString(`</div>`)
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}

// CommitData is the view model for the outcome of a member commit/reject action.
type CommitData struct {
	SessionID string
	Committed bool
	Rejected  bool
	Detail    string
	Error     string
}

// CommitResult renders the outcome of an explicit commit or reject, then offers
// to review host changes again.
func CommitResult(data CommitData) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		switch {
		case data.Error != "":
			b.WriteString(`<p class="error" role="alert">` + templ.EscapeString(data.Error) + `</p>`)
		case data.Committed:
			b.WriteString(`<p class="ok">Committed to your host files.</p>`)
			if strings.TrimSpace(data.Detail) != "" {
				b.WriteString(`<pre class="diff">` + templ.EscapeString(data.Detail) + `</pre>`)
			}
		case data.Rejected:
			b.WriteString(`<p class="muted">Rejected. Nothing was committed; the changes remain only in the sandbox.</p>`)
		}
		b.WriteString(reviewButton(data.SessionID))
		_, err := io.WriteString(w, b.String())
		return err
	})
}

// reviewButton renders the HTMX action that (re)loads the pending diff into the
// #hostchanges panel.
func reviewButton(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	get := "/sessions/" + templ.EscapeString(sessionID) + "/diff"
	return `<button hx-get="` + get + `" hx-target="#hostchanges" hx-swap="innerHTML">Review host changes</button>`
}

// --- Matrix fan-out (design §6.3) ---------------------------------------------

// MatrixAgentOption is one selectable entitled agent on the matrix form.
type MatrixAgentOption struct {
	ID      string
	Name    string
	Version string
}

// MatrixData is the view model for the matrix form page: the entitled agents to
// fan across, plus any prior input echoed back on error.
type MatrixData struct {
	Agents      []MatrixAgentOption
	AgentsError string
	Directive   string
	Scorer      string
	Error       string
}

// MatrixPage renders the matrix fan-out form: a directive, a scorer, and a set
// of entitled agents to run it across. Results stream into #matrix-results.
func MatrixPage(data MatrixData) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		body := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			var b strings.Builder
			b.WriteString(`<section class="card">`)
			b.WriteString(`<p><a href="/">&larr; Home</a></p>`)
			b.WriteString(`<h1>Matrix</h1>`)
			b.WriteString(`<p class="muted">Fan one directive across several agents, score each branch, and promote the winner. Each branch runs as its own governed session (design §6.3).</p>`)
			if data.Error != "" {
				b.WriteString(`<p class="error" role="alert">` + templ.EscapeString(data.Error) + `</p>`)
			}
			if data.AgentsError != "" {
				b.WriteString(`<p class="error" role="alert">` + templ.EscapeString(data.AgentsError) + `</p>`)
			} else if len(data.Agents) == 0 {
				b.WriteString(`<p class="muted">No agents are available to you yet.</p>`)
			} else {
				b.WriteString(`<form hx-post="/matrix" hx-target="#matrix-results" hx-swap="innerHTML">`)
				b.WriteString(`<p><label>Directive<br><textarea name="directive" rows="3" required placeholder="Describe what each agent should do…">` + templ.EscapeString(data.Directive) + `</textarea></label></p>`)
				b.WriteString(`<p><label>Scorer<br><textarea name="scorer" rows="2" required placeholder="Ask the agent to print a single numeric score; higher wins.">` + templ.EscapeString(data.Scorer) + `</textarea></label></p>`)
				b.WriteString(`<fieldset><legend>Agents</legend>`)
				for _, a := range data.Agents {
					label := a.Name
					if a.Version != "" {
						label += " (v" + a.Version + ")"
					}
					b.WriteString(`<label class="agent"><input type="checkbox" name="agent" value="` + templ.EscapeString(a.ID) + `"/> ` + templ.EscapeString(label) + `</label> `)
				}
				b.WriteString(`</fieldset>`)
				b.WriteString(`<p><button type="submit" class="primary">Run matrix</button></p>`)
				b.WriteString(`</form>`)
			}
			b.WriteString(`<div id="matrix-results"></div>`)
			b.WriteString(`</section>`)
			_, err := io.WriteString(w, b.String())
			return err
		})
		return Layout("Matrix", true).Render(templ.WithChildren(ctx, body), w)
	})
}

// MatrixRow is one ranked branch in the matrix result table.
type MatrixRow struct {
	Rank    string
	Agent   string
	Session string
	Score   string
	Status  string
	Detail  string
}

// MatrixResultData is the view model for the ranked matrix result fragment.
type MatrixResultData struct {
	Error         string
	Rows          []MatrixRow
	Winner        string
	AllTerminated bool
}

// MatrixResults renders the ranked branch table, the winner, and the
// terminate-all confirmation as the inner markup of #matrix-results.
func MatrixResults(data MatrixResultData) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		if data.Error != "" {
			b.WriteString(`<p class="error" role="alert">` + templ.EscapeString(data.Error) + `</p>`)
		}
		if len(data.Rows) > 0 {
			b.WriteString(`<table><thead><tr><th class="num">Rank</th><th>Agent</th><th>Session</th><th class="num">Score</th><th>Status</th></tr></thead><tbody>`)
			for _, r := range data.Rows {
				b.WriteString(`<tr>`)
				b.WriteString(`<td class="num">` + templ.EscapeString(r.Rank) + `</td>`)
				b.WriteString(`<td>` + templ.EscapeString(r.Agent) + `</td>`)
				b.WriteString(`<td class="mono">` + templ.EscapeString(orDashStr(r.Session)) + `</td>`)
				b.WriteString(`<td class="num">` + templ.EscapeString(orDashStr(r.Score)) + `</td>`)
				b.WriteString(`<td>` + templ.EscapeString(r.Status))
				if strings.TrimSpace(r.Detail) != "" {
					b.WriteString(` <span class="meta">` + templ.EscapeString(r.Detail) + `</span>`)
				}
				b.WriteString(`</td></tr>`)
			}
			b.WriteString(`</tbody></table>`)
		}
		if data.Winner != "" {
			b.WriteString(`<p class="ok">Winner: <strong>` + templ.EscapeString(data.Winner) + `</strong></p>`)
		} else if data.Error == "" {
			b.WriteString(`<p class="muted">No winning branch — all branches failed.</p>`)
		}
		if data.AllTerminated {
			b.WriteString(`<p class="muted">All sessions terminated.</p>`)
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}
