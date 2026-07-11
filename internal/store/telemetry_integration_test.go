//go:build integration

// Integration coverage for the telemetry repositories (migration 000006):
// trajectories and their fork/rollback node tree. Mirrors
// catalog_integration_test.go and reuses the shared helpers (requirePool,
// newOrg, uniq, missingUUID). Each test hangs its rows off a throwaway
// organization whose cleanup cascades, so nothing leaks between tests.
package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestIntegrationTrajectories(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent := newAgent(t, ctx, pool, org.ID)
	session, err := NewSessionStore(pool).Create(ctx, CreateSessionParams{OrgID: org.ID, AgentID: agent.ID, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	trajectories := NewTrajectoryStore(pool)

	// Defaults: nil session, nil metadata -> '{}', started_at defaults to now().
	defaulted, err := trajectories.Create(ctx, CreateTrajectoryParams{OrgID: org.ID})
	if err != nil {
		t.Fatalf("Create defaulted: %v", err)
	}
	if defaulted.SessionID != nil || string(defaulted.MetadataJSON) != "{}" || defaulted.StartedAt.IsZero() || defaulted.EndedAt != nil {
		t.Fatalf("defaulted trajectory unexpected: %+v", defaulted)
	}

	// A trajectory tied to a session with a backfilled started_at.
	started := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	tied, err := trajectories.Create(ctx, CreateTrajectoryParams{
		OrgID: org.ID, SessionID: &session.ID, MetadataJSON: json.RawMessage(`{"schema":"v1"}`), StartedAt: &started,
	})
	if err != nil {
		t.Fatalf("Create tied: %v", err)
	}
	if tied.SessionID == nil || *tied.SessionID != session.ID || !tied.StartedAt.Equal(started) {
		t.Fatalf("tied trajectory unexpected: %+v", tied)
	}

	got, err := trajectories.GetByID(ctx, defaulted.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != defaulted.ID {
		t.Fatalf("GetByID = %+v, want id %s", got, defaulted.ID)
	}

	list, err := trajectories.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	// Newest started_at first: the backfilled trajectory (24h ago) sorts last.
	if len(list) != 2 || list[0].ID != defaulted.ID || list[1].ID != tied.ID {
		t.Fatalf("ListByOrg = %+v, want [defaulted tied] by started_at desc", list)
	}

	ended := time.Now().UTC().Truncate(time.Microsecond)
	closed, err := trajectories.SetEnded(ctx, defaulted.ID, ended)
	if err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if closed.EndedAt == nil || !closed.EndedAt.Equal(ended) {
		t.Fatalf("SetEnded = %+v, want ended_at %s", closed, ended)
	}

	if _, err := trajectories.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if _, err := trajectories.SetEnded(ctx, missingUUID, ended); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetEnded(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationTrajectoryNodes(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	traj, err := NewTrajectoryStore(pool).Create(ctx, CreateTrajectoryParams{OrgID: org.ID})
	if err != nil {
		t.Fatalf("create trajectory: %v", err)
	}
	nodes := NewTrajectoryNodeStore(pool)

	base := time.Now().UTC().Truncate(time.Microsecond)
	// Root node: nil parent, default field_class 'structure', nil tool.
	root, err := nodes.Create(ctx, CreateTrajectoryNodeParams{
		TrajectoryID: traj.ID, EventID: "e1", EventType: "session_start", OccurredAt: base,
	})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	if root.ParentID != nil || root.FieldClass != TrajectoryFieldClassStructure || root.ToolName != nil || string(root.MetadataJSON) != "{}" {
		t.Fatalf("root node defaults unexpected: %+v", root)
	}

	// Child node referencing the root, carrying a payload and a tool name.
	tool := "fs.read"
	child, err := nodes.Create(ctx, CreateTrajectoryNodeParams{
		TrajectoryID: traj.ID, ParentID: &root.ID, EventID: "e2", EventType: "tool_call",
		ToolName: &tool, FieldClass: TrajectoryFieldClassPayload,
		MetadataJSON: json.RawMessage(`{"path":"/etc"}`), OccurredAt: base.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if child.ParentID == nil || *child.ParentID != root.ID || child.FieldClass != TrajectoryFieldClassPayload {
		t.Fatalf("child node unexpected: %+v", child)
	}
	if child.ToolName == nil || *child.ToolName != tool {
		t.Fatalf("child node tool_name = %v, want %s", child.ToolName, tool)
	}

	byID, err := nodes.GetByID(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if byID.ID != child.ID {
		t.Fatalf("GetByID = %+v, want id %s", byID, child.ID)
	}
	byEvent, err := nodes.GetByEventID(ctx, traj.ID, "e1")
	if err != nil {
		t.Fatalf("GetByEventID: %v", err)
	}
	if byEvent.ID != root.ID {
		t.Fatalf("GetByEventID = %+v, want root %s", byEvent, root.ID)
	}

	list, err := nodes.ListByTrajectory(ctx, traj.ID)
	if err != nil {
		t.Fatalf("ListByTrajectory: %v", err)
	}
	if len(list) != 2 || list[0].ID != root.ID || list[1].ID != child.ID {
		t.Fatalf("ListByTrajectory = %+v, want [root child] by occurred_at", list)
	}

	// Duplicate (trajectory_id, event_id) -> ErrConflict.
	if _, err := nodes.Create(ctx, CreateTrajectoryNodeParams{
		TrajectoryID: traj.ID, EventID: "e1", EventType: "dup", OccurredAt: base,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := nodes.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if _, err := nodes.GetByEventID(ctx, traj.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByEventID(missing) = %v, want ErrNotFound", err)
	}
}
