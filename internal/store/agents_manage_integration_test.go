//go:build integration

// Integration coverage for the AgentStore.Update / AgentStore.Delete methods
// added in batch 15, mirroring the style of TestIntegrationAgents in
// catalog_integration_test.go: exercise the happy path plus the conflict and
// not-found edges against a live Postgres, and confirm the ON DELETE CASCADE
// removes an agent's versions with it. Shares the requirePool / newOrg helpers
// and skips when Postgres is unreachable.
package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestIntegrationAgentUpdateDelete(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agents := NewAgentStore(pool)

	created, err := agents.Create(ctx, org.ID, "planner", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A second agent so we can prove a rename onto its name collides.
	if _, err := agents.Create(ctx, org.ID, "researcher", nil); err != nil {
		t.Fatalf("Create researcher: %v", err)
	}

	// Advance the version pointer so we can prove Update preserves it (Update
	// touches only name/description/updated_at).
	bumped, err := agents.SetCurrentVersion(ctx, created.ID, 3)
	if err != nil {
		t.Fatalf("SetCurrentVersion: %v", err)
	}

	desc := "plans the work"
	updated, err := agents.Update(ctx, created.ID, "planner-v2", &desc)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "planner-v2" || updated.Description == nil || *updated.Description != desc {
		t.Fatalf("Update returned %+v, want name planner-v2 and description set", updated)
	}
	if updated.CurrentVersion != 3 {
		t.Fatalf("Update changed current_version: got %d, want 3", updated.CurrentVersion)
	}
	if updated.UpdatedAt.Before(bumped.UpdatedAt) {
		t.Fatalf("Update moved updated_at backwards: %v < %v", updated.UpdatedAt, bumped.UpdatedAt)
	}

	// Clearing the description back to NULL is a valid update.
	cleared, err := agents.Update(ctx, created.ID, "planner-v2", nil)
	if err != nil {
		t.Fatalf("Update(nil desc): %v", err)
	}
	if cleared.Description != nil {
		t.Fatalf("Update(nil desc) description = %v, want nil", cleared.Description)
	}

	// Renaming onto an existing (org_id, name) collides.
	if _, err := agents.Update(ctx, created.ID, "researcher", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update rename collision = %v, want ErrConflict", err)
	}
	if _, err := agents.Update(ctx, missingUUID, "x", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update(missing) = %v, want ErrNotFound", err)
	}

	// Give the agent a version so we can confirm the cascade removes it.
	versions := NewAgentVersionStore(pool)
	if _, err := versions.Create(ctx, CreateAgentVersionParams{
		AgentID:       created.ID,
		Version:       1,
		SystemPrompt:  "do the thing",
		AllowedTools:  json.RawMessage(`["read"]`),
		FanOutCeiling: 1,
	}); err != nil {
		t.Fatalf("create agent version: %v", err)
	}

	if err := agents.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := agents.GetByID(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID after Delete = %v, want ErrNotFound", err)
	}
	if err := agents.Delete(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(absent) = %v, want ErrNotFound", err)
	}
	// The version was removed with the agent (ON DELETE CASCADE).
	remaining, err := versions.ListByAgent(ctx, created.ID)
	if err != nil {
		t.Fatalf("ListByAgent after Delete: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("agent versions after Delete = %d, want 0 (cascade)", len(remaining))
	}
}
