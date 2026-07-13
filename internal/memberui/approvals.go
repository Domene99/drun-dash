package memberui

import (
	"context"
	"net/url"
	"strconv"
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

// ApprovalList mirrors the OpenAPI ApprovalList schema: a page of approvals plus
// the optional keyset cursor for the next page.
type ApprovalList struct {
	Items      []Approval `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// Approval status values (OpenAPI ApprovalStatus). approved is the only decision
// that resumes a blocked action; the rest keep it blocked (ADR-0004 §6).
const (
	approvalPending  = "pending"
	approvalApproved = "approved"
	approvalDenied   = "denied"
	approvalExpired  = "expired"
)

// maxApprovalPages bounds how many pages GetApprovalStatus walks looking for one
// approval, so a large escalation queue can never turn a status poll into an
// unbounded scan.
const maxApprovalPages = 20

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

// ListApprovals returns one page of the escalation queue (GET /approvals),
// optionally filtered by status and continued from cursor. It is the read side
// the console polls to resume or deny a blocked step, since there is no
// single-approval GET endpoint (ADR-0004 §6).
func (c *Client) ListApprovals(ctx context.Context, token, status, cursor string) (*ApprovalList, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	q.Set("limit", strconv.Itoa(100))
	var list ApprovalList
	if err := c.getJSON(ctx, token, "/approvals", q, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// GetApprovalStatus returns the current status of one approval by id. Because
// the control plane exposes no single-approval GET, it lists the escalation
// queue and filters by id, paging (bounded by maxApprovalPages) until it finds
// the approval or runs out of pages. A not-found approval returns an empty
// status and no error, which the caller treats as "not yet decided".
func (c *Client) GetApprovalStatus(ctx context.Context, token, approvalID string) (string, error) {
	cursor := ""
	for page := 0; page < maxApprovalPages; page++ {
		list, err := c.ListApprovals(ctx, token, "", cursor)
		if err != nil {
			return "", err
		}
		for _, a := range list.Items {
			if a.ID == approvalID {
				return a.Status, nil
			}
		}
		if list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	return "", nil
}
