package store

import (
	"encoding/json"
	"time"
)

// The structs and enum below map one-to-one to the telemetry tables created by
// migration 000006: ingested trajectory metadata and the fork/rollback node
// tree. Conventions match models.go — UUIDs are strings, TIMESTAMPTZ is
// time.Time, nullable columns are pointers, and jsonb columns are
// json.RawMessage.

// TrajectoryFieldClass mirrors the Postgres trajectory_field_class enum (and
// pkg/trajectory's FieldClass): whether a [TrajectoryNode] carries only
// structure or an explicitly-stored payload.
type TrajectoryFieldClass string

// The trajectory_field_class enum values.
const (
	TrajectoryFieldClassStructure TrajectoryFieldClass = "structure"
	TrajectoryFieldClassPayload   TrajectoryFieldClass = "payload"
)

// Valid reports whether c is one of the defined trajectory_field_class values.
func (c TrajectoryFieldClass) Valid() bool {
	switch c {
	case TrajectoryFieldClassStructure, TrajectoryFieldClassPayload:
		return true
	default:
		return false
	}
}

// Trajectory is the graph of a governed run (table trajectories). SessionID is
// nullable so a trajectory can be ingested without a live session; deleting the
// session cascades to its trajectories.
type Trajectory struct {
	ID           string          // id (uuid)
	OrgID        string          // org_id → organizations.id
	SessionID    *string         // session_id → sessions.id (nullable, CASCADE)
	MetadataJSON json.RawMessage // metadata_json (jsonb object)
	StartedAt    time.Time       // started_at
	EndedAt      *time.Time      // ended_at (nullable)
	CreatedAt    time.Time       // created_at
}

// TrajectoryNode is one node in a [Trajectory]'s fork/rollback tree (table
// trajectory_nodes; unique per (trajectory_id, event_id)). ParentID is the
// self-referential edge — NULL at the root. By default only structure is
// stored; raw payloads are not persisted unless FieldClass is payload.
type TrajectoryNode struct {
	ID           string               // id (uuid)
	TrajectoryID string               // trajectory_id → trajectories.id (CASCADE)
	ParentID     *string              // parent_id → trajectory_nodes.id (nullable, CASCADE)
	EventID      string               // event_id (unique within a trajectory)
	EventType    string               // event_type
	ToolName     *string              // tool_name (nullable)
	FieldClass   TrajectoryFieldClass // field_class (trajectory_field_class enum)
	MetadataJSON json.RawMessage      // metadata_json (jsonb object)
	OccurredAt   time.Time            // occurred_at
	CreatedAt    time.Time            // created_at
}
