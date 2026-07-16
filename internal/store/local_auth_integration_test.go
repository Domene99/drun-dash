//go:build integration

// Integration coverage for the local-auth repositories (ADR-0008): the
// password/email additions to MemberStore, the InvitationStore, and the
// transactional AccountStore (signup / invite / accept). Mirrors the other
// *_integration_test.go files — shares requirePool / newOrg / uniq / seedBuiltins
// and skips when Postgres is unreachable; each test hangs its rows off a
// throwaway org whose cleanup cascades.
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIntegrationSignup(t *testing.T) {
	pool, ctx := requirePool(t)
	accounts := NewAccountStore(pool)

	slug := uniq("acme")
	name := "Ada Owner"
	owner, err := accounts.Signup(ctx, SignupParams{
		OrgName:      "Acme Inc",
		OrgSlug:      slug,
		Email:        "Owner@Example.com",
		DisplayName:  &name,
		PasswordHash: "hash-owner",
	})
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, owner.OrgID)
	})

	if owner.OrgID == "" || !owner.Active {
		t.Fatalf("Signup returned unexpected owner: %+v", owner)
	}
	if owner.Role != RoleAdmin {
		t.Fatalf("owner coarse role = %q, want %q", owner.Role, RoleAdmin)
	}
	if owner.PasswordHash == nil || *owner.PasswordHash != "hash-owner" {
		t.Fatalf("owner password hash = %v, want hash-owner", owner.PasswordHash)
	}
	if !strings.HasPrefix(owner.IDPSubject, "local:") {
		t.Fatalf("owner idp_subject = %q, want a local: synthetic subject", owner.IDPSubject)
	}

	// The four built-in roles were seeded for the new org.
	roles, err := NewRoleStore(pool).ListByOrg(ctx, owner.OrgID)
	if err != nil {
		t.Fatalf("ListByOrg roles: %v", err)
	}
	if len(roles) != len(BuiltinRoles) {
		t.Fatalf("seeded roles = %d, want %d", len(roles), len(BuiltinRoles))
	}

	// The owner holds the built-in Owner role.
	held, err := NewMemberRoleStore(pool).ListRolesForMember(ctx, owner.ID)
	if err != nil {
		t.Fatalf("ListRolesForMember: %v", err)
	}
	var hasOwner bool
	for _, r := range held {
		if r.Builtin && r.Name == BuiltinOwner {
			hasOwner = true
		}
	}
	if !hasOwner {
		t.Fatalf("owner does not hold the built-in Owner role: %+v", held)
	}

	// Duplicate slug → ErrConflict (and the transaction leaves no partial org).
	if _, err := accounts.Signup(ctx, SignupParams{OrgName: "Dup", OrgSlug: slug, Email: "x@example.com", PasswordHash: "h"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate-slug Signup = %v, want ErrConflict", err)
	}
}

func TestIntegrationMemberPassword(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	members := NewMemberStore(pool)

	hash := "hash-1"
	created, err := members.Create(ctx, CreateMemberParams{
		OrgID:        org.ID,
		IDPSubject:   "local:" + uniq("s"),
		Email:        "Ada@Example.com",
		PasswordHash: &hash,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.PasswordHash == nil || *created.PasswordHash != hash {
		t.Fatalf("created password hash = %v, want %q", created.PasswordHash, hash)
	}

	// GetByOrgEmail matches case-insensitively on lower(email).
	got, err := members.GetByOrgEmail(ctx, org.ID, "ada@example.com")
	if err != nil {
		t.Fatalf("GetByOrgEmail: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("GetByOrgEmail = %s, want %s", got.ID, created.ID)
	}

	// Email is unique per org, case-insensitively — a differing-case duplicate
	// is a conflict.
	if _, err := members.Create(ctx, CreateMemberParams{OrgID: org.ID, IDPSubject: "local:" + uniq("s2"), Email: "ADA@example.com"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate-email Create = %v, want ErrConflict", err)
	}

	// SetPasswordHash replaces the stored hash.
	if err := members.SetPasswordHash(ctx, created.ID, "hash-2"); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	reloaded, err := members.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if reloaded.PasswordHash == nil || *reloaded.PasswordHash != "hash-2" {
		t.Fatalf("reloaded password hash = %v, want hash-2", reloaded.PasswordHash)
	}

	// Missing lookups / updates → ErrNotFound.
	if _, err := members.GetByOrgEmail(ctx, org.ID, "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByOrgEmail(missing) = %v, want ErrNotFound", err)
	}
	if err := members.SetPasswordHash(ctx, "00000000-0000-0000-0000-000000000000", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetPasswordHash(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationInvitations(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	memberRole := builtins[BuiltinMember]

	accounts := NewAccountStore(pool)
	invitations := NewInvitationStore(pool)

	// Invite creates a pending (inactive) member and the invitation.
	member, inv, err := accounts.InviteMember(ctx, InviteParams{
		OrgID:     org.ID,
		Email:     "invitee@example.com",
		RoleIDs:   []string{memberRole.ID},
		TokenHash: "token-hash-1",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	if member.Active {
		t.Fatalf("invited member is active, want pending/inactive")
	}
	if member.PasswordHash != nil {
		t.Fatalf("invited member has a password hash, want nil")
	}
	if inv.MemberID != member.ID || len(inv.RoleIDs) != 1 || inv.RoleIDs[0] != memberRole.ID {
		t.Fatalf("invitation = %+v, want member %s with role %s", inv, member.ID, memberRole.ID)
	}

	// GetByTokenHash resolves it; ListPendingByOrg surfaces it.
	got, err := invitations.GetByTokenHash(ctx, "token-hash-1")
	if err != nil || got.ID != inv.ID {
		t.Fatalf("GetByTokenHash = (%+v, %v), want id %s", got, err, inv.ID)
	}
	pending, err := invitations.ListPendingByOrg(ctx, org.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingByOrg = (%d, %v), want 1", len(pending), err)
	}

	// A second invite to the same email is a conflict (the org already holds it).
	if _, _, err := accounts.InviteMember(ctx, InviteParams{OrgID: org.ID, Email: "invitee@example.com", TokenHash: "token-hash-x", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate-email InviteMember = %v, want ErrConflict", err)
	}

	// Accept activates the member, sets the password, and assigns the roles.
	name := "Invited User"
	accepted, err := accounts.AcceptInvite(ctx, AcceptInviteParams{TokenHash: "token-hash-1", DisplayName: &name, PasswordHash: "pw-hash"})
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if accepted.ID != member.ID || !accepted.Active {
		t.Fatalf("accepted member = %+v, want active id %s", accepted, member.ID)
	}
	if accepted.PasswordHash == nil || *accepted.PasswordHash != "pw-hash" {
		t.Fatalf("accepted password hash = %v, want pw-hash", accepted.PasswordHash)
	}
	held, err := NewMemberRoleStore(pool).ListRolesForMember(ctx, accepted.ID)
	if err != nil {
		t.Fatalf("ListRolesForMember: %v", err)
	}
	if len(held) != 1 || held[0].ID != memberRole.ID {
		t.Fatalf("assigned roles = %+v, want [%s]", held, memberRole.ID)
	}

	// The token is single-use: a second acceptance fails.
	if _, err := accounts.AcceptInvite(ctx, AcceptInviteParams{TokenHash: "token-hash-1", PasswordHash: "pw2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second AcceptInvite = %v, want ErrNotFound", err)
	}

	// Consume is likewise single-use.
	_, inv2, err := accounts.InviteMember(ctx, InviteParams{OrgID: org.ID, Email: "invitee2@example.com", TokenHash: "token-hash-2", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("InviteMember 2: %v", err)
	}
	if err := invitations.Consume(ctx, inv2.ID); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := invitations.Consume(ctx, inv2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Consume = %v, want ErrNotFound", err)
	}

	// An expired invitation is not redeemable.
	if _, _, err := accounts.InviteMember(ctx, InviteParams{OrgID: org.ID, Email: "invitee3@example.com", TokenHash: "token-hash-3", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("InviteMember 3: %v", err)
	}
	if _, err := accounts.AcceptInvite(ctx, AcceptInviteParams{TokenHash: "token-hash-3", PasswordHash: "pw3"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired AcceptInvite = %v, want ErrNotFound", err)
	}
}
