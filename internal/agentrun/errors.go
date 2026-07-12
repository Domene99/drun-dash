package agentrun

import (
	"errors"
	"fmt"
)

// ErrToolBlocked marks a tool call that the executor refused because policy
// forbids it (the tool is outside the session's allowed_tools, or the drun
// engine declined it under the compiled config). A [ToolExecutor] returns this
// sentinel — or an error that wraps it with %w — instead of a normal error, and
// the loop routes it to the escalation hook ([Config.OnBlocked]) rather than
// treating it as a hard failure. Distinguishing "blocked by policy" from "the
// tool ran and errored" is what lets a block become a human approval (ADR-0004
// §6) instead of a dead error.
var ErrToolBlocked = errors.New("agentrun: tool call blocked by policy")

// ErrProxyBlocked marks a model call the governed proxy rejected fail-closed —
// an HTTP 401 or 403, meaning the scoped proxy grant is expired, revoked, or
// otherwise not authorized (ADR-0003 §3, ADR-0004 §4/§6). The loop stops and
// surfaces this rather than retrying unbrokered; the caller, which holds the
// member context, is the party that raises the approval. Match it with
// errors.Is(err, ErrProxyBlocked); the concrete error is a [*ProxyError] whose
// StatusCode is available by type assertion.
var ErrProxyBlocked = errors.New("agentrun: model call blocked by proxy grant")

// ErrMaxIterations reports that the loop hit its iteration bound
// ([Config.MaxIterations], defaulting to [DefaultMaxIterations]) before the
// model produced a final answer. It is the fail-closed stop for a model that
// keeps calling tools forever, so a stuck run cannot spin unmetered (ADR-0004
// §4). The steps already streamed to [Config.OnStep] describe what ran.
var ErrMaxIterations = errors.New("agentrun: reached max iterations without a final answer")

// ErrNoChoices reports that the proxy returned a 2xx response with no
// choices[0].message to interpret. It is a hard error: the loop cannot proceed
// without a model turn, and never fabricates one.
var ErrNoChoices = errors.New("agentrun: proxy response contained no choices")

// ProxyError is the concrete error for a non-2xx response from the governed
// proxy. StatusCode is the HTTP status and Body is the (bounded) response body,
// which for our control plane is the flat ADR-0002 Error envelope. For a 401 or
// 403 it also satisfies errors.Is(err, [ErrProxyBlocked]) so callers can treat a
// blocked grant specially (surface an approval) while still handling every other
// non-2xx (a 5xx, a malformed upstream) as a plain hard error that stops the
// loop.
type ProxyError struct {
	StatusCode int
	Body       string
}

func (e *ProxyError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("agentrun: proxy returned status %d", e.StatusCode)
	}
	return fmt.Sprintf("agentrun: proxy returned status %d: %s", e.StatusCode, e.Body)
}

// Is reports a match for [ErrProxyBlocked] when the status is a fail-closed
// grant rejection (401/403), so errors.Is(err, ErrProxyBlocked) is true for
// exactly those two statuses and false for any other non-2xx.
func (e *ProxyError) Is(target error) bool {
	return target == ErrProxyBlocked && (e.StatusCode == 401 || e.StatusCode == 403)
}
