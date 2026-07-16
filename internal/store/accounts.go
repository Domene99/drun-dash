package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AccountStore performs the multi-table local-auth writes that ADR-0008 keeps
// atomic: org self-signup (org + built-in roles + Owner member), member
// invitation (pending member + invitation token), and invite acceptance
// (activate member + set password + assign the invited roles + consume the
// token). Each runs in one transaction so a partial failure leaves nothing
// behind — no org without roles, no invited member without an invitation, no
// half-accepted invite. It composes the per-aggregate stores over the same
// pgx.Tx (they all take a [Querier]).
type AccountStore struct {
	db Querier
}

// NewAccountStore returns an AccountStore backed by db (typically the pool; it
// opens its own transaction per operation).
func NewAccountStore(db Querier) *AccountStore {
	return &AccountStore{db: db}
}

func (s *AccountStore) begin(ctx context.Context) (pgx.Tx, error) {
	tb, ok := s.db.(txBeginner)
	if !ok {
		return nil, fmt.Errorf("db does not support transactions")
	}
	return tb.Begin(ctx)
}

// newLocalSubject returns a synthetic, opaque, non-null idp_subject for a
// password-provisioned member (ADR-0008 §3b): the "local:" prefix namespaces it
// away from any real IdP subject, and 16 bytes of randomness make it unique per
// org, so the members (org_id, idp_subject) NOT-NULL unique index stays intact
// without any schema change. RBAC keys off the member id, never this value.
func newLocalSubject() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generate local subject: %w", err)
	}
	return "local:" + hex.EncodeToString(b[:]), nil
}

// SignupParams carries the fields for [AccountStore.Signup]. OrgSlug is the
// resolved slug (the caller derives it from OrgName when omitted); PasswordHash
// is the bcrypt hash the caller already computed for the owner's password.
type SignupParams struct {
	OrgName      string
	OrgSlug      string
	Email        string
	DisplayName  *string
	PasswordHash string
}

// Signup creates a new organization and its founding Owner in one transaction
// (ADR-0008 §4): it inserts the org, seeds the four built-in roles
// ([RoleStore.SeedBuiltins]), creates the first member (active, synthetic local
// subject, the coarse admin role, the supplied password hash), and assigns that
// member the built-in Owner RBAC role — so the org has an Owner from creation
// (the ADR-0006 §7 guardrail holds with no owner-less window). It returns the
// created member. A duplicate org slug or a duplicate email maps to [ErrConflict]
// (the transaction rolls back, leaving no partial org).
func (s *AccountStore) Signup(ctx context.Context, p SignupParams) (Member, error) {
	subject, err := newLocalSubject()
	if err != nil {
		return Member{}, err
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return Member{}, fmt.Errorf("store: signup %q: %w", p.OrgSlug, err)
	}
	defer tx.Rollback(ctx)

	org, err := NewOrganizationStore(tx).Create(ctx, p.OrgSlug, p.OrgName)
	if err != nil {
		return Member{}, fmt.Errorf("store: signup %q: %w", p.OrgSlug, err)
	}

	roles, err := NewRoleStore(tx).SeedBuiltins(ctx, org.ID)
	if err != nil {
		return Member{}, fmt.Errorf("store: signup %q: %w", p.OrgSlug, err)
	}
	owner, ok := BuiltinRoleByName(roles, BuiltinOwner)
	if !ok {
		return Member{}, fmt.Errorf("store: signup %q: seeded roles missing %s", p.OrgSlug, BuiltinOwner)
	}

	hash := p.PasswordHash
	member, err := NewMemberStore(tx).Create(ctx, CreateMemberParams{
		OrgID:        org.ID,
		IDPSubject:   subject,
		Email:        p.Email,
		DisplayName:  p.DisplayName,
		Role:         RoleAdmin,
		PasswordHash: &hash,
	})
	if err != nil {
		return Member{}, fmt.Errorf("store: signup %q: %w", p.OrgSlug, err)
	}

	if _, err := NewMemberRoleStore(tx).AssignRole(ctx, member.ID, owner.ID); err != nil {
		return Member{}, fmt.Errorf("store: signup %q: %w", p.OrgSlug, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Member{}, fmt.Errorf("store: signup %q: %w", p.OrgSlug, mapError(err))
	}
	return member, nil
}

// InviteParams carries the fields for [AccountStore.InviteMember]. TokenHash is
// the SHA-256 hex of the raw invite token; RoleIDs are the roles to assign on
// acceptance (validated by the caller to belong to the org); ExpiresAt is the
// invite's TTL boundary.
type InviteParams struct {
	OrgID     string
	Email     string
	RoleIDs   []string
	TokenHash string
	ExpiresAt time.Time
}

// InviteMember creates a pending member and its invitation in one transaction
// (ADR-0008 §6): the member is inactive (active = false), password-less, with a
// synthetic local subject and the given email, so it holds the org's email slot
// but cannot authenticate until acceptance. The invitation stores only the
// token hash plus the pre-selected roles. It returns the pending member and the
// invitation. A duplicate email in the org maps to [ErrConflict].
func (s *AccountStore) InviteMember(ctx context.Context, p InviteParams) (Member, Invitation, error) {
	subject, err := newLocalSubject()
	if err != nil {
		return Member{}, Invitation{}, err
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return Member{}, Invitation{}, fmt.Errorf("store: invite %q: %w", p.Email, err)
	}
	defer tx.Rollback(ctx)

	inactive := false
	member, err := NewMemberStore(tx).Create(ctx, CreateMemberParams{
		OrgID:      p.OrgID,
		IDPSubject: subject,
		Email:      p.Email,
		Role:       RoleMember,
		Active:     &inactive,
	})
	if err != nil {
		return Member{}, Invitation{}, fmt.Errorf("store: invite %q: %w", p.Email, err)
	}

	inv, err := NewInvitationStore(tx).Create(ctx, CreateInvitationParams{
		OrgID:     p.OrgID,
		MemberID:  member.ID,
		TokenHash: p.TokenHash,
		RoleIDs:   p.RoleIDs,
		ExpiresAt: p.ExpiresAt,
	})
	if err != nil {
		return Member{}, Invitation{}, fmt.Errorf("store: invite %q: %w", p.Email, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Member{}, Invitation{}, fmt.Errorf("store: invite %q: %w", p.Email, mapError(err))
	}
	return member, inv, nil
}

// AcceptInviteParams carries the fields for [AccountStore.AcceptInvite].
// TokenHash is the SHA-256 hex of the presented token; PasswordHash is the
// bcrypt hash of the acceptor's chosen password; DisplayName is optional.
type AcceptInviteParams struct {
	TokenHash    string
	DisplayName  *string
	PasswordHash string
}

// AcceptInvite redeems an invitation in one transaction (ADR-0008 §6): it looks
// the token hash up among the unexpired, unconsumed invitations, then activates
// the pending member, sets its display name and password hash, assigns the
// invitation's pre-selected roles (so a teammate cannot self-escalate — the
// roles come from the invite, not the acceptor), and consumes the token
// (single-use). It returns the now-active member. An unknown, expired, or
// already-consumed token maps to [ErrNotFound], which the handler surfaces as a
// single generic error that reveals nothing.
func (s *AccountStore) AcceptInvite(ctx context.Context, p AcceptInviteParams) (Member, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Member{}, fmt.Errorf("store: accept invite: %w", err)
	}
	defer tx.Rollback(ctx)

	// Only an unexpired, unconsumed invitation is redeemable; anything else is
	// indistinguishable from "unknown token" (ErrNotFound).
	const lookup = `SELECT ` + invitationColumns + `
		FROM invitations
		WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > now()`
	inv, err := scanInvitation(tx.QueryRow(ctx, lookup, p.TokenHash))
	if err != nil {
		return Member{}, fmt.Errorf("store: accept invite: %w", mapError(err))
	}

	const activate = `UPDATE members SET display_name = $2, password_hash = $3, active = TRUE WHERE id = $1`
	tag, err := tx.Exec(ctx, activate, inv.MemberID, p.DisplayName, p.PasswordHash)
	if err != nil {
		return Member{}, fmt.Errorf("store: accept invite: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		// The invitation's member vanished (e.g. deleted); treat as unredeemable.
		return Member{}, fmt.Errorf("store: accept invite: %w", ErrNotFound)
	}

	// Assign the invited roles. ON CONFLICT DO NOTHING keeps it idempotent if a
	// role was somehow already assigned; a role deleted since the invite simply
	// fails its FK and aborts the whole acceptance (rolled back).
	for _, roleID := range inv.RoleIDs {
		const assign = `INSERT INTO member_roles (member_id, role_id) VALUES ($1, $2)
			ON CONFLICT (member_id, role_id) DO NOTHING`
		if _, err := tx.Exec(ctx, assign, inv.MemberID, roleID); err != nil {
			return Member{}, fmt.Errorf("store: accept invite: assign role %q: %w", roleID, mapError(err))
		}
	}

	// Consume last, guarded on consumed_at IS NULL so a concurrent acceptance
	// cannot double-spend the token.
	const consume = `UPDATE invitations SET consumed_at = now() WHERE id = $1 AND consumed_at IS NULL`
	ctag, err := tx.Exec(ctx, consume, inv.ID)
	if err != nil {
		return Member{}, fmt.Errorf("store: accept invite: %w", mapError(err))
	}
	if ctag.RowsAffected() == 0 {
		return Member{}, fmt.Errorf("store: accept invite: %w", ErrNotFound)
	}

	member, err := scanMember(tx.QueryRow(ctx, `SELECT `+memberColumns+` FROM members WHERE id = $1`, inv.MemberID))
	if err != nil {
		return Member{}, fmt.Errorf("store: accept invite: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return Member{}, fmt.Errorf("store: accept invite: %w", mapError(err))
	}
	return member, nil
}
