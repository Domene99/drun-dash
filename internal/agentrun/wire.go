package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// chatCompletionsPath is the OpenAI-compatible endpoint the governed proxy
// exposes; the loop joins it onto Config.ProxyBaseURL. The proxy forwards it
// upstream and injects the master key, so this is the only model endpoint the
// loop ever calls (ADR-0004 §4).
const chatCompletionsPath = "/v1/chat/completions"

// maxResponseBytes bounds how much of a proxy response the loop reads. Chat
// completion responses (non-streamed, the shape this loop uses) are small; the
// cap keeps a runaway or hostile body from growing memory without bound.
const maxResponseBytes = 8 << 20 // 8 MiB

// Message is one entry in the OpenAI-compatible chat conversation. The loop
// seeds it with the member's directive (role "user"), appends the model's
// replies (role "assistant", possibly carrying ToolCalls), and appends tool
// results (role "tool", carrying ToolCallID). It is exported so a caller can
// inspect [Result.Messages] and seed a follow-up turn with prior history.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a single function call the model proposes in an assistant
// message. ID correlates the eventual tool-result message; Function names the
// tool and carries its arguments as a JSON string (per the OpenAI wire format),
// which the loop passes verbatim to the [ToolExecutor].
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name + JSON-string arguments of a proposed tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatRequest is the POST body sent to the proxy. Tools is omitted when empty so
// a directive with no tools is a plain completion.
type chatRequest struct {
	Model    string     `json:"model"`
	Messages []Message  `json:"messages"`
	Tools    []toolWire `json:"tools,omitempty"`
}

// toolWire is the OpenAI "tools" array entry: a function with its JSON-Schema
// parameters. Built once from Config.Tools at engine construction.
type toolWire struct {
	Type     string       `json:"type"`
	Function functionWire `json:"function"`
}

type functionWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatResponse is the subset of the proxy/upstream response the loop reads: the
// first choice's message, which is either a final answer (Content) or a set of
// ToolCalls to execute.
type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
}

// callModel performs one governed model call: it POSTs the conversation and tool
// schema to {ProxyBaseURL}/v1/chat/completions with the scoped proxy token and
// returns choices[0].message. It never holds or sends a master key — the proxy
// injects the upstream credential server-side.
//
// It is fail-closed on the response status: a 401/403 becomes a [*ProxyError]
// that matches [ErrProxyBlocked] (a blocked grant the caller escalates, never a
// retry); any other non-2xx becomes a plain [*ProxyError] hard error. A
// transport failure, an unreadable body, or unparseable JSON is likewise a hard
// error — the loop stops rather than falling back to anything ungoverned.
func (e *engine) callModel(ctx context.Context, messages []Message) (Message, error) {
	body, err := json.Marshal(chatRequest{
		Model:    e.cfg.Model,
		Messages: messages,
		Tools:    e.tools,
	})
	if err != nil {
		return Message{}, fmt.Errorf("agentrun: marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return Message{}, fmt.Errorf("agentrun: build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.cfg.ProxyToken)

	resp, err := e.client.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("agentrun: proxy request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Message{}, fmt.Errorf("agentrun: read proxy response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// A 401/403 also satisfies errors.Is(err, ErrProxyBlocked); every other
		// non-2xx is a plain hard error. Both stop the loop.
		return Message{}, &ProxyError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Message{}, fmt.Errorf("agentrun: decode proxy response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return Message{}, ErrNoChoices
	}
	return cr.Choices[0].Message, nil
}
