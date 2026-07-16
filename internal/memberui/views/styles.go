package views

import "github.com/a-h/templ"

// This file supplies the two small, hand-authored pieces the member shell needs
// on top of the shared design system (internal/uikit, ADR-0005): the
// member-scoped conversation stylesheet and the top-bar user menu.
//
// uikit owns the design system — the tokens (:root light + dark) and every
// generic component (Shell, Card, Table, Button, Badge, Alert, …). The member
// app does NOT invent new design tokens or restyle those components. It only
// adds the handful of conversation-specific presentation rules uikit does not
// carry (the transcript bubbles, tool-step chips, the diff/timeline panels, the
// device-code display), and every one of them is built on uikit's CSS custom
// properties, so the member surface themes with the rest and introduces no new
// colors. Keeping these here (rather than a served asset) keeps the member
// package self-contained: uikit's stylesheet and script are still the only
// mounted assets both apps share.

// memberConsoleCSS is the member-scoped conversation styling, layered on the
// uikit tokens loaded by uikit.Head. Every rule references a uikit custom
// property (--space-*, --surface-*, --text-*, semantic --*-fg/-bg/-border), so
// it inherits light/dark theming from the design system automatically.
const memberConsoleCSS = `
/* Device-code display on the sign-in page. */
.mc-usercode {
	font-family: var(--font-mono);
	font-size: var(--text-2xl);
	font-weight: var(--weight-bold);
	letter-spacing: 0.18em;
	color: var(--text);
	margin: var(--space-3) 0;
}

/* Conversation transcript: user vs assistant bubbles. */
.mc-transcript {
	display: flex;
	flex-direction: column;
	gap: var(--space-4);
	margin-bottom: var(--space-4);
}
.mc-transcript:empty { display: none; }
.mc-turn { display: flex; flex-direction: column; gap: var(--space-2); }
.mc-msg {
	padding: var(--space-3) var(--space-4);
	border-radius: var(--radius-md);
	max-width: 85%;
	font-size: var(--text-sm);
	line-height: var(--leading-normal);
}
.mc-msg--user {
	align-self: flex-end;
	background: var(--accent-subtle);
	color: var(--text);
	border: 1px solid var(--info-border);
}
.mc-msg--agent {
	align-self: flex-start;
	min-width: 14rem;
	background: var(--surface-sunken);
	color: var(--text);
	border: 1px solid var(--border);
	display: flex;
	flex-direction: column;
	gap: var(--space-1);
}

/* One streamed turn step (tool call / result / final answer / block). */
.mc-step {
	display: flex;
	align-items: baseline;
	flex-wrap: wrap;
	gap: var(--space-2);
	font-size: var(--text-sm);
	color: var(--text);
}
.mc-step code {
	background: var(--surface);
	border: 1px solid var(--border);
	border-radius: var(--radius-sm);
	padding: 0 var(--space-1);
}
.mc-step--model,
.mc-step--result { color: var(--text-muted); }
.mc-step--final {
	font-size: var(--text-md);
	color: var(--text);
	white-space: normal;
}
.mc-step--blocked,
.mc-step--error { color: var(--danger-fg); }
.mc-step--escalated { color: var(--warning-fg); }
.mc-step-meta { color: var(--text-subtle); font-size: var(--text-xs); }

/* Tool set offered to the model. */
.mc-tools {
	display: flex;
	flex-wrap: wrap;
	gap: var(--space-2);
	margin: var(--space-2) 0 0;
}

/* The prominent composer at the foot of the conversation. */
.mc-composer {
	display: flex;
	flex-direction: column;
	gap: var(--space-3);
	margin-top: var(--space-4);
}
.mc-composer .mc-composer-actions {
	display: flex;
	justify-content: flex-end;
}

/* Diff-before-commit panel + checkpoint timeline. */
.mc-diff {
	background: var(--surface-sunken);
	color: var(--text);
	border: 1px solid var(--border);
	border-radius: var(--radius-md);
	padding: var(--space-3);
	overflow: auto;
	max-height: 24rem;
	font-family: var(--font-mono);
	font-size: var(--text-xs);
	white-space: pre;
}
.mc-timeline {
	margin: 0;
	padding-left: var(--space-5);
	display: flex;
	flex-direction: column;
	gap: var(--space-1);
	font-size: var(--text-sm);
}
.mc-timeline code {
	background: var(--surface-sunken);
	border: 1px solid var(--border);
	border-radius: var(--radius-sm);
	padding: 0 var(--space-1);
}
.mc-actions {
	display: flex;
	gap: var(--space-2);
	margin-top: var(--space-2);
}
.mc-summary {
	display: flex;
	flex-wrap: wrap;
	align-items: center;
	gap: var(--space-2) var(--space-4);
	margin-bottom: var(--space-3);
	color: var(--text-muted);
	font-size: var(--text-sm);
}
.mc-summary strong { color: var(--text); font-weight: var(--weight-medium); }
`

// memberStyleTag is the conversation stylesheet wrapped in a <style> element,
// injected into the document head via uikit.Head's Extra slot.
const memberStyleTag = "<style>" + memberConsoleCSS + "</style>"

// memberHead renders the member-scoped conversation stylesheet as extra <head>
// markup for uikit.Head. It is raw, trusted CSS authored in this package.
func memberHead() templ.Component { return templ.Raw(memberStyleTag) }

// memberUserMenu returns the top-bar user menu for the member shell: the Sign
// out control when the member is signed in, or nil on the login page (where
// there is no session to end). A nil component is omitted by uikit.TopBar.
func memberUserMenu(showNav bool) templ.Component {
	if !showNav {
		return nil
	}
	return SignOut()
}
