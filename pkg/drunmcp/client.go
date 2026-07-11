// Package drunmcp is a minimal Go client for drun-mcp, the drun sandbox's MCP
// server. drun-mcp speaks MCP over the Streamable HTTP transport (not SSE-only
// GET streams): every call is an HTTP POST whose response is either a JSON
// object or a Server-Sent Events stream. See the README for transport details
// and failure modes.
package drunmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultEndpoint is drun-mcp's hardcoded Streamable HTTP address. Port 7273 is
// fixed in drun today.
const DefaultEndpoint = "http://127.0.0.1:7273/mcp"

// Client is a minimal MCP client for drun-mcp. It is safe for sequential use by
// a single goroutine; concurrent calls on one Client are not supported because
// JSON-RPC ids and the transport session id are shared serially.
type Client struct {
	transport   *transport
	nextID      atomic.Int64
	clientInfo  Implementation
	initialized bool
}

// Implementation identifies a peer (client or server) in the MCP handshake.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithHTTPClient supplies a custom *http.Client, e.g. one with a specific
// timeout or transport. If unset, a client with a 30s timeout is used.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.transport.httpClient = hc
		}
	}
}

// WithClientInfo overrides the client name and version reported to the server
// during Initialize.
func WithClientInfo(name, version string) Option {
	return func(c *Client) {
		c.clientInfo = Implementation{Name: name, Version: version}
	}
}

// NewClient returns a Client targeting endpoint. Pass DefaultEndpoint for the
// standard drun-mcp address.
func NewClient(endpoint string, opts ...Option) *Client {
	c := &Client{
		transport: &transport{
			endpoint:   endpoint,
			httpClient: &http.Client{Timeout: 30 * time.Second},
		},
		clientInfo: Implementation{Name: "drunmcp-go", Version: "0.1.0"},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// InitializeResult is the server's reply to the initialize handshake.
type InitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ServerInfo      Implementation  `json:"serverInfo"`
	Instructions    string          `json:"instructions,omitempty"`
}

// Initialize performs the MCP initialize handshake and then sends the
// notifications/initialized notification, as the MCP lifecycle requires. It
// must be called before ListTools or CallTool.
//
// drun-mcp has no /health endpoint, so a successful Initialize is also the way
// to probe that the server is up.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      c.clientInfo,
	}
	var result InitializeResult
	if err := c.call(ctx, "initialize", params, &result); err != nil {
		return nil, err
	}

	// Lock in the negotiated protocol version for subsequent requests.
	if result.ProtocolVersion != "" {
		c.transport.protocolVersion = result.ProtocolVersion
	} else {
		c.transport.protocolVersion = ProtocolVersion
	}
	c.initialized = true

	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return nil, fmt.Errorf("drunmcp: send initialized notification: %w", err)
	}
	return &result, nil
}

// Tool describes a tool advertised by the server via tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListToolsResult is the response to tools/list.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// ListTools returns the tools the server exposes.
func (c *Client) ListTools(ctx context.Context) (*ListToolsResult, error) {
	var result ListToolsResult
	if err := c.call(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ContentBlock is one entry of a tool result's content array.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallToolResult is the response to tools/call. drun-mcp returns each tool's
// structured payload as a JSON object in the text content block; many tools
// also populate StructuredContent.
type CallToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	// IsError reports a tool-level error (as opposed to a transport or
	// JSON-RPC error, which are returned as Go errors).
	IsError bool `json:"isError,omitempty"`
}

// Text returns the concatenation of all text content blocks.
func (r *CallToolResult) Text() string {
	var b strings.Builder
	for _, block := range r.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// Decode unmarshals the tool result's structured payload into v. It prefers
// StructuredContent and falls back to the concatenated text blocks, which for
// drun-mcp session tools is a JSON object.
func (r *CallToolResult) Decode(v any) error {
	if r.IsError {
		return fmt.Errorf("drunmcp: tool returned an error: %s", r.Text())
	}
	raw := r.StructuredContent
	if len(raw) == 0 {
		raw = json.RawMessage(strings.TrimSpace(r.Text()))
	}
	if len(raw) == 0 {
		return fmt.Errorf("drunmcp: tool result has no content to decode")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("drunmcp: decode tool result: %w", err)
	}
	return nil
}

// CallTool invokes a tool by name. args is marshalled as the tool's
// "arguments" object; pass nil for a tool that takes no arguments.
func (c *Client) CallTool(ctx context.Context, name string, args any) (*CallToolResult, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	var result CallToolResult
	if err := c.call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// call sends a request expecting a response and unmarshals result into out.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	req := &jsonRPCRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	resp, err := c.transport.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("drunmcp: no response for %s", method)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("drunmcp: decode %s result: %w", method, err)
		}
	}
	return nil
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) notify(ctx context.Context, method string, params any) error {
	req := &jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params}
	_, err := c.transport.roundTrip(ctx, req)
	return err
}
