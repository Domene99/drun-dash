package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient wires a Client to a mock control-plane server.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, srv.Client())
}

func TestClientWhoami(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer tok-123")
		}
		if r.URL.Path != "/auth/whoami" {
			t.Errorf("path = %q, want /auth/whoami", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subject":"user-1","organization_id":"org-1","roles":["admin","viewer"]}`))
	})

	id, err := c.Whoami(context.Background(), "tok-123")
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if id.Subject != "user-1" || id.OrganizationID != "org-1" {
		t.Errorf("identity = %+v", id)
	}
	if len(id.Roles) != 2 || id.Roles[0] != "admin" {
		t.Errorf("roles = %v", id.Roles)
	}
}

func TestClientListAgents(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"a1","name":"Planner","latest_version":3}],"next_cursor":"c2"}`))
	})

	list, err := c.ListAgents(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "Planner" {
		t.Fatalf("items = %+v", list.Items)
	}
	if list.Items[0].LatestVersion == nil || *list.Items[0].LatestVersion != 3 {
		t.Errorf("latest_version = %v", list.Items[0].LatestVersion)
	}
	if list.NextCursor != "c2" {
		t.Errorf("next_cursor = %q", list.NextCursor)
	}
}

func TestClientListSessions(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"session_id":"s1","agent_id":"a1","status":"running","fan_out_ceiling":4,"created_at":"2026-07-01T00:00:00Z"}]}`))
	})

	list, err := c.ListSessions(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Status != "running" {
		t.Fatalf("items = %+v", list.Items)
	}
	if list.Items[0].FanOutCeiling != 4 {
		t.Errorf("fan_out_ceiling = %d, want 4", list.Items[0].FanOutCeiling)
	}
}

func TestClientQuerySpend(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("from") != "F" || q.Get("to") != "T" || q.Get("group_by") != "agent" {
			t.Errorf("query = %v", q)
		}
		_, _ = w.Write([]byte(`{"from":"F","to":"T","currency":"USD","total":12.5,"groups":[{"key":"a1","amount":12.5,"tokens":100}]}`))
	})

	report, err := c.QuerySpend(context.Background(), "tok", "F", "T", "agent")
	if err != nil {
		t.Fatalf("QuerySpend: %v", err)
	}
	if report.Total != 12.5 || report.Currency != "USD" {
		t.Errorf("report = %+v", report)
	}
	if len(report.Groups) != 1 || report.Groups[0].Tokens == nil || *report.Groups[0].Tokens != 100 {
		t.Errorf("groups = %+v", report.Groups)
	}
}

func TestClientListBudgets(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/analytics/budgets" {
			t.Errorf("path = %q, want /analytics/budgets", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"b1","name":"Monthly","limit":1000,"spent":250,"currency":"USD","period":"monthly"}]}`))
	})

	list, err := c.ListBudgets(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListBudgets: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("items = %+v", list.Items)
	}
	b := list.Items[0]
	if b.Name != "Monthly" || b.Limit != 1000 || b.Spent != 250 || b.Period != "monthly" {
		t.Errorf("budget = %+v", b)
	}
}

func TestClientListApprovals(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/approvals" {
			t.Errorf("path = %q, want /approvals", r.URL.Path)
		}
		if got := r.URL.Query().Get("status"); got != "pending" {
			t.Errorf("status = %q, want pending", got)
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"ap1","session_id":"s1","action":"spawn subagent","status":"pending","created_at":"2026-07-01T00:00:00Z"}]}`))
	})

	list, err := c.ListApprovals(context.Background(), "tok", "pending")
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Status != "pending" || list.Items[0].Action != "spawn subagent" {
		t.Fatalf("items = %+v", list.Items)
	}
}

func TestClientListApprovalsNoStatus(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["status"]; ok {
			t.Errorf("status param should be omitted when empty: %v", r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	})

	if _, err := c.ListApprovals(context.Background(), "tok", ""); err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
}

func TestClientDecideApproval(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/approvals/ap1/decision" {
			t.Errorf("path = %q, want /approvals/ap1/decision", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Decision != "approve" || body.Reason != "looks fine" {
			t.Errorf("body = %+v", body)
		}
		_, _ = w.Write([]byte(`{"id":"ap1","session_id":"s1","action":"spawn","status":"approved","decided_by":"user-1","created_at":"2026-07-01T00:00:00Z"}`))
	})

	ap, err := c.DecideApproval(context.Background(), "tok", "ap1", "approve", "looks fine")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if ap.Status != "approved" || ap.DecidedBy != "user-1" {
		t.Errorf("approval = %+v", ap)
	}
}

func TestClientGetSessionTrajectory(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/s1/trajectory" {
			t.Errorf("path = %q, want /sessions/s1/trajectory", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		_, _ = w.Write([]byte(`{"items":[` +
			`{"event_id":"e1","type":"session_start","occurred_at":"2026-07-01T00:00:00Z","field_class":"structure"},` +
			`{"event_id":"e2","type":"tool_call","occurred_at":"2026-07-01T00:00:01Z","field_class":"payload","parent_event_id":"e1","fields":[{"key":"tool_name","field_class":"payload","value":"search"}]}` +
			`]}`))
	})

	list, err := c.GetSessionTrajectory(context.Background(), "tok", "s1")
	if err != nil {
		t.Fatalf("GetSessionTrajectory: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("items = %+v", list.Items)
	}
	if list.Items[1].ParentEventID != "e1" || list.Items[1].Type != "tool_call" {
		t.Errorf("child event = %+v", list.Items[1])
	}
	if len(list.Items[1].Fields) != 1 || list.Items[1].Fields[0].Key != "tool_name" {
		t.Errorf("child fields = %+v", list.Items[1].Fields)
	}
}

func TestClientDecideApprovalConflict(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"already_decided","message":"already decided"}`))
	})

	_, err := c.DecideApproval(context.Background(), "tok", "ap1", "approve", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusConflict || apiErr.Message != "already decided" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestClientUnauthorized(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"authentication required"}`))
	})

	_, err := c.Whoami(context.Background(), "bad")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Whoami err = %v, want ErrUnauthorized", err)
	}
}

func TestClientDecideApprovalUnauthorized(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"authentication required"}`))
	})

	_, err := c.DecideApproval(context.Background(), "tok", "ap1", "deny", "")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("DecideApproval err = %v, want ErrUnauthorized", err)
	}
}

func TestClientFlatAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"boom"}`))
	})

	_, err := c.ListAgents(context.Background(), "tok")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ListAgents err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusInternalServerError || apiErr.Code != "internal_error" || apiErr.Message != "boom" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestClientNonJSONError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>gateway error</html>`))
	})

	_, err := c.ListSessions(context.Background(), "tok")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ListSessions err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", apiErr.Status)
	}
}
