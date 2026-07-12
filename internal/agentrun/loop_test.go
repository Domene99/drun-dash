package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// scriptedProxy is a fake governed proxy implemented as an http.RoundTripper, so
// tests drive the loop end to end in-process without binding a socket. It
// answers /v1/chat/completions from a queue of scripted responses (one per model
// call) and records every request, so a test can assert what the loop sent (auth
// header, path, tools, fed-back tool results).
type scriptedProxy struct {
	t         *testing.T
	responses []proxyResponse
	calls     int
	requests  []recordedRequest
}

// proxyResponse is one scripted answer: an HTTP status and a raw JSON body.
type proxyResponse struct {
	status int
	body   string
}

// recordedRequest captures what the loop sent for later assertions.
type recordedRequest struct {
	authorization string
	path          string
	decoded       chatRequest
}

func (p *scriptedProxy) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := recordedRequest{
		authorization: r.Header.Get("Authorization"),
		path:          r.URL.Path,
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&rec.decoded); err != nil {
			p.t.Errorf("proxy: decode request body: %v", err)
		}
	}
	p.requests = append(p.requests, rec)

	if p.calls >= len(p.responses) {
		p.t.Errorf("proxy: unexpected call %d (only %d scripted)", p.calls+1, len(p.responses))
		return newResponse(r, http.StatusInternalServerError, "no scripted response"), nil
	}
	resp := p.responses[p.calls]
	p.calls++
	return newResponse(r, resp.status, resp.body), nil
}

func newResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// newProxy builds a scripted proxy over the given responses.
func newProxy(t *testing.T, responses ...proxyResponse) *scriptedProxy {
	t.Helper()
	return &scriptedProxy{t: t, responses: responses}
}

// finalBody is a scripted final-answer response (no tool calls).
func finalBody(content string) proxyResponse {
	return proxyResponse{status: http.StatusOK, body: marshalChoice(Message{Role: "assistant", Content: content}, "stop")}
}

// toolBody is a scripted response asking for a single tool call.
func toolBody(callID, name, args string) proxyResponse {
	msg := Message{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: callID, Type: "function", Function: FunctionCall{Name: name, Arguments: args}}},
	}
	return proxyResponse{status: http.StatusOK, body: marshalChoice(msg, "tool_calls")}
}

func marshalChoice(msg Message, finish string) string {
	b, _ := json.Marshal(chatResponse{Choices: []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{{Message: msg, FinishReason: finish}}})
	return string(b)
}

// funcExecutor adapts a function to a ToolExecutor.
type funcExecutor func(ctx context.Context, name string, args json.RawMessage) (string, error)

func (f funcExecutor) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return f(ctx, name, args)
}

// recordingExecutor records each call and returns a fixed result, so a test can
// assert the loop dispatched the tool with the model's raw arguments.
type recordingExecutor struct {
	calls  []executedCall
	result string
	err    error
}

type executedCall struct {
	name string
	args string
}

func (e *recordingExecutor) Execute(_ context.Context, name string, args json.RawMessage) (string, error) {
	e.calls = append(e.calls, executedCall{name: name, args: string(args)})
	return e.result, e.err
}

// baseConfig builds a valid config whose HTTP client is backed by the scripted
// proxy, so no socket is bound.
func baseConfig(proxy *scriptedProxy, exec ToolExecutor) Config {
	return Config{
		ProxyBaseURL: "http://proxy.test",
		ProxyToken:   "dgp_testtoken",
		Model:        "test-model",
		Executor:     exec,
		HTTPClient:   &http.Client{Transport: proxy},
	}
}

func TestRunFinalAnswerImmediately(t *testing.T) {
	proxy := newProxy(t, finalBody("all done"))
	exec := &recordingExecutor{}

	var steps []Step
	cfg := baseConfig(proxy, exec)
	cfg.Tools = []ToolSpec{{Name: "session_bash", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}}
	cfg.OnStep = func(s Step) { steps = append(steps, s) }

	res, err := Run(context.Background(), cfg, "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "all done" {
		t.Errorf("Final = %q, want %q", res.Final, "all done")
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", res.Iterations)
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor called %d times, want 0", len(exec.calls))
	}

	if len(proxy.requests) != 1 {
		t.Fatalf("proxy calls = %d, want 1", len(proxy.requests))
	}
	req := proxy.requests[0]
	if req.authorization != "Bearer dgp_testtoken" {
		t.Errorf("Authorization = %q", req.authorization)
	}
	if req.path != "/v1/chat/completions" {
		t.Errorf("path = %q", req.path)
	}
	if len(req.decoded.Tools) != 1 || req.decoded.Tools[0].Function.Name != "session_bash" {
		t.Errorf("tools not forwarded: %+v", req.decoded.Tools)
	}
	if req.decoded.Tools[0].Type != "function" {
		t.Errorf("tool type = %q, want function", req.decoded.Tools[0].Type)
	}
	if req.decoded.Model != "test-model" {
		t.Errorf("model = %q", req.decoded.Model)
	}
	if len(req.decoded.Messages) != 1 || req.decoded.Messages[0].Content != "do it" {
		t.Errorf("seed message = %+v", req.decoded.Messages)
	}

	assertKinds(t, steps, StepModelCall, StepFinal)
	if steps[1].Text != "all done" {
		t.Errorf("final step text = %q", steps[1].Text)
	}
}

func TestRunToolCallThenFinal(t *testing.T) {
	proxy := newProxy(t,
		toolBody("call-1", "session_bash", `{"command":"ls"}`),
		finalBody("listed"),
	)
	exec := &recordingExecutor{result: "file-a\nfile-b"}

	var steps []Step
	cfg := baseConfig(proxy, exec)
	cfg.OnStep = func(s Step) { steps = append(steps, s) }

	res, err := Run(context.Background(), cfg, "list files")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "listed" {
		t.Errorf("Final = %q", res.Final)
	}
	if res.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", res.Iterations)
	}

	if len(exec.calls) != 1 || exec.calls[0].name != "session_bash" || exec.calls[0].args != `{"command":"ls"}` {
		t.Fatalf("executor calls = %+v", exec.calls)
	}

	// The second model call must carry the tool result fed back with the
	// matching tool_call_id.
	if len(proxy.requests) != 2 {
		t.Fatalf("proxy calls = %d, want 2", len(proxy.requests))
	}
	last := lastMessage(proxy.requests[1].decoded.Messages)
	if last.Role != "tool" || last.ToolCallID != "call-1" || last.Content != "file-a\nfile-b" {
		t.Errorf("fed-back tool message = %+v", last)
	}

	assertKinds(t, steps, StepModelCall, StepToolCall, StepToolResult, StepModelCall, StepFinal)

	// Structure-only metadata: sizes/shape, not raw payload.
	toolCall := steps[1]
	if toolCall.ToolName != "session_bash" || toolCall.ToolCallID != "call-1" {
		t.Errorf("tool_call step = %+v", toolCall)
	}
	if toolCall.ArgsMeta == nil || toolCall.ArgsMeta.ValueType != "json" || toolCall.ArgsMeta.ByteSize != len(`{"command":"ls"}`) {
		t.Errorf("args meta = %+v", toolCall.ArgsMeta)
	}
	result := steps[2]
	if result.ResultMeta == nil || result.ResultMeta.ValueType != "text" || result.ResultMeta.ByteSize != len("file-a\nfile-b") {
		t.Errorf("result meta = %+v", result.ResultMeta)
	}
}

func TestRunProxyBlockedIsTyped(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			proxy := newProxy(t, proxyResponse{status: status, body: `{"error":{"code":"unauthorized"}}`})
			_, err := Run(context.Background(), baseConfig(proxy, &recordingExecutor{}), "go")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrProxyBlocked) {
				t.Errorf("errors.Is(err, ErrProxyBlocked) = false; err = %v", err)
			}
			var pe *ProxyError
			if !errors.As(err, &pe) || pe.StatusCode != status {
				t.Errorf("ProxyError status = %v, want %d", err, status)
			}
		})
	}
}

func TestRunProxyHardErrorNotBlocked(t *testing.T) {
	proxy := newProxy(t, proxyResponse{status: http.StatusInternalServerError, body: `boom`})
	_, err := Run(context.Background(), baseConfig(proxy, &recordingExecutor{}), "go")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrProxyBlocked) {
		t.Error("500 must not match ErrProxyBlocked")
	}
	var pe *ProxyError
	if !errors.As(err, &pe) || pe.StatusCode != http.StatusInternalServerError {
		t.Errorf("want ProxyError 500, got %v", err)
	}
}

func TestRunToolBlockedDeniedFeedsGovernedResult(t *testing.T) {
	proxy := newProxy(t,
		toolBody("call-1", "session_write_file", `{"path":"/etc/x"}`),
		finalBody("adapted"),
	)
	// Executor blocks on policy; count executions to prove it is not re-run.
	var execCount int
	exec := funcExecutor(func(_ context.Context, name string, _ json.RawMessage) (string, error) {
		execCount++
		return "", fmt.Errorf("write outside sandbox: %w", ErrToolBlocked)
	})

	var blockedAction, blockedReason string
	var steps []Step
	cfg := baseConfig(proxy, exec)
	cfg.OnStep = func(s Step) { steps = append(steps, s) }
	cfg.OnBlocked = func(action, reason string) Decision {
		blockedAction, blockedReason = action, reason
		return Denied
	}

	res, err := Run(context.Background(), cfg, "write file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "adapted" {
		t.Errorf("Final = %q", res.Final)
	}
	if execCount != 1 {
		t.Errorf("executor ran %d times on denial, want 1 (never re-executed)", execCount)
	}
	if blockedAction != "session_write_file" {
		t.Errorf("escalation action = %q", blockedAction)
	}
	if !strings.Contains(blockedReason, "write outside sandbox") {
		t.Errorf("escalation reason = %q", blockedReason)
	}

	// A governed denial is fed back to the model, not a fabricated result.
	last := lastMessage(proxy.requests[1].decoded.Messages)
	if last.Role != "tool" || !strings.Contains(last.Content, "blocked by policy") {
		t.Errorf("fed-back denial = %+v", last)
	}

	assertKinds(t, steps, StepModelCall, StepToolCall, StepBlocked, StepEscalated, StepModelCall, StepFinal)
	if steps[3].Decision != Denied {
		t.Errorf("escalated decision = %v, want Denied", steps[3].Decision)
	}
}

func TestRunToolBlockedNilHookDeniesFailClosed(t *testing.T) {
	proxy := newProxy(t,
		toolBody("call-1", "session_bash", `{}`),
		finalBody("ok"),
	)
	exec := funcExecutor(func(context.Context, string, json.RawMessage) (string, error) {
		return "", ErrToolBlocked
	})
	cfg := baseConfig(proxy, exec) // no OnBlocked hook
	if _, err := Run(context.Background(), cfg, "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := lastMessage(proxy.requests[1].decoded.Messages)
	if last.Role != "tool" || !strings.Contains(last.Content, "not approved") {
		t.Errorf("nil hook must fail closed with a denial, got %+v", last)
	}
}

func TestRunToolBlockedApprovedProceeds(t *testing.T) {
	proxy := newProxy(t,
		toolBody("call-1", "session_bash", `{"command":"echo hi"}`),
		finalBody("done"),
	)
	// First call blocks; after approval the second call succeeds.
	var execCount int
	exec := funcExecutor(func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		execCount++
		if execCount == 1 {
			return "", ErrToolBlocked
		}
		return "hi", nil
	})

	var steps []Step
	cfg := baseConfig(proxy, exec)
	cfg.OnStep = func(s Step) { steps = append(steps, s) }
	cfg.OnBlocked = func(string, string) Decision { return Approved }

	res, err := Run(context.Background(), cfg, "run")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "done" {
		t.Errorf("Final = %q", res.Final)
	}
	if execCount != 2 {
		t.Errorf("executor ran %d times, want 2 (block then approved)", execCount)
	}
	last := lastMessage(proxy.requests[1].decoded.Messages)
	if last.Content != "hi" {
		t.Errorf("approved result fed back = %q, want %q", last.Content, "hi")
	}
	assertKinds(t, steps, StepModelCall, StepToolCall, StepBlocked, StepEscalated, StepToolResult, StepModelCall, StepFinal)
}

func TestRunApprovedButStillBlockedIsHardError(t *testing.T) {
	proxy := newProxy(t, toolBody("call-1", "session_bash", `{}`))
	exec := funcExecutor(func(context.Context, string, json.RawMessage) (string, error) {
		return "", ErrToolBlocked // never succeeds, even after approval
	})
	cfg := baseConfig(proxy, exec)
	cfg.OnBlocked = func(string, string) Decision { return Approved }
	_, err := Run(context.Background(), cfg, "go")
	if err == nil || !strings.Contains(err.Error(), "approved tool") {
		t.Fatalf("want hard error after re-block, got %v", err)
	}
}

func TestRunHardExecutorErrorStops(t *testing.T) {
	proxy := newProxy(t, toolBody("call-1", "session_bash", `{}`))
	exec := funcExecutor(func(context.Context, string, json.RawMessage) (string, error) {
		return "", errors.New("disk on fire")
	})
	_, err := Run(context.Background(), baseConfig(proxy, exec), "go")
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("want hard error, got %v", err)
	}
}

func TestRunMaxIterations(t *testing.T) {
	// Always ask for another tool call, so the loop can never finish.
	responses := make([]proxyResponse, 3)
	for i := range responses {
		responses[i] = toolBody(fmt.Sprintf("call-%d", i), "session_bash", `{}`)
	}
	proxy := newProxy(t, responses...)
	exec := &recordingExecutor{result: "ok"}
	cfg := baseConfig(proxy, exec)
	cfg.MaxIterations = 3

	_, err := Run(context.Background(), cfg, "spin")
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("want ErrMaxIterations, got %v", err)
	}
	if got := len(exec.calls); got != 3 {
		t.Errorf("executor ran %d times, want 3 (bounded)", got)
	}
}

func TestRunNoChoices(t *testing.T) {
	proxy := newProxy(t, proxyResponse{status: http.StatusOK, body: `{"choices":[]}`})
	_, err := Run(context.Background(), baseConfig(proxy, &recordingExecutor{}), "go")
	if !errors.Is(err, ErrNoChoices) {
		t.Fatalf("want ErrNoChoices, got %v", err)
	}
}

func TestRunContextCancellation(t *testing.T) {
	proxy := newProxy(t, finalBody("unreached"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := Run(ctx, baseConfig(proxy, &recordingExecutor{}), "go")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	valid := Config{ProxyBaseURL: "http://x", ProxyToken: "t", Model: "m", Executor: &recordingExecutor{}}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	tests := []struct {
		name  string
		muto  func(*Config)
		field string
	}{
		{"no base url", func(c *Config) { c.ProxyBaseURL = "" }, "ProxyBaseURL"},
		{"no token", func(c *Config) { c.ProxyToken = "" }, "ProxyToken"},
		{"no model", func(c *Config) { c.Model = "" }, "Model"},
		{"no executor", func(c *Config) { c.Executor = nil }, "Executor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			tc.muto(&c)
			err := c.validate()
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("want error naming %q, got %v", tc.field, err)
			}
		})
	}
	// Run rejects an invalid config before making any call.
	if _, err := Run(context.Background(), Config{}, "go"); err == nil {
		t.Error("Run accepted an empty config")
	}
}

func TestOnStepPanicIsRecovered(t *testing.T) {
	proxy := newProxy(t, finalBody("fine"))
	cfg := baseConfig(proxy, &recordingExecutor{})
	cfg.OnStep = func(Step) { panic("sink blew up") }
	res, err := Run(context.Background(), cfg, "go")
	if err != nil {
		t.Fatalf("a panicking OnStep must not fail the run: %v", err)
	}
	if res.Final != "fine" {
		t.Errorf("Final = %q", res.Final)
	}
}

func TestDefaultMaxIterationsApplied(t *testing.T) {
	// With MaxIterations unset, the loop bounds at DefaultMaxIterations.
	responses := make([]proxyResponse, DefaultMaxIterations)
	for i := range responses {
		responses[i] = toolBody(fmt.Sprintf("c-%d", i), "session_bash", `{}`)
	}
	proxy := newProxy(t, responses...)
	exec := &recordingExecutor{result: "ok"}
	_, err := Run(context.Background(), baseConfig(proxy, exec), "spin")
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("want ErrMaxIterations, got %v", err)
	}
	if len(exec.calls) != DefaultMaxIterations {
		t.Errorf("executor ran %d times, want %d", len(exec.calls), DefaultMaxIterations)
	}
}

// assertKinds asserts the sequence of step kinds.
func assertKinds(t *testing.T, steps []Step, want ...StepKind) {
	t.Helper()
	if len(steps) != len(want) {
		t.Fatalf("got %d steps %v, want %d %v", len(steps), kinds(steps), len(want), want)
	}
	for i, w := range want {
		if steps[i].Kind != w {
			t.Errorf("step %d kind = %v, want %v (all: %v)", i, steps[i].Kind, w, kinds(steps))
		}
	}
}

func kinds(steps []Step) []StepKind {
	out := make([]StepKind, len(steps))
	for i, s := range steps {
		out[i] = s.Kind
	}
	return out
}

func lastMessage(msgs []Message) Message {
	return msgs[len(msgs)-1]
}
