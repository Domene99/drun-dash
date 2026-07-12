// Package memberui implements the drun-dash member console: a small,
// server-rendered HTTP app (Go + templ + HTMX) an organization member runs on
// their own workstation to sign in, see the agents they are entitled to, and
// launch or terminate a governed session.
//
// The member console is a separate binary from the control plane and never
// imports control-plane packages; it talks to the control plane over HTTP
// through [Client]. Members sign in through the OAuth 2.0 device-authorization
// flow (RFC 8628): the console requests a device+user code, shows the user code
// and a verification link the member approves in the admin UI, then polls for
// the minted control-plane session JWT. The JWT is stored in a secure, HttpOnly
// cookie and attached as an Authorization: Bearer header on every subsequent
// API call (see server.go).
//
// This package deliberately mirrors internal/ui (the admin UI) without
// importing it: the two frontends serve different audiences, so the small
// server/cookie/client pieces are replicated rather than shared.
package memberui

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

// requestTimeout bounds each individual control-plane request so one slow
// endpoint cannot hang a page render indefinitely.
const requestTimeout = 15 * time.Second

// maxResponseBytes caps how much of a control-plane response body the client
// reads, guarding against an unexpectedly large or hostile payload.
const maxResponseBytes int64 = 1 << 20

// deviceGrantType is the RFC 8628 grant_type the device-token poll must send.
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// ErrUnauthorized is returned when the control plane rejects the session token
// with HTTP 401. The server maps it to a redirect back to /login (clearing the
// stale cookie) rather than surfacing a generic error.
var ErrUnauthorized = errors.New("memberui: control plane rejected the session token")

// Device-flow poll outcomes, mapped from the RFC 8628 OAuth error codes the
// control plane's POST /auth/device/token returns while a request is not yet
// grantable. The server drives the login page from these: keep polling on
// pending/slow-down, stop with a message on denied/expired.
var (
	// ErrAuthorizationPending means the member has not yet approved the request;
	// the console should keep polling.
	ErrAuthorizationPending = errors.New("memberui: authorization pending")
	// ErrSlowDown means the console polled faster than the interval; it should
	// keep polling but back off.
	ErrSlowDown = errors.New("memberui: slow down")
	// ErrAccessDenied means the member denied the request.
	ErrAccessDenied = errors.New("memberui: access denied")
	// ErrExpiredToken means the device_code expired or is unknown; the member
	// must restart the login.
	ErrExpiredToken = errors.New("memberui: device code expired")
)

// APIError is a non-2xx control-plane response decoded from the flat Error
// envelope (openapi/v1.yaml #/components/schemas/Error: a bare {code, message}
// object). Status carries the HTTP status even when the body is not a
// well-formed Error, so callers can feature-detect (for example, a 404).
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

// Client is a tiny control-plane API client performing the handful of calls the
// member console needs: the device-authorization flow, the caller's entitled
// agents, and session launch/terminate. Construct it with [NewClient]; the zero
// value is not usable.
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

// DeviceAuthorization mirrors the DeviceAuthorizationResponse schema (POST
// /auth/device/code): the codes and verification URIs the console shows the
// member, plus the recommended poll interval.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// tokenResponse mirrors the TokenResponse schema (the device-token grant): the
// minted control-plane Bearer JWT plus its metadata.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
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

// SessionLaunch mirrors the SessionLaunchResponse schema (POST
// /sessions/launch): everything the client needs to run a governed session
// locally. PolicyBundle is held opaque (the console does not enforce policy
// itself in this shell); ProxyToken is a secret the console holds server-side
// and never renders or logs.
type SessionLaunch struct {
	SessionID     string          `json:"session_id"`
	ProxyBaseURL  string          `json:"proxy_base_url"`
	ProxyToken    string          `json:"proxy_token"`
	FanOutCeiling int             `json:"fan_out_ceiling"`
	ExpiresAt     string          `json:"expires_at,omitempty"`
	PolicyBundle  json.RawMessage `json:"policy_bundle,omitempty"`
}

// Session mirrors the Session schema returned by the terminate endpoint.
type Session struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	Status        string `json:"status"`
	FanOutCeiling int    `json:"fan_out_ceiling"`
	CreatedAt     string `json:"created_at"`
	TerminatedAt  string `json:"terminated_at,omitempty"`
}

// StartDeviceAuthorization calls POST /auth/device/code to begin the device
// flow for clientID. It is unauthenticated (the flow is how the member gets a
// token in the first place), so no Bearer credential is sent.
func (c *Client) StartDeviceAuthorization(ctx context.Context, clientID string) (*DeviceAuthorization, error) {
	body := struct {
		ClientID string `json:"client_id"`
	}{ClientID: clientID}
	var da DeviceAuthorization
	if err := c.postJSON(ctx, "", "/auth/device/code", body, &da); err != nil {
		return nil, err
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return nil, errors.New("memberui: /auth/device/code returned no device_code/user_code")
	}
	return &da, nil
}

// PollDeviceToken calls POST /auth/device/token with the device_code. When the
// request has been approved it returns the minted control-plane session JWT.
// While the request is not yet grantable the control plane returns an RFC 8628
// OAuth error, which this maps to one of [ErrAuthorizationPending],
// [ErrSlowDown], [ErrAccessDenied], or [ErrExpiredToken] so the caller can
// decide whether to keep polling.
func (c *Client) PollDeviceToken(ctx context.Context, deviceCode string) (string, error) {
	body := struct {
		DeviceCode string `json:"device_code"`
		GrantType  string `json:"grant_type"`
	}{DeviceCode: deviceCode, GrantType: deviceGrantType}

	var tr tokenResponse
	err := c.postJSON(ctx, "", "/auth/device/token", body, &tr)
	if err == nil {
		if tr.AccessToken == "" {
			return "", errors.New("memberui: /auth/device/token returned no access_token")
		}
		return tr.AccessToken, nil
	}

	// The device-token endpoint reports not-yet-grantable states with the RFC
	// 8628 OAuth error envelope ({error, error_description}), decoded here into
	// APIError.Code. Map the known codes to sentinels; anything else propagates.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "authorization_pending":
			return "", ErrAuthorizationPending
		case "slow_down":
			return "", ErrSlowDown
		case "access_denied":
			return "", ErrAccessDenied
		case "expired_token":
			return "", ErrExpiredToken
		}
	}
	return "", err
}

// GetEntitledAgents returns the agents the caller may launch. It prefers GET
// /me/agents (the caller's entitled agents) and falls back to GET /agents (the
// full catalog) when /me/agents is not present yet — the control-plane endpoint
// ships in a sibling change, so the console feature-detects on a 404 rather
// than hard-depending on merge order.
func (c *Client) GetEntitledAgents(ctx context.Context, token string) (*AgentList, error) {
	var list AgentList
	err := c.getJSON(ctx, token, "/me/agents", nil, &list)
	if err == nil {
		return &list, nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		var fallback AgentList
		if err := c.getJSON(ctx, token, "/agents", nil, &fallback); err != nil {
			return nil, err
		}
		return &fallback, nil
	}
	return nil, err
}

// LaunchSession calls POST /sessions/launch for agentID and returns the launch
// response, including the secret proxy_token the caller must hold server-side.
func (c *Client) LaunchSession(ctx context.Context, token, agentID string) (*SessionLaunch, error) {
	body := struct {
		AgentID string `json:"agent_id"`
	}{AgentID: agentID}
	var launch SessionLaunch
	if err := c.postJSON(ctx, token, "/sessions/launch", body, &launch); err != nil {
		return nil, err
	}
	return &launch, nil
}

// TerminateSession calls POST /sessions/{id}/terminate to end a running session
// and revoke its proxy credentials. It returns the updated session.
func (c *Client) TerminateSession(ctx context.Context, token, sessionID string) (*Session, error) {
	var session Session
	path := "/sessions/" + url.PathEscape(sessionID) + "/terminate"
	if err := c.postJSON(ctx, token, path, struct{}{}, &session); err != nil {
		return nil, err
	}
	return &session, nil
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
// token omits the Authorization header, for the unauthenticated device-flow
// calls. Status mapping mirrors [Client.getJSON].
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
			return fmt.Errorf("memberui: decode %s response: %w", path, err)
		}
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		return apiErrorFromResponse(resp.StatusCode, body)
	}
}

// apiErrorFromResponse builds an [*APIError] from a non-2xx response. It decodes
// both the flat Error envelope ({code, message}) and the RFC 8628 OAuth error
// envelope ({error, error_description}) into the same struct, so the
// device-token poll and the ordinary endpoints share one error path.
func apiErrorFromResponse(status int, body []byte) error {
	e := &APIError{Status: status}
	var envelope struct {
		Code             string `json:"code"`
		Message          string `json:"message"`
		OAuthError       string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		switch {
		case envelope.Code != "":
			e.Code = envelope.Code
			e.Message = envelope.Message
		case envelope.OAuthError != "":
			e.Code = envelope.OAuthError
			e.Message = envelope.ErrorDescription
		}
	}
	return e
}
