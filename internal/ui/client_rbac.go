package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// This file adds the control-plane client methods the RBAC governance pages
// (Roles, Members) and the read-only Agents/Policies detail views need. They
// follow the same shape as the calls in client.go: send the caller's session
// token as a Bearer credential and decode the typed success body or the flat
// Error envelope. The response schemas mirror openapi/v1.yaml (ADR-0006 §7).

// RoleAgent mirrors the RoleAgent schema: one agent granted by a role, with the
// optional per-grant policy-version and fan-out overrides. On a create/update
// request only AgentID is required.
type RoleAgent struct {
	AgentID         string `json:"agent_id"`
	PolicyVersionID string `json:"policy_version_id,omitempty"`
	MaxFanOut       *int   `json:"max_fan_out,omitempty"`
}

// Role mirrors the Role schema: a role with its permission set and agent access
// hydrated. Builtin marks the four seeded roles whose name and permissions are
// immutable; AllAgents grants every agent in the org.
type Role struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Builtin     bool        `json:"builtin"`
	AllAgents   bool        `json:"all_agents"`
	Permissions []string    `json:"permissions"`
	Agents      []RoleAgent `json:"agents"`
	CreatedAt   string      `json:"created_at"`
}

// RoleList mirrors the RoleList schema — a plain {items} envelope (an org's
// roles are a small, bounded set returned whole).
type RoleList struct {
	Items []Role `json:"items"`
}

// RoleCreate mirrors the RoleCreate request body (POST /roles). Name is
// required; the created role is always custom.
type RoleCreate struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	AllAgents   bool        `json:"all_agents"`
	Permissions []string    `json:"permissions"`
	Agents      []RoleAgent `json:"agents"`
}

// RoleUpdate mirrors the RoleUpdate request body (PATCH /roles/{roleId}). Every
// field is a pointer so an omitted field is left unchanged and a supplied
// permissions/agents array replaces that set wholesale.
type RoleUpdate struct {
	Name        *string      `json:"name,omitempty"`
	Description *string      `json:"description,omitempty"`
	AllAgents   *bool        `json:"all_agents,omitempty"`
	Permissions *[]string    `json:"permissions,omitempty"`
	Agents      *[]RoleAgent `json:"agents,omitempty"`
}

// Member mirrors the Member schema: a member with the roles assigned directly
// to them (roles inherited via a team are not listed here).
type Member struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Active      bool   `json:"active"`
	Roles       []Role `json:"roles"`
}

// MemberList mirrors the MemberList schema — a plain {items} envelope.
type MemberList struct {
	Items []Member `json:"items"`
}

// AgentVersion mirrors the AgentVersion schema: one immutable published version
// of an agent.
type AgentVersion struct {
	AgentID        string `json:"agent_id"`
	Version        int    `json:"version"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
	Notes          string `json:"notes,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// AgentVersionList mirrors the AgentVersionList schema.
type AgentVersionList struct {
	Items []AgentVersion `json:"items"`
}

// Entitlement mirrors the Entitlement schema: which principal (user or group)
// may launch an agent and under which policy.
type Entitlement struct {
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
	PolicyID      string `json:"policy_id,omitempty"`
	MaxFanOut     *int   `json:"max_fan_out,omitempty"`
}

// EntitlementList mirrors the EntitlementList schema.
type EntitlementList struct {
	Items []Entitlement `json:"items"`
}

// Policy mirrors the Policy schema: a policy boundary and its current version
// pointer.
type Policy struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	CurrentVersion int    `json:"current_version"`
	CreatedAt      string `json:"created_at"`
}

// PolicyList mirrors the PolicyList schema: a page of policies plus the optional
// keyset cursor.
type PolicyList struct {
	Items      []Policy `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// PolicyBoundary mirrors the PolicyBoundary schema: the declarative boundary a
// policy version enforces. Every field is optional (FanOutCeiling is a pointer
// so an omitted ceiling differs from 0); the concrete rule schema is free-form
// and passed through as Rules.
type PolicyBoundary struct {
	AllowedTools  []string       `json:"allowed_tools,omitempty"`
	Network       string         `json:"network,omitempty"`
	FanOutCeiling *int           `json:"fan_out_ceiling,omitempty"`
	Rules         map[string]any `json:"rules,omitempty"`
}

// PolicyCreate is the POST /policies body: a new policy plus its initial
// boundary (the control plane auto-creates version 1). Name and Boundary are
// required.
type PolicyCreate struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Boundary    PolicyBoundary `json:"boundary"`
}

// SigningMetadata mirrors the SigningMetadata schema: the detached-signature
// provenance recorded for a policy version.
type SigningMetadata struct {
	Algorithm string `json:"algorithm,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	Signature string `json:"signature,omitempty"`
	SignedAt  string `json:"signed_at,omitempty"`
}

// PolicyVersion mirrors the PolicyVersion schema: one immutable, signed version
// of a policy boundary. ID is the version's stable identifier used to pin a
// policy onto an agent version (AgentVersionCreate.policy_version_id); it is
// optional here so an older control plane that omits it simply yields no pin.
type PolicyVersion struct {
	ID        string          `json:"id,omitempty"`
	PolicyID  string          `json:"policy_id"`
	Version   int             `json:"version"`
	Digest    string          `json:"digest"`
	Signing   SigningMetadata `json:"signing"`
	CreatedAt string          `json:"created_at"`
}

// PolicyVersionList mirrors the PolicyVersionList schema.
type PolicyVersionList struct {
	Items []PolicyVersion `json:"items"`
}

// ListRoles calls GET /roles and returns the org's roles (built-ins plus custom)
// with their permissions and agent access hydrated. Requires roles.manage.
func (c *Client) ListRoles(ctx context.Context, token string) (*RoleList, error) {
	var list RoleList
	if err := c.getJSON(ctx, token, "/roles", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// CreateRole calls POST /roles to create a custom role and returns it.
func (c *Client) CreateRole(ctx context.Context, token string, body RoleCreate) (*Role, error) {
	var role Role
	if err := c.postJSON(ctx, token, "/roles", body, &role); err != nil {
		return nil, err
	}
	return &role, nil
}

// GetRole calls GET /roles/{roleId} and returns the role.
func (c *Client) GetRole(ctx context.Context, token, id string) (*Role, error) {
	var role Role
	if err := c.getJSON(ctx, token, "/roles/"+url.PathEscape(id), nil, &role); err != nil {
		return nil, err
	}
	return &role, nil
}

// UpdateRole calls PATCH /roles/{roleId} with the supplied partial update and
// returns the re-read role reflecting every applied change.
func (c *Client) UpdateRole(ctx context.Context, token, id string, body RoleUpdate) (*Role, error) {
	var role Role
	if err := c.patchJSON(ctx, token, "/roles/"+url.PathEscape(id), body, &role); err != nil {
		return nil, err
	}
	return &role, nil
}

// DeleteRole calls DELETE /roles/{roleId}. A built-in role is a 409.
func (c *Client) DeleteRole(ctx context.Context, token, id string) error {
	return c.deleteJSON(ctx, token, "/roles/"+url.PathEscape(id))
}

// ListMembers calls GET /members and returns the org's members with the roles
// assigned directly to each. Visible to members.manage or audit.view.
func (c *Client) ListMembers(ctx context.Context, token string) (*MemberList, error) {
	var list MemberList
	if err := c.getJSON(ctx, token, "/members", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// UpdateMemberRoles calls PUT /members/{memberId}/roles to replace a member's
// directly-assigned role set with roleIDs, and returns the updated member.
func (c *Client) UpdateMemberRoles(ctx context.Context, token, memberID string, roleIDs []string) (*Member, error) {
	if roleIDs == nil {
		roleIDs = []string{}
	}
	body := struct {
		RoleIDs []string `json:"role_ids"`
	}{RoleIDs: roleIDs}
	var member Member
	path := "/members/" + url.PathEscape(memberID) + "/roles"
	if err := c.putJSON(ctx, token, path, body, &member); err != nil {
		return nil, err
	}
	return &member, nil
}

// GetAgent calls GET /agents/{agentId} and returns the catalog agent.
func (c *Client) GetAgent(ctx context.Context, token, id string) (*Agent, error) {
	var agent Agent
	if err := c.getJSON(ctx, token, "/agents/"+url.PathEscape(id), nil, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// ListAgentVersions calls GET /agents/{agentId}/versions and returns the agent's
// published versions.
func (c *Client) ListAgentVersions(ctx context.Context, token, id string) (*AgentVersionList, error) {
	var list AgentVersionList
	if err := c.getJSON(ctx, token, "/agents/"+url.PathEscape(id)+"/versions", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// ListAgentEntitlements calls GET /agents/{agentId}/entitlements and returns the
// principals entitled to launch the agent.
func (c *Client) ListAgentEntitlements(ctx context.Context, token, id string) (*EntitlementList, error) {
	var list EntitlementList
	if err := c.getJSON(ctx, token, "/agents/"+url.PathEscape(id)+"/entitlements", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// ListPolicies calls GET /policies and returns the first page of policies.
func (c *Client) ListPolicies(ctx context.Context, token string) (*PolicyList, error) {
	var list PolicyList
	if err := c.getJSON(ctx, token, "/policies", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// CreatePolicy calls POST /policies to create a policy and its first version
// (version 1) from the supplied boundary, returning the policy. A duplicate name
// is a 409. Requires policies.manage.
func (c *Client) CreatePolicy(ctx context.Context, token string, req PolicyCreate) (*Policy, error) {
	var policy Policy
	if err := c.postJSON(ctx, token, "/policies", req, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

// GetPolicy calls GET /policies/{policyId} and returns the policy.
func (c *Client) GetPolicy(ctx context.Context, token, id string) (*Policy, error) {
	var policy Policy
	if err := c.getJSON(ctx, token, "/policies/"+url.PathEscape(id), nil, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

// ListPolicyVersions calls GET /policies/{policyId}/versions and returns the
// policy's immutable, signed versions.
func (c *Client) ListPolicyVersions(ctx context.Context, token, id string) (*PolicyVersionList, error) {
	var list PolicyVersionList
	if err := c.getJSON(ctx, token, "/policies/"+url.PathEscape(id)+"/versions", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// GetPolicyRoles calls GET /policies/{policyId}/roles and returns the ids of the
// roles permitted to access agents governed by this policy (policy-centric
// access, ADR-0010). An empty slice means the policy grants no role.
func (c *Client) GetPolicyRoles(ctx context.Context, token, policyID string) ([]string, error) {
	var resp struct {
		RoleIDs []string `json:"role_ids"`
	}
	path := "/policies/" + url.PathEscape(policyID) + "/roles"
	if err := c.getJSON(ctx, token, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.RoleIDs, nil
}

// SetPolicyRoles calls PUT /policies/{policyId}/roles to replace the policy's
// permitted-role set with roleIDs. Editing permitted roles is free and unsigned,
// unlike publishing a new boundary version (which needs a policyctl signature),
// so this is the one policy mutation the browser performs. Requires
// policies.manage.
func (c *Client) SetPolicyRoles(ctx context.Context, token, policyID string, roleIDs []string) error {
	if roleIDs == nil {
		roleIDs = []string{}
	}
	body := struct {
		RoleIDs []string `json:"role_ids"`
	}{RoleIDs: roleIDs}
	path := "/policies/" + url.PathEscape(policyID) + "/roles"
	return c.putJSON(ctx, token, path, body, nil)
}

// putJSON issues a PUT with a JSON body, mirroring postJSON's status mapping.
func (c *Client) putJSON(ctx context.Context, token, path string, reqBody, dst any) error {
	return c.bodyJSON(ctx, http.MethodPut, token, path, reqBody, dst)
}

// patchJSON issues a PATCH with a JSON body, mirroring postJSON's status mapping.
func (c *Client) patchJSON(ctx context.Context, token, path string, reqBody, dst any) error {
	return c.bodyJSON(ctx, http.MethodPatch, token, path, reqBody, dst)
}

// deleteJSON issues a DELETE and treats any 2xx (including 204 No Content) as
// success. A 401 maps to [ErrUnauthorized]; any other non-2xx maps to an
// [*APIError].
func (c *Client) deleteJSON(ctx context.Context, token, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	return c.do(req, path, nil)
}

// bodyJSON is the shared body-carrying request path for PUT and PATCH. It
// marshals reqBody, sets the auth/content headers, and decodes a 2xx into dst
// (when non-nil), reusing [Client.do] for the status-to-error mapping.
func (c *Client) bodyJSON(ctx context.Context, method, token, path string, reqBody, dst any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
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
