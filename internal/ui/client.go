// Package ui implements the drun-dash admin web app: a small, server-rendered
// HTTP server (Go + templ + HTMX) that renders a login page and a dashboard.
//
// The admin UI is a separate binary from the control plane and never imports
// control-plane packages; it talks to the control plane over HTTP through
// [Client]. Operators authenticate by pasting a control-plane session token on
// the login page; the server stores it in a secure, HttpOnly cookie and
// attaches it as an Authorization: Bearer header on every API call (see
// server.go). The full OIDC authorization-code browser flow is a documented
// follow-up (see README.md).
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds each individual control-plane request. It is
// deliberately per-request so one slow endpoint cannot hang a page render
// indefinitely.
const requestTimeout = 15 * time.Second

// maxResponseBytes caps how much of a control-plane response body the client
// reads, guarding against an unexpectedly large or hostile payload.
const maxResponseBytes int64 = 1 << 20

// ErrUnauthorized is returned when the control plane rejects the session token
// with HTTP 401. The server maps it to a redirect back to the login page (and
// clears the stale cookie) rather than surfacing a generic error.
var ErrUnauthorized = errors.New("ui: control plane rejected the session token")

// APIError is a non-2xx control-plane response decoded from the flat Error
// envelope (openapi/v1.yaml #/components/schemas/Error: a bare {code, message}
// object, not wrapped in {"error": ...}). Status carries the HTTP status even
// when the body is not a well-formed Error.
type APIError struct {
	Status  int
	Code    string
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("control plane: %s: %s", e.Code, e.Message)
	case e.Message != "":
		return "control plane: " + e.Message
	case e.Code != "":
		return "control plane: " + e.Code
	default:
		return fmt.Sprintf("control plane: unexpected status %d", e.Status)
	}
}

// Client is a tiny read-only control-plane API client. It performs the handful
// of GET calls the dashboard needs, sending the caller's session token as a
// Bearer credential and decoding either the typed success body or the flat
// Error envelope. Construct it with [NewClient]; the zero value is not usable.
type Client struct {
	baseURL string
	hc      *http.Client
}

// NewClient returns a Client targeting the control plane at baseURL (for
// example "http://localhost:8080"). A trailing slash is trimmed so path
// concatenation stays clean. If hc is nil a client with a per-request timeout
// is used; tests pass their own to route requests at a mock server.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), hc: hc}
}

// --- response schemas, mirroring openapi/v1.yaml ---

// Identity mirrors the Identity schema (GET /auth/whoami).
type Identity struct {
	Subject        string   `json:"subject"`
	OrganizationID string   `json:"organization_id"`
	Email          string   `json:"email,omitempty"`
	Roles          []string `json:"roles"`
}

// Agent mirrors the Agent schema. LatestVersion is a pointer so "no published
// version" is distinguishable from version 0.
type Agent struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	LatestVersion *int   `json:"latest_version,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// AgentList mirrors the AgentList schema: a page of agents plus the optional
// keyset cursor.
type AgentList struct {
	Items      []Agent `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// Session mirrors the Session schema.
type Session struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	AgentVersion  *int   `json:"agent_version,omitempty"`
	PolicyID      string `json:"policy_id,omitempty"`
	PolicyVersion *int   `json:"policy_version,omitempty"`
	Status        string `json:"status"`
	FanOutCeiling int    `json:"fan_out_ceiling"`
	CreatedAt     string `json:"created_at"`
	TerminatedAt  string `json:"terminated_at,omitempty"`
}

// SessionList mirrors the SessionList schema.
type SessionList struct {
	Items      []Session `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// SpendGroup mirrors the SpendGroup schema. Tokens is a pointer so an absent
// count is distinguishable from zero.
type SpendGroup struct {
	Key    string  `json:"key"`
	Amount float64 `json:"amount"`
	Tokens *int64  `json:"tokens,omitempty"`
}

// SpendReport mirrors the SpendReport schema.
type SpendReport struct {
	From     string       `json:"from"`
	To       string       `json:"to"`
	Currency string       `json:"currency"`
	Total    float64      `json:"total"`
	Groups   []SpendGroup `json:"groups"`
}

// Whoami calls GET /auth/whoami and returns the authenticated identity.
func (c *Client) Whoami(ctx context.Context, token string) (*Identity, error) {
	var id Identity
	if err := c.getJSON(ctx, token, "/auth/whoami", nil, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// ListAgents calls GET /agents and returns the first page of the catalog.
func (c *Client) ListAgents(ctx context.Context, token string) (*AgentList, error) {
	var list AgentList
	if err := c.getJSON(ctx, token, "/agents", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// ListSessions calls GET /sessions and returns the first page of governed
// sessions.
func (c *Client) ListSessions(ctx context.Context, token string) (*SessionList, error) {
	var list SessionList
	if err := c.getJSON(ctx, token, "/sessions", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// QuerySpend calls GET /analytics/spend for the given window and grouping. Any
// of from, to, and groupBy may be empty, in which case the parameter is
// omitted and the control plane applies its default.
func (c *Client) QuerySpend(ctx context.Context, token, from, to, groupBy string) (*SpendReport, error) {
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	if groupBy != "" {
		q.Set("group_by", groupBy)
	}
	var report SpendReport
	if err := c.getJSON(ctx, token, "/analytics/spend", q, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// getJSON issues an authenticated GET to path (with optional query params) and
// decodes a 200 body into dst. A 401 maps to [ErrUnauthorized]; any other
// non-200 maps to an [*APIError] carrying the flat Error envelope when present.
func (c *Client) getJSON(ctx context.Context, token, path string, query url.Values, dst any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("ui: decode %s response: %w", path, err)
		}
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		return apiErrorFromResponse(resp.StatusCode, body)
	}
}

// apiErrorFromResponse builds an [*APIError] from a non-2xx response, decoding
// the flat Error envelope when the body is well-formed and falling back to the
// bare status otherwise.
func apiErrorFromResponse(status int, body []byte) error {
	e := &APIError{Status: status}
	var flat struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &flat); err == nil {
		e.Code = flat.Code
		e.Message = flat.Message
	}
	return e
}
