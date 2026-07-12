package memberui

import (
	"context"
)

// Approval mirrors the OpenAPI Approval schema returned by POST /approvals: the
// created escalation request and its current status.
type Approval struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at,omitempty"`
}

// CreateApproval raises an escalation request (POST /approvals) when a tool call
// the model proposed exceeds the session's policy boundary (ADR-0004 §6). action
// names the blocked operation and reason explains why it was blocked; decisionCtx
// carries structure-only decision aids — never raw private payloads, consistent
// with the residency posture (§5). The returned Approval starts pending until an
// admin decides it in the admin UI.
func (c *Client) CreateApproval(ctx context.Context, token, sessionID, action, reason string, decisionCtx map[string]any) (*Approval, error) {
	body := struct {
		SessionID string         `json:"session_id"`
		Action    string         `json:"action"`
		Reason    string         `json:"reason,omitempty"`
		Context   map[string]any `json:"context,omitempty"`
	}{SessionID: sessionID, Action: action, Reason: reason, Context: decisionCtx}
	var approval Approval
	if err := c.postJSON(ctx, token, "/approvals", body, &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}
