package memberui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// newTestClient wires a Client at a mock control-plane whose handler is fn. It
// serves the handler in-memory through an http.RoundTripper (no socket bind, via
// handlerClient) so the client tests run inside the network-isolated drun
// sandbox as well as on a host.
func newTestClient(t *testing.T, fn http.HandlerFunc) *Client {
	t.Helper()
	return NewClient("http://cp", handlerClient(fn))
}

func TestStartDeviceAuthorization(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/auth/device/code" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			ClientID string `json:"client_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ClientID != "the-client" {
			t.Errorf("client_id = %q, want %q", body.ClientID, "the-client")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(DeviceAuthorization{
			DeviceCode:              "dev-code",
			UserCode:                "WXYZ-1234",
			VerificationURI:         "http://admin/device",
			VerificationURIComplete: "http://admin/device?user_code=WXYZ-1234",
			ExpiresIn:               600,
			Interval:                5,
		})
	})

	da, err := c.StartDeviceAuthorization(context.Background(), "the-client")
	if err != nil {
		t.Fatalf("StartDeviceAuthorization: %v", err)
	}
	if da.DeviceCode != "dev-code" || da.UserCode != "WXYZ-1234" || da.Interval != 5 {
		t.Errorf("unexpected authorization: %+v", da)
	}
}

func TestStartDeviceAuthorization_MissingCodes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceAuthorization{})
	})
	if _, err := c.StartDeviceAuthorization(context.Background(), "x"); err == nil {
		t.Fatal("expected error on empty device/user code, got nil")
	}
}

func TestPollDeviceToken_Approved(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/device/token" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body struct {
			DeviceCode string `json:"device_code"`
			GrantType  string `json:"grant_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.GrantType != deviceGrantType {
			t.Errorf("grant_type = %q, want %q", body.GrantType, deviceGrantType)
		}
		if body.DeviceCode != "dev-code" {
			t.Errorf("device_code = %q", body.DeviceCode)
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "jwt-token", TokenType: "Bearer", ExpiresIn: 3600})
	})

	tok, err := c.PollDeviceToken(context.Background(), "dev-code")
	if err != nil {
		t.Fatalf("PollDeviceToken: %v", err)
	}
	if tok != "jwt-token" {
		t.Errorf("token = %q, want %q", tok, "jwt-token")
	}
}

func TestPollDeviceToken_OAuthErrors(t *testing.T) {
	tests := []struct {
		oauthCode string
		want      error
	}{
		{"authorization_pending", ErrAuthorizationPending},
		{"slow_down", ErrSlowDown},
		{"access_denied", ErrAccessDenied},
		{"expired_token", ErrExpiredToken},
	}
	for _, tt := range tests {
		t.Run(tt.oauthCode, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tt.oauthCode, "error_description": "d"})
			})
			_, err := c.PollDeviceToken(context.Background(), "dev-code")
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestPollDeviceToken_UnknownError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "internal", "message": "boom"})
	})
	_, err := c.PollDeviceToken(context.Background(), "dev-code")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v, want APIError with 500", err)
	}
}

func TestGetEntitledAgents_PrefersMeAgents(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/agents" {
			t.Errorf("path = %q, want /me/agents", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(AgentList{Items: []Agent{{ID: "a1", Name: "Alpha"}}})
	})
	list, err := c.GetEntitledAgents(context.Background(), "tok")
	if err != nil {
		t.Fatalf("GetEntitledAgents: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != "a1" {
		t.Errorf("unexpected list: %+v", list)
	}
}

func TestGetEntitledAgents_FallsBackOn404(t *testing.T) {
	var paths []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/me/agents" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "not_found", "message": "no route"})
			return
		}
		_ = json.NewEncoder(w).Encode(AgentList{Items: []Agent{{ID: "cat", Name: "Catalog"}}})
	})
	list, err := c.GetEntitledAgents(context.Background(), "tok")
	if err != nil {
		t.Fatalf("GetEntitledAgents: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != "cat" {
		t.Errorf("unexpected list: %+v", list)
	}
	if len(paths) != 2 || paths[0] != "/me/agents" || paths[1] != "/agents" {
		t.Errorf("paths = %v, want [/me/agents /agents]", paths)
	}
}

func TestGetEntitledAgents_Unauthorized(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := c.GetEntitledAgents(context.Background(), "tok"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestLaunchSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions/launch" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			AgentID string `json:"agent_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.AgentID != "agent-9" {
			t.Errorf("agent_id = %q", body.AgentID)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(SessionLaunch{
			SessionID:     "sess-1",
			ProxyBaseURL:  "http://proxy",
			ProxyToken:    "dgp_secret",
			FanOutCeiling: 4,
			ExpiresAt:     "2026-07-11T00:00:00Z",
		})
	})
	launch, err := c.LaunchSession(context.Background(), "tok", "agent-9")
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	if launch.SessionID != "sess-1" || launch.ProxyToken != "dgp_secret" || launch.FanOutCeiling != 4 {
		t.Errorf("unexpected launch: %+v", launch)
	}
}

func TestTerminateSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions/sess-1/terminate" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(Session{SessionID: "sess-1", Status: "terminated"})
	})
	session, err := c.TerminateSession(context.Background(), "tok", "sess-1")
	if err != nil {
		t.Fatalf("TerminateSession: %v", err)
	}
	if session.Status != "terminated" {
		t.Errorf("status = %q, want terminated", session.Status)
	}
}

func TestAPIError_FlatEnvelope(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "forbidden", "message": "not entitled"})
	})
	_, err := c.LaunchSession(context.Background(), "tok", "a")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want APIError", err)
	}
	if apiErr.Status != http.StatusForbidden || apiErr.Code != "forbidden" || apiErr.Message != "not entitled" {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
}

func TestLogin_Success(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/auth/login" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("login must be unauthenticated, got Authorization %q", got)
		}
		var body struct {
			OrgSlug  string `json:"org_slug"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.OrgSlug != "acme" || body.Email != "a@example.com" || body.Password != "supersecret" {
			t.Errorf("unexpected body %+v", body)
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "jwt-token", TokenType: "Bearer", ExpiresIn: 3600})
	})
	tok, err := c.Login(context.Background(), "acme", "a@example.com", "supersecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok != "jwt-token" {
		t.Errorf("token = %q, want jwt-token", tok)
	}
}

func TestLogin_InvalidCredentials(t *testing.T) {
	// The control plane answers every failure with a generic 401.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized", "message": "invalid credentials"})
	})
	_, err := c.Login(context.Background(), "acme", "a@example.com", "wrong")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestLogin_MissingToken(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{})
	})
	if _, err := c.Login(context.Background(), "acme", "a@e.com", "pw"); err == nil {
		t.Fatal("expected error on empty access_token, got nil")
	}
}
