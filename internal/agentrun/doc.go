// Package agentrun implements the governed agent turn loop at the core of the
// member conversational console (ADR-0004 §4). A natural-language directive is
// turned into governed actions by a tool-calling loop: the model is called
// *through the governed LLM proxy* (so every call is brokered, metered, and
// budget-checked), the model proposes tool calls, the loop executes them
// *locally in the drun sandbox* via an injected executor, the results feed back,
// and the loop repeats until the model is done, a bound is hit, or an action is
// blocked.
//
// The package is deliberately standalone, dependency-light, and mock-testable.
// It imports only the standard library and knows nothing about drun, the
// control plane, or the console's web layer:
//
//   - The model call is an OpenAI-compatible POST to
//     {ProxyBaseURL}/v1/chat/completions with Authorization: Bearer {ProxyToken}.
//     The loop never holds a master key and never calls a model endpoint
//     directly; the proxy injects the real upstream key server-side.
//   - The tool set is caller-supplied ([Config.Tools]); the caller computes
//     drun ListTools ∩ policy allowed_tools and passes the resulting specs in.
//     This package just offers those tools to the model and dispatches calls to
//     the injected [ToolExecutor].
//   - It is bounded and fail-closed (ADR-0004 §4): it stops on a max-iteration
//     cap; on a proxy 401/403 it surfaces a typed [ErrProxyBlocked] and does
//     NOT retry unbrokered; on an executor block ([ErrToolBlocked]) it invokes
//     the [Config.OnBlocked] escalation hook and never executes or fabricates a
//     result; on any hard error it stops with a clear error rather than falling
//     back to anything ungoverned.
//
// The caller (a later batch) adapts drun and the approval endpoints to the
// [ToolExecutor] and [Config.OnBlocked] seams, and drains [Step]s from
// [Config.OnStep] for streaming and structure-only trajectory emission.
package agentrun
