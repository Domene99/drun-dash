package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/store"
)

// The agentVersion and entitlement seams (newAgentVersionStore,
// newEntitlementStore) and their in-memory fakes (fakeAgentVersionStore,
// fakeEntitlementStore) are shared with the launch handler and defined in
// launch_test.go; these tests reuse them. Only the principal-validation seam
// and its fake are new here.

// fakePrincipalReader resolves principal ids to their owning org. A missing id
// yields store.ErrNotFound (the real store's behaviour), so the handler's
// "unknown principal" path is exercised without a database.
type fakePrincipalReader struct {
	orgByID map[string]string
	err     error
}

func (f fakePrincipalReader) OrgID(_ context.Context, id string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	org, ok := f.orgByID[id]
	if !ok {
		return "", store.ErrNotFound
	}
	return org, nil
}

func setAgentVersionStore(t *testing.T, s agentVersionStore) {
	t.Helper()
	orig := newAgentVersionStore
	newAgentVersionStore = func(store.Querier) agentVersionStore { return s }
	t.Cleanup(func() { newAgentVersionStore = orig })
}

// setAgentVersionTx points the publish transaction seam at the given fakes,
// running the create+set-current closure inline with no pool (the fakes record
// what the handler asked of them). It mirrors setEntitlementStore's tx wiring.
func setAgentVersionTx(t *testing.T, vs agentVersionStore, as agentStore) {
	t.Helper()
	orig := withAgentVersionTx
	withAgentVersionTx = func(_ context.Context, _ *db.Pool, fn func(agentVersionStore, agentStore) error) error {
		return fn(vs, as)
	}
	t.Cleanup(func() { withAgentVersionTx = orig })
}

// setEntitlementStore points both the read seam (newEntitlementStore, used by
// listAgentEntitlements) and the transactional seam (withEntitlementTx, used by
// setAgentEntitlements) at s, running the replace closure inline with no pool.
func setEntitlementStore(t *testing.T, s entitlementStore) {
	t.Helper()
	origNew := newEntitlementStore
	origTx := withEntitlementTx
	newEntitlementStore = func(store.Querier) entitlementStore { return s }
	withEntitlementTx = func(_ context.Context, _ *db.Pool, fn func(entitlementStore) error) error {
		return fn(s)
	}
	t.Cleanup(func() {
		newEntitlementStore = origNew
		withEntitlementTx = origTx
	})
}

// setPrincipalReaders points the member and team validation seams at the given
// readers for the duration of the test.
func setPrincipalReaders(t *testing.T, member, team principalReader) {
	t.Helper()
	origM := newMemberReader
	origT := newTeamReader
	newMemberReader = func(store.Querier) principalReader { return member }
	newTeamReader = func(store.Querier) principalReader { return team }
	t.Cleanup(func() {
		newMemberReader = origM
		newTeamReader = origT
	})
}

func storeAgentVersion(agentID string, version int, prompt string, tools string, fanOut int, created time.Time) store.AgentVersion {
	return store.AgentVersion{
		ID:            "ver-" + agentID,
		AgentID:       agentID,
		Version:       version,
		SystemPrompt:  prompt,
		AllowedTools:  json.RawMessage(tools),
		FanOutCeiling: fanOut,
		CreatedAt:     created,
	}
}

// --- updateAgent ------------------------------------------------------------

// TestUpdateAgent proves the happy path merges supplied fields, forwards them
// to the store, and returns the updated agent.
func TestUpdateAgent(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{
		getAgent:    storeAgent("a1", "org-1", "old", strptr("was"), 1, created),
		updateAgent: storeAgent("a1", "org-1", "new", strptr("now"), 1, created),
	}
	h, hdr := agentHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{"name":"new","description":"now"}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotUpdateID != "a1" || fake.gotUpdateName != "new" {
		t.Errorf("update = (%q, %q), want (a1, new)", fake.gotUpdateID, fake.gotUpdateName)
	}
	if fake.gotUpdateDesc == nil || *fake.gotUpdateDesc != "now" {
		t.Errorf("update desc = %v, want now", fake.gotUpdateDesc)
	}
	var body apiAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Name != "new" || body.Description == nil || *body.Description != "now" {
		t.Errorf("body = %+v, want name new desc now", body)
	}
}

// TestUpdateAgentPartial proves an omitted field keeps the stored value: a body
// with only description leaves the name as loaded, and vice versa.
func TestUpdateAgentPartial(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	t.Run("only description", func(t *testing.T) {
		fake := &fakeAgentStore{
			getAgent:    storeAgent("a1", "org-1", "keepname", strptr("old"), 0, created),
			updateAgent: storeAgent("a1", "org-1", "keepname", strptr("fresh"), 0, created),
		}
		h, hdr := agentHarness(t, fake, "org-1")
		rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{"description":"fresh"}`, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if fake.gotUpdateName != "keepname" {
			t.Errorf("update name = %q, want keepname (unchanged)", fake.gotUpdateName)
		}
		if fake.gotUpdateDesc == nil || *fake.gotUpdateDesc != "fresh" {
			t.Errorf("update desc = %v, want fresh", fake.gotUpdateDesc)
		}
	})

	t.Run("only name", func(t *testing.T) {
		fake := &fakeAgentStore{
			getAgent:    storeAgent("a1", "org-1", "old", strptr("keepdesc"), 0, created),
			updateAgent: storeAgent("a1", "org-1", "renamed", strptr("keepdesc"), 0, created),
		}
		h, hdr := agentHarness(t, fake, "org-1")
		rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{"name":"renamed"}`, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if fake.gotUpdateName != "renamed" {
			t.Errorf("update name = %q, want renamed", fake.gotUpdateName)
		}
		if fake.gotUpdateDesc == nil || *fake.gotUpdateDesc != "keepdesc" {
			t.Errorf("update desc = %v, want keepdesc (unchanged)", fake.gotUpdateDesc)
		}
	})
}

// TestUpdateAgentDefaultPolicyIgnored proves a supplied default_policy_id is
// accepted (not a 400) and never echoed back.
func TestUpdateAgentDefaultPolicyIgnored(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{
		getAgent:    storeAgent("a1", "org-1", "n", nil, 0, created),
		updateAgent: storeAgent("a1", "org-1", "n", nil, 0, created),
	}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{"default_policy_id":"pol-9"}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if _, ok := rawKeys(t, rec.Body.Bytes())["default_policy_id"]; ok {
		t.Error("response contains default_policy_id; it must be omitted")
	}
}

func TestUpdateAgentEmptyName(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{"name":""}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotUpdateID != "" {
		t.Error("Update called despite empty-name validation failure")
	}
}

func TestUpdateAgentCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{getAgent: storeAgent("a1", "org-2", "other", nil, 0, created)}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{"name":"x"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotUpdateID != "" {
		t.Error("Update called on a cross-org agent")
	}
}

func TestUpdateAgentNotFound(t *testing.T) {
	fake := &fakeAgentStore{getErr: store.ErrNotFound}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodPatch, "/agents/missing", `{"name":"x"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateAgentConflict(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{
		getAgent:  storeAgent("a1", "org-1", "n", nil, 0, created),
		updateErr: store.ErrConflict,
	}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{"name":"taken"}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateAgentBadJSON(t *testing.T) {
	fake := &fakeAgentStore{}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodPatch, "/agents/a1", `{`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// --- deleteAgent ------------------------------------------------------------

func TestDeleteAgent(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodDelete, "/agents/a1", "", hdr)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotDeleteID != "a1" {
		t.Errorf("delete id = %q, want a1", fake.gotDeleteID)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

func TestDeleteAgentCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{getAgent: storeAgent("a1", "org-2", "other", nil, 0, created)}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodDelete, "/agents/a1", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotDeleteID != "" {
		t.Error("Delete called on a cross-org agent")
	}
}

func TestDeleteAgentNotFound(t *testing.T) {
	fake := &fakeAgentStore{getErr: store.ErrNotFound}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodDelete, "/agents/missing", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// --- listAgentVersions ------------------------------------------------------

// TestListAgentVersions proves the version history maps to the DTO with a
// computed, deterministic manifest_digest and no notes field.
func TestListAgentVersions(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	v1 := storeAgentVersion("a1", 1, "prompt one", `["read"]`, 1, created)
	v2 := storeAgentVersion("a1", 2, "prompt two", `["read","write"]`, 4, created)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 2, created)}
	verFake := &fakeAgentVersionStore{listVersions: []store.AgentVersion{v1, v2}}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionStore(t, verFake)

	rec := doJSON(t, h, http.MethodGet, "/agents/a1/versions", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if verFake.gotAgentID != "a1" {
		t.Errorf("list agent = %q, want a1", verFake.gotAgentID)
	}
	var body apiAgentVersionList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	// Digest is computed from the stored manifest; it must match the mapper and
	// differ between two versions with distinct content.
	if body.Items[0].ManifestDigest != computeManifestDigest(v1) {
		t.Errorf("digest[0] = %q, want %q", body.Items[0].ManifestDigest, computeManifestDigest(v1))
	}
	if body.Items[0].ManifestDigest == body.Items[1].ManifestDigest {
		t.Error("distinct versions produced the same manifest_digest")
	}
	// notes has no backing column and must not be emitted.
	if _, ok := rawKeys(t, mustItem(t, rec.Body.Bytes(), 0))["notes"]; ok {
		t.Error("response contains notes; it must be omitted")
	}
}

// TestManifestDigestDeterministic proves identical manifests hash identically
// and a single field change flips the digest.
func TestManifestDigestDeterministic(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	a := storeAgentVersion("a1", 1, "p", `["read"]`, 1, created)
	b := storeAgentVersion("a1", 1, "p", `["read"]`, 1, created)
	if computeManifestDigest(a) != computeManifestDigest(b) {
		t.Error("identical manifests produced different digests")
	}
	c := a
	c.FanOutCeiling = 9
	if computeManifestDigest(a) == computeManifestDigest(c) {
		t.Error("changing fan_out_ceiling did not change the digest")
	}
}

func TestListAgentVersionsCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-2", "other", nil, 0, created)}
	verFake := &fakeAgentVersionStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionStore(t, verFake)
	rec := doJSON(t, h, http.MethodGet, "/agents/a1/versions", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if verFake.gotAgentID != "" {
		t.Error("version store queried for a cross-org agent")
	}
}

func TestListAgentVersionsEmpty(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionStore(t, &fakeAgentVersionStore{listVersions: nil})
	rec := doJSON(t, h, http.MethodGet, "/agents/a1/versions", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !containsItemsArray(rec.Body.String()) {
		t.Errorf("body = %s, want items:[]", rec.Body.String())
	}
}

// --- publishAgentVersion ----------------------------------------------------

// publishBody is a full AgentVersionCreate payload used by the publish tests.
const publishBody = `{"system_prompt":"do things",` +
	`"allowed_tools":["read","write"],` +
	`"policy_version_id":"pv1","endpoint_id":"ep1","budget_id":"bd1",` +
	`"fan_out_ceiling":3,"notes":"first cut"}`

// TestPublishAgentVersion proves the happy path: the manifest fields are
// forwarded to the store at the next version number (current_version+1), the
// agent's current_version is advanced to it, and the response carries a
// computed manifest_digest (201).
func TestPublishAgentVersion(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "planner", nil, 4, created)}
	verFake := &fakeAgentVersionStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionTx(t, verFake, agentFake)

	rec := doJSON(t, h, http.MethodPost, "/agents/a1/versions", publishBody, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// The store received the manifest fields at version current_version+1.
	if !verFake.createCalled {
		t.Fatal("Create was not called")
	}
	p := verFake.gotCreate
	if p.AgentID != "a1" || p.Version != 5 {
		t.Errorf("create = (agent %q, version %d), want (a1, 5)", p.AgentID, p.Version)
	}
	if p.SystemPrompt != "do things" || string(p.AllowedTools) != `["read","write"]` {
		t.Errorf("create prompt/tools = (%q, %s), want (do things, [\"read\",\"write\"])", p.SystemPrompt, p.AllowedTools)
	}
	if p.PolicyVersionID == nil || *p.PolicyVersionID != "pv1" ||
		p.EndpointID == nil || *p.EndpointID != "ep1" ||
		p.BudgetID == nil || *p.BudgetID != "bd1" {
		t.Errorf("create pins = (%v, %v, %v), want (pv1, ep1, bd1)", p.PolicyVersionID, p.EndpointID, p.BudgetID)
	}
	if p.FanOutCeiling != 3 {
		t.Errorf("create fan_out_ceiling = %d, want 3", p.FanOutCeiling)
	}

	// current_version advanced to the new version.
	if !agentFake.setVersionCalled || agentFake.gotSetVersionID != "a1" || agentFake.gotSetVersion != 5 {
		t.Errorf("SetCurrentVersion = (%q, %d, called=%v), want (a1, 5, true)",
			agentFake.gotSetVersionID, agentFake.gotSetVersion, agentFake.setVersionCalled)
	}

	// The response digest is computed from the stored manifest.
	var body apiAgentVersion
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := computeManifestDigest(store.AgentVersion{
		AgentID:         "a1",
		Version:         5,
		SystemPrompt:    "do things",
		AllowedTools:    json.RawMessage(`["read","write"]`),
		PolicyVersionID: strptr("pv1"),
		EndpointID:      strptr("ep1"),
		BudgetID:        strptr("bd1"),
		FanOutCeiling:   3,
	})
	if body.ManifestDigest != want {
		t.Errorf("manifest_digest = %q, want %q", body.ManifestDigest, want)
	}
	// notes has no backing column and must not be echoed.
	if _, ok := rawKeys(t, rec.Body.Bytes())["notes"]; ok {
		t.Error("response contains notes; it must be omitted")
	}
}

// TestPublishAgentVersionDefaultsTools proves an omitted allowed_tools stores
// the empty array (the column default), not null.
func TestPublishAgentVersionDefaultsTools(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "planner", nil, 0, created)}
	verFake := &fakeAgentVersionStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionTx(t, verFake, agentFake)

	rec := doJSON(t, h, http.MethodPost, "/agents/a1/versions", `{"system_prompt":"p"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if string(verFake.gotCreate.AllowedTools) != "[]" {
		t.Errorf("allowed_tools = %s, want []", verFake.gotCreate.AllowedTools)
	}
	if verFake.gotCreate.Version != 1 {
		t.Errorf("version = %d, want 1 (0+1)", verFake.gotCreate.Version)
	}
}

// TestPublishAgentVersionMissingSystemPrompt proves an empty/omitted
// system_prompt is a 400 and the store is never touched.
func TestPublishAgentVersionMissingSystemPrompt(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "planner", nil, 0, created)}
	verFake := &fakeAgentVersionStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionTx(t, verFake, agentFake)

	rec := doJSON(t, h, http.MethodPost, "/agents/a1/versions", `{"allowed_tools":["read"]}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if verFake.createCalled || agentFake.setVersionCalled {
		t.Error("store mutated despite a validation failure")
	}
}

// TestPublishAgentVersionForbidden proves the agents.manage gate: a caller
// without it gets 403 and the handler never runs.
func TestPublishAgentVersionForbidden(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "planner", nil, 0, created)}
	verFake := &fakeAgentVersionStore{}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAgentStore(t, agentFake)
	setAgentVersionTx(t, verFake, agentFake)
	setResolver(t, permResolver(store.PermPoliciesManage)) // some permission, but not agents.manage
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/agents/a1/versions", publishBody, hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if verFake.createCalled || agentFake.gotGetID != "" {
		t.Error("handler reached the store despite a permission miss")
	}
}

// TestPublishAgentVersionCrossOrg proves a version cannot be published against
// an agent in another org: 404, and the store is never mutated.
func TestPublishAgentVersionCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-2", "other", nil, 0, created)}
	verFake := &fakeAgentVersionStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionTx(t, verFake, agentFake)

	rec := doJSON(t, h, http.MethodPost, "/agents/a1/versions", publishBody, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if verFake.createCalled || agentFake.setVersionCalled {
		t.Error("store mutated for a cross-org agent")
	}
}

// TestPublishAgentVersionConflict proves a losing race on the unique
// (agent_id, version) constraint surfaces as 409.
func TestPublishAgentVersionConflict(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "planner", nil, 2, created)}
	verFake := &fakeAgentVersionStore{createErr: store.ErrConflict}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setAgentVersionTx(t, verFake, agentFake)

	rec := doJSON(t, h, http.MethodPost, "/agents/a1/versions", publishBody, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestPublishAgentVersionAtomic proves the insert and the current_version bump
// share one transaction: when SetCurrentVersion fails, the closure returns an
// error so the transaction rolls back (never commits) and the response is not a
// 201 — the just-inserted version is discarded rather than left orphaned.
func TestPublishAgentVersionAtomic(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{
		getAgent:      storeAgent("a1", "org-1", "planner", nil, 2, created),
		setVersionErr: store.ErrNotFound,
	}
	verFake := &fakeAgentVersionStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")

	// A tx wrapper that commits only when the closure returns nil, so the test
	// can assert the failed publish rolled back.
	committed := false
	orig := withAgentVersionTx
	withAgentVersionTx = func(_ context.Context, _ *db.Pool, fn func(agentVersionStore, agentStore) error) error {
		err := fn(verFake, agentFake)
		if err == nil {
			committed = true
		}
		return err
	}
	t.Cleanup(func() { withAgentVersionTx = orig })

	rec := doJSON(t, h, http.MethodPost, "/agents/a1/versions", publishBody, hdr)
	if rec.Code == http.StatusCreated {
		t.Fatalf("status = 201, want an error status (body %s)", rec.Body.String())
	}
	if committed {
		t.Error("transaction committed despite the SetCurrentVersion failure")
	}
	// Both operations were attempted inside the same transaction.
	if !verFake.createCalled || !agentFake.setVersionCalled {
		t.Errorf("create/set-current called = (%v, %v), want both true (one tx)",
			verFake.createCalled, agentFake.setVersionCalled)
	}
}

// --- listAgentEntitlements --------------------------------------------------

// TestListAgentEntitlements proves the principal mapping both ways plus the
// policy_id and max_fan_out passthrough (ADR-0002 §4c).
func TestListAgentEntitlements(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fanOut := 5
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	entFake := &fakeEntitlementStore{ents: []store.Entitlement{
		{ID: "e1", AgentID: "a1", MemberID: strptr("m1"), PolicyVersionID: strptr("pv1"), MaxFanOut: &fanOut, CreatedAt: created},
		{ID: "e2", AgentID: "a1", TeamID: strptr("t1"), CreatedAt: created},
	}}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, entFake)

	rec := doJSON(t, h, http.MethodGet, "/agents/a1/entitlements", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if entFake.gotAgentID != "a1" {
		t.Errorf("list agent = %q, want a1", entFake.gotAgentID)
	}
	var body apiEntitlementList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	m := body.Items[0]
	if m.PrincipalType != "user" || m.PrincipalID != "m1" {
		t.Errorf("item0 principal = (%q, %q), want (user, m1)", m.PrincipalType, m.PrincipalID)
	}
	if m.PolicyID == nil || *m.PolicyID != "pv1" || m.MaxFanOut == nil || *m.MaxFanOut != 5 {
		t.Errorf("item0 policy/fanout = (%v, %v), want (pv1, 5)", m.PolicyID, m.MaxFanOut)
	}
	g := body.Items[1]
	if g.PrincipalType != "group" || g.PrincipalID != "t1" {
		t.Errorf("item1 principal = (%q, %q), want (group, t1)", g.PrincipalType, g.PrincipalID)
	}
	// A nil policy_id / max_fan_out must be omitted.
	if keys := rawKeys(t, mustItem(t, rec.Body.Bytes(), 1)); func() bool { _, a := keys["policy_id"]; _, b := keys["max_fan_out"]; return a || b }() {
		t.Error("group item emitted a nil policy_id/max_fan_out")
	}
}

func TestListAgentEntitlementsCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-2", "other", nil, 0, created)}
	entFake := &fakeEntitlementStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, entFake)
	rec := doJSON(t, h, http.MethodGet, "/agents/a1/entitlements", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if entFake.gotAgentID != "" {
		t.Error("entitlement store queried for a cross-org agent")
	}
}

// --- setAgentEntitlements ---------------------------------------------------

// TestSetAgentEntitlements proves the replace: existing grants are deleted, new
// ones created with the reverse principal mapping (user→member_id,
// group→team_id), and the resulting list returned.
func TestSetAgentEntitlements(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	entFake := &fakeEntitlementStore{ents: []store.Entitlement{
		{ID: "old1", AgentID: "a1", MemberID: strptr("m0")},
		{ID: "old2", AgentID: "a1", TeamID: strptr("t0")},
	}}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, entFake)
	setPrincipalReaders(t,
		fakePrincipalReader{orgByID: map[string]string{"m1": "org-1"}},
		fakePrincipalReader{orgByID: map[string]string{"t1": "org-1"}},
	)

	body := `{"items":[` +
		`{"principal_type":"user","principal_id":"m1","policy_id":"pv1","max_fan_out":3},` +
		`{"principal_type":"group","principal_id":"t1"}` +
		`]}`
	rec := doJSON(t, h, http.MethodPut, "/agents/a1/entitlements", body, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// The prior grants were deleted first.
	if len(entFake.gotDeletedIDs) != 2 || entFake.gotDeletedIDs[0] != "old1" || entFake.gotDeletedIDs[1] != "old2" {
		t.Errorf("deleted = %v, want [old1 old2]", entFake.gotDeletedIDs)
	}
	// The new grants map principal → member/team correctly.
	if len(entFake.gotCreated) != 2 {
		t.Fatalf("created = %d params, want 2", len(entFake.gotCreated))
	}
	c0 := entFake.gotCreated[0]
	if c0.MemberID == nil || *c0.MemberID != "m1" || c0.TeamID != nil {
		t.Errorf("created0 = %+v, want member_id m1", c0)
	}
	if c0.PolicyVersionID == nil || *c0.PolicyVersionID != "pv1" || c0.MaxFanOut == nil || *c0.MaxFanOut != 3 {
		t.Errorf("created0 policy/fanout = (%v, %v), want (pv1, 3)", c0.PolicyVersionID, c0.MaxFanOut)
	}
	c1 := entFake.gotCreated[1]
	if c1.TeamID == nil || *c1.TeamID != "t1" || c1.MemberID != nil {
		t.Errorf("created1 = %+v, want team_id t1", c1)
	}

	var out apiEntitlementList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 2 {
		t.Errorf("returned items = %d, want 2", len(out.Items))
	}
}

// TestSetAgentEntitlementsEmptyClears proves an empty payload deletes every
// grant and returns an empty list without creating anything.
func TestSetAgentEntitlementsEmptyClears(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	entFake := &fakeEntitlementStore{ents: []store.Entitlement{
		{ID: "old1", AgentID: "a1", MemberID: strptr("m0")},
	}}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, entFake)
	setPrincipalReaders(t, fakePrincipalReader{}, fakePrincipalReader{})

	rec := doJSON(t, h, http.MethodPut, "/agents/a1/entitlements", `{"items":[]}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(entFake.gotDeletedIDs) != 1 || entFake.gotDeletedIDs[0] != "old1" {
		t.Errorf("deleted = %v, want [old1]", entFake.gotDeletedIDs)
	}
	if len(entFake.gotCreated) != 0 {
		t.Errorf("created = %d, want 0", len(entFake.gotCreated))
	}
	if !containsItemsArray(rec.Body.String()) {
		t.Errorf("body = %s, want items:[]", rec.Body.String())
	}
}

// TestSetAgentEntitlementsUnknownPrincipal proves a principal that does not
// exist is a 400 and nothing is deleted (validation precedes the replace).
func TestSetAgentEntitlementsUnknownPrincipal(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	entFake := &fakeEntitlementStore{ents: []store.Entitlement{{ID: "old1", AgentID: "a1", MemberID: strptr("m0")}}}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, entFake)
	setPrincipalReaders(t, fakePrincipalReader{orgByID: map[string]string{}}, fakePrincipalReader{})

	rec := doJSON(t, h, http.MethodPut, "/agents/a1/entitlements",
		`{"items":[{"principal_type":"user","principal_id":"ghost"}]}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if len(entFake.gotDeletedIDs) != 0 {
		t.Error("existing grants deleted despite an invalid principal")
	}
}

// TestSetAgentEntitlementsCrossOrgPrincipal proves a principal in another org
// is rejected as unknown (400), indistinguishable from missing.
func TestSetAgentEntitlementsCrossOrgPrincipal(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	entFake := &fakeEntitlementStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, entFake)
	setPrincipalReaders(t, fakePrincipalReader{orgByID: map[string]string{"m1": "org-2"}}, fakePrincipalReader{})

	rec := doJSON(t, h, http.MethodPut, "/agents/a1/entitlements",
		`{"items":[{"principal_type":"user","principal_id":"m1"}]}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSetAgentEntitlementsBadPrincipalType(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-1", "n", nil, 0, created)}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, &fakeEntitlementStore{})
	setPrincipalReaders(t, fakePrincipalReader{}, fakePrincipalReader{})
	rec := doJSON(t, h, http.MethodPut, "/agents/a1/entitlements",
		`{"items":[{"principal_type":"robot","principal_id":"x"}]}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSetAgentEntitlementsCrossOrgAgent(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	agentFake := &fakeAgentStore{getAgent: storeAgent("a1", "org-2", "other", nil, 0, created)}
	entFake := &fakeEntitlementStore{}
	h, hdr := agentHarness(t, agentFake, "org-1")
	setEntitlementStore(t, entFake)
	setPrincipalReaders(t, fakePrincipalReader{}, fakePrincipalReader{})
	rec := doJSON(t, h, http.MethodPut, "/agents/a1/entitlements",
		`{"items":[{"principal_type":"user","principal_id":"m1"}]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if len(entFake.gotDeletedIDs) != 0 || len(entFake.gotCreated) != 0 {
		t.Error("store mutated for a cross-org agent")
	}
}

// mustItem extracts items[i] from an {items:[...]} envelope as raw JSON so a
// test can assert on the exact keys emitted for that element.
func mustItem(t *testing.T, body []byte, i int) []byte {
	t.Helper()
	var env struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if i >= len(env.Items) {
		t.Fatalf("item %d out of range (len %d)", i, len(env.Items))
	}
	return env.Items[i]
}
