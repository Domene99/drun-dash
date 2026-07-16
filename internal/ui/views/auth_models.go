package views

// This file holds the view models for the local-auth onboarding pages (ADR-0008)
// and the agent-create/edit forms: the "create your organization" signup page,
// the invitee accept-invite page, and the new/edit-agent forms. Like the rest of
// the package they are pure presentation — internal/ui assembles them from form
// input and control-plane responses and the templ views render them.

// SignupData is the view model for the "Create your organization" page
// (/signup). The field values are echoed back on a failed submit so the operator
// does not have to retype them; Error carries a readable failure message (a
// validation problem or a duplicate slug/email).
type SignupData struct {
	Error   string
	OrgName string
	OrgSlug string
	Email   string
	Name    string
}

// AcceptInviteData is the view model for the invitee page (/accept-invite). Token
// is the raw invite token carried through from the link (rendered as a hidden
// field); an empty token renders a "missing token" notice instead of the form.
// Name is echoed back on a failed submit; Error carries a readable message.
type AcceptInviteData struct {
	Token string
	Error string
	Name  string
}

// PolicyOption is one selectable policy in the agent form's default-policy
// dropdown. Name is the primary label; ID is the form value.
type PolicyOption struct {
	ID   string
	Name string
}

// AgentNewData is the view model for the new-agent form (/agents/new). CanManage
// is expressed via NoAccess (the form is replaced with a permission notice when
// the caller lacks agents.manage). The field values echo back on a failed submit.
// Policies feeds the optional default-policy dropdown; Endpoints feeds the
// optional model-endpoint dropdown, and PolicyID/EndpointID echo the current
// selection back on a failed submit so the operator does not have to reselect.
//
// Tools, network, and concurrency (fan-out) are no longer collected here: under
// policy-centric access (ADR-0010) the selected policy owns that boundary, so an
// agent is just identity + system prompt + policy + endpoint.
type AgentNewData struct {
	Chrome       Chrome
	Error        string
	NoAccess     bool
	Name         string
	Description  string
	SystemPrompt string
	Policies     []PolicyOption
	PolicyID     string
	Endpoints    []EndpointOption
	EndpointID   string
}

// AgentEditData is the view model for the edit-agent form
// (/agents/{id}/edit). It edits the agent's identity (name/description, PATCH
// /agents/{id}) and optionally publishes a new immutable version (system prompt +
// policy + endpoint, POST /agents/{id}/versions). CanManage is expressed via
// NoAccess (the form is replaced with a permission notice when the caller lacks
// agents.manage). ID identifies the agent; the remaining fields echo current or
// submitted values back so the operator does not have to retype/reselect.
//
// Like AgentNewData, the boundary (tools/network/fan-out) is governed by the
// selected policy, not collected here.
type AgentEditData struct {
	Chrome       Chrome
	Error        string
	NoAccess     bool
	ID           string
	Name         string
	Description  string
	SystemPrompt string
	Policies     []PolicyOption
	PolicyID     string
	Endpoints    []EndpointOption
	EndpointID   string
}
