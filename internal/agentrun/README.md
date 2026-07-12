# internal/agentrun

The **governed agent turn loop** at the core of the member conversational
console (ADR-0004 §4). A natural-language directive becomes governed actions
through a tool-calling loop: the model reasons *centrally* (through the governed
LLM proxy, so every call is brokered, metered, and budget-checked) and the tools
execute *locally* (in the drun sandbox, via an injected executor).

This package is **standalone, stdlib-only, and mock-tested**. It does **not**
import `internal/memberui`, the control plane, or `pkg/drunmcp`. The caller (a
later batch) adapts drun and the approval endpoints to the interfaces below.

## The loop (`Run`)

```go
res, err := agentrun.Run(ctx, agentrun.Config{
    ProxyBaseURL: launch.ProxyBaseURL, // {base}/v1/chat/completions
    ProxyToken:   launch.ProxyToken,   // scoped grant; never a master key
    Model:        "gpt-4o",
    Tools:        specs,               // drun ListTools ∩ policy.allowed_tools
    Executor:     drunExecutor,        // runs tool calls in the sandbox
    OnStep:       trajectorySink,      // streaming + structure-only trajectory
    OnBlocked:    raiseApproval,       // escalation hook (POST /approvals)
}, directive)
```

Each iteration:

1. **Model call** — OpenAI-compatible `POST {ProxyBaseURL}/v1/chat/completions`
   with `Authorization: Bearer {ProxyToken}` and the tool schema. The proxy
   injects the real upstream key; the loop never holds one.
2. **Interpret** — a final `content` ends the turn; `tool_calls[]` go to (3).
3. **Execute** each tool call locally via the injected `ToolExecutor`.
4. **Feed back** each result as a `role:"tool"` message with the matching
   `tool_call_id`, then loop to (1) — until a final answer, a bound, or a block.

## Interfaces the caller supplies

- **`ToolSpec`** — `{Name, Description string; Parameters json.RawMessage}` (JSON
  Schema). The caller computes `ListTools ∩ policy.allowed_tools` and passes the
  resulting specs, so the model is only ever offered permitted tools.
- **`ToolExecutor`** — `Execute(ctx, name, args) (result string, err error)`.
  Return `ErrToolBlocked` (or an error wrapping it with `%w`) to signal a policy
  block; any other error is a hard failure.
- **`OnStep(Step)`** — best-effort sink for streaming and **structure-only**
  trajectory. A `Step` carries tool argument/result *metadata* (`FieldMeta`:
  byte size + coarse value type), never raw payloads; `Text` carries
  model-authored content (a final answer, a block reason) the caller renders
  locally and promotes to trajectory payload only under policy. A panic in the
  sink is recovered so it can never crash a run.
- **`OnBlocked(action, reason) Decision`** — the escalation hook. On a blocked
  tool call the loop calls it (in production it raises `POST /approvals` and
  blocks until decided). `Approved` proceeds with the action **once**; `Denied`
  (the zero value, and the default for a nil hook) feeds a governed "action
  denied" result back to the model — the blocked action never runs and no result
  is fabricated.

## Bounded and fail-closed (ADR-0004 §4)

- **Bounded** — stops after `MaxIterations` model calls
  (default `DefaultMaxIterations`) with `ErrMaxIterations`, so a stuck model
  cannot spin unmetered.
- **Proxy `401`/`403`** — returns a `*ProxyError` matching
  `errors.Is(err, ErrProxyBlocked)`; the loop does **not** retry unbrokered. The
  caller (holding member context) raises the approval. Any other non-2xx is a
  plain hard error.
- **Executor block** — routed to `OnBlocked`; never executed or faked on denial.
- **Any hard error** (transport, decode, `ErrNoChoices`, a non-blocked executor
  error) stops the run — never a fallback to anything ungoverned.

## What this package does not do

It does not talk to drun, the control plane, or the proxy's internals; it does
not author or enforce policy; it does not decide the per-tool JSON schema (the
caller mirrors each drun tool's `inputSchema`). It offers the given tools to the
model through the governed proxy, dispatches calls to the injected executor, and
emits structure-only steps — nothing more.
