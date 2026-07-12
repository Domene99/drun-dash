package views

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

// This file holds the conversational run view (ADR-0004 §3–§5). Unlike the other
// views it is authored directly as Go [templ.Component]s rather than generated
// from a .templ file: the run view needs string-returning fragment renderers for
// the Server-Sent Events stream (each step is one `data:` line), which templ's
// component-only output does not express, and the console's build/test runs in a
// network-isolated sandbox without the `templ` binary. The components compose
// with the generated [Layout] exactly as the generated views do.

// sseExtensionURL is the htmx Server-Sent Events extension, pulled in only on the
// run page (the login/home pages do not stream). It drives the live turn
// transcript: the assistant bubble subscribes to the run's SSE stream and
// appends each rendered step. Pinned for reproducibility, matching the core htmx
// pin in [Layout].
const sseExtensionURL = "https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.2/dist/sse.min.js"

// RunData is the view model for the run view: the launched session's non-secret
// facts (never the proxy token), the resolved model, and the bound drun engine's
// state. When the engine could not be bound EngineError explains why and the
// chat input is withheld — fail-closed, no ungoverned run (ADR-0004 §3/§4).
type RunData struct {
	SessionID    string
	Agent        string
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

// RunPage renders the full run view inside the shared [Layout]: the session
// summary, the bound engine's tool set (or the bind error), and — when the run
// is ready — the conversation transcript and chat input.
func RunPage(data RunData) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		body := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			return renderRunBody(w, data)
		})
		return Layout("Run", true).Render(templ.WithChildren(ctx, body), w)
	})
}

// renderRunBody writes the run view's body markup.
func renderRunBody(w io.Writer, data RunData) error {
	var b strings.Builder

	b.WriteString(`<script src="` + sseExtensionURL + `" defer></script>`)

	// Session summary.
	b.WriteString(`<section class="card">`)
	b.WriteString(`<p><a href="/">&larr; All sessions</a></p>`)
	b.WriteString(`<h1>Run · ` + templ.EscapeString(data.Agent) + `</h1>`)
	b.WriteString(`<table><tbody>`)
	summaryRow(&b, "Session", data.SessionID, true)
	summaryRow(&b, "Proxy base URL", data.ProxyBaseURL, true)
	summaryRow(&b, "Model", orDashStr(data.Model), true)
	summaryRow(&b, "Fan-out", data.FanOut, false)
	summaryRow(&b, "Expires", data.Expires, false)
	b.WriteString(`</tbody></table>`)
	b.WriteString(`</section>`)

	// Engine / tool-set state.
	b.WriteString(`<section class="card">`)
	b.WriteString(`<h2>Sandbox engine</h2>`)
	switch {
	case data.EngineError != "":
		b.WriteString(`<p class="error" role="alert">` + templ.EscapeString(data.EngineError) + `</p>`)
		b.WriteString(`<p class="muted">Start your local drun engine and reload this page. Nothing runs until the engine is reachable.</p>`)
	case data.EngineReady:
		scope := "all discovered drun tools (the engine and proxy enforce policy at execution)"
		if data.PolicyScoped {
			scope = "the policy bundle's allowed tools"
		}
		b.WriteString(`<p class="muted">Bound to the local drun engine. Tools offered to the model: ` + templ.EscapeString(scope) + `.</p>`)
		if len(data.Tools) > 0 {
			b.WriteString(`<p class="tools">`)
			for i, t := range data.Tools {
				if i > 0 {
					b.WriteString(" ")
				}
				b.WriteString(`<code>` + templ.EscapeString(t) + `</code>`)
			}
			b.WriteString(`</p>`)
		}
	default:
		b.WriteString(`<p class="muted">Connecting to the local drun engine…</p>`)
	}
	b.WriteString(`</section>`)

	// Conversation.
	b.WriteString(`<section class="card">`)
	b.WriteString(`<h2>Conversation</h2>`)
	b.WriteString(`<div id="transcript"></div>`)
	switch {
	case data.ModelMissing:
		b.WriteString(`<p class="error" role="alert">No model is configured. Set MEMBER_CONSOLE_MODEL to enable runs.</p>`)
	case !data.EngineReady:
		b.WriteString(`<p class="muted">The chat is available once the drun engine is bound.</p>`)
	default:
		b.WriteString(chatForm(data.SessionID))
	}
	b.WriteString(`</section>`)

	_, err := io.WriteString(w, b.String())
	return err
}

// chatForm renders the directive input. Submitting appends the turn (the user's
// message plus a live-streaming assistant bubble) to the transcript via HTMX.
func chatForm(sessionID string) string {
	post := "/sessions/" + templ.EscapeString(sessionID) + "/turn"
	return `<form class="chat" hx-post="` + post + `" hx-target="#transcript" hx-swap="beforeend" hx-on::after-request="this.reset()">` +
		`<textarea name="directive" rows="3" placeholder="Describe what the agent should do…" required></textarea>` +
		`<button type="submit" class="primary">Send</button>` +
		`</form>`
}

// summaryRow writes one label/value row of the session summary, monospacing the
// value when mono is set.
func summaryRow(b *strings.Builder, label, value string, mono bool) {
	cls := ""
	if mono {
		cls = ` class="mono"`
	}
	b.WriteString(`<tr><th>` + templ.EscapeString(label) + `</th><td` + cls + `>` + templ.EscapeString(orDashStr(value)) + `</td></tr>`)
}

// TurnStarted renders the fragment appended to the transcript when a directive
// is submitted: the member's message, then an assistant bubble that subscribes
// to the run's SSE stream and appends each step until the stream sends `done`.
func TurnStarted(sessionID, directive string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		stream := "/sessions/" + templ.EscapeString(sessionID) + "/stream"
		html := `<div class="turn">` +
			`<div class="msg user">` + multiline(directive) + `</div>` +
			`<div class="msg agent" hx-ext="sse" sse-connect="` + stream + `" sse-swap="step" hx-swap="beforeend" sse-close="done"></div>` +
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
		return `<div class="step model">Calling the model…</div>`
	case "tool_call":
		return `<div class="step tool">&#128295; <code>` + templ.EscapeString(s.ToolName) + `</code>` + metaSpan(s) + `</div>`
	case "tool_result":
		return `<div class="step result">&#8627; <code>` + templ.EscapeString(s.ToolName) + `</code> result` + metaSpan(s) + `</div>`
	case "final":
		return `<div class="step final">` + multiline(s.Text) + `</div>`
	case "blocked":
		return `<div class="step blocked">&#9940; <code>` + templ.EscapeString(s.ToolName) + `</code> blocked by policy: ` + multiline(s.Text) + `</div>`
	case "escalated":
		return `<div class="step escalated"><code>` + templ.EscapeString(s.ToolName) + `</code> escalation: ` + templ.EscapeString(orDashStr(s.Decision)) + `</div>`
	default:
		return ""
	}
}

// RenderErrorFragment renders a turn-ending error as a single-line step fragment.
func RenderErrorFragment(msg string) string {
	return `<div class="step error" role="alert">` + multiline(msg) + `</div>`
}

// metaSpan renders the structure-only shape/size of a tool call's arguments or
// result, or nothing when no metadata is known.
func metaSpan(s StepView) string {
	if !s.HasMeta {
		return ""
	}
	return ` <span class="meta">(` + templ.EscapeString(orDashStr(s.ValueType)) + `, ` + strconv.Itoa(s.ByteSize) + ` B)</span>`
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
