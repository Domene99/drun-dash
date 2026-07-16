package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// inviteTTL is how long an invitation token stays valid (ADR-0008 §6): 7 days.
const inviteTTL = 7 * 24 * time.Hour

// accountStore is the narrow slice of *[store.AccountStore] the local-auth
// handlers use for the multi-table, transactional flows. Naming it as an
// interface lets the handler tests inject an in-memory fake, matching the other
// store seams.
type accountStore interface {
	Signup(ctx context.Context, p store.SignupParams) (store.Member, error)
	InviteMember(ctx context.Context, p store.InviteParams) (store.Member, store.Invitation, error)
	AcceptInvite(ctx context.Context, p store.AcceptInviteParams) (store.Member, error)
}

var newAccountStore = func(q store.Querier) accountStore {
	return store.NewAccountStore(q)
}

// orgResolver is the narrow org-lookup the login handler needs (resolve an org
// by slug so the member can be found by (org_id, lower(email))).
type orgResolver interface {
	GetBySlug(ctx context.Context, slug string) (store.Organization, error)
}

var newOrgResolver = func(q store.Querier) orgResolver {
	return store.NewOrganizationStore(q)
}

// loginMemberStore is the narrow member-lookup the login handler needs: resolve
// a member by (org_id, lower(email)).
type loginMemberStore interface {
	GetByOrgEmail(ctx context.Context, orgID, email string) (store.Member, error)
}

var newLoginMemberStore = func(q store.Querier) loginMemberStore {
	return store.NewMemberStore(q)
}

// signupRequest is the POST /auth/signup body (ADR-0008 §4). org_slug is
// optional — derived from org_name when omitted; name is the owner's display
// name (optional).
type signupRequest struct {
	OrgName  string `json:"org_name"`
	OrgSlug  string `json:"org_slug"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// loginRequest is the POST /auth/login body (ADR-0008 §5). The org slug scopes
// the (org_id, lower(email)) lookup — email is unique per org, not globally.
type loginRequest struct {
	OrgSlug  string `json:"org_slug"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// acceptInviteRequest is the POST /auth/accept-invite body (ADR-0008 §6). token
// is the raw invite token; name is the acceptor's display name (optional).
type acceptInviteRequest struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// inviteRequest is the POST /members/invite body (ADR-0008 §6). role_ids are the
// roles to assign the invited member on acceptance (may be empty).
type inviteRequest struct {
	Email   string   `json:"email"`
	RoleIDs []string `json:"role_ids"`
}

// inviteResponse is the POST /members/invite success body: the pending member,
// plus the invite link and raw token surfaced once (no mailer in v1 — the admin
// hands the link off, ADR-0008 §6) and the token's expiry.
type inviteResponse struct {
	Member      apiMember `json:"member"`
	InviteToken string    `json:"invite_token"`
	InviteLink  string    `json:"invite_link"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// signup implements POST /auth/signup (ADR-0008 §4): the public entry point that
// creates an organization, seeds its built-in roles, and makes the caller its
// Owner, returning a session token so they are immediately signed in. A
// duplicate org slug or email is a generic 409; a bad body or password a 400.
func (d Deps) signup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req signupRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.OrgName == "" {
		WriteValidationError(w, "org_name", "org_name is required")
		return
	}
	if req.Email == "" {
		WriteValidationError(w, "email", "email is required")
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		WriteValidationError(w, "password", msg)
		return
	}

	slug := req.OrgSlug
	if slug == "" {
		slug = deriveSlug(req.OrgName)
	}
	if slug == "" {
		WriteValidationError(w, "org_slug", "could not derive a slug from org_name; provide org_slug")
		return
	}

	hash, err := hashPassword(req.Password)
	if err != nil {
		writeInternalError(w)
		return
	}

	member, err := newAccountStore(d.Pool).Signup(ctx, store.SignupParams{
		OrgName:      req.OrgName,
		OrgSlug:      slug,
		Email:        req.Email,
		DisplayName:  optionalString(req.Name),
		PasswordHash: hash,
	})
	if err != nil {
		// ErrConflict → 409 "resource already exists" (generic: does not say
		// whether the slug or the email was the collision). Anything else → 500.
		WriteStoreError(w, err)
		return
	}
	d.mintSession(w, ctx, member)
}

// login implements POST /auth/login (ADR-0008 §5): resolve the org by slug, the
// member by (org_id, lower(email)), verify the password, confirm the member is
// active, and mint a session. Every failure — unknown org, unknown email, no
// password set, wrong password, inactive — returns the same generic 401 on a
// constant-time path (a dummy bcrypt compare runs even when no member/hash is
// found), so the endpoint reveals nothing about which orgs or members exist.
func (d Deps) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.OrgSlug == "" || req.Email == "" || req.Password == "" {
		// A missing credential can never authenticate; answer with the same
		// generic 401 rather than a field-level 400 that hints at the flow.
		writeUnauthorized(w)
		return
	}

	org, err := newOrgResolver(d.Pool).GetBySlug(ctx, req.OrgSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			verifyPassword(nil, req.Password) // constant-time: match the has-member path
			writeUnauthorized(w)
			return
		}
		writeInternalError(w)
		return
	}

	member, err := newLoginMemberStore(d.Pool).GetByOrgEmail(ctx, org.ID, req.Email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			verifyPassword(nil, req.Password) // constant-time
			writeUnauthorized(w)
			return
		}
		writeInternalError(w)
		return
	}

	if !verifyPassword(member.PasswordHash, req.Password) {
		writeUnauthorized(w)
		return
	}
	if !member.Active {
		writeUnauthorized(w)
		return
	}
	d.mintSession(w, ctx, member)
}

// acceptInvite implements POST /auth/accept-invite (ADR-0008 §6): the public
// endpoint a teammate uses to redeem their invite link. It hashes the presented
// token, and if it maps to an unexpired, unconsumed invitation, activates the
// pending member with the supplied name and password, assigns the invitation's
// pre-selected roles (no self-escalation), consumes the token (single-use), and
// mints a session. An invalid/expired/consumed token is a single generic 401.
func (d Deps) acceptInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req acceptInviteRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.Token == "" {
		WriteValidationError(w, "token", "token is required")
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		WriteValidationError(w, "password", msg)
		return
	}

	hash, err := hashPassword(req.Password)
	if err != nil {
		writeInternalError(w)
		return
	}

	member, err := newAccountStore(d.Pool).AcceptInvite(ctx, store.AcceptInviteParams{
		TokenHash:    hashToken(req.Token),
		DisplayName:  optionalString(req.Name),
		PasswordHash: hash,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Invalid, expired, or already-consumed — reveal nothing.
			writeUnauthorized(w)
			return
		}
		writeInternalError(w)
		return
	}
	d.mintSession(w, ctx, member)
}

// inviteMember implements POST /members/invite (ADR-0008 §6). In the caller's
// org it creates a pending (inactive) member holding the given email and an
// invitation carrying the pre-selected roles, then returns the invite link and
// raw token once (no mailer — the link is surfaced and logged). Requires
// members.manage (mounted with the gate). A duplicate email is a 409; an unknown
// or cross-org role_id is a 404 (ADR-0002).
func (d Deps) inviteMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	var req inviteRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.Email == "" {
		WriteValidationError(w, "email", "email is required")
		return
	}

	// Validate every requested role exists and belongs to the caller's org
	// before any write, deduping. A cross-org or unknown role_id is a 404.
	rs := newRoleStore(d.Pool)
	seen := make(map[string]bool, len(req.RoleIDs))
	roleIDs := make([]string, 0, len(req.RoleIDs))
	for _, roleID := range req.RoleIDs {
		if roleID == "" {
			WriteValidationError(w, "role_ids", "role id must not be empty")
			return
		}
		if seen[roleID] {
			continue
		}
		role, err := rs.GetByID(ctx, roleID)
		if err != nil {
			WriteStoreError(w, err)
			return
		}
		if !SameOrg(claims.OrgID, role.OrgID) {
			WriteNotFound(w, "resource not found")
			return
		}
		seen[roleID] = true
		roleIDs = append(roleIDs, roleID)
	}

	rawToken, tokenHash, err := newInviteToken()
	if err != nil {
		writeInternalError(w)
		return
	}
	expiresAt := time.Now().Add(inviteTTL)

	member, _, err := newAccountStore(d.Pool).InviteMember(ctx, store.InviteParams{
		OrgID:     claims.OrgID,
		Email:     req.Email,
		RoleIDs:   roleIDs,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		// ErrConflict → 409 (the org already has this email). Otherwise 500.
		WriteStoreError(w, err)
		return
	}

	link := d.inviteLink(rawToken)
	// No mailer in v1: surface the link (returned below) and log it so an admin
	// can hand it off (ADR-0008 §6).
	slog.Info("member invitation created",
		slog.String("org_id", claims.OrgID),
		slog.String("member_id", member.ID),
		slog.String("email", req.Email),
		slog.String("invite_link", link))

	WriteCreated(w, inviteResponse{
		Member:      toAPIMember(member, nil),
		InviteToken: rawToken,
		InviteLink:  link,
		ExpiresAt:   expiresAt,
	})
}

// mintSession mints a session token for m and writes the standard TokenResponse
// (200). It is the shared tail of signup/login/accept-invite: the minted token
// is indistinguishable from an OIDC-minted one — same claims, TTL, and
// revocation (ADR-0008 §1). Subject mirrors members.idp_subject (a synthetic
// "local:…" for password members), so /auth/whoami resolves the member by
// (org, sub) exactly as for OIDC.
func (d Deps) mintSession(w http.ResponseWriter, ctx context.Context, m store.Member) {
	token, err := d.Issuer.Mint(ctx, auth.Claims{
		OrgID:            m.OrgID,
		MemberID:         m.ID,
		Roles:            []string{string(m.Role)},
		RegisteredClaims: jwt.RegisteredClaims{Subject: m.IDPSubject},
	})
	if err != nil {
		writeInternalError(w)
		return
	}
	WriteOK(w, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int(d.AccessTTL.Seconds()),
	})
}

// inviteLink builds the accept-invite URL the admin hands off, anchored at the
// configured admin-UI origin. When that origin is unset it degrades to a bare
// path (still usable against the same host).
func (d Deps) inviteLink(token string) string {
	base := strings.TrimRight(d.AdminUIBaseURL, "/")
	return base + "/accept-invite?token=" + url.QueryEscape(token)
}

// newInviteToken returns a high-entropy invite token (32 random bytes, URL-safe
// base64) and its SHA-256 hex hash. Only the hash is persisted; the raw token
// is returned to the caller once (ADR-0008 §6).
func newInviteToken() (raw, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])
	return raw, hashToken(raw), nil
}

// hashToken returns the SHA-256 hex digest of a raw invite token — the form
// stored and looked up. The token is high-entropy random, so a fast hash is
// sufficient (bcrypt is reserved for low-entropy passwords, ADR-0008 §6).
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// deriveSlug turns a human org name into a URL-safe slug: lower-cased, every run
// of non-alphanumeric characters collapsed to a single hyphen, with leading and
// trailing hyphens trimmed. It returns "" when nothing usable remains (the
// caller then requires an explicit org_slug).
func deriveSlug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// optionalString maps an empty string to nil (store NULL) and any other value to
// its address, for the optional display-name fields.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
