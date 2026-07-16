package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/ui/views"
)

// This file wires the local-auth onboarding pages (ADR-0008) and the
// agent-create form onto the control-plane client: the "create your
// organization" signup, the invitee accept-invite page, the Members-page invite
// action, and the new-agent form. The signup/accept-invite endpoints are public
// (they mint a session and set the cookie); invite and agent-create sit behind
// requireAuth and the control plane's per-endpoint permission gate.

// minUIPasswordLen mirrors the control plane's settable-password floor
// (internal/httpapi/password.go, ADR-0008 §2). The UI validates it up front for
// a friendly message; the control plane remains the source of truth.
const minUIPasswordLen = 8

// --- Signup ----------------------------------------------------------------

// handleSignup renders the "Create your organization" page.
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, views.Signup(views.SignupData{}))
}

// handleSignupSubmit creates an organization and its Owner from the submitted
// form, then signs the owner in (sets the session cookie) and lands on the
// Overview. Validation problems and a duplicate slug/email re-render the page
// with a readable message and the submitted values preserved.
func (s *Server) handleSignupSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.Signup(views.SignupData{Error: "Could not read the submitted form."}))
		return
	}
	req := SignupRequest{
		OrgName:  strings.TrimSpace(r.PostFormValue("org_name")),
		OrgSlug:  strings.TrimSpace(r.PostFormValue("org_slug")),
		Email:    strings.TrimSpace(r.PostFormValue("email")),
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		Password: r.PostFormValue("password"),
	}
	confirm := r.PostFormValue("password_confirm")

	data := views.SignupData{OrgName: req.OrgName, OrgSlug: req.OrgSlug, Email: req.Email, Name: req.Name}
	switch {
	case req.OrgName == "":
		data.Error = "An organization name is required."
	case req.Email == "":
		data.Error = "Your email is required."
	case len(req.Password) < minUIPasswordLen:
		data.Error = fmt.Sprintf("Password must be at least %d characters.", minUIPasswordLen)
	case req.Password != confirm:
		data.Error = "The two passwords do not match."
	}
	if data.Error != "" {
		s.render(w, r, http.StatusBadRequest, views.Signup(data))
		return
	}

	token, err := s.client.Signup(r.Context(), req)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: signup failed", slog.Any("error", err))
		data.Error = signupErrorMessage(err)
		s.render(w, r, http.StatusOK, views.Signup(data))
		return
	}

	s.setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// signupErrorMessage renders a control-plane signup failure for display,
// translating the generic 409 into a note that the slug or email is taken.
func signupErrorMessage(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return "That organization slug or email is already in use. Try a different slug, or sign in instead."
	}
	return "Could not create the organization: " + readableError(err)
}

// --- Accept invite ---------------------------------------------------------

// handleAcceptInvite renders the invitee page. The raw token from the link is
// carried through into the form; a missing token renders a notice instead.
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	s.render(w, r, http.StatusOK, views.AcceptInvite(views.AcceptInviteData{Token: token}))
}

// handleAcceptInviteSubmit redeems the invite token with the acceptor's name and
// password, then signs them in (sets the session cookie) and lands in the app.
// An invalid/expired/consumed token (the generic 401) and validation problems
// re-render the page with a readable message.
func (s *Server) handleAcceptInviteSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, views.AcceptInvite(views.AcceptInviteData{Error: "Could not read the submitted form."}))
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")

	data := views.AcceptInviteData{Token: token, Name: name}
	switch {
	case token == "":
		data.Error = "This invitation link is missing its token."
	case len(password) < minUIPasswordLen:
		data.Error = fmt.Sprintf("Password must be at least %d characters.", minUIPasswordLen)
	case password != confirm:
		data.Error = "The two passwords do not match."
	}
	if data.Error != "" {
		s.render(w, r, http.StatusBadRequest, views.AcceptInvite(data))
		return
	}

	sessionToken, err := s.client.AcceptInvite(r.Context(), token, name, password)
	switch {
	case errors.Is(err, ErrUnauthorized):
		data.Error = "This invitation is invalid, expired, or already used. Ask your administrator for a new link."
		s.render(w, r, http.StatusUnauthorized, views.AcceptInvite(data))
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: accept invite failed", slog.Any("error", err))
		data.Error = "Could not accept the invitation: " + readableError(err)
		s.render(w, r, http.StatusOK, views.AcceptInvite(data))
		return
	}

	s.setSessionCookie(w, sessionToken)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- Invite member ---------------------------------------------------------

// handleMemberInvite creates a pending member and its invitation from the
// Members-page dialog, then re-renders the Members page with the returned invite
// link surfaced for the admin to copy and hand off (no mailer in local dev). A
// rejected token redirects to /login; a validation or conflict error is surfaced
// inline on the Members page.
func (s *Server) handleMemberInvite(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderMembersError(w, r, token, "Could not read the submitted form.")
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	if email == "" {
		s.renderMembersError(w, r, token, "An email address is required to invite a member.")
		return
	}
	roleIDs := r.PostForm["role_ids"]

	result, err := s.client.InviteMember(r.Context(), token, email, roleIDs)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: invite member failed", slog.Any("error", err))
		s.renderMembersError(w, r, token, "Could not invite member: "+readableError(err))
		return
	}
	s.renderMembersInvited(w, r, token, email, result.InviteLink)
}

// renderMembersInvited re-renders the Members page after a successful invite,
// carrying the invite link so the admin can copy it (the invited pending member
// also appears in the roster).
func (s *Server) renderMembersInvited(w http.ResponseWriter, r *http.Request, token, email, link string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "members", "Members")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.membersData(r.Context(), token, chrome, perms, "")
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	data.InviteLink = link
	data.InviteEmail = email
	s.render(w, r, http.StatusOK, views.Members(data))
}

// --- Agent create ----------------------------------------------------------

// handleAgentNew renders the create-agent form, gated on agents.manage. Without
// that permission it renders a view-only notice (the control plane would reject
// the create anyway). It loads the policy catalog for the optional default-policy
// dropdown, best-effort.
func (s *Server) handleAgentNew(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, perms, err := s.buildChrome(r.Context(), token, "agents", "New agent")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.AgentNewData{Chrome: chrome}
	if !hasPermission(perms, permAgentsManage) {
		data.NoAccess = true
		s.render(w, r, http.StatusOK, views.AgentNew(data))
		return
	}
	data.Policies = s.policyOptions(r.Context(), token)
	data.Endpoints = s.endpointOptions(r.Context(), token)
	s.render(w, r, http.StatusOK, views.AgentNew(data))
}

// handleAgentCreate registers the agent (POST /agents) and publishes its first
// immutable version (POST /agents/{id}/versions) from the submitted form. On
// success it lands on the new agent's detail page. A validation problem or a
// failed create re-renders the form with the values preserved; if the agent was
// created but publishing v1 failed, it still lands on the detail page (the agent
// exists) and logs the publish failure.
func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderAgentNewError(w, r, token, views.AgentNewData{}, "Could not read the submitted form.")
		return
	}
	form := views.AgentNewData{
		Name:         strings.TrimSpace(r.PostFormValue("name")),
		Description:  strings.TrimSpace(r.PostFormValue("description")),
		SystemPrompt: r.PostFormValue("system_prompt"),
	}
	policyID := strings.TrimSpace(r.PostFormValue("default_policy_id"))
	endpointID := strings.TrimSpace(r.PostFormValue("endpoint_id"))
	form.PolicyID = policyID
	form.EndpointID = endpointID

	switch {
	case form.Name == "":
		s.renderAgentNewError(w, r, token, form, "An agent name is required.")
		return
	case strings.TrimSpace(form.SystemPrompt) == "":
		s.renderAgentNewError(w, r, token, form, "A system prompt is required to publish version 1.")
		return
	}

	agent, err := s.client.CreateAgent(r.Context(), token, AgentCreate{
		Name:            form.Name,
		Description:     form.Description,
		DefaultPolicyID: policyID,
	})
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: create agent failed", slog.Any("error", err))
		s.renderAgentNewError(w, r, token, form, "Could not create agent: "+readableError(err))
		return
	}

	// Pin the selected policy's latest version and the model endpoint onto
	// version 1 so the published agent is runnable. Resolving the policy version
	// is best-effort (an empty id simply leaves the version's policy unpinned;
	// the agent's default policy still governs it).
	policyVersionID := ""
	if policyID != "" {
		policyVersionID = s.latestPolicyVersionID(r.Context(), token, policyID)
	}
	// The policy boundary governs tools, network, and fan-out (ADR-0010), so the
	// version is published with no tools and the default fan-out: identity +
	// system prompt + policy + endpoint is all this form collects.
	_, err = s.client.PublishAgentVersion(r.Context(), token, agent.ID, AgentVersionCreate{
		SystemPrompt:    form.SystemPrompt,
		AllowedTools:    []string{},
		PolicyVersionID: policyVersionID,
		EndpointID:      endpointID,
		Notes:           "Initial version",
	})
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		// The agent was created; publishing v1 failed. Land on the detail page
		// (the source of truth) rather than the form, so resubmitting cannot
		// create a duplicate agent. The detail page shows no published version.
		s.logger.ErrorContext(r.Context(), "admin-ui: publish agent version failed", slog.Any("error", err))
	}
	http.Redirect(w, r, "/agents/"+agent.ID, http.StatusSeeOther)
}

// renderAgentNewError re-renders the create-agent form with a message after a
// failed submit, rebuilding the chrome and policy options and preserving the
// submitted field values.
func (s *Server) renderAgentNewError(w http.ResponseWriter, r *http.Request, token string, form views.AgentNewData, msg string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "agents", "New agent")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	form.Chrome = chrome
	form.Error = msg
	if !hasPermission(perms, permAgentsManage) {
		form.NoAccess = true
	} else {
		form.Policies = s.policyOptions(r.Context(), token)
		form.Endpoints = s.endpointOptions(r.Context(), token)
	}
	s.render(w, r, http.StatusOK, views.AgentNew(form))
}

// --- Agent edit ------------------------------------------------------------

// handleAgentEdit renders the edit-agent form, gated on agents.manage. Without
// that permission it renders a view-only notice (the control plane would reject
// the update anyway). It loads the agent's current identity and default policy to
// pre-fill the form, plus the policy/endpoint catalogs for the version-publish
// selects, best-effort.
func (s *Server) handleAgentEdit(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	agentID := chi.URLParam(r, "agentId")
	chrome, perms, err := s.buildChrome(r.Context(), token, "agents", "Edit agent")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.AgentEditData{Chrome: chrome, ID: agentID}
	if !hasPermission(perms, permAgentsManage) {
		data.NoAccess = true
		s.render(w, r, http.StatusOK, views.AgentEdit(data))
		return
	}
	agent, err := s.client.GetAgent(r.Context(), token, agentID)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: get agent for edit failed", slog.Any("error", err))
		data.Error = "Could not load agent: " + readableError(err)
		s.render(w, r, http.StatusOK, views.AgentEdit(data))
		return
	}
	data.Name = agent.Name
	data.Description = agent.Description
	data.PolicyID = agent.DefaultPolicyID
	data.Policies = s.policyOptions(r.Context(), token)
	data.Endpoints = s.endpointOptions(r.Context(), token)
	s.render(w, r, http.StatusOK, views.AgentEdit(data))
}

// handleAgentEditSubmit updates an agent's identity (PATCH /agents/{id}) and,
// when a system prompt is supplied, publishes a new immutable version pinning the
// selected policy + endpoint (POST /agents/{id}/versions). Versions are
// immutable, so a blank system prompt only renames (no new version). On success
// it lands on the agent's detail page. Under policy-centric access the version is
// published with no tools and the default fan-out; the policy governs the
// boundary (ADR-0010). Requires agents.manage.
func (s *Server) handleAgentEditSubmit(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	agentID := chi.URLParam(r, "agentId")
	if err := r.ParseForm(); err != nil {
		s.renderAgentEditError(w, r, token, views.AgentEditData{ID: agentID}, "Could not read the submitted form.")
		return
	}
	form := views.AgentEditData{
		ID:           agentID,
		Name:         strings.TrimSpace(r.PostFormValue("name")),
		Description:  strings.TrimSpace(r.PostFormValue("description")),
		SystemPrompt: r.PostFormValue("system_prompt"),
	}
	policyID := strings.TrimSpace(r.PostFormValue("default_policy_id"))
	endpointID := strings.TrimSpace(r.PostFormValue("endpoint_id"))
	form.PolicyID = policyID
	form.EndpointID = endpointID

	if form.Name == "" {
		s.renderAgentEditError(w, r, token, form, "An agent name is required.")
		return
	}

	// Update identity. Both fields are sent so a cleared description clears it.
	name := form.Name
	description := form.Description
	_, err := s.client.UpdateAgent(r.Context(), token, agentID, &name, &description)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: update agent failed", slog.Any("error", err))
		s.renderAgentEditError(w, r, token, form, "Could not update agent: "+readableError(err))
		return
	}

	// Publish a new immutable version only when a system prompt is supplied.
	if strings.TrimSpace(form.SystemPrompt) != "" {
		policyVersionID := ""
		if policyID != "" {
			policyVersionID = s.latestPolicyVersionID(r.Context(), token, policyID)
		}
		_, err := s.client.PublishAgentVersion(r.Context(), token, agentID, AgentVersionCreate{
			SystemPrompt:    form.SystemPrompt,
			AllowedTools:    []string{},
			PolicyVersionID: policyVersionID,
			EndpointID:      endpointID,
			Notes:           "Edited via admin UI",
		})
		if errors.Is(err, ErrUnauthorized) {
			s.redirectToLogin(w, r)
			return
		}
		if err != nil {
			s.logger.ErrorContext(r.Context(), "admin-ui: publish agent version on edit failed", slog.Any("error", err))
			s.renderAgentEditError(w, r, token, form, "Agent updated, but publishing a new version failed: "+readableError(err))
			return
		}
	}
	http.Redirect(w, r, "/agents/"+agentID, http.StatusSeeOther)
}

// renderAgentEditError re-renders the edit-agent form with a message after a
// failed submit, rebuilding the chrome and policy/endpoint options and preserving
// the submitted field values.
func (s *Server) renderAgentEditError(w http.ResponseWriter, r *http.Request, token string, form views.AgentEditData, msg string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "agents", "Edit agent")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	form.Chrome = chrome
	form.Error = msg
	if !hasPermission(perms, permAgentsManage) {
		form.NoAccess = true
	} else {
		form.Policies = s.policyOptions(r.Context(), token)
		form.Endpoints = s.endpointOptions(r.Context(), token)
	}
	s.render(w, r, http.StatusOK, views.AgentEdit(form))
}

// policyOptions lists the org's policies for the agent form's default-policy
// dropdown. Best-effort: a control-plane error yields no options (the field just
// offers "No policy") rather than failing the page.
func (s *Server) policyOptions(ctx context.Context, token string) []views.PolicyOption {
	policies, err := s.client.ListPolicies(ctx, token)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			s.logger.ErrorContext(ctx, "admin-ui: list policies for agent form failed", slog.Any("error", err))
		}
		return nil
	}
	out := make([]views.PolicyOption, 0, len(policies.Items))
	for _, p := range policies.Items {
		out = append(out, views.PolicyOption{ID: p.ID, Name: p.Name})
	}
	return out
}

// parseToolList splits the allowed-tools textarea into a tool-identifier slice,
// accepting comma- or newline-separated entries and dropping blanks.
func parseToolList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// latestPolicyVersionID resolves the id of a policy's latest version, for
// pinning a policy onto a new agent version. Best-effort: any failure — or a
// control plane that does not expose a version id — yields "" so the caller
// simply leaves the version's policy unpinned. When several versions are
// returned it picks the highest version number.
func (s *Server) latestPolicyVersionID(ctx context.Context, token, policyID string) string {
	list, err := s.client.ListPolicyVersions(ctx, token, policyID)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			s.logger.ErrorContext(ctx, "admin-ui: list policy versions for agent pin failed", slog.Any("error", err))
		}
		return ""
	}
	best, bestVersion := "", -1
	for _, v := range list.Items {
		if v.ID != "" && v.Version > bestVersion {
			best, bestVersion = v.ID, v.Version
		}
	}
	return best
}
