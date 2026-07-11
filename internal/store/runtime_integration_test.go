//go:build integration

// Integration coverage for the runtime + metering repositories (migration
// 000005): sessions, credential grants, append-only usage events, and the
// approval queue. Mirrors catalog_integration_test.go — happy-path
// create/get/list plus the not-found and conflict paths — and reuses the
// helpers in integration_test.go (requirePool, newOrg, uniq) and
// catalog_integration_test.go (newMember, missingUUID, mustNumeric,
// wantNumeric). Each test hangs its rows off a throwaway organization whose
// cleanup cascades, so nothing leaks between tests.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// newAgent creates a throwaway agent in orgID for sessions to reference.
func newAgent(t *testing.T, ctx context.Context, db Querier, orgID string) Agent {
	t.Helper()
	a, err := NewAgentStore(db).Create(ctx, orgID, uniq("agent"), nil)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return a
}

func TestIntegrationSessions(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent := newAgent(t, ctx, pool, org.ID)
	member := newMember(t, ctx, pool, org.ID)
	sessions := NewSessionStore(pool)

	// Defaults: empty status -> pending, nil labels -> '{}', nil terminated_at.
	defaulted, err := sessions.Create(ctx, CreateSessionParams{OrgID: org.ID, AgentID: agent.ID, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("Create defaulted: %v", err)
	}
	if defaulted.Status != SessionStatusPending || string(defaulted.Labels) != "{}" {
		t.Fatalf("defaulted session unexpected: %+v", defaulted)
	}
	if defaulted.FanOutCeiling != 1 || defaulted.TerminatedAt != nil || defaulted.CreatedAt.IsZero() {
		t.Fatalf("defaulted session unexpected: %+v", defaulted)
	}
	if defaulted.AgentVersion != nil || defaulted.PolicyVersionID != nil || defaulted.EnvironmentID != nil || defaulted.LaunchedBy != nil {
		t.Fatalf("defaulted session should have nil optional refs: %+v", defaulted)
	}

	// A fully-specified session.
	ver := 3
	full, err := sessions.Create(ctx, CreateSessionParams{
		OrgID: org.ID, AgentID: agent.ID, AgentVersion: &ver, LaunchedBy: &member.ID,
		Status: SessionStatusActive, FanOutCeiling: 5, Labels: json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatalf("Create full: %v", err)
	}
	if full.Status != SessionStatusActive || full.AgentVersion == nil || *full.AgentVersion != ver {
		t.Fatalf("full session unexpected: %+v", full)
	}
	if full.LaunchedBy == nil || *full.LaunchedBy != member.ID || string(full.Labels) != `{"env": "prod"}` {
		t.Fatalf("full session unexpected: %+v", full)
	}

	got, err := sessions.GetByID(ctx, defaulted.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != defaulted.ID {
		t.Fatalf("GetByID = %+v, want id %s", got, defaulted.ID)
	}

	byOrg, err := sessions.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(byOrg) != 2 || byOrg[0].CreatedAt.Before(byOrg[1].CreatedAt) {
		t.Fatalf("ListByOrg = %+v, want 2 rows newest-first", byOrg)
	}
	byAgent, err := sessions.ListByAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("ListByAgent: %v", err)
	}
	if len(byAgent) != 2 {
		t.Fatalf("ListByAgent len = %d, want 2", len(byAgent))
	}

	// Terminal status stamps terminated_at.
	term, err := sessions.SetStatus(ctx, defaulted.ID, SessionStatusTerminated)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if term.Status != SessionStatusTerminated || term.TerminatedAt == nil {
		t.Fatalf("SetStatus(terminated) did not stamp terminated_at: %+v", term)
	}
	// A non-terminal transition leaves terminated_at untouched.
	reactivated, err := sessions.SetStatus(ctx, defaulted.ID, SessionStatusActive)
	if err != nil {
		t.Fatalf("SetStatus active: %v", err)
	}
	if reactivated.TerminatedAt == nil || !reactivated.TerminatedAt.Equal(*term.TerminatedAt) {
		t.Fatalf("SetStatus(active) changed terminated_at: %+v", reactivated)
	}

	if _, err := sessions.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if _, err := sessions.SetStatus(ctx, missingUUID, SessionStatusFailed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetStatus(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationCredentialGrants(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent := newAgent(t, ctx, pool, org.ID)
	session, err := NewSessionStore(pool).Create(ctx, CreateSessionParams{OrgID: org.ID, AgentID: agent.ID, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	grants := NewCredentialGrantStore(pool)

	expires := time.Now().Add(time.Hour)
	// Nil scopes fall back to '[]'.
	grant, err := grants.Create(ctx, CreateCredentialGrantParams{
		SessionID: session.ID, ProxyTokenHash: uniq("hash"), ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if string(grant.Scopes) != "[]" || grant.RevokedAt != nil || grant.IssuedAt.IsZero() {
		t.Fatalf("grant defaults unexpected: %+v", grant)
	}
	if _, err := grants.Create(ctx, CreateCredentialGrantParams{
		SessionID: session.ID, ProxyTokenHash: uniq("hash"), Scopes: json.RawMessage(`["models:read"]`), ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("Create second: %v", err)
	}

	found, err := grants.GetByTokenHash(ctx, grant.ProxyTokenHash)
	if err != nil {
		t.Fatalf("GetByTokenHash: %v", err)
	}
	if found.ID != grant.ID {
		t.Fatalf("GetByTokenHash = %+v, want id %s", found, grant.ID)
	}

	list, err := grants.ListBySession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListBySession len = %d, want 2", len(list))
	}

	revoked, err := grants.Revoke(ctx, grant.ID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("Revoke did not stamp revoked_at")
	}

	// Duplicate proxy_token_hash -> ErrConflict.
	if _, err := grants.Create(ctx, CreateCredentialGrantParams{
		SessionID: session.ID, ProxyTokenHash: grant.ProxyTokenHash, ExpiresAt: expires,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := grants.GetByTokenHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByTokenHash(missing) = %v, want ErrNotFound", err)
	}
	if _, err := grants.Revoke(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revoke(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationUsageEvents(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent := newAgent(t, ctx, pool, org.ID)
	session, err := NewSessionStore(pool).Create(ctx, CreateSessionParams{OrgID: org.ID, AgentID: agent.ID, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	events := NewUsageEventStore(pool)

	model := "claude-opus-4-8"
	full, err := events.Append(ctx, AppendUsageEventParams{
		OrgID: org.ID, SessionID: &session.ID, AgentID: &agent.ID, Model: &model,
		PromptTokens: 100, CompletionTokens: 40, CostAmount: mustNumeric(t, "0.125"), Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("Append full: %v", err)
	}
	if full.ID == 0 || full.TotalTokens != 140 || full.Currency != "EUR" || full.OccurredAt.IsZero() {
		t.Fatalf("Append full unexpected: %+v", full)
	}
	wantNumeric(t, full.CostAmount, "0.125")

	// Minimal append: only org. Tokens/cost default to 0, currency to USD.
	minimal, err := events.Append(ctx, AppendUsageEventParams{OrgID: org.ID})
	if err != nil {
		t.Fatalf("Append minimal: %v", err)
	}
	if minimal.TotalTokens != 0 || minimal.Currency != "USD" || minimal.SessionID != nil {
		t.Fatalf("Append minimal unexpected: %+v", minimal)
	}
	wantNumeric(t, minimal.CostAmount, "0")

	byOrg, err := events.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(byOrg) != 2 {
		t.Fatalf("ListByOrg len = %d, want 2", len(byOrg))
	}
	bySession, err := events.ListBySession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(bySession) != 1 || bySession[0].ID != full.ID {
		t.Fatalf("ListBySession = %+v, want [%d]", bySession, full.ID)
	}
}

func TestIntegrationApprovalRequests(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent := newAgent(t, ctx, pool, org.ID)
	member := newMember(t, ctx, pool, org.ID)
	session, err := NewSessionStore(pool).Create(ctx, CreateSessionParams{OrgID: org.ID, AgentID: agent.ID, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	approvals := NewApprovalRequestStore(pool)

	// Defaults: pending, nil reason, '{}' context.
	req, err := approvals.Create(ctx, CreateApprovalRequestParams{OrgID: org.ID, SessionID: session.ID, Action: "spend.increase"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.Status != ApprovalStatusPending || req.Reason != nil || string(req.ContextJSON) != "{}" || req.DecidedAt != nil {
		t.Fatalf("Create defaults unexpected: %+v", req)
	}
	reason := "over budget"
	other, err := approvals.Create(ctx, CreateApprovalRequestParams{
		OrgID: org.ID, SessionID: session.ID, Action: "tool.exec", Reason: &reason, ContextJSON: json.RawMessage(`{"tool":"fs.write"}`),
	})
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}

	got, err := approvals.GetByID(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != req.ID {
		t.Fatalf("GetByID = %+v, want id %s", got, req.ID)
	}

	all, err := approvals.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(all) != 2 || all[0].ID != req.ID {
		t.Fatalf("ListByOrg = %+v, want 2 rows oldest-first", all)
	}
	pending, err := approvals.ListPendingByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListPendingByOrg: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("ListPendingByOrg len = %d, want 2", len(pending))
	}

	decided, err := approvals.Decide(ctx, req.ID, ApprovalStatusApproved, &member.ID)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decided.Status != ApprovalStatusApproved || decided.DecidedAt == nil || decided.DecidedBy == nil || *decided.DecidedBy != member.ID {
		t.Fatalf("Decide unexpected: %+v", decided)
	}

	// The pending queue now excludes the decided request.
	pending, err = approvals.ListPendingByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListPendingByOrg after decide: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != other.ID {
		t.Fatalf("ListPendingByOrg = %+v, want only %s", pending, other.ID)
	}

	// Deciding an already-decided request is a no-op that reports ErrNotFound.
	if _, err := approvals.Decide(ctx, req.ID, ApprovalStatusDenied, &member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Decide(already decided) = %v, want ErrNotFound", err)
	}
	if _, err := approvals.Decide(ctx, missingUUID, ApprovalStatusApproved, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Decide(missing) = %v, want ErrNotFound", err)
	}
	if _, err := approvals.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
}
