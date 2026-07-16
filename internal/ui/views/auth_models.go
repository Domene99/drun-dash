package views

// This file holds the view models for the local-auth onboarding pages (ADR-0008)
// and the agent-create form: the "create your organization" signup page, the
// invitee accept-invite page, and the new-agent form. Like the rest of the
// package they are pure presentation — internal/ui assembles them from form
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

// PolicyOption is one selectable policy in the agent-create form's default-policy
// dropdown. Name is the primary label; ID is the form value.
type PolicyOption struct {
	ID   string
	Name string
}

// AgentNewData is the view model for the new-agent form (/agents/new). CanManage
// is expressed via NoAccess (the form is replaced with a permission notice when
// the caller lacks agents.manage). The field values echo back on a failed submit;
// AllowedTools is the raw textarea text (comma/newline separated). Policies feeds
// the optional default-policy dropdown.
type AgentNewData struct {
	Chrome        Chrome
	Error         string
	NoAccess      bool
	Name          string
	Description   string
	SystemPrompt  string
	AllowedTools  string
	FanOutCeiling string
	Policies      []PolicyOption
}
