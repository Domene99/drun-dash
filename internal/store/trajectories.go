package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrajectoryStore reads and writes the trajectories table.
type TrajectoryStore struct {
	db Querier
}

// NewTrajectoryStore returns a TrajectoryStore backed by db.
func NewTrajectoryStore(db Querier) *TrajectoryStore {
	return &TrajectoryStore{db: db}
}

const trajectoryColumns = "id, org_id, session_id, metadata_json, started_at, ended_at, created_at"

func scanTrajectory(row pgx.Row) (Trajectory, error) {
	var t Trajectory
	err := row.Scan(&t.ID, &t.OrgID, &t.SessionID, &t.MetadataJSON, &t.StartedAt, &t.EndedAt, &t.CreatedAt)
	return t, err
}

// CreateTrajectoryParams carries the fields for [TrajectoryStore.Create]. A nil
// SessionID stores NULL (a trajectory can be ingested without a live session).
// A nil MetadataJSON stores the column default ('{}'). A nil StartedAt defaults
// to now(), matching the column default; pass a value to backfill a historical
// start time.
type CreateTrajectoryParams struct {
	OrgID        string
	SessionID    *string
	MetadataJSON json.RawMessage
	StartedAt    *time.Time
}

// Create inserts a trajectory and returns the stored row, letting the database
// assign id and created_at. A missing session (or org) is rejected by the
// foreign key.
func (s *TrajectoryStore) Create(ctx context.Context, p CreateTrajectoryParams) (Trajectory, error) {
	const q = `INSERT INTO trajectories (org_id, session_id, metadata_json, started_at)
		VALUES ($1, $2, COALESCE($3, '{}'::jsonb), COALESCE($4, now())) RETURNING ` + trajectoryColumns
	traj, err := scanTrajectory(s.db.QueryRow(ctx, q, p.OrgID, p.SessionID, p.MetadataJSON, p.StartedAt))
	if err != nil {
		return Trajectory{}, fmt.Errorf("store: create trajectory for org %q: %w", p.OrgID, mapError(err))
	}
	return traj, nil
}

// GetByID returns the trajectory with the given id, or [ErrNotFound].
func (s *TrajectoryStore) GetByID(ctx context.Context, id string) (Trajectory, error) {
	const q = `SELECT ` + trajectoryColumns + ` FROM trajectories WHERE id = $1`
	traj, err := scanTrajectory(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Trajectory{}, fmt.Errorf("store: trajectory by id %q: %w", id, mapError(err))
	}
	return traj, nil
}

// ListByOrg returns every trajectory for orgID, newest first (by started_at
// descending, then id for a stable order).
func (s *TrajectoryStore) ListByOrg(ctx context.Context, orgID string) ([]Trajectory, error) {
	const q = `SELECT ` + trajectoryColumns + ` FROM trajectories WHERE org_id = $1 ORDER BY started_at DESC, id`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list trajectories for org %q: %w", orgID, mapError(err))
	}
	trajs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Trajectory, error) {
		return scanTrajectory(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list trajectories for org %q: %w", orgID, mapError(err))
	}
	return trajs, nil
}

// ListByOrgPage returns up to limit trajectories for orgID ordered by
// (created_at, id), starting strictly after the (afterCreatedAt, afterID)
// position. A zero afterCreatedAt (afterID ignored) starts from the beginning
// of the list. The caller typically passes limit+1 to detect whether another
// page exists and to derive the next cursor from the last kept row; this method
// only runs the seek and returns the rows. It is the keyset-paginated
// counterpart to [ListByOrg] (which orders by started_at and is left unchanged)
// per ADR-0002 §3. Note the key is created_at (the append-only, immutable row
// timestamp), not started_at (which can be backfilled to a historical value).
//
// A non-positive limit is rejected as a programming error: the handler layer is
// the primary bound (parsing and clamping the query's limit to [1, 200]), so a
// limit of zero or below reaching the store is a bug, not a request for an
// empty page.
func (s *TrajectoryStore) ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]Trajectory, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list trajectories page for org %q: limit must be positive, got %d", orgID, limit)
	}
	q := `SELECT ` + trajectoryColumns + ` FROM trajectories WHERE org_id = $1`
	args := []any{orgID, limit}
	if !afterCreatedAt.IsZero() {
		q += ` AND (created_at, id) > ($3, $4)`
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY created_at, id LIMIT $2`
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list trajectories page for org %q: %w", orgID, mapError(err))
	}
	trajs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Trajectory, error) {
		return scanTrajectory(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list trajectories page for org %q: %w", orgID, mapError(err))
	}
	return trajs, nil
}

// SetEnded stamps ended_at with endedAt on the trajectory with the given id and
// returns the updated row. It returns [ErrNotFound] if no such trajectory
// exists.
func (s *TrajectoryStore) SetEnded(ctx context.Context, id string, endedAt time.Time) (Trajectory, error) {
	const q = `UPDATE trajectories SET ended_at = $2 WHERE id = $1 RETURNING ` + trajectoryColumns
	traj, err := scanTrajectory(s.db.QueryRow(ctx, q, id, endedAt))
	if err != nil {
		return Trajectory{}, fmt.Errorf("store: set trajectory %q ended: %w", id, mapError(err))
	}
	return traj, nil
}
