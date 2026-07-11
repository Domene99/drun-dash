package ui

import (
	"context"
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
