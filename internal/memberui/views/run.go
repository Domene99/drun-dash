package views

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/Domene99/drun-dash/internal/uikit"
)

// This file holds the conversational run view (ADR-0004 §3–§5) — the chat-first
// member surface (ADR-0007 §4). Unlike the login/home pages it is authored
// directly as Go [templ.Component]s rather than generated from a .templ file:
// the run view needs string-returning fragment renderers for the Server-Sent
// Events stream (each step is one `data:` line), which templ's component-only
// output does not express. The markup is restyled onto the shared design system
// (internal/uikit, ADR-0005): it uses uikit's uk-* component classes and the
// small member conversation classes (mc-*, see styles.go) so it renders with the
// same tokens as the admin audit view. It composes with the generated [Layout]
// exactly as the generated views do.

// RunData is the view model for the run view: the launched session's non-secret
// facts (never the proxy token), the resolved model, and the bound drun engine's
// state. When the engine could not be bound EngineError explains why and the
// chat input is withheld — fail-closed, no ungoverned run (ADR-0004 §3/§4).
type RunData struct {
	SessionID    string
	Agent        string
	AgentName    string
	ProxyBaseURL string
	FanOut       string
	Expires      string
	Model        string

	EngineReady  bool
	EngineError  string
	Tools        []string
	PolicyScoped bool
	ModelMissing bool
}

// StepView is the render model for one streamed turn step (ADR-0004 §4). Only
// structure survives here: the tool name and the argument/result shape and size;
// Text carries model-authored content (a final answer or a block reason) that
// the console renders locally.
type StepView struct {
	Kind      string
	ToolName  string
	Text      string
	ValueType string
	ByteSize  int
	HasMeta   bool
	Decision  string
}

// copyScript is the tiny inline vanilla-JS copy helper included on the run page
// (issue #294). The run view is hosted in a native webview with no macOS Edit
// menu, so Cmd+C never fires and a member cannot copy the agent's replies or
// code out of the conversation. This installs a single delegated click handler
// on [data-copy] controls (the mc-copy buttons rendered on agent bubbles, final
// answers, and code/diff blocks): it reads the target element's textContent and
// writes it to the clipboard via the async Clipboard API, briefly showing a
// "Copied" state. It degrades gracefully — when navigator.clipboard is missing
// (older/insecure webviews) it selects the text instead and never throws. No
// framework, no CDN.
const copyScript = `<script>
(function () {
	function target(btn) {
		var sel = btn.getAttribute("data-copy");
		if (sel) {
			var scope = btn.parentElement;
			return (scope && scope.querySelector(sel)) || btn.closest(sel);
		}
		return btn.parentElement;
	}
	function textOf(el) {
		if (!el) return "";
		var clone = el.cloneNode(true);
		var copies = clone.querySelectorAll(".mc-copy");
		for (var i = 0; i < copies.length; i++) copies[i].remove();
		return (clone.innerText || clone.textContent || "").replace(/^\s+|\s+$/g, "");
	}
	function selectText(el) {
		if (!el) return;
		try {
			var sel = window.getSelection();
			var range = document.createRange();
			range.selectNodeContents(el);
			sel.removeAllRanges();
			sel.addRange(range);
		} catch (e) { /* selection unsupported — nothing to do */ }
	}
	function flash(btn) {
		var prev = btn.getAttribute("data-label") || btn.textContent;
		btn.setAttribute("data-label", prev);
		btn.classList.add("is-copied");
		btn.textContent = "Copied";
		setTimeout(function () {
			btn.classList.remove("is-copied");
			btn.textContent = prev;
		}, 1200);
	}
	document.addEventListener("click", function (e) {
		var btn = e.target.closest(".mc-copy");
		if (!btn) return;
		e.preventDefault();
		var el = target(btn);
		var value = textOf(el);
		if (navigator.clipboard && navigator.clipboard.writeText) {
			navigator.clipboard.writeText(value).then(function () { flash(btn); }, function () { selectText(el); });
		} else {
			selectText(el);
		}
	});
})();
</script>`

// copyButton renders the small "Copy" affordance (issue #294). copySelector,
// when non-empty, is a CSS selector resolved (by copyScript) within the button's
// parent to find the element whose text is copied; empty means copy the button's
// parent element. title is the accessible/hover hint. The rendered control is a
// single line of HTML so it is safe to embed in an SSE `data:` step fragment.
func copyButton(copySelector, title string) string {
	s := `<button type="button" class="mc-copy" data-copy="` + templ.EscapeString(copySelector) + `"`
	if title != "" {
		s += ` title="` + templ.EscapeString(title) + `"`
	}
	s += `>Copy</button>`
	return s
}

// RunPage renders the full run view inside the shared [Layout]: the session
// summary, the bound engine's tool set (or the bind error), and — when the run
// is ready — the conversation transcript and chat input.
func RunPage(data RunData) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		body := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			return renderRunBody(w, data)
		})
		return Layout("Conversation", true).Render(templ.WithChildren(ctx, body), w)
	})
}

// runTitle is the readable page title for a run: the agent's name, falling back
// to its id when the name was not carried through (ADR-0005 §4 — a name, not a
// UUID, is the headline).
func runTitle(data RunData) string {
	if strings.TrimSpace(data.AgentName) != "" {
		return data.AgentName
	}
	return orDashStr(data.Agent)
}

// renderRunBody writes the run view's body markup.
func renderRunBody(w io.Writer, data RunData) error {
	var b strings.Builder

	// The htmx SSE extension is pulled in only on the run page (login/home do
	// not stream); it drives the live turn transcript. Served locally by
	// uikit.Handler() so the governed app never depends on a CDN — matching the
	// core htmx that uikit.Head links.
	b.WriteString(`<script src="` + uikit.SSEExtensionURL() + `" defer></script>`)

	// The tiny inline copy helper (issue #294): gives every mc-copy button a
	// working Clipboard-API copy, so the agent's replies and code are copyable
	// inside the native webview that has no Edit menu. Loaded only here.
	b.WriteString(copyScript)

	// Page header: agent name as the readable title, with a back link.
	b.WriteString(`<div class="uk-page-header">`)
	b.WriteString(`<div class="uk-page-header-text">`)
	b.WriteString(`<h1>` + templ.EscapeString(runTitle(data)) + `</h1>`)
	b.WriteString(`<p class="uk-page-header-desc">A governed conversation — the model runs through the proxy, tools run in your local drun sandbox.</p>`)
	b.WriteString(`</div>`)
	b.WriteString(`<div class="uk-page-header-action"><a class="uk-btn uk-btn--secondary uk-btn--sm" href="/">&larr; All conversations</a></div>`)
	b.WriteString(`</div>`)

	// Session summary: readable facts, with the session UUID as a copyable chip.
	b.WriteString(`<section class="uk-card">`)
	b.WriteString(`<div class="mc-summary">`)
	b.WriteString(idChip("Session", data.SessionID))
	b.WriteString(`<span>Model <strong>` + templ.EscapeString(orDashStr(data.Model)) + `</strong></span>`)
	b.WriteString(`<span>Fan-out <strong>` + templ.EscapeString(orDashStr(data.FanOut)) + `</strong></span>`)
	b.WriteString(`<span>Expires <strong>` + templ.EscapeString(orDashStr(data.Expires)) + `</strong></span>`)
	b.WriteString(`</div>`)
	b.WriteString(`<p class="uk-muted">Proxy base URL: <span class="uk-mono">` + templ.EscapeString(orDashStr(data.ProxyBaseURL)) + `</span></p>`)
	b.WriteString(`</section>`)

	// Engine / tool-set state.
	b.WriteString(`<section class="uk-card">`)
	b.WriteString(`<div class="uk-card-header"><h2>Sandbox engine</h2></div>`)
	switch {
	case data.EngineError != "":
		b.WriteString(alertDanger(data.EngineError))
		b.WriteString(`<p class="uk-muted">Start your local drun engine and reload this page. Nothing runs until the engine is reachable.</p>`)
	case data.EngineReady:
		scope := "all discovered drun tools (the engine and proxy enforce policy at execution)"
		if data.PolicyScoped {
			scope = "the policy bundle's allowed tools"
		}
		b.WriteString(`<p class="uk-muted">Bound to the local drun engine. Tools offered to the model: ` + templ.EscapeString(scope) + `.</p>`)
		if len(data.Tools) > 0 {
			b.WriteString(`<div class="mc-tools">`)
			for _, t := range data.Tools {
				b.WriteString(`<span class="uk-badge uk-badge--neutral uk-mono">` + templ.EscapeString(t) + `</span>`)
			}
			b.WriteString(`</div>`)
		}
	default:
		b.WriteString(`<p class="uk-muted">Connecting to the local drun engine…</p>`)
	}
	b.WriteString(`</section>`)

	if data.EngineReady {
		// Checkpoint timeline (design §6.3): HTMX-refreshed fragment that shows
		// the run's checkpoints (and any forks) as the loop proceeds.
		b.WriteString(`<section class="uk-card">`)
		b.WriteString(`<div class="uk-card-header"><h2>Checkpoint timeline</h2></div>`)
		b.WriteString(`<div id="timeline" hx-get="/sessions/` + templ.EscapeString(data.SessionID) + `/timeline" hx-trigger="load, every 5s"><p class="uk-muted">Loading timeline…</p></div>`)
		b.WriteString(`</section>`)

		// Diff-before-commit (design §6.3): the only path to host files, gated
		// on an explicit member review + approve action.
		b.WriteString(`<section class="uk-card">`)
		b.WriteString(`<div class="uk-card-header"><h2>Host changes</h2></div>`)
		b.WriteString(`<p class="uk-muted">The agent's work stays in the sandbox. Nothing is written to your host files until you review the diff and approve.</p>`)
		b.WriteString(`<div id="hostchanges">` + reviewButton(data.SessionID) + `</div>`)
		b.WriteString(`</section>`)
	}

	// Conversation.
	b.WriteString(`<section class="uk-card">`)
	b.WriteString(`<div class="uk-card-header"><h2>Conversation</h2></div>`)
	b.WriteString(`<div id="transcript" class="mc-transcript"></div>`)
	switch {
	case data.ModelMissing:
		b.WriteString(alertDanger("No model is configured. Set MEMBER_CONSOLE_MODEL to enable runs."))
	case !data.EngineReady:
		b.WriteString(`<p class="uk-muted">The chat is available once the drun engine is bound.</p>`)
	default:
		b.WriteString(chatForm(data.SessionID))
	}
	b.WriteString(`</section>`)

	_, err := io.WriteString(w, b.String())
	return err
}

// chatForm renders the prominent directive composer. Submitting appends the turn
// (the user's message plus a live-streaming assistant bubble) to the transcript
// via HTMX. The composer's <textarea> accepts paste normally in the webview — no
// special handling is needed for pasting into a focused input (issue #294).
func chatForm(sessionID string) string {
	post := "/sessions/" + templ.EscapeString(sessionID) + "/turn"
	return `<form class="mc-composer" hx-post="` + post + `" hx-target="#transcript" hx-swap="beforeend" hx-on::after-request="this.reset()">` +
		`<textarea class="uk-textarea" name="directive" rows="3" placeholder="Describe what the agent should do…" required></textarea>` +
		`<div class="mc-composer-actions"><button type="submit" class="uk-btn uk-btn--primary">Send</button></div>` +
		`</form>`
}

// idChip renders a copyable, monospaced, truncated identifier — the shared uikit
// ID chip (ADR-0005 §4), emitted as a string so it composes with the SSE-driven
// run view's string builder. Clicking copies the full id via theme-toggle.js.
func idChip(label, id string) string {
	esc := templ.EscapeString(id)
	s := `<button type="button" class="uk-id-chip" data-uk-copy="` + esc + `" title="Copy ` + esc + `">`
	if label != "" {
		s += `<span class="uk-muted">` + templ.EscapeString(label) + `</span>`
	}
	s += `<span class="uk-id-chip-value">` + templ.EscapeString(shortID(id)) + `</span></button>`
	return s
}

// alertDanger renders an inline danger alert (uikit) as a string for the run
// view's string builder.
func alertDanger(msg string) string {
	return `<div class="uk-alert uk-alert--danger" role="alert"><div class="uk-alert-body">` + templ.EscapeString(msg) + `</div></div>`
}

// TurnStarted renders the fragment appended to the transcript when a directive
// is submitted: the member's message bubble, then an assistant bubble that
// subscribes to the run's SSE stream and appends each step until the stream
// sends `done`. The assistant bubble carries a Copy button (issue #294) that
// copies the whole streamed reply — the copyScript strips the button's own text.
func TurnStarted(sessionID, directive string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		stream := "/sessions/" + templ.EscapeString(sessionID) + "/stream"
		html := `<div class="mc-turn">` +
			`<div class="mc-msg mc-msg--user">` + multiline(directive) + `</div>` +
			`<div class="mc-msg mc-msg--agent" hx-ext="sse" sse-connect="` + stream + `" sse-swap="step" hx-swap="beforeend" sse-close="done">` +
			copyButton("", "Copy the agent's reply") +
			`</div>` +
			`</div>`
		_, err := io.WriteString(w, html)
		return err
	})
}

// RenderStepFragment renders one streamed step as the single-line HTML that the
// SSE `step` event carries in its `data:` field. It must contain no raw newlines
// (each would start a new SSE data line), so model text is escaped and its
// newlines become <br>.
func RenderStepFragment(s StepView) string {
	switch s.Kind {
	case "model_call":
		return `<div class="mc-step mc-step--model">Calling the model…</div>`
	case "tool_call":
		return `<div class="mc-step mc-step--tool">&#128295; <code>` + templ.EscapeString(s.ToolName) + `</code>` + metaSpan(s) + `</div>`
	case "tool_result":
		return `<div class="mc-step mc-step--result">&#8627; <code>` + templ.EscapeString(s.ToolName) + `</code> result` + metaSpan(s) + `</div>`
	case "final":
		// The final answer carries the model's text — often code the member
		// wants to copy verbatim. Wrap it so a per-answer Copy button (issue
		// #294) can target just the answer text.
		return `<div class="mc-step mc-step--final"><span class="mc-final-text">` + multiline(s.Text) + `</span>` + copyButton(".mc-final-text", "Copy this answer") + `</div>`
	case "blocked":
		return `<div class="mc-step mc-step--blocked">&#9940; <code>` + templ.EscapeString(s.ToolName) + `</code> blocked by policy: ` + multiline(s.Text) + `</div>`
	case "escalated":
		return `<div class="mc-step mc-step--escalated"><code>` + templ.EscapeString(s.ToolName) + `</code> escalation: ` + templ.EscapeString(orDashStr(s.Decision)) + `</div>`
	default:
		return ""
	}
}

// RenderErrorFragment renders a turn-ending error as a single-line step fragment.
func RenderErrorFragment(msg string) string {
	return `<div class="mc-step mc-step--error" role="alert">` + multiline(msg) + `</div>`
}

// metaSpan renders the structure-only shape/size of a tool call's arguments or
// result, or nothing when no metadata is known.
func metaSpan(s StepView) string {
	if !s.HasMeta {
		return ""
	}
	return ` <span class="mc-step-meta">(` + templ.EscapeString(orDashStr(s.ValueType)) + `, ` + strconv.Itoa(s.ByteSize) + ` B)</span>`
}

// multiline escapes text for HTML and converts newlines to <br> so it survives
// as a single SSE data line.
func multiline(s string) string {
	esc := templ.EscapeString(s)
	esc = strings.ReplaceAll(esc, "\r\n", "\n")
	esc = strings.ReplaceAll(esc, "\r", "\n")
	return strings.ReplaceAll(esc, "\n", "<br>")
}

// orDashStr returns s, or an em dash when s is empty.
func orDashStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// RenderEscalationFragment renders the pending/resolved state of a policy
// escalation as a single-line SSE step fragment (ADR-0004 §6). status is the
// approval status the console polled: "pending" while it waits for a decision,
// "approved" when the blocked tool will resume, "error" when the approval could
// not be raised, or a terminal denial ("denied"/"expired"). It must contain no
// raw newlines (it is one SSE data line).
func RenderEscalationFragment(action, approvalID, status string) string {
	code := "<code>" + templ.EscapeString(action) + "</code>"
	switch status {
	case "pending":
		id := ""
		if approvalID != "" {
			id = ` <code>` + templ.EscapeString(approvalID) + `</code>`
		}
		return `<div class="mc-step mc-step--escalated">&#9203; ` + code + ` — waiting for approval` + id + `…</div>`
	case "approved":
		return `<div class="mc-step mc-step--escalated">&#9989; ` + code + ` — approved; resuming the action</div>`
	case "error":
		return `<div class="mc-step mc-step--escalated">&#9888; ` + code + ` — could not raise an approval; denied</div>`
	default:
		return `<div class="mc-step mc-step--escalated">&#9940; ` + code + ` — ` + templ.EscapeString(orDashStr(status)) + `; not run</div>`
	}
}
