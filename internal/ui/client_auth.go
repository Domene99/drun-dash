package ui

import (
	"context"
	"errors"
	"net/url"
)

// This file adds the control-plane client methods the local-auth onboarding
// pages (ADR-0008) and the agent-create form need: password signup/login, invite
// acceptance, member invitation, and agent creation + first-version publish.
// They follow the same shape as the calls in client.go — send the caller's
// session token as a Bearer credential (or none, for the public auth endpoints)
// and decode the typed success body or the flat Error envelope. Request/response
// schemas mirror openapi/v1.yaml and internal/httpapi/local_auth.go.

// SignupRequest is the POST /auth/signup body: create an organization and make
// the caller its Owner. OrgSlug is optional — the control plane derives it from
// OrgName when omitted; Name is the owner's optional display name.
type SignupRequest struct {
	OrgName  string `json:"org_name"`
	OrgSlug  string `json:"org_slug,omitempty"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Password string `json:"password"`
}

// Signup calls POST /auth/signup and returns the minted session token. The
// endpoint is public (no Bearer credential). A duplicate slug/email surfaces as
// an [*APIError] with status 409.
func (c *Client) Signup(ctx context.Context, req SignupRequest) (string, error) {
	var tr TokenResponse
	if err := c.postJSON(ctx, "", "/auth/signup", req, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", errors.New("ui: /auth/signup returned no access_token")
	}
	return tr.AccessToken, nil
}

// Login calls POST /auth/login with the org slug, email, and password and
// returns the minted session token. The endpoint is public. Every failure —
// unknown org/email, wrong password, inactive member — is the same generic 401,
// which the client maps to [ErrUnauthorized]; the caller renders it as an
// invalid-credentials message rather than a session-expired redirect.
func (c *Client) Login(ctx context.Context, orgSlug, email, password string) (string, error) {
	body := struct {
		OrgSlug  string `json:"org_slug"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}{OrgSlug: orgSlug, Email: email, Password: password}
	var tr TokenResponse
	if err := c.postJSON(ctx, "", "/auth/login", body, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", errors.New("ui: /auth/login returned no access_token")
	}
	return tr.AccessToken, nil
}

// AcceptInvite calls POST /auth/accept-invite, redeeming a raw invite token to
// activate the pending member with the supplied name and password, and returns
// the minted session token. The endpoint is public. An invalid, expired, or
// already-consumed token is a generic 401 ([ErrUnauthorized]).
func (c *Client) AcceptInvite(ctx context.Context, token, name, password string) (string, error) {
	body := struct {
		Token    string `json:"token"`
		Name     string `json:"name,omitempty"`
		Password string `json:"password"`
	}{Token: token, Name: name, Password: password}
	var tr TokenResponse
	if err := c.postJSON(ctx, "", "/auth/accept-invite", body, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", errors.New("ui: /auth/accept-invite returned no access_token")
	}
	return tr.AccessToken, nil
}

// InviteResult mirrors the POST /members/invite success body: the pending member
// plus the invite link and raw token, surfaced once (no mailer in v1 — the admin
// hands the link off, ADR-0008 §6), and the token's expiry.
type InviteResult struct {
	Member      Member `json:"member"`
	InviteToken string `json:"invite_token"`
	InviteLink  string `json:"invite_link"`
	ExpiresAt   string `json:"expires_at"`
}

// InviteMember calls POST /members/invite to create a pending member and its
// invitation carrying the pre-selected roles, returning the invite link to hand
// off. Requires members.manage. A duplicate email is a 409; an unknown or
// cross-org role id is a 404.
func (c *Client) InviteMember(ctx context.Context, token, email string, roleIDs []string) (*InviteResult, error) {
	if roleIDs == nil {
		roleIDs = []string{}
	}
	body := struct {
		Email   string   `json:"email"`
		RoleIDs []string `json:"role_ids"`
	}{Email: email, RoleIDs: roleIDs}
	var result InviteResult
	if err := c.postJSON(ctx, token, "/members/invite", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AgentCreate is the POST /agents body (the AgentCreate schema): a new catalog
// agent. Name is required; DefaultPolicyID is the optional policy boundary.
type AgentCreate struct {
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	DefaultPolicyID string `json:"default_policy_id,omitempty"`
}

// CreateAgent calls POST /agents to register a catalog agent and returns it.
// Requires agents.manage.
func (c *Client) CreateAgent(ctx context.Context, token string, req AgentCreate) (*Agent, error) {
	var agent Agent
	if err := c.postJSON(ctx, token, "/agents", req, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// AgentUpdate is the PATCH /agents/{agentId} body (the apiAgentUpdate schema):
// a partial identity update. Both fields are optional pointers so an unset field
// is omitted from the request and left unchanged server-side.
type AgentUpdate struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

// UpdateAgent calls PATCH /agents/{agentId} with the supplied partial update and
// returns the updated agent. Requires agents.manage.
func (c *Client) UpdateAgent(ctx context.Context, token, id string, name, description *string) (*Agent, error) {
	var agent Agent
	path := "/agents/" + url.PathEscape(id)
	if err := c.patchJSON(ctx, token, path, AgentUpdate{Name: name, Description: description}, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// AgentVersionCreate is the POST /agents/{id}/versions body (the
// AgentVersionCreate schema): the immutable version manifest. SystemPrompt is
// required; the endpoint/budget/policy-version pins are optional and can be set
// later.
type AgentVersionCreate struct {
	SystemPrompt    string   `json:"system_prompt"`
	AllowedTools    []string `json:"allowed_tools,omitempty"`
	PolicyVersionID string   `json:"policy_version_id,omitempty"`
	EndpointID      string   `json:"endpoint_id,omitempty"`
	BudgetID        string   `json:"budget_id,omitempty"`
	FanOutCeiling   int      `json:"fan_out_ceiling"`
	Notes           string   `json:"notes,omitempty"`
}

// PublishAgentVersion calls POST /agents/{agentId}/versions to publish an
// immutable version and returns it. Requires agents.manage.
func (c *Client) PublishAgentVersion(ctx context.Context, token, agentID string, req AgentVersionCreate) (*AgentVersion, error) {
	var version AgentVersion
	path := "/agents/" + url.PathEscape(agentID) + "/versions"
	if err := c.postJSON(ctx, token, path, req, &version); err != nil {
		return nil, err
	}
	return &version, nil
}
