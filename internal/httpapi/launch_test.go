package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/store"
	"github.com/Domene99/drun-dash/pkg/policy"
)

// ---- launch-specific fakes ------------------------------------------------

type fakeAgentVersionStore struct {
	byVersion map[int]store.AgentVersion
	err       error

	gotAgentID string
	gotVersion int
}

func (f *fakeAgentVersionStore) GetByAgentAndVersion(_ context.Context, agentID string, version int) (store.AgentVersion, error) {
	f.gotAgentID, f.gotVersion = agentID, version
	if f.err != nil {
		return store.AgentVersion{}, f.err
	}
	av, ok := f.byVersion[version]
	if !ok {
		return store.AgentVersion{}, store.ErrNotFound
	}
	return av, nil
}

type fakeEntitlementStore struct {
	ents       []store.Entitlement
	err        error
	gotAgentID string
}

func (f *fakeEntitlementStore) ListByAgent(_ context.Context, agentID string) ([]store.Entitlement, error) {
	f.gotAgentID = agentID
	return f.ents, f.err
}

type fakeTeamMembershipStore struct {
	teams       []store.Team
	err         error
	gotMemberID string
}

func (f *fakeTeamMembershipStore) ListTeams(_ context.Context, memberID string) ([]store.Team, error) {
	f.gotMemberID = memberID
	return f.teams, f.err
}

type fakePolicyVersionStore struct {
	pv    store.PolicyVersion
	err   error
	gotID string
}

func (f *fakePolicyVersionStore) GetByID(_ context.Context, id string) (store.PolicyVersion, error) {
	f.gotID = id
	if f.err != nil {
		return store.PolicyVersion{}, f.err
	}
	return f.pv, nil
}

type fakeSessionLaunchWriter struct {
	session store.Session
	err     error

	gotSession store.CreateSessionParams
	gotGrant   store.CreateCredentialGrantParams
	called     bool
}

func (f *fakeSessionLaunchWriter) Persist(_ context.Context, sp store.CreateSessionParams, gp store.CreateCredentialGrantParams) (store.Session, store.CredentialGrant, error) {
	f.called = true
	// The production writer stamps the created session id onto the grant; mirror
	// that so assertions on the grant see the same session id.
	s := f.session
	if s.ID == "" {
		s.ID = "sess-1"
	}
	gp.SessionID = s.ID
	f.gotSession, f.gotGrant = sp, gp
	if f.err != nil {
		return store.Session{}, store.CredentialGrant{}, f.err
	}
	return s, store.CredentialGrant{ID: "grant-1", SessionID: s.ID}, nil
}

type fakeSessionTerminator struct {
	session store.Session
	err     error
	gotID   string
}

func (f *fakeSessionTerminator) Terminate(_ context.Context, sessionID string) (store.Session, error) {
	f.gotID = sessionID
	return f.session, f.err
}

// ---- harness --------------------------------------------------------------

// launchFixture bundles every seam a launch test overrides plus the store rows
// they return, so a test tweaks one field and mounts the router.
type launchFixture struct {
	agent    *fakeAgentStore
	versions *fakeAgentVersionStore
	ents     *fakeEntitlementStore
	teams    *fakeTeamMembershipStore
	policies *fakePolicyVersionStore
	writer   *fakeSessionLaunchWriter
}

// defaultLaunchFixture returns a fully entitled, correctly configured launch:
// agent agt_1 in org-1, current version 4, a member entitlement for member-1
// with max_fan_out 3, an agent-version fan-out of 5, and a boundary with
// fan_out_ceiling 8 — so a requested_fan_out of 10 clamps to 3.
func defaultLaunchFixture() *launchFixture {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	av := store.AgentVersion{
		ID:              "av-1",
		AgentID:         "agt_1",
		Version:         4,
		PolicyVersionID: strptr("pv-1"),
		EndpointID:      strptr("ep-1"),
		FanOutCeiling:   5,
	}
	boundary := `{"allowed_tools":["bash","read"],"network":"allowlist","fan_out_ceiling":8,"rules":{"network_allowlist":["api.example.com"],"bash_timeout_ms":5000}}`
	return &launchFixture{
		agent:    &fakeAgentStore{getAgent: storeAgent("agt_1", "org-1", "planner", nil, 4, created)},
		versions: &fakeAgentVersionStore{byVersion: map[int]store.AgentVersion{4: av}},
		ents: &fakeEntitlementStore{ents: []store.Entitlement{
			{ID: "ent-1", AgentID: "agt_1", MemberID: strptr("member-1"), MaxFanOut: intptr(3)},
		}},
		teams: &fakeTeamMembershipStore{},
		policies: &fakePolicyVersionStore{pv: store.PolicyVersion{
			ID: "pv-1", PolicyID: "pol-1", Version: 7, ContentJSON: json.RawMessage(boundary),
		}},
		writer: &fakeSessionLaunchWriter{session: store.Session{ID: "sess-1", OrgID: "org-1"}},
	}
}

// launchHarness wires a Deps with a bundle-signing key, points every launch
// seam at the fixture, mounts the router, and returns the handler, an auth
// header for member-1 in org-1, and the bundle public key for verification.
func launchHarness(t *testing.T, f *launchFixture) (http.Handler, http.Header, ed25519.PublicKey) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate bundle key: %v", err)
	}
	deps.BundleSigningKey = priv
	deps.BundleSigningKID = "bundle-test"
	deps.ProxyBaseURL = "https://proxy.test"
	deps.SessionTTL = time.Hour

	setLaunchSeams(t, f)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}
	return h, hdr, pub
}

func setLaunchSeams(t *testing.T, f *launchFixture) {
	t.Helper()
	oa, oav, oe, ot, op, ow := newAgentStore, newAgentVersionStore, newEntitlementStore, newTeamMembershipStore, newPolicyVersionStore, newSessionLaunchWriter
	newAgentStore = func(store.Querier) agentStore { return f.agent }
	newAgentVersionStore = func(store.Querier) agentVersionReader { return f.versions }
	newEntitlementStore = func(store.Querier) entitlementReader { return f.ents }
	newTeamMembershipStore = func(store.Querier) teamMembershipReader { return f.teams }
	newPolicyVersionStore = func(store.Querier) policyVersionReader { return f.policies }
	newSessionLaunchWriter = func(*db.Pool) sessionLaunchWriter { return f.writer }
	t.Cleanup(func() {
		newAgentStore, newAgentVersionStore, newEntitlementStore, newTeamMembershipStore, newPolicyVersionStore, newSessionLaunchWriter = oa, oav, oe, ot, op, ow
	})
}

// ---- happy path -----------------------------------------------------------

// TestLaunchSession is the end-to-end proof: a fully entitled caller gets a 201
// with a verifiable bundle, a scoped proxy token whose hash (not raw form) is
// what the writer stored, the effective (clamped) fan-out, and an active
// session whose params carry the resolved policy version.
func TestLaunchSession(t *testing.T) {
	f := defaultLaunchFixture()
	h, hdr, pub := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch",
		`{"agent_id":"agt_1","requested_fan_out":10,"labels":{"team":"blue"}}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	var body apiSessionLaunchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", body.SessionID)
	}
	if body.ProxyBaseURL != "https://proxy.test" {
		t.Errorf("proxy_base_url = %q", body.ProxyBaseURL)
	}
	if len(body.ProxyToken) < 4 || body.ProxyToken[:4] != "dgp_" {
		t.Errorf("proxy_token = %q, want dgp_ prefix", body.ProxyToken)
	}
	// requested 10 vs agent-version 5, boundary 8, entitlement 3 → min = 3.
	if body.FanOutCeiling != 3 {
		t.Errorf("fan_out_ceiling = %d, want 3 (clamped)", body.FanOutCeiling)
	}
	if body.ExpiresAt == nil {
		t.Fatal("expires_at = nil, want the session lifetime")
	}

	// The raw token is never stored: the writer sees only its SHA-256 hash.
	wantHash := sha256.Sum256([]byte(body.ProxyToken))
	if f.writer.gotGrant.ProxyTokenHash != hex.EncodeToString(wantHash[:]) {
		t.Errorf("stored token hash = %q, want sha256(raw)", f.writer.gotGrant.ProxyTokenHash)
	}
	if f.writer.gotGrant.EndpointID == nil || *f.writer.gotGrant.EndpointID != "ep-1" {
		t.Errorf("grant endpoint = %v, want ep-1", f.writer.gotGrant.EndpointID)
	}
	if !f.writer.gotGrant.ExpiresAt.Equal(*body.ExpiresAt) {
		t.Errorf("grant expiry %v != response expiry %v", f.writer.gotGrant.ExpiresAt, *body.ExpiresAt)
	}

	// Session params: active, resolved policy version, clamped fan-out, launcher.
	sp := f.writer.gotSession
	if sp.Status != store.SessionStatusActive {
		t.Errorf("session status = %q, want active", sp.Status)
	}
	if sp.PolicyVersionID == nil || *sp.PolicyVersionID != "pv-1" {
		t.Errorf("session policy_version_id = %v, want pv-1", sp.PolicyVersionID)
	}
	if sp.FanOutCeiling != 3 {
		t.Errorf("session fan_out_ceiling = %d, want 3", sp.FanOutCeiling)
	}
	if sp.AgentVersion == nil || *sp.AgentVersion != 4 {
		t.Errorf("session agent_version = %v, want 4", sp.AgentVersion)
	}
	if sp.LaunchedBy == nil || *sp.LaunchedBy != "member-1" {
		t.Errorf("session launched_by = %v, want member-1", sp.LaunchedBy)
	}

	// The delivered bundle verifies fail-closed against the bundle key, and its
	// compiled config reflects the boundary.
	bundleBytes := rawKeys(t, rec.Body.Bytes())["policy_bundle"]
	loaded, err := policy.Load(bundleBytes)
	if err != nil {
		t.Fatalf("policy.Load(policy_bundle): %v", err)
	}
	if err := loaded.Verify(time.Now(), pub); err != nil {
		t.Errorf("bundle Verify: %v", err)
	}
	if loaded.PolicyID != "pol-1" || loaded.PolicyVersion != 7 {
		t.Errorf("bundle identity = (%s, %d), want (pol-1, 7)", loaded.PolicyID, loaded.PolicyVersion)
	}
	if got := loaded.DrunConfig.DomainAllowlist; len(got) != 1 || got[0] != "api.example.com" {
		t.Errorf("bundle domain_allowlist = %v, want [api.example.com]", got)
	}
	if got := loaded.DrunConfig.BashCommandAllowlist; len(got) != 2 {
		t.Errorf("bundle bash_command_allowlist = %v, want 2 tools", got)
	}
}

// TestLaunchSessionTeamEntitlement proves a caller with no direct grant is
// entitled via team membership.
func TestLaunchSessionTeamEntitlement(t *testing.T) {
	f := defaultLaunchFixture()
	f.ents.ents = []store.Entitlement{
		{ID: "ent-2", AgentID: "agt_1", TeamID: strptr("team-9"), PolicyVersionID: strptr("pv-override")},
	}
	f.teams.teams = []store.Team{{ID: "team-9", OrgID: "org-1", Name: "blue"}}
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	// The team entitlement's policy-version override wins over the agent version.
	if f.policies.gotID != "pv-override" {
		t.Errorf("resolved policy version = %q, want pv-override (entitlement override)", f.policies.gotID)
	}
	if f.teams.gotMemberID != "member-1" {
		t.Errorf("team lookup member = %q, want member-1", f.teams.gotMemberID)
	}
}

// TestLaunchSessionEntitlementOverride proves a member entitlement's
// policy-version override is used over the agent version's pin.
func TestLaunchSessionEntitlementOverride(t *testing.T) {
	f := defaultLaunchFixture()
	f.ents.ents = []store.Entitlement{
		{ID: "ent-1", AgentID: "agt_1", MemberID: strptr("member-1"), PolicyVersionID: strptr("pv-override")},
	}
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if f.policies.gotID != "pv-override" {
		t.Errorf("resolved policy version = %q, want pv-override", f.policies.gotID)
	}
}

// TestLaunchSessionVersionPin proves an explicit agent_version pin is resolved
// (and a missing pin is a 404).
func TestLaunchSessionVersionPin(t *testing.T) {
	f := defaultLaunchFixture()
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1","agent_version":4}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if f.versions.gotVersion != 4 {
		t.Errorf("resolved version = %d, want 4", f.versions.gotVersion)
	}
}

// ---- error paths ----------------------------------------------------------

func TestLaunchSessionValidation(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{"missing agent_id", `{}`},
		{"empty agent_id", `{"agent_id":""}`},
		{"negative fan_out", `{"agent_id":"agt_1","requested_fan_out":-1}`},
		{"malformed json", `{`},
		{"unknown field", `{"agent_id":"agt_1","bogus":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := defaultLaunchFixture()
			h, hdr, _ := launchHarness(t, f)
			rec := doJSON(t, h, http.MethodPost, "/sessions/launch", tc.body, hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if f.writer.called {
				t.Error("writer called on a rejected request")
			}
		})
	}
}

func TestLaunchSessionAgentCrossOrg(t *testing.T) {
	f := defaultLaunchFixture()
	f.agent.getAgent = storeAgent("agt_1", "org-2", "other", nil, 4, time.Now())
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestLaunchSessionAgentNotFound(t *testing.T) {
	f := defaultLaunchFixture()
	f.agent.getErr = store.ErrNotFound
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"missing"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestLaunchSessionPinnedVersionMissing(t *testing.T) {
	f := defaultLaunchFixture()
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1","agent_version":99}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestLaunchSessionNoPublishedVersion(t *testing.T) {
	f := defaultLaunchFixture()
	f.agent.getAgent = storeAgent("agt_1", "org-1", "planner", nil, 0, time.Now()) // current_version 0
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestLaunchSessionNotEntitled proves the one contract-sanctioned 403: the agent
// exists in the caller's org but no entitlement matches.
func TestLaunchSessionNotEntitled(t *testing.T) {
	f := defaultLaunchFixture()
	f.ents.ents = []store.Entitlement{
		{ID: "ent-x", AgentID: "agt_1", MemberID: strptr("someone-else")},
	}
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if f.writer.called {
		t.Error("writer called for an unentitled caller")
	}
}

// TestLaunchSessionMisconfig proves server-side misconfigurations are 500s
// (never a silent unbounded launch).
func TestLaunchSessionMisconfig(t *testing.T) {
	t.Run("no effective policy version", func(t *testing.T) {
		f := defaultLaunchFixture()
		av := f.versions.byVersion[4]
		av.PolicyVersionID = nil // and the entitlement carries no override
		f.versions.byVersion[4] = av
		h, hdr, _ := launchHarness(t, f)
		rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("no endpoint", func(t *testing.T) {
		f := defaultLaunchFixture()
		av := f.versions.byVersion[4]
		av.EndpointID = nil
		f.versions.byVersion[4] = av
		h, hdr, _ := launchHarness(t, f)
		rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("unsigned bundle key", func(t *testing.T) {
		f := defaultLaunchFixture()
		deps := testDeps(t, fakeOIDC{}, fakeMembers{})
		deps.ProxyBaseURL = "https://proxy.test"
		deps.SessionTTL = time.Hour
		// BundleSigningKey left nil.
		setLaunchSeams(t, f)
		h := mountRouter(deps)
		hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}
		rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
		}
	})
}

// TestLaunchSessionWriterError proves a failed atomic write surfaces as a 500
// (no session, no grant — the writer's rollback is exercised in the store).
func TestLaunchSessionWriterError(t *testing.T) {
	f := defaultLaunchFixture()
	f.writer.err = context.DeadlineExceeded
	h, hdr, _ := launchHarness(t, f)

	rec := doJSON(t, h, http.MethodPost, "/sessions/launch", `{"agent_id":"agt_1"}`, hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// ---- terminate ------------------------------------------------------------

func setSessionTerminator(t *testing.T, term sessionTerminator) {
	t.Helper()
	orig := newSessionTerminator
	newSessionTerminator = func(*db.Pool) sessionTerminator { return term }
	t.Cleanup(func() { newSessionTerminator = orig })
}

func terminateHarness(t *testing.T, sess sessionStore, term sessionTerminator) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setSessionStore(t, sess)
	setSessionTerminator(t, term)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}
	return h, hdr
}

// TestTerminateSession proves the happy path returns 200 with the terminated
// session and forwards the id to the terminator.
func TestTerminateSession(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	term := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	sess := &fakeSessionStore{getSession: storeSession("s1", "org-1", "agt_1", nil, store.SessionStatusActive, 2, created, nil)}
	terminator := &fakeSessionTerminator{session: storeSession("s1", "org-1", "agt_1", nil, store.SessionStatusTerminated, 2, created, &term)}
	h, hdr := terminateHarness(t, sess, terminator)

	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/terminate", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if terminator.gotID != "s1" {
		t.Errorf("terminate id = %q, want s1", terminator.gotID)
	}
	var body apiSession
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "terminated" {
		t.Errorf("status = %q, want terminated", body.Status)
	}
	if body.TerminatedAt == nil || !body.TerminatedAt.Equal(term) {
		t.Errorf("terminated_at = %v, want %v", body.TerminatedAt, term)
	}
}

// TestTerminateSessionIdempotent proves terminating an already-terminated
// session still returns 200.
func TestTerminateSessionIdempotent(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	term := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	sess := &fakeSessionStore{getSession: storeSession("s1", "org-1", "agt_1", nil, store.SessionStatusTerminated, 2, created, &term)}
	terminator := &fakeSessionTerminator{session: storeSession("s1", "org-1", "agt_1", nil, store.SessionStatusTerminated, 2, created, &term)}
	h, hdr := terminateHarness(t, sess, terminator)

	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/terminate", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestTerminateSessionCrossOrg proves a session owned by another org is a 404
// (not 403), and the terminator is never called.
func TestTerminateSessionCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	sess := &fakeSessionStore{getSession: storeSession("s1", "org-2", "agt_1", nil, store.SessionStatusActive, 2, created, nil)}
	terminator := &fakeSessionTerminator{}
	h, hdr := terminateHarness(t, sess, terminator)

	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/terminate", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if terminator.gotID != "" {
		t.Error("terminator called for a cross-org session")
	}
}

func TestTerminateSessionNotFound(t *testing.T) {
	sess := &fakeSessionStore{getErr: store.ErrNotFound}
	h, hdr := terminateHarness(t, sess, &fakeSessionTerminator{})

	rec := doJSON(t, h, http.MethodPost, "/sessions/missing/terminate", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestLaunchTerminateRequireAuth proves both new routes sit behind RequireAuth.
func TestLaunchTerminateRequireAuth(t *testing.T) {
	tests := []struct{ method, path string }{
		{http.MethodPost, "/sessions/launch"},
		{http.MethodPost, "/sessions/s1/terminate"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			h := mountRouter(deps)
			rec := doJSON(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}
