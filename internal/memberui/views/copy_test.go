package views

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// These tests cover the copy-out affordances added for issue #294: the run view
// is hosted in a native webview with no macOS Edit menu (Cmd+C never fires), so
// the transcript must be text-selectable and every agent reply / code block must
// carry a working Copy button. They assert the markup and the inline helper are
// present; the clipboard behavior itself is exercised in the browser.

func renderComp(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func TestRunPageShipsCopyHelperAndSelectableTranscript(t *testing.T) {
	body := renderComp(t, RunPage(RunData{SessionID: "sess-1", EngineReady: true, Model: "m"}))

	// The inline vanilla-JS copy helper is included on the run page: a delegated
	// [data-copy] handler that writes to the async Clipboard API.
	if !strings.Contains(body, "data-copy") {
		t.Errorf("run page missing the [data-copy] copy helper: %q", body)
	}
	if !strings.Contains(body, "navigator.clipboard") {
		t.Errorf("run page copy helper should use the async Clipboard API: %q", body)
	}
	// No framework / CDN was pulled in for the helper.
	if strings.Contains(body, "jsdelivr") || strings.Contains(body, "unpkg") {
		t.Errorf("run page must not load copy support from a CDN: %q", body)
	}

	// The transcript, bubbles, and steps are explicitly text-selectable and
	// nothing re-disables selection on them.
	if !strings.Contains(body, "user-select: text") {
		t.Errorf("run page should make the transcript text-selectable: %q", body)
	}
	if strings.Contains(memberConsoleCSS, "user-select: none;\n") && !strings.Contains(memberConsoleCSS, ".mc-copy") {
		t.Errorf("no transcript rule should disable text selection")
	}
}

func TestTurnStartedAgentBubbleHasCopyButton(t *testing.T) {
	frag := renderComp(t, TurnStarted("sess-1", "list files"))

	if !strings.Contains(frag, `class="mc-copy"`) {
		t.Errorf("agent bubble missing a Copy button: %q", frag)
	}
	if !strings.Contains(frag, "data-copy") {
		t.Errorf("agent bubble Copy button missing the data-copy hook: %q", frag)
	}
	// The bubble still opens the SSE stream — the button must not disturb it.
	if !strings.Contains(frag, `sse-connect="/sessions/sess-1/stream"`) {
		t.Errorf("agent bubble lost its SSE connection: %q", frag)
	}
}

func TestFinalStepHasCopyButtonAndStaysSingleLine(t *testing.T) {
	frag := RenderStepFragment(StepView{Kind: "final", Text: "line one\nline two"})

	if !strings.Contains(frag, `class="mc-copy"`) || !strings.Contains(frag, `data-copy=".mc-final-text"`) {
		t.Errorf("final answer missing a targeted Copy button: %q", frag)
	}
	if !strings.Contains(frag, `class="mc-final-text"`) {
		t.Errorf("final answer text not wrapped for copying: %q", frag)
	}
	// The fragment is one SSE `data:` line — model newlines became <br>, none survive raw.
	if strings.Contains(frag, "\n") {
		t.Errorf("final step fragment must contain no raw newline: %q", frag)
	}
	if !strings.Contains(frag, "line one<br>line two") {
		t.Errorf("final step should preserve line breaks as <br>: %q", frag)
	}
}

func TestDiffReviewHasCopyButton(t *testing.T) {
	frag := renderComp(t, DiffReview(DiffData{SessionID: "sess-1", Diff: "--- a\n+++ b\n"}))

	if !strings.Contains(frag, `class="mc-code"`) {
		t.Errorf("diff review missing the copyable code wrapper: %q", frag)
	}
	if !strings.Contains(frag, `class="mc-copy"`) || !strings.Contains(frag, `data-copy=".mc-diff"`) {
		t.Errorf("diff review missing a Copy button targeting the diff: %q", frag)
	}
}
