package drunmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AcceptHeader is the value drun-mcp's Streamable HTTP transport requires on
// every request. The server responds with 406 Not Acceptable if the Accept
// header does not advertise text/event-stream, so this constant is sent on
// every POST — including notifications.
const AcceptHeader = "application/json, text/event-stream"

// ProtocolVersion is the MCP protocol revision this client speaks in the
// initialize handshake.
const ProtocolVersion = "2025-06-18"

// jsonRPCRequest is a single JSON-RPC 2.0 request. A nil ID marks a
// notification, which expects no response body.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse is a single JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object returned by the server. It satisfies
// the error interface so it can be returned directly from client calls.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("drunmcp: rpc error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("drunmcp: rpc error %d: %s", e.Code, e.Message)
}

// transport carries JSON-RPC messages over drun-mcp's Streamable HTTP
// transport. Each call is a single POST whose response is either a JSON object
// (Content-Type: application/json) or a Server-Sent Events stream
// (Content-Type: text/event-stream); both encode the same JSON-RPC response.
type transport struct {
	endpoint   string
	httpClient *http.Client

	// sessionID is the Mcp-Session-Id the server may assign on initialize; it
	// is echoed back on every subsequent request when non-empty.
	sessionID string
	// protocolVersion, once negotiated, is sent as the MCP-Protocol-Version
	// header on subsequent requests. It is empty until Initialize completes so
	// the initialize request itself carries no version header.
	protocolVersion string
}

// roundTrip sends one JSON-RPC request and returns the matching response. For
// notifications (req.ID == nil) it returns (nil, nil) once the server accepts
// the message.
func (t *transport) roundTrip(ctx context.Context, req *jsonRPCRequest) (*jsonRPCResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("drunmcp: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("drunmcp: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", AcceptHeader)
	if t.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	if t.protocolVersion != "" {
		httpReq.Header.Set("MCP-Protocol-Version", t.protocolVersion)
	}

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("drunmcp: POST %s: %w", t.endpoint, err)
	}
	defer resp.Body.Close()

	// The server assigns a session id on initialize; remember it for the rest
	// of this client's lifetime.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.sessionID = sid
	}

	if err := statusError(t.endpoint, resp); err != nil {
		return nil, err
	}

	// A notification carries no id and expects no body (202 Accepted).
	if req.ID == nil {
		io.Copy(io.Discard, resp.Body)
		return nil, nil
	}

	switch mediaType(resp.Header.Get("Content-Type")) {
	case "text/event-stream":
		return parseSSEResponse(resp.Body, req.ID)
	case "application/json", "":
		return parseJSONResponse(resp.Body)
	default:
		return nil, fmt.Errorf("drunmcp: unexpected Content-Type %q from %s",
			resp.Header.Get("Content-Type"), t.endpoint)
	}
}

// statusError maps notable HTTP status codes to actionable errors. The 406 and
// 404 cases correspond to the failure modes documented in the drun-mcp skill.
func statusError(endpoint string, resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotAcceptable: // 406
		return fmt.Errorf("drunmcp: 406 Not Acceptable from %s: the Accept header must include %q "+
			"(this client sends %q); a 406 means the request reached drun-mcp without text/event-stream",
			endpoint, "text/event-stream", AcceptHeader)
	case resp.StatusCode == http.StatusNotFound: // 404
		return fmt.Errorf("drunmcp: 404 Not Found from %s: no MCP endpoint at this path. "+
			"drun-mcp serves MCP at /mcp and has no /health endpoint — probe liveness with Initialize instead",
			endpoint)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("drunmcp: unexpected status %d from %s: %s",
			resp.StatusCode, endpoint, strings.TrimSpace(string(snippet)))
	}
}

// mediaType returns the lowercased media type from a Content-Type header,
// stripping any parameters such as "; charset=utf-8".
func mediaType(header string) string {
	if i := strings.IndexByte(header, ';'); i >= 0 {
		header = header[:i]
	}
	return strings.ToLower(strings.TrimSpace(header))
}

func parseJSONResponse(r io.Reader) (*jsonRPCResponse, error) {
	var resp jsonRPCResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, fmt.Errorf("drunmcp: decode json response: %w", err)
	}
	return &resp, nil
}

// parseSSEResponse reads a Server-Sent Events stream and returns the first
// JSON-RPC response whose id matches wantID. Non-JSON frames and unrelated
// messages (e.g. server-side log events) are skipped.
func parseSSEResponse(r io.Reader, wantID *int64) (*jsonRPCResponse, error) {
	scanner := bufio.NewScanner(r)
	// SSE data lines (e.g. a large diff) can exceed the default 64KB token.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var data strings.Builder
	// flush attempts to decode the accumulated data buffer as a matching
	// JSON-RPC response. It returns (resp, true) on a match and always clears
	// the buffer.
	flush := func() *jsonRPCResponse {
		defer data.Reset()
		if data.Len() == 0 {
			return nil
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal([]byte(data.String()), &resp); err != nil {
			return nil // not a JSON-RPC frame; ignore
		}
		if matchID(resp.ID, wantID) {
			return &resp
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "": // end of an event frame
			if resp := flush(); resp != nil {
				return resp, nil
			}
		case strings.HasPrefix(line, ":"): // SSE comment
			continue
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(d)
		default:
			// event:, id:, retry: and other fields are irrelevant here.
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("drunmcp: read event stream: %w", err)
	}
	// Handle a final frame not terminated by a blank line.
	if resp := flush(); resp != nil {
		return resp, nil
	}
	return nil, fmt.Errorf("drunmcp: event stream ended without a response for request id %d", *wantID)
}

// matchID reports whether a response id equals the request id we are waiting
// for. Both drun-mcp and this client use integer ids.
func matchID(raw json.RawMessage, want *int64) bool {
	if want == nil || len(raw) == 0 {
		return false
	}
	var id int64
	if err := json.Unmarshal(raw, &id); err != nil {
		return false
	}
	return id == *want
}
