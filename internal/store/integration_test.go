//go:build integration

// The store integration tests exercise the tenancy + identity repositories
// against a real Postgres 16 instance. They are excluded from the default build
// and only compile/run under the `integration` build tag:
//
//	go test -tags integration ./internal/store/...
//
// Requirements (mirroring internal/db/integration_test.go):
//   - A reachable Postgres. By default they use config.DefaultDatabaseURL (the
//     local Docker dev stack in deploy/docker); set DATABASE_URL to override.
//   - The repository migrations directory. By default it is resolved relative
//     to this package (../../migrations); set MIGRATIONS_PATH to override.
//
// If Postgres is unreachable the tests SKIP rather than fail, so a machine with
// no database does not produce a red build. Each test creates its own throwaway
// organization and deletes it on cleanup; deletion cascades to every dependent
// row, so tests do not interfere with one another or leave residue behind.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/db"
)

func databaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return config.DefaultDatabaseURL
}

func migrationsPath() string {
	if v := os.Getenv("MIGRATIONS_PATH"); v != "" {
		return v
	}
	return "../../migrations"
}

// uniqueSeq guards against name collisions when two rows are created within the
// same nanosecond.
var uniqueSeq atomic.Uint64

// uniq returns a process-unique identifier with the given prefix, suitable for
// slugs, names, and idp subjects that carry unique constraints.
func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), uniqueSeq.Add(1))
}

// requirePool connects, pings (skipping if Postgres is unreachable), and
// applies every migration so the schema is present.
func requirePool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := db.Connect(ctx, &config.Config{DatabaseURL: databaseURL()})
	if err != nil {
		t.Fatalf("db.Connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable at %s (%v); start it and rerun with -tags integration", databaseURL(), err)
	}
	if err := db.Migrate(databaseURL(), migrationsPath()); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return pool, ctx
}

// newOrg creates a throwaway organization and registers cleanup that deletes it
// (cascading to all dependent rows).
func newOrg(t *testing.T, ctx context.Context, pool *db.Pool) Organization {
	t.Helper()
	org, err := NewOrganizationStore(pool).Create(ctx, uniq("org"), "Test Org")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, org.ID)
	})
	return org
}

func TestIntegrationOrganizations(t *testing.T) {
	pool, ctx := requirePool(t)
	orgs := NewOrganizationStore(pool)

	slug := uniq("acme")
	created, err := orgs.Create(ctx, slug, "Acme Inc")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, created.ID)
	})
	if created.ID == "" || created.Slug != slug || created.Name != "Acme Inc" || created.CreatedAt.IsZero() {
		t.Fatalf("Create returned incomplete row: %+v", created)
	}

	byID, err := orgs.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if byID != created {
		t.Fatalf("GetByID = %+v, want %+v", byID, created)
	}

	bySlug, err := orgs.GetBySlug(ctx, slug)
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if bySlug != created {
		t.Fatalf("GetBySlug = %+v, want %+v", bySlug, created)
	}

	list, err := orgs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsOrg(list, created.ID) {
		t.Fatalf("List does not contain created org %s", created.ID)
	}

	// Duplicate slug -> ErrConflict.
	if _, err := orgs.Create(ctx, slug, "Dupe"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate slug Create error = %v, want ErrConflict", err)
	}

	// Missing lookups -> ErrNotFound.
	if _, err := orgs.GetByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if _, err := orgs.GetBySlug(ctx, uniq("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBySlug(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationEnvironments(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	envs := NewEnvironmentStore(pool)

	prod, err := envs.Create(ctx, org.ID, "prod")
	if err != nil {
		t.Fatalf("Create prod: %v", err)
	}
	if _, err := envs.Create(ctx, org.ID, "staging"); err != nil {
		t.Fatalf("Create staging: %v", err)
	}

	got, err := envs.GetByID(ctx, prod.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != prod {
		t.Fatalf("GetByID = %+v, want %+v", got, prod)
	}

	list, err := envs.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].Name != "prod" || list[1].Name != "staging" {
		t.Fatalf("ListByOrg = %+v, want [prod staging] ordered by name", list)
	}

	// Duplicate (org_id, name) -> ErrConflict.
	if _, err := envs.Create(ctx, org.ID, "prod"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate env Create error = %v, want ErrConflict", err)
	}
	if _, err := envs.GetByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationTeams(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	teams := NewTeamStore(pool)

	platform, err := teams.Create(ctx, org.ID, "platform")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := teams.Create(ctx, org.ID, "research"); err != nil {
		t.Fatalf("Create research: %v", err)
	}

	got, err := teams.GetByID(ctx, platform.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != platform {
		t.Fatalf("GetByID = %+v, want %+v", got, platform)
	}

	list, err := teams.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].Name != "platform" || list[1].Name != "research" {
		t.Fatalf("ListByOrg = %+v, want [platform research] ordered by name", list)
	}

	if _, err := teams.Create(ctx, org.ID, "platform"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate team Create error = %v, want ErrConflict", err)
	}
	if _, err := teams.GetByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationMembers(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	members := NewMemberStore(pool)

	sub := uniq("idp")
	display := "Ada Lovelace"
	created, err := members.Create(ctx, CreateMemberParams{
		OrgID:       org.ID,
		IDPSubject:  sub,
		Email:       "ada@example.com",
		DisplayName: &display,
		Role:        RoleAdmin,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Role != RoleAdmin || !created.Active || created.DisplayName == nil || *created.DisplayName != display {
		t.Fatalf("Create returned unexpected row: %+v", created)
	}

	// Empty role defaults to member; nil display name stored as NULL.
	defaulted, err := members.Create(ctx, CreateMemberParams{
		OrgID:      org.ID,
		IDPSubject: uniq("idp"),
		Email:      "grace@example.com",
	})
	if err != nil {
		t.Fatalf("Create defaulted: %v", err)
	}
	if defaulted.Role != RoleMember {
		t.Fatalf("defaulted role = %q, want %q", defaulted.Role, RoleMember)
	}
	if defaulted.DisplayName != nil {
		t.Fatalf("defaulted display name = %v, want nil", *defaulted.DisplayName)
	}

	byID, err := members.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if byID.ID != created.ID {
		t.Fatalf("GetByID = %+v, want id %s", byID, created.ID)
	}

	bySub, err := members.GetByIDPSubject(ctx, org.ID, sub)
	if err != nil {
		t.Fatalf("GetByIDPSubject: %v", err)
	}
	if bySub.ID != created.ID {
		t.Fatalf("GetByIDPSubject = %+v, want id %s", bySub, created.ID)
	}

	list, err := members.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListByOrg len = %d, want 2", len(list))
	}

	deactivated, err := members.SetActive(ctx, created.ID, false)
	if err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if deactivated.Active {
		t.Fatal("SetActive(false) left member active")
	}

	// Duplicate (org_id, idp_subject) -> ErrConflict.
	if _, err := members.Create(ctx, CreateMemberParams{OrgID: org.ID, IDPSubject: sub, Email: "dupe@example.com"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate member Create error = %v, want ErrConflict", err)
	}
	// Missing targets -> ErrNotFound.
	if _, err := members.GetByIDPSubject(ctx, org.ID, uniq("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByIDPSubject(missing) = %v, want ErrNotFound", err)
	}
	if _, err := members.SetActive(ctx, "00000000-0000-0000-0000-000000000000", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetActive(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationTeamMemberships(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)

	team, err := NewTeamStore(pool).Create(ctx, org.ID, "platform")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	member, err := NewMemberStore(pool).Create(ctx, CreateMemberParams{
		OrgID: org.ID, IDPSubject: uniq("idp"), Email: "linus@example.com",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	memberships := NewTeamMembershipStore(pool)

	tm, err := memberships.Add(ctx, team.ID, member.ID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if tm.TeamID != team.ID || tm.MemberID != member.ID || tm.AddedAt.IsZero() {
		t.Fatalf("Add returned incomplete row: %+v", tm)
	}

	// Duplicate membership -> ErrConflict.
	if _, err := memberships.Add(ctx, team.ID, member.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Add error = %v, want ErrConflict", err)
	}

	gotMembers, err := memberships.ListMembers(ctx, team.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(gotMembers) != 1 || gotMembers[0].ID != member.ID {
		t.Fatalf("ListMembers = %+v, want [%s]", gotMembers, member.ID)
	}

	gotTeams, err := memberships.ListTeams(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(gotTeams) != 1 || gotTeams[0].ID != team.ID {
		t.Fatalf("ListTeams = %+v, want [%s]", gotTeams, team.ID)
	}

	if err := memberships.Remove(ctx, team.ID, member.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Removing again -> ErrNotFound.
	if err := memberships.Remove(ctx, team.ID, member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove(absent) = %v, want ErrNotFound", err)
	}

	// Lists are now empty, not errors.
	empty, err := memberships.ListMembers(ctx, team.ID)
	if err != nil {
		t.Fatalf("ListMembers after remove: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("ListMembers after remove = %+v, want empty", empty)
	}
}

func containsOrg(orgs []Organization, id string) bool {
	for _, o := range orgs {
		if o.ID == id {
			return true
		}
	}
	return false
}
