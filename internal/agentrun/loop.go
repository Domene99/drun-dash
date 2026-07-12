package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// DefaultMaxIterations bounds the loop when Config.MaxIterations is unset (<= 0).
// One iteration is one model call plus the tool calls it proposes, so the loop
// makes at most this many governed model calls before stopping with
// [ErrMaxIterations] (ADR-0004 §4).
const DefaultMaxIterations = 16

// ToolSpec is one tool offered to the model. Name and Description are shown to
// the model; Parameters is the JSON Schema for the tool's arguments object,
// passed through verbatim. The caller builds these from drun ListTools ∩ policy
// allowed_tools so the model is only ever offered tools the org permits.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolExecutor runs a model-proposed tool call locally (in the drun sandbox, in
// production). It receives the tool name and the raw JSON arguments string from
// the model and returns the result to feed back, or an error.
//
// To signal a policy block — the tool is outside allowed_tools, or the engine
// refused it — return [ErrToolBlocked] (or an error wrapping it with %w). The
// loop then escalates via [Config.OnBlocked] instead of treating it as a hard
// failure. Any other non-nil error is a hard error that stops the loop.
type ToolExecutor interface {
	Execute(ctx context.Context, name string, args json.RawMessage) (result string, err error)
}

// Decision is the outcome an escalation ([Config.OnBlocked]) reports for a
// policy-blocked action. The zero value is [Denied], so a nil hook or an
// unset decision fails closed — a blocked action is never executed unless the
// hook explicitly returns [Approved].
type Decision int

const (
	// Denied is the fail-closed zero value: the blocked action is not executed;
	// a governed "denied" result is fed back to the model so it can adapt.
	Denied Decision = iota
	// Approved means an admin approved the escalation; the loop proceeds with
	// the action exactly once.
	Approved
)

// StepKind classifies a [Step] in the loop's trajectory.
type StepKind int

const (
	// StepModelCall precedes each governed proxy call.
	StepModelCall StepKind = iota
	// StepToolCall records a tool call the model proposed (before execution).
	StepToolCall
	// StepToolResult records that a tool call produced a result.
	StepToolResult
	// StepFinal records the model's final answer; the turn is done.
	StepFinal
	// StepBlocked records that the executor refused a tool call under policy.
	StepBlocked
	// StepEscalated records the decision the escalation hook returned.
	StepEscalated
)

// String renders a StepKind for logs and tests.
func (k StepKind) String() string {
	switch k {
	case StepModelCall:
		return "model_call"
	case StepToolCall:
		return "tool_call"
	case StepToolResult:
		return "tool_result"
	case StepFinal:
		return "final"
	case StepBlocked:
		return "blocked"
	case StepEscalated:
		return "escalated"
	default:
		return "unknown"
	}
}

// FieldMeta is the *structure-only* summary of a tool call's arguments or
// result — its size and coarse value type, never the raw bytes. It is what a
// [Step] carries so the caller can emit residency-preserving, structure-class
// trajectory (ADR-0004 §5) without shipping the member's private payloads
// off-box.
type FieldMeta struct {
	// ByteSize is the length in bytes of the argument or result value.
	ByteSize int
	// ValueType is a coarse shape hint: "json" when the value parses as JSON,
	// otherwise "text".
	ValueType string
}

// Step is one observable event in the turn loop, delivered to [Config.OnStep]
// for streaming to the UI and for structure-only trajectory emission. Tool
// arguments and results appear only as [FieldMeta] (size/shape), never as raw
// payload; Text carries model-authored content (a final answer, or a block
// reason) that the caller renders locally and promotes to trajectory payload
// only under policy.
type Step struct {
	// Kind is what happened.
	Kind StepKind
	// Iteration is the 0-based loop iteration the step belongs to.
	Iteration int
	// ToolName is set for StepToolCall/StepToolResult/StepBlocked/StepEscalated.
	ToolName string
	// ToolCallID is the model's correlation id for the tool call, when known.
	ToolCallID string
	// ArgsMeta summarizes the tool-call arguments (StepToolCall).
	ArgsMeta *FieldMeta
	// ResultMeta summarizes the tool result (StepToolResult).
	ResultMeta *FieldMeta
	// Text carries model-authored content: the final answer (StepFinal) or the
	// block reason (StepBlocked/StepEscalated). Empty otherwise.
	Text string
	// Decision is the escalation outcome on a StepEscalated step.
	Decision Decision
}

// Config configures a single [Run] of the turn loop. ProxyBaseURL, ProxyToken,
// Model, and Executor are required; the rest have safe defaults.
type Config struct {
	// ProxyBaseURL is the governed LLM proxy base (e.g. from a session launch);
	// the loop POSTs to {ProxyBaseURL}/v1/chat/completions.
	ProxyBaseURL string
	// ProxyToken is the scoped, launch-issued proxy grant. It is the only model
	// credential the loop uses; it is never a master key.
	ProxyToken string
	// Model is the model id passed through to the upstream.
	Model string
	// Tools are the tools offered to the model — the caller's
	// drun ListTools ∩ policy allowed_tools.
	Tools []ToolSpec
	// Executor runs the model's tool calls locally.
	Executor ToolExecutor
	// MaxIterations bounds the number of governed model calls; <= 0 uses
	// [DefaultMaxIterations].
	MaxIterations int
	// HTTPClient calls the proxy; nil uses http.DefaultClient.
	HTTPClient *http.Client
	// OnStep, when set, receives every [Step] for streaming and trajectory
	// emission. It is best-effort — a panic is recovered so the sink can never
	// crash a run — but it should be fast and non-blocking, as it runs on the
	// turn's critical path.
	OnStep func(Step)
	// OnBlocked is the escalation hook invoked when the executor blocks a tool
	// call under policy ([ErrToolBlocked]). It names the blocked action and the
	// reason and returns a [Decision]; in production it raises POST /approvals
	// and blocks until decided. A nil hook denies (fail-closed).
	OnBlocked func(action, reason string) Decision
}

// Result is the outcome of a completed [Run]: the model's final answer, the
// number of iterations it took, and the full conversation (usable to seed a
// follow-up turn).
type Result struct {
	Final      string
	Iterations int
	Messages   []Message
}

// validate checks the required fields, collecting every missing one so the
// caller sees them all at once.
func (c Config) validate() error {
	var missing []string
	if strings.TrimSpace(c.ProxyBaseURL) == "" {
		missing = append(missing, "ProxyBaseURL")
	}
	if strings.TrimSpace(c.ProxyToken) == "" {
		missing = append(missing, "ProxyToken")
	}
	if strings.TrimSpace(c.Model) == "" {
		missing = append(missing, "Model")
	}
	if c.Executor == nil {
		missing = append(missing, "Executor")
	}
	if len(missing) > 0 {
		return fmt.Errorf("agentrun: invalid config: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// engine holds the resolved, per-run state: the config, the HTTP client, the
// precomputed OpenAI tools array, and the resolved endpoint URL.
type engine struct {
	cfg      Config
	client   *http.Client
	tools    []toolWire
	endpoint string
}

// newEngine resolves Config defaults and precomputes the wire-format tools once.
func newEngine(cfg Config) *engine {
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	tools := make([]toolWire, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		tools = append(tools, toolWire{
			Type: "function",
			Function: functionWire{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return &engine{
		cfg:      cfg,
		client:   client,
		tools:    tools,
		endpoint: strings.TrimRight(cfg.ProxyBaseURL, "/") + chatCompletionsPath,
	}
}

// Run drives the governed turn loop for one directive and returns the final
// answer. It seeds the conversation with the directive, then repeats: call the
// model through the proxy, and if it returns tool calls execute each locally and
// feed the results back, until the model returns a final answer, the iteration
// bound is hit ([ErrMaxIterations]), or a hard/blocked error stops it.
//
// It is fail-closed throughout (ADR-0004 §4): a proxy 401/403 returns a
// [*ProxyError] matching [ErrProxyBlocked] and is not retried; an executor
// [ErrToolBlocked] is escalated via [Config.OnBlocked] and never executed or
// faked on denial; any other error stops the run rather than degrading to
// something ungoverned. ctx cancellation is honored between steps.
func Run(ctx context.Context, cfg Config, directive string) (*Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	eng := newEngine(cfg)

	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	messages := []Message{{Role: "user", Content: directive}}

	for iter := 0; iter < maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		eng.emit(Step{Kind: StepModelCall, Iteration: iter})
		msg, err := eng.callModel(ctx, messages)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)

		// No tool calls means the model gave a final answer; the turn is done.
		if len(msg.ToolCalls) == 0 {
			eng.emit(Step{Kind: StepFinal, Iteration: iter, Text: msg.Content})
			return &Result{Final: msg.Content, Iterations: iter + 1, Messages: messages}, nil
		}

		// Execute each proposed tool call serially (a drun client is
		// sequential; ADR-0004 §4) and feed every result back before calling
		// the model again.
		for _, tc := range msg.ToolCalls {
			result, err := eng.dispatch(ctx, tc, iter)
			if err != nil {
				return nil, err
			}
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}
	}

	return nil, ErrMaxIterations
}

// dispatch executes one tool call and returns the string to feed back to the
// model. A clean result is fed back as-is. An [ErrToolBlocked] is escalated (see
// escalate). Any other executor error is a hard error that stops the loop.
func (e *engine) dispatch(ctx context.Context, tc ToolCall, iter int) (string, error) {
	name := tc.Function.Name
	args := json.RawMessage(tc.Function.Arguments)

	e.emit(Step{
		Kind:       StepToolCall,
		Iteration:  iter,
		ToolName:   name,
		ToolCallID: tc.ID,
		ArgsMeta:   metaFor(args),
	})

	result, err := e.cfg.Executor.Execute(ctx, name, args)
	if err != nil {
		if errors.Is(err, ErrToolBlocked) {
			return e.escalate(ctx, tc, iter, err)
		}
		return "", fmt.Errorf("agentrun: tool %q failed: %w", name, err)
	}

	e.emitResult(iter, tc, result)
	return result, nil
}

// escalate handles a policy-blocked tool call: it records the block, invokes the
// escalation hook, and acts on the decision. On [Denied] (including a nil hook,
// fail-closed) it feeds a governed denial back to the model and does NOT execute
// the action or fabricate a real result. On [Approved] it proceeds with the
// action exactly once; if the executor still refuses or errors after approval,
// that is a hard error (the loop never re-escalates the same call).
func (e *engine) escalate(ctx context.Context, tc ToolCall, iter int, blockErr error) (string, error) {
	name := tc.Function.Name
	reason := blockErr.Error()

	e.emit(Step{Kind: StepBlocked, Iteration: iter, ToolName: name, ToolCallID: tc.ID, Text: reason})

	decision := Denied
	if e.cfg.OnBlocked != nil {
		decision = e.cfg.OnBlocked(name, reason)
	}

	e.emit(Step{Kind: StepEscalated, Iteration: iter, ToolName: name, ToolCallID: tc.ID, Text: reason, Decision: decision})

	if decision != Approved {
		// Feed a governed denial back so the model can adapt; the blocked
		// action never runs and no result is fabricated (ADR-0004 §6).
		return fmt.Sprintf("The %q action was blocked by policy and the escalation was not approved: %s", name, reason), nil
	}

	// Approved: proceed with the action exactly once.
	args := json.RawMessage(tc.Function.Arguments)
	result, err := e.cfg.Executor.Execute(ctx, name, args)
	if err != nil {
		return "", fmt.Errorf("agentrun: approved tool %q failed: %w", name, err)
	}
	e.emitResult(iter, tc, result)
	return result, nil
}

// emitResult records a StepToolResult with the result's structure-only summary.
func (e *engine) emitResult(iter int, tc ToolCall, result string) {
	e.emit(Step{
		Kind:       StepToolResult,
		Iteration:  iter,
		ToolName:   tc.Function.Name,
		ToolCallID: tc.ID,
		ResultMeta: metaFor([]byte(result)),
	})
}

// emit delivers a step to the best-effort sink. A panic in the caller's OnStep
// is recovered so an observability sink can never crash a governed run.
func (e *engine) emit(s Step) {
	if e.cfg.OnStep == nil {
		return
	}
	defer func() { _ = recover() }()
	e.cfg.OnStep(s)
}

// metaFor builds the structure-only summary of a raw value: its byte size and a
// coarse value type ("json" when it parses as JSON, else "text"). It never
// retains the bytes themselves.
func metaFor(raw []byte) *FieldMeta {
	vt := "text"
	if len(raw) > 0 && json.Valid(raw) {
		vt = "json"
	}
	return &FieldMeta{ByteSize: len(raw), ValueType: vt}
}
