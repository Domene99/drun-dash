package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// OrganizationStore reads and writes the organizations table.
type OrganizationStore struct {
	db Querier
}

// NewOrganizationStore returns an OrganizationStore backed by db, which may be
// a *db.Pool or any transaction satisfying [Querier].
func NewOrganizationStore(db Querier) *OrganizationStore {
	return &OrganizationStore{db: db}
}

// orgColumns is the SELECT/RETURNING column list, in the order scanOrganization
// expects.
const orgColumns = "id, slug, name, created_at"

func scanOrganization(row pgx.Row) (Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt)
	return o, err
}

// Create inserts an organization with the given slug and name, letting the
// database assign id and created_at, and returns the stored row. A duplicate
// slug yields [ErrConflict].
func (s *OrganizationStore) Create(ctx context.Context, slug, name string) (Organization, error) {
	const q = `INSERT INTO organizations (slug, name) VALUES ($1, $2) RETURNING ` + orgColumns
	org, err := scanOrganization(s.db.QueryRow(ctx, q, slug, name))
	if err != nil {
		return Organization{}, fmt.Errorf("store: create organization %q: %w", slug, mapError(err))
	}
	return org, nil
}

// GetByID returns the organization with the given id, or [ErrNotFound].
func (s *OrganizationStore) GetByID(ctx context.Context, id string) (Organization, error) {
	const q = `SELECT ` + orgColumns + ` FROM organizations WHERE id = $1`
	org, err := scanOrganization(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Organization{}, fmt.Errorf("store: organization by id %q: %w", id, mapError(err))
	}
	return org, nil
}

// GetBySlug returns the organization with the given slug, or [ErrNotFound].
func (s *OrganizationStore) GetBySlug(ctx context.Context, slug string) (Organization, error) {
	const q = `SELECT ` + orgColumns + ` FROM organizations WHERE slug = $1`
	org, err := scanOrganization(s.db.QueryRow(ctx, q, slug))
	if err != nil {
		return Organization{}, fmt.Errorf("store: organization by slug %q: %w", slug, mapError(err))
	}
	return org, nil
}

// List returns every organization ordered by creation time (then id, to keep
// the order stable for rows sharing a timestamp).
func (s *OrganizationStore) List(ctx context.Context) ([]Organization, error) {
	const q = `SELECT ` + orgColumns + ` FROM organizations ORDER BY created_at, id`
	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list organizations: %w", mapError(err))
	}
	orgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Organization, error) {
		return scanOrganization(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list organizations: %w", mapError(err))
	}
	return orgs, nil
}
