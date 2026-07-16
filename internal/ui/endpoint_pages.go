package ui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/ui/views"
)

// This file wires the model-endpoints page (Batch 29) onto the control-plane
// client. The Endpoints page lets an owner register upstream model endpoints and
// store each one's real API key (sealed server-side; never returned) so that
// published agents pinned to an endpoint can actually run. Management actions are
// gated on endpoints.manage from the caller's effective permissions (GET
// /auth/whoami): the UI never offers an action the server would reject — the
// server still enforces; this is the UX layer. The REST resource is
// /model-endpoints; the admin UI mounts its pages at /endpoints.

// endpointProviders is the provider vocabulary offered in the endpoint
// create/edit forms (ADR-0002). The control plane validates the final value; the
// list keeps the common choices one click away.
func endpointProviders() []string {
	return []string{"anthropic", "openai", "openai-compatible"}
}

// handleEndpoints renders the Endpoints page. It reports whether the caller may
// manage endpoints (endpoints.manage) so the view offers or hides the
// create/edit/delete controls. Without it the control plane rejects the
// endpoints API, so the page shows a view-only notice instead of an error.
func (s *Server) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, perms, err := s.buildChrome(r.Context(), token, "endpoints", "Endpoints")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.endpointsData(r.Context(), token, chrome, perms, "")
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.Endpoints(data))
}

// endpointsData assembles the Endpoints view model: the manage flag, the
// provider options for the forms, and the endpoint rows. A section failure is
// embedded as data.Error; only a rejected token is returned as ErrUnauthorized
// so the caller can redirect. formError seeds a message after a failed write.
func (s *Server) endpointsData(ctx context.Context, token string, chrome views.Chrome, perms []string, formError string) (views.EndpointsData, error) {
	data := views.EndpointsData{
		Chrome:    chrome,
		CanManage: hasPermission(perms, permEndpointsManage),
		Providers: endpointProviders(),
		Error:     formError,
	}
	if !data.CanManage {
		data.NoAccess = true
		return data, nil
	}

	list, err := s.client.ListEndpoints(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.EndpointsData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list endpoints failed", slog.Any("error", err))
		if data.Error == "" {
			data.Error = "Could not load endpoints: " + readableError(err)
		}
	default:
		data.Endpoints = endpointRows(list.Items, s.now())
	}
	return data, nil
}

// handleEndpointCreate registers a model endpoint from the submitted form and
// returns to the Endpoints page. A rejected token redirects to /login; a
// validation problem, a duplicate name (409), or a supplied key with no
// server-side vault key configured (400) re-renders the page with the message.
func (s *Server) handleEndpointCreate(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderEndpointsError(w, r, token, "Could not read the submitted form.")
		return
	}
	req := EndpointCreate{
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		Provider: strings.TrimSpace(r.PostFormValue("provider")),
		BaseURL:  strings.TrimSpace(r.PostFormValue("base_url")),
		APIKey:   r.PostFormValue("api_key"),
	}
	switch {
	case req.Name == "":
		s.renderEndpointsError(w, r, token, "An endpoint name is required.")
		return
	case req.BaseURL == "":
		s.renderEndpointsError(w, r, token, "A base URL is required.")
		return
	}

	_, err := s.client.CreateEndpoint(r.Context(), token, req)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: create endpoint failed", slog.Any("error", err))
		s.renderEndpointsError(w, r, token, "Could not create endpoint: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

// handleEndpointUpdate applies the edit dialog's changes to an endpoint. The API
// key field is write-only: a blank value is omitted (keeping the stored key), a
// non-blank one re-seals it. Active reflects the checkbox.
func (s *Server) handleEndpointUpdate(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		s.renderEndpointsError(w, r, token, "Could not read the submitted form.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	provider := strings.TrimSpace(r.PostFormValue("provider"))
	baseURL := strings.TrimSpace(r.PostFormValue("base_url"))
	active := r.PostFormValue("active") != ""
	switch {
	case name == "":
		s.renderEndpointsError(w, r, token, "An endpoint name is required.")
		return
	case baseURL == "":
		s.renderEndpointsError(w, r, token, "A base URL is required.")
		return
	}
	body := EndpointUpdate{Name: &name, Provider: &provider, BaseURL: &baseURL, Active: &active}
	if key := r.PostFormValue("api_key"); strings.TrimSpace(key) != "" {
		body.APIKey = &key
	}

	_, err := s.client.UpdateEndpoint(r.Context(), token, id, body)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: update endpoint failed", slog.Any("error", err))
		s.renderEndpointsError(w, r, token, "Could not update endpoint: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

// handleEndpointDelete deletes an endpoint and returns to the Endpoints page.
func (s *Server) handleEndpointDelete(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	id := chi.URLParam(r, "id")
	err := s.client.DeleteEndpoint(r.Context(), token, id)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: delete endpoint failed", slog.Any("error", err))
		s.renderEndpointsError(w, r, token, "Could not delete endpoint: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

// renderEndpointsError re-renders the Endpoints page carrying a message after a
// failed write, rebuilding the live data so the table still reflects current
// state.
func (s *Server) renderEndpointsError(w http.ResponseWriter, r *http.Request, token, msg string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "endpoints", "Endpoints")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.endpointsData(r.Context(), token, chrome, perms, msg)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.Endpoints(data))
}

// endpointRows maps model endpoints to Endpoints-table rows. The API key is
// never carried — only SecretSet (whether a sealed key is on file).
func endpointRows(endpoints []Endpoint, now time.Time) []views.EndpointRow {
	out := make([]views.EndpointRow, 0, len(endpoints))
	for _, e := range endpoints {
		out = append(out, views.EndpointRow{
			ID:        e.ID,
			Name:      e.Name,
			Provider:  e.Provider,
			BaseURL:   e.BaseURL,
			Active:    e.Active,
			SecretSet: e.SecretSet,
			Created:   relativeTime(now, e.CreatedAt),
		})
	}
	return out
}

// endpointOptions lists the org's endpoints for the agent-create form's
// model-endpoint dropdown. Best-effort: a control-plane error yields no options
// (the field just offers "No endpoint") rather than failing the page. Each label
// carries the provider and a hint when no key is on file, so the operator can
// tell which endpoints are actually runnable.
func (s *Server) endpointOptions(ctx context.Context, token string) []views.EndpointOption {
	list, err := s.client.ListEndpoints(ctx, token)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			s.logger.ErrorContext(ctx, "admin-ui: list endpoints for agent form failed", slog.Any("error", err))
		}
		return nil
	}
	out := make([]views.EndpointOption, 0, len(list.Items))
	for _, e := range list.Items {
		out = append(out, views.EndpointOption{ID: e.ID, Name: endpointOptionLabel(e)})
	}
	return out
}

// endpointOptionLabel builds the agent-form dropdown label for an endpoint: its
// name and provider, plus a "no key" hint when no sealed key is on file (such an
// endpoint is pinned but not yet runnable).
func endpointOptionLabel(e Endpoint) string {
	label := e.Name
	if e.Provider != "" {
		label += " (" + e.Provider + ")"
	}
	if !e.SecretSet {
		label += " — no key"
	}
	return label
}
