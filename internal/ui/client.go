// Package ui implements the drun-dash admin web app: a small, server-rendered
// HTTP server (Go + templ + HTMX) that renders a login page and a dashboard.
//
// The admin UI is a separate binary from the control plane and never imports
// control-plane packages; it talks to the control plane over HTTP through
// [Client]. The primary sign-in is a local email + password login scoped to an
// organization (ADR-0008): the owner signs up their org ([Client.Signup]), logs
// in ([Client.Login]), and invites teammates who accept via [Client.AcceptInvite]
// — each minting a control-plane session JWT. An optional OIDC authorization-code
// browser flow (see oidc.go) exchanges a verified ID token for a session JWT via
// [Client.ExchangeSession] (POST /auth/session), and a paste-a-token dev login
// stays available for local development. However the member signs in, the JWT is
// stored in a secure, HttpOnly cookie and attached as an Authorization: Bearer
// header on every subsequent API call (see server.go).
package ui

import (
	"bytes"
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

// Client is a tiny control-plane API client. It performs the handful of calls
// the dashboard needs, sending the caller's session token as a Bearer
// credential and decoding either the typed success body or the flat Error
// envelope. Construct it with [NewClient]; the zero value is not usable.
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

// Identity mirrors the Identity schema (GET /auth/whoami). Permissions carries
// the caller's effective permission set (ADR-0006 §7), resolved by the control
// plane per request; the admin UI uses it to show or hide the management actions
// the caller lacks permission for. Name, OrgName, and OrgSlug are the enriched,
// human-readable fields the top bar renders in plain text so the visible
// identity is a person and an organization, never a raw "local:<uuid>" subject
// or an org UUID (all optional — an older control plane simply omits them).
type Identity struct {
	Subject        string   `json:"subject"`
	OrganizationID string   `json:"organization_id"`
	Name           string   `json:"name,omitempty"`
	Email          string   `json:"email,omitempty"`
	OrgName        string   `json:"org_name,omitempty"`
	OrgSlug        string   `json:"org_slug,omitempty"`
	Roles          []string `json:"roles"`
	Permissions    []string `json:"permissions"`
}

// TokenResponse mirrors the TokenResponse schema (POST /auth/session and the
// device-token grant): the minted control-plane Bearer JWT plus its metadata.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// Agent mirrors the Agent schema. LatestVersion is a pointer so "no published
// version" is distinguishable from version 0.
type Agent struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	DefaultPolicyID string `json:"default_policy_id,omitempty"`
	LatestVersion   *int   `json:"latest_version,omitempty"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at,omitempty"`
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

// Budget mirrors the Budget schema: a configured spend ceiling for a period and
// how much of it has been consumed so far.
type Budget struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Limit    float64 `json:"limit"`
	Spent    float64 `json:"spent"`
	Currency string  `json:"currency"`
	Period   string  `json:"period"`
}

// BudgetList mirrors the BudgetList schema.
type BudgetList struct {
	Items []Budget `json:"items"`
}

// Approval mirrors the Approval schema: one entry in the escalation queue. The
// decided_by / decided_at fields are populated only once a decision is made.
type Approval struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Status    string `json:"status"`
	DecidedBy string `json:"decided_by,omitempty"`
	DecidedAt string `json:"decided_at,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ApprovalList mirrors the ApprovalList schema.
type ApprovalList struct {
	Items      []Approval `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// TrajectoryField mirrors the TrajectoryField schema: one captured field within
// a trajectory event. Value is present only for payload-class fields the policy
// permitted; ByteSize is a pointer so an absent size differs from zero.
type TrajectoryField struct {
	Key        string          `json:"key"`
	FieldClass string          `json:"field_class"`
	Value      json.RawMessage `json:"value,omitempty"`
	ValueType  string          `json:"value_type,omitempty"`
	ByteSize   *int64          `json:"byte_size,omitempty"`
}

// TrajectoryEvent mirrors the TrajectoryEvent schema: one node in a session's
// captured decision graph. ParentEventID links a child to its parent event;
// root events leave it empty. FieldClass is the event's capture class
// (structure or payload).
type TrajectoryEvent struct {
	EventID       string            `json:"event_id"`
	Type          string            `json:"type"`
	OccurredAt    string            `json:"occurred_at"`
	FieldClass    string            `json:"field_class"`
	ParentEventID string            `json:"parent_event_id,omitempty"`
	Fields        []TrajectoryField `json:"fields,omitempty"`
}

// TrajectoryEventList mirrors the TrajectoryEventList schema: a page of a
// session's trajectory events, forming a tree via parent_event_id.
type TrajectoryEventList struct {
	Items      []TrajectoryEvent `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// ExchangeSession calls POST /auth/session, handing the control plane the OIDC
// ID token the admin UI obtained from the identity provider (and the admin's
// organization) in exchange for a control-plane session JWT. The endpoint is
// unauthenticated — the ID token is the credential — so no Bearer token is
// sent; the control plane verifies the ID token's signature and claims. It
// returns the access_token from the [TokenResponse].
func (c *Client) ExchangeSession(ctx context.Context, idToken, orgID string) (string, error) {
	body := struct {
		IDToken        string `json:"id_token"`
		OrganizationID string `json:"organization_id,omitempty"`
	}{IDToken: idToken, OrganizationID: orgID}
	var tr TokenResponse
	if err := c.postJSON(ctx, "", "/auth/session", body, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", errors.New("ui: /auth/session returned no access_token")
	}
	return tr.AccessToken, nil
}

// ApproveDevice calls POST /auth/device/approve to approve or deny a pending
// CLI device-authorization request, identified by the user_code the operator
// confirmed in the browser. decision must be "approve" or "deny". The member's
// session token authorizes the call; any 2xx response is success. This is the
// step that completes the CLI `drun-dash login` device flow.
func (c *Client) ApproveDevice(ctx context.Context, token, userCode, decision string) error {
	body := struct {
		UserCode string `json:"user_code"`
		Decision string `json:"decision"`
	}{UserCode: userCode, Decision: decision}
	return c.postJSON(ctx, token, "/auth/device/approve", body, nil)
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

// ListBudgets calls GET /analytics/budgets and returns the configured budgets
// with their current utilization (spent versus limit).
func (c *Client) ListBudgets(ctx context.Context, token string) (*BudgetList, error) {
	var list BudgetList
	if err := c.getJSON(ctx, token, "/analytics/budgets", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// ListApprovals calls GET /approvals, optionally filtered by status (an empty
// status returns every state). It backs the admin UI approval inbox.
func (c *Client) ListApprovals(ctx context.Context, token, status string) (*ApprovalList, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	var list ApprovalList
	if err := c.getJSON(ctx, token, "/approvals", q, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// DecideApproval calls POST /approvals/{id}/decision to approve or deny a queued
// escalation. decision must be "approve" or "deny"; reason is an optional
// rationale recorded in the audit trail. It returns the updated approval.
func (c *Client) DecideApproval(ctx context.Context, token, id, decision, reason string) (*Approval, error) {
	body := struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason,omitempty"`
	}{Decision: decision, Reason: reason}
	var approval Approval
	path := "/approvals/" + url.PathEscape(id) + "/decision"
	if err := c.postJSON(ctx, token, path, body, &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}

// GetSessionTrajectory calls GET /sessions/{sessionId}/trajectory and returns
// the session's recorded trajectory events (the fork/rollback decision graph).
// The events form a tree via parent_event_id; the caller reconstructs and
// renders it.
func (c *Client) GetSessionTrajectory(ctx context.Context, token, sessionID string) (*TrajectoryEventList, error) {
	var list TrajectoryEventList
	path := "/sessions/" + url.PathEscape(sessionID) + "/trajectory"
	if err := c.getJSON(ctx, token, path, nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// getJSON issues a GET to path (with optional query params) and decodes a 2xx
// body into dst. An empty token omits the Authorization header. A 401 maps to
// [ErrUnauthorized]; any other non-2xx maps to an [*APIError] carrying the flat
// Error envelope when present.
func (c *Client) getJSON(ctx context.Context, token, path string, query url.Values, dst any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")

	return c.do(req, path, dst)
}

// postJSON issues a POST to path with a JSON request body and decodes a 2xx
// body into dst (dst may be nil when the response body is not needed). An empty
// token omits the Authorization header, for the unauthenticated /auth/session
// exchange. Status mapping mirrors [Client.getJSON]: 401 maps to
// [ErrUnauthorized]; any other non-2xx maps to an [*APIError].
func (c *Client) postJSON(ctx context.Context, token, path string, reqBody, dst any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	return c.do(req, path, dst)
}

// do performs req, reads a bounded response body, and decodes a 2xx into dst
// (when dst is non-nil). It centralizes the status-to-error mapping shared by
// getJSON and postJSON: 401 maps to [ErrUnauthorized]; any other non-2xx maps
// to an [*APIError].
func (c *Client) do(req *http.Request, path string, dst any) error {
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
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if dst == nil {
			return nil
		}
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
