package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These tests exercise the pure error-translation and value logic that needs no
// live database. Anything requiring a real Postgres lives in
// integration_test.go, behind the `integration` build tag.

func TestMapError(t *testing.T) {
	uniqueViolation := &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: "organizations_slug_key"}
	otherPgErr := &pgconn.PgError{Code: "23503", ConstraintName: "environments_org_id_fkey"}

	tests := []struct {
		name       string
		in         error
		wantNil    bool
		wantTarget error // errors.Is target, when non-nil
		wantSame   error // exact identity, when non-nil
	}{
		{name: "nil passes through", in: nil, wantNil: true},
		{name: "no rows becomes not found", in: pgx.ErrNoRows, wantTarget: ErrNotFound},
		{name: "wrapped no rows becomes not found", in: fmt.Errorf("scan: %w", pgx.ErrNoRows), wantTarget: ErrNotFound},
		{name: "unique violation becomes conflict", in: uniqueViolation, wantTarget: ErrConflict},
		{name: "wrapped unique violation becomes conflict", in: fmt.Errorf("exec: %w", uniqueViolation), wantTarget: ErrConflict},
		{name: "other pg error passes through", in: otherPgErr, wantSame: otherPgErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapError(tt.in)
			switch {
			case tt.wantNil:
				if got != nil {
					t.Fatalf("mapError(%v) = %v, want nil", tt.in, got)
				}
			case tt.wantSame != nil:
				if !errors.Is(got, tt.wantSame) {
					t.Fatalf("mapError(%v) = %v, want %v", tt.in, got, tt.wantSame)
				}
			case tt.wantTarget != nil:
				if !errors.Is(got, tt.wantTarget) {
					t.Fatalf("mapError(%v) = %v, want errors.Is %v", tt.in, got, tt.wantTarget)
				}
			}
		})
	}
}

// The conflict error must name the violated constraint to aid diagnostics.
func TestMapErrorConflictNamesConstraint(t *testing.T) {
	err := mapError(&pgconn.PgError{Code: pgUniqueViolation, ConstraintName: "teams_org_id_name_key"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "teams_org_id_name_key") {
		t.Fatalf("conflict error %q does not name the constraint", err.Error())
	}
}

func TestRoleValid(t *testing.T) {
	valid := []Role{RoleAdmin, RoleMember, RoleAuditor}
	for _, r := range valid {
		if !r.Valid() {
			t.Errorf("Role(%q).Valid() = false, want true", r)
		}
	}
	invalid := []Role{"", "owner", "Admin", "MEMBER"}
	for _, r := range invalid {
		if r.Valid() {
			t.Errorf("Role(%q).Valid() = true, want false", r)
		}
	}
}
