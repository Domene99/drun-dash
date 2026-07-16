package views

import (
	"strings"
	"testing"
)

// These hermetic tests render the Batch-29 governance surfaces — the Endpoints
// page, the Policies create action, and the agent-create endpoint/policy pins —
// against fully-resolved view models, asserting the permission gating, the
// write-only API-key affordance (the key is never rendered back), and the form
// prefill. They complement the handler tests in internal/ui.

func TestRenderEndpointsListAndKeyBadges(t *testing.T) {
	body := render(t, Endpoints(EndpointsData{
		Chrome:    sampleChrome("endpoints"),
		CanManage: true,
		Providers: []string{"anthropic", "openai", "openai-compatible"},
		Endpoints: []EndpointRow{
			{ID: "e1", Name: "Anthropic prod", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Active: true, SecretSet: true, Created: TimeView{Relative: "now"}},
			{ID: "e2", Name: "OpenAI dev", Provider: "openai", BaseURL: "https://api.openai.com", Active: false, SecretSet: false, Created: TimeView{Relative: "now"}},
		},
	}))
	mustContain(t, body,
		"Endpoints", "New endpoint",
		"Anthropic prod", "anthropic", "https://api.anthropic.com",
		"Key configured", "No key", // secret_set badges (never the key itself)
		"uk-badge--success", // active StatusBadge
		`data-uk-modal-open="endpoint-create"`,
		`data-uk-modal-open="endpoint-edit-e1"`, `data-uk-modal-open="endpoint-delete-e1"`,
		"API key", "Leave blank to keep current key", // write-only key affordance in edit modal
		"Provider", "Base URL",
	)
	// The create/edit forms preselect the row's provider.
	if !strings.Contains(body, `value="anthropic" selected`) {
		t.Errorf("edit modal must preselect the endpoint's provider:\n%s", body)
	}
}

func TestRenderEndpointsGating(t *testing.T) {
	noAccess := render(t, Endpoints(EndpointsData{Chrome: sampleChrome("endpoints"), NoAccess: true}))
	mustContain(t, noAccess, "No access", "endpoints.manage")
	if strings.Contains(noAccess, "New endpoint") {
		t.Errorf("no-access endpoints page must not offer New endpoint")
	}

	viewOnly := render(t, Endpoints(EndpointsData{
		Chrome:    sampleChrome("endpoints"),
		Endpoints: []EndpointRow{{ID: "e1", Name: "Anthropic prod", Provider: "anthropic", SecretSet: true, Created: TimeView{Relative: "now"}}},
	}))
	mustContain(t, viewOnly, "Anthropic prod")
	if strings.Contains(viewOnly, "New endpoint") || strings.Contains(viewOnly, `data-uk-modal-open="endpoint-edit-e1"`) {
		t.Errorf("view-only endpoints page must not offer management controls")
	}
}

func TestRenderEndpointsEmpty(t *testing.T) {
	body := render(t, Endpoints(EndpointsData{Chrome: sampleChrome("endpoints"), CanManage: true, Providers: []string{"anthropic"}}))
	mustContain(t, body, "No endpoints yet", "uk-empty", "New endpoint")
}

func TestRenderPoliciesCreateGating(t *testing.T) {
	body := render(t, Policies(PoliciesData{
		Chrome:    sampleChrome("policies"),
		CanManage: true,
		Policies:  []PolicyListRow{{ID: "p1", Name: "Prod", CurrentVersion: "1", Created: TimeView{Relative: "now"}}},
	}))
	mustContain(t, body,
		"New policy", `data-uk-modal-open="policy-create"`,
		"Allowed tools", "Network", "allowlist", "unrestricted", "Fan-out ceiling",
	)

	viewOnly := render(t, Policies(PoliciesData{Chrome: sampleChrome("policies")}))
	if strings.Contains(viewOnly, "New policy") {
		t.Errorf("view-only policies page must not offer New policy")
	}
}

func TestRenderAgentNewPinsEndpointAndPolicy(t *testing.T) {
	body := render(t, AgentNew(AgentNewData{
		Chrome:     sampleChrome("agents"),
		Policies:   []PolicyOption{{ID: "p1", Name: "Prod"}},
		Endpoints:  []EndpointOption{{ID: "e1", Name: "Anthropic prod (anthropic)"}},
		PolicyID:   "p1",
		EndpointID: "e1",
	}))
	mustContain(t, body,
		"Model endpoint", `name="endpoint_id"`, "Anthropic prod (anthropic)",
		"Default policy", `name="default_policy_id"`, "Prod",
		// The policy owns tools/network/fan-out (ADR-0010); the form says so.
		"Tools, network, and concurrency are governed by the selected policy.",
	)
	// The selected policy and endpoint are echoed back as the chosen options.
	if !strings.Contains(body, `value="p1" selected`) || !strings.Contains(body, `value="e1" selected`) {
		t.Errorf("agent form must preselect the chosen policy and endpoint:\n%s", body)
	}
	// Tools and fan-out are no longer collected — the policy governs them.
	if strings.Contains(body, `name="allowed_tools"`) || strings.Contains(body, `name="fan_out_ceiling"`) {
		t.Errorf("agent form must not collect tools or fan-out:\n%s", body)
	}
}
