package views

import (
	"context"
	"io"
	"strings"

	"github.com/a-h/templ"
)

// This file holds the run view's dynamic panels — the checkpoint timeline and
// the diff-before-commit review (design §6.3). Like run.go they are authored
// directly as Go [templ.Component]s rather than generated from .templ files:
// they are HTMX fragments swapped into the run page, restyled onto the shared
// design system (internal/uikit, ADR-0005) with uikit's uk-* classes and the
// member conversation classes (mc-*, see styles.go). They compose with the
// generated [Layout] exactly as the generated views do.
//
// The matrix fan-out view was removed from the member surface (ADR-0007 §4): the
// member experience is chat-first, and matrix — a batch-oriented, numeric-scored
// fan-out — does not belong here (its scorer fed model text to a numeric scorer).
// The internal/matrix package is untouched; only the member console's view of it
// is gone.

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
			b.WriteString(`<p class="uk-muted">A run is in progress; the timeline refreshes when it settles.</p>`)
		case data.Error != "":
			b.WriteString(alertDanger(data.Error))
		case len(data.Entries) == 0:
			b.WriteString(`<p class="uk-muted">No checkpoints yet.</p>`)
		default:
			b.WriteString(`<ol class="mc-timeline">`)
			for _, e := range data.Entries {
				b.WriteString(`<li><code>#` + templ.EscapeString(e.ID) + `</code>`)
				if e.IsCurrent {
					b.WriteString(` <span class="uk-badge uk-badge--accent">current</span>`)
				}
				if e.Label != "" {
					b.WriteString(` <strong>` + templ.EscapeString(e.Label) + `</strong>`)
				}
				b.WriteString(` <span class="mc-step-meta">parent ` + templ.EscapeString(e.Parent) + ` · ` + templ.EscapeString(e.Delta) + ` files</span>`)
				for _, f := range e.Forks {
					b.WriteString(` <span class="mc-step-meta">forked &rarr; <code>` + templ.EscapeString(f) + `</code></span>`)
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
			b.WriteString(`<p class="uk-muted">A run is in progress; review host changes once it settles.</p>`)
			b.WriteString(reviewButton(data.SessionID))
		case data.Error != "":
			b.WriteString(alertDanger(data.Error))
			b.WriteString(reviewButton(data.SessionID))
		case data.Empty:
			b.WriteString(`<p class="uk-muted">No pending host changes. Nothing to commit.</p>`)
			b.WriteString(reviewButton(data.SessionID))
		default:
			b.WriteString(`<p class="uk-muted">Review the changes the agent made in the sandbox. Nothing is written to your host files until you approve.</p>`)
			b.WriteString(`<pre class="mc-diff">` + templ.EscapeString(data.Diff) + `</pre>`)
			commit := "/sessions/" + templ.EscapeString(data.SessionID) + "/commit"
			reject := "/sessions/" + templ.EscapeString(data.SessionID) + "/reject"
			b.WriteString(`<div class="mc-actions">`)
			b.WriteString(`<form hx-post="` + commit + `" hx-target="#hostchanges" hx-swap="innerHTML">` +
				`<button type="submit" class="uk-btn uk-btn--primary">Approve &amp; commit to host</button></form>`)
			b.WriteString(`<form hx-post="` + reject + `" hx-target="#hostchanges" hx-swap="innerHTML">` +
				`<button type="submit" class="uk-btn uk-btn--danger">Reject</button></form>`)
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
			b.WriteString(alertDanger(data.Error))
		case data.Committed:
			b.WriteString(`<div class="uk-alert uk-alert--success" role="status"><div class="uk-alert-body">Committed to your host files.</div></div>`)
			if strings.TrimSpace(data.Detail) != "" {
				b.WriteString(`<pre class="mc-diff">` + templ.EscapeString(data.Detail) + `</pre>`)
			}
		case data.Rejected:
			b.WriteString(`<p class="uk-muted">Rejected. Nothing was committed; the changes remain only in the sandbox.</p>`)
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
	return `<button class="uk-btn uk-btn--secondary uk-btn--sm" hx-get="` + get + `" hx-target="#hostchanges" hx-swap="innerHTML">Review host changes</button>`
}
