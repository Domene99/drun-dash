package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrajectoryNodeStore reads and writes the trajectory_nodes table.
type TrajectoryNodeStore struct {
	db Querier
}

// NewTrajectoryNodeStore returns a TrajectoryNodeStore backed by db.
func NewTrajectoryNodeStore(db Querier) *TrajectoryNodeStore {
	return &TrajectoryNodeStore{db: db}
}

const trajectoryNodeColumns = "id, trajectory_id, parent_id, event_id, event_type, tool_name, field_class, metadata_json, occurred_at, created_at"

func scanTrajectoryNode(row pgx.Row) (TrajectoryNode, error) {
	var n TrajectoryNode
	err := row.Scan(&n.ID, &n.TrajectoryID, &n.ParentID, &n.EventID, &n.EventType,
		&n.ToolName, &n.FieldClass, &n.MetadataJSON, &n.OccurredAt, &n.CreatedAt)
	return n, err
}

// CreateTrajectoryNodeParams carries the fields for
// [TrajectoryNodeStore.Create]. A nil ParentID marks a root node; a nil
// ToolName stores NULL. An empty FieldClass stores the column default
// ('structure'); a nil MetadataJSON stores the default ('{}'). EventID must be
// unique within the trajectory and OccurredAt is required.
type CreateTrajectoryNodeParams struct {
	TrajectoryID string
	ParentID     *string
	EventID      string
	EventType    string
	ToolName     *string
	FieldClass   TrajectoryFieldClass
	MetadataJSON json.RawMessage
	OccurredAt   time.Time
}

// Create inserts a trajectory node and returns the stored row. An empty
// FieldClass defaults to structure. A duplicate (trajectory_id, event_id)
// yields [ErrConflict]; a missing trajectory or parent node is rejected by the
// foreign key.
func (s *TrajectoryNodeStore) Create(ctx context.Context, p CreateTrajectoryNodeParams) (TrajectoryNode, error) {
	if p.FieldClass == "" {
		p.FieldClass = TrajectoryFieldClassStructure
	}
	const q = `INSERT INTO trajectory_nodes
		(trajectory_id, parent_id, event_id, event_type, tool_name, field_class, metadata_json, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, '{}'::jsonb), $8) RETURNING ` + trajectoryNodeColumns
	node, err := scanTrajectoryNode(s.db.QueryRow(ctx, q,
		p.TrajectoryID, p.ParentID, p.EventID, p.EventType, p.ToolName, p.FieldClass, p.MetadataJSON, p.OccurredAt))
	if err != nil {
		return TrajectoryNode{}, fmt.Errorf("store: create trajectory node %q for trajectory %q: %w", p.EventID, p.TrajectoryID, mapError(err))
	}
	return node, nil
}

// GetByID returns the trajectory node with the given id, or [ErrNotFound].
func (s *TrajectoryNodeStore) GetByID(ctx context.Context, id string) (TrajectoryNode, error) {
	const q = `SELECT ` + trajectoryNodeColumns + ` FROM trajectory_nodes WHERE id = $1`
	node, err := scanTrajectoryNode(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return TrajectoryNode{}, fmt.Errorf("store: trajectory node by id %q: %w", id, mapError(err))
	}
	return node, nil
}

// GetByEventID returns the node with the given event_id within trajectoryID
// (the pair is unique), or [ErrNotFound].
func (s *TrajectoryNodeStore) GetByEventID(ctx context.Context, trajectoryID, eventID string) (TrajectoryNode, error) {
	const q = `SELECT ` + trajectoryNodeColumns + ` FROM trajectory_nodes WHERE trajectory_id = $1 AND event_id = $2`
	node, err := scanTrajectoryNode(s.db.QueryRow(ctx, q, trajectoryID, eventID))
	if err != nil {
		return TrajectoryNode{}, fmt.Errorf("store: trajectory node %q in trajectory %q: %w", eventID, trajectoryID, mapError(err))
	}
	return node, nil
}

// ListByTrajectory returns every node of trajectoryID in replay order (by
// occurred_at, then created_at then id to keep the order stable for nodes
// sharing a timestamp).
func (s *TrajectoryNodeStore) ListByTrajectory(ctx context.Context, trajectoryID string) ([]TrajectoryNode, error) {
	const q = `SELECT ` + trajectoryNodeColumns + ` FROM trajectory_nodes WHERE trajectory_id = $1 ORDER BY occurred_at, created_at, id`
	rows, err := s.db.Query(ctx, q, trajectoryID)
	if err != nil {
		return nil, fmt.Errorf("store: list trajectory nodes for trajectory %q: %w", trajectoryID, mapError(err))
	}
	nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TrajectoryNode, error) {
		return scanTrajectoryNode(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list trajectory nodes for trajectory %q: %w", trajectoryID, mapError(err))
	}
	return nodes, nil
}
