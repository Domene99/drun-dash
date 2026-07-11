package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
)

// Device-authorization flow constants (RFC 8628, ADR-0001 §2).
const (
	// deviceGrantType is the only grant_type accepted by POST
	// /auth/device/token.
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// deviceCodeTTL is the lifetime of a pending authorization; Redis expires
	// the record automatically, so there is no reaper job. Reported verbatim
	// as expires_in.
	deviceCodeTTL       = 600 * time.Second
	deviceCodeExpiresIn = 600
	// devicePollInterval is the minimum polling interval handed to the CLI and
	// the initial effective interval enforced by slow_down.
	devicePollInterval = 5
	// slowDownStep is how much the effective interval is raised each time the
	// CLI polls faster than the current interval (RFC 8628 §3.5).
	slowDownStep = 5

	// Redis key prefixes. The device record is keyed by the SHA-256 of the
	// opaque device_code so the raw code is never stored; a second key maps the
	// user_code back to the device_code for the browser approval step.
	deviceKeyPrefix   = "authz:device:"
	userCodeKeyPrefix = "authz:user_code:"

	// Authorization states stored on the record.
	deviceStatePending  = "pending"
	deviceStateApproved = "approved"
	deviceStateDenied   = "denied"

	// userCodeAlphabet is an ambiguity-free alphabet (no vowels — so no
	// accidental words — and none of the visually confusable 0/O/1/I/L glyphs).
	// The user code is two four-character groups, formatted XXXX-XXXX.
	userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
	userCodeGroupLen = 4
	userCodeGroups   = 2
)

// DeviceStore is the narrow slice of the Redis command surface the
// device-authorization handlers use. *[github.com/Domene99/drun-dash/internal/redis.Client]
// (which embeds a *go-redis Client) satisfies it directly; tests inject an
// in-memory fake. Keeping it an interface — rather than the concrete client —
// lets the device vertical be exercised without a live Redis, matching how the
// auth handlers depend on the narrow MemberReader.
type DeviceStore interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	TTL(ctx context.Context, key string) *redis.DurationCmd
}

// deviceRecord is the pending-authorization state stored in Redis under
// authz:device:<sha256hex(device_code)> (ADR-0001 §2). It is JSON-encoded.
type deviceRecord struct {
	// State is one of pending|approved|denied.
	State string `json:"state"`
	// UserCode is the short human-typeable code bound to this record; it is
	// also the (unhashed) key suffix of the authz:user_code:<user_code> lookup.
	UserCode string `json:"user_code"`
	// ClientID / Scope are the CLI's requested client and scopes, echoed from
	// the code request.
	ClientID string `json:"client_id"`
	Scope    string `json:"scope,omitempty"`
	// LastPollUnix is the unix time of the last accepted token poll; it drives
	// slow_down enforcement. Zero at creation so the CLI's first poll is never
	// throttled.
	LastPollUnix int64 `json:"last_poll_unix"`
	// Interval is the current effective polling interval in seconds. It starts
	// at devicePollInterval and is raised by slowDownStep each time the CLI
	// polls too fast. Persisted so the raise is cumulative across polls.
	Interval int `json:"interval"`
	// Subject/MemberID/OrgID/Roles are bound from the approving member's
	// principal once the request is approved; empty while pending/denied.
	// Subject mirrors members.idp_subject so the minted session token resolves
	// the same Member as /auth/session (ADR-0001 §3).
	Subject  string   `json:"sub,omitempty"`
	MemberID string   `json:"member_id"`
	OrgID    string   `json:"org_id"`
	Roles    []string `json:"roles"`
}

// deviceAuthorizationRequest is the POST /auth/device/code body
// (openapi/v1.yaml #/components/schemas/DeviceAuthorizationRequest).
type deviceAuthorizationRequest struct {
	ClientID string `json:"client_id"`
	Scope    string `json:"scope,omitempty"`
}

// deviceAuthorizationResponse is the POST /auth/device/code success body
// (openapi/v1.yaml #/components/schemas/DeviceAuthorizationResponse).
type deviceAuthorizationResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceTokenRequest is the POST /auth/device/token body
// (openapi/v1.yaml #/components/schemas/DeviceTokenRequest).
type deviceTokenRequest struct {
	ClientID   string `json:"client_id,omitempty"`
	DeviceCode string `json:"device_code"`
	GrantType  string `json:"grant_type"`
}

// deviceApproveRequest is the POST /auth/device/approve body. This endpoint is
// an admin-UI→control-plane extension and is not part of openapi/v1.yaml; see
// internal/httpapi/README.md.
type deviceApproveRequest struct {
	UserCode string `json:"user_code"`
	Decision string `json:"decision"`
}

// deviceApproveResponse acknowledges an approval decision with the resulting
// record state.
type deviceApproveResponse struct {
	UserCode string `json:"user_code"`
	State    string `json:"state"`
}

// oauthError is the RFC 6749 / RFC 8628 error envelope returned by the device
// token endpoint (openapi/v1.yaml #/components/schemas/OAuthError). It is a
// distinct shape from the control-plane's flat Error envelope, matching the
// contract's documented OAuth surface.
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// writeOAuthError writes an OAuthError body with the given status (always 400
// for the RFC 8628 poll errors).
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	_ = httpx.WriteJSON(w, status, oauthError{Error: code, ErrorDescription: description})
}

// startDeviceAuthorization implements POST /auth/device/code (operationId
// startDeviceAuthorization), the public entry point of the device flow. It
// mints an opaque device_code and a short user_code, records a pending
// authorization in Redis under both keys with a 600s TTL, and returns the
// verification URIs the CLI prints for the user.
func (d Deps) startDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	var req deviceAuthorizationRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.ClientID == "" {
		WriteValidationError(w, "client_id", "client_id is required")
		return
	}

	deviceCode, err := randomDeviceCode()
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}
	userCode, err := generateUserCode()
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	rec := deviceRecord{
		State:        deviceStatePending,
		UserCode:     userCode,
		ClientID:     req.ClientID,
		Scope:        req.Scope,
		LastPollUnix: 0,
		Interval:     devicePollInterval,
		Roles:        []string{},
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	ctx := r.Context()
	if err := d.Redis.Set(ctx, deviceKeyName(deviceCode), string(payload), deviceCodeTTL).Err(); err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}
	if err := d.Redis.Set(ctx, userCodeKeyName(userCode), deviceCode, deviceCodeTTL).Err(); err != nil {
		// The device key is already written; it self-expires via its TTL, so a
		// failure here just means this attempt cannot be approved. Report a
		// clean 500 and let the CLI retry.
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	verificationURI := d.AdminUIBaseURL + "/device"
	resp := deviceAuthorizationResponse{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: verificationURI + "?user_code=" + url.QueryEscape(userCode),
		ExpiresIn:               deviceCodeExpiresIn,
		Interval:                devicePollInterval,
	}
	WriteOK(w, resp)
}

// pollDeviceToken implements POST /auth/device/token (operationId
// pollDeviceToken), the public polling endpoint. It maps the record state onto
// the RFC 8628 error codes and, once the request is approved, mints the session
// JWT and deletes the record so the code is single-use.
func (d Deps) pollDeviceToken(w http.ResponseWriter, r *http.Request) {
	var req deviceTokenRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.GrantType != deviceGrantType {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"grant_type must be "+deviceGrantType)
		return
	}
	if req.DeviceCode == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "device_code is required")
		return
	}

	ctx := r.Context()
	key := deviceKeyName(req.DeviceCode)
	rec, ttl, err := d.loadDeviceRecord(ctx, key)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// Not found or TTL-expired: the code is no longer valid.
			writeOAuthError(w, http.StatusBadRequest, "expired_token", "the device_code has expired or is unknown")
			return
		}
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	now := time.Now().Unix()
	// Rate-limit: a poll that arrives sooner than the effective interval is a
	// slow_down. The first poll (LastPollUnix == 0) is always allowed.
	if rec.LastPollUnix != 0 && now-rec.LastPollUnix < int64(rec.Interval) {
		rec.LastPollUnix = now
		rec.Interval += slowDownStep
		d.saveDeviceRecord(ctx, key, rec, ttl)
		writeOAuthError(w, http.StatusBadRequest, "slow_down", "polling too frequently")
		return
	}

	switch rec.State {
	case deviceStateApproved:
		token, err := d.Issuer.Mint(ctx, auth.Claims{
			OrgID:            rec.OrgID,
			MemberID:         rec.MemberID,
			Roles:            rec.Roles,
			RegisteredClaims: jwt.RegisteredClaims{Subject: rec.Subject},
		})
		if err != nil {
			_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
			return
		}
		// Single-use: drop both keys so the code cannot be replayed.
		_ = d.Redis.Del(ctx, key, userCodeKeyName(rec.UserCode)).Err()
		WriteOK(w, tokenResponse{
			AccessToken: token,
			TokenType:   "Bearer",
			ExpiresIn:   int(d.AccessTTL.Seconds()),
		})
	case deviceStateDenied:
		writeOAuthError(w, http.StatusBadRequest, "access_denied", "the request was denied")
	default: // pending
		// Record this poll so the next one is measured against it.
		rec.LastPollUnix = now
		d.saveDeviceRecord(ctx, key, rec, ttl)
		writeOAuthError(w, http.StatusBadRequest, "authorization_pending", "the request is pending approval")
	}
}

// approveDevice implements POST /auth/device/approve, the protected
// admin-UI→control-plane extension (behind RequireAuth). It resolves the
// user_code the member typed to its device record and binds the approving
// principal (or marks it denied), re-storing the record under its remaining
// TTL. It is intentionally not part of openapi/v1.yaml (documented in
// internal/httpapi/README.md).
func (d Deps) approveDevice(w http.ResponseWriter, r *http.Request) {
	principal := authmw.MustPrincipal(r.Context())

	var req deviceApproveRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.UserCode == "" {
		WriteValidationError(w, "user_code", "user_code is required")
		return
	}
	if req.Decision != "approve" && req.Decision != "deny" {
		WriteValidationError(w, "decision", "decision must be \"approve\" or \"deny\"")
		return
	}

	ctx := r.Context()
	deviceCode, err := d.Redis.Get(ctx, userCodeKeyName(req.UserCode)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			WriteNotFound(w, "unknown or expired user_code")
			return
		}
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	key := deviceKeyName(deviceCode)
	rec, ttl, err := d.loadDeviceRecord(ctx, key)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			WriteNotFound(w, "unknown or expired user_code")
			return
		}
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	if req.Decision == "approve" {
		rec.State = deviceStateApproved
		rec.Subject = principal.Subject
		rec.MemberID = principal.MemberID
		rec.OrgID = principal.OrgID
		rec.Roles = principal.Roles
		if rec.Roles == nil {
			rec.Roles = []string{}
		}
	} else {
		rec.State = deviceStateDenied
	}

	if err := d.saveDeviceRecord(ctx, key, rec, ttl); err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	WriteOK(w, deviceApproveResponse{UserCode: req.UserCode, State: rec.State})
}

// loadDeviceRecord fetches and decodes the record at key along with its
// remaining TTL. A missing or TTL-expired key returns redis.Nil so callers can
// map it to expired_token / 404.
func (d Deps) loadDeviceRecord(ctx context.Context, key string) (deviceRecord, time.Duration, error) {
	raw, err := d.Redis.Get(ctx, key).Result()
	if err != nil {
		return deviceRecord{}, 0, err
	}
	ttl, err := d.Redis.TTL(ctx, key).Result()
	if err != nil {
		return deviceRecord{}, 0, err
	}
	// TTL returns -2 for a missing key and -1 for a key with no expiry; neither
	// should happen after a successful Get on a key we always write with a TTL.
	// Fall back to the full lifetime so a re-Set never drops the expiry.
	if ttl <= 0 {
		ttl = deviceCodeTTL
	}
	var rec deviceRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return deviceRecord{}, 0, err
	}
	return rec, ttl, nil
}

// saveDeviceRecord re-encodes rec and writes it back under key with the given
// TTL, preserving the record's original expiry. The error is returned for the
// approval path (which must report a failure); the poll path treats a failed
// bookkeeping write as best-effort.
func (d Deps) saveDeviceRecord(ctx context.Context, key string, rec deviceRecord, ttl time.Duration) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return d.Redis.Set(ctx, key, string(payload), ttl).Err()
}

// deviceKeyName returns the Redis key for a device_code: the SHA-256 hex of the
// opaque code, so the raw code is never stored.
func deviceKeyName(deviceCode string) string {
	sum := sha256.Sum256([]byte(deviceCode))
	return deviceKeyPrefix + hex.EncodeToString(sum[:])
}

// userCodeKeyName returns the Redis key that maps a user_code to its
// device_code.
func userCodeKeyName(userCode string) string {
	return userCodeKeyPrefix + userCode
}

// randomDeviceCode returns 32 bytes of cryptographic randomness encoded as an
// unpadded URL-safe base64 string — the opaque device_code the CLI holds.
func randomDeviceCode() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// generateUserCode returns a short, human-typeable code drawn from
// userCodeAlphabet, formatted as XXXX-XXXX. Characters are sampled with
// rejection sampling so the ambiguity-free alphabet stays uniformly
// distributed (no modulo bias).
func generateUserCode() (string, error) {
	n := userCodeGroupLen * userCodeGroups
	chars := make([]byte, 0, n+userCodeGroups-1)
	for i := 0; i < n; i++ {
		if i > 0 && i%userCodeGroupLen == 0 {
			chars = append(chars, '-')
		}
		c, err := randomAlphabetChar()
		if err != nil {
			return "", err
		}
		chars = append(chars, c)
	}
	return string(chars), nil
}

// randomAlphabetChar returns one uniformly random character from
// userCodeAlphabet using rejection sampling over a single random byte.
func randomAlphabetChar() (byte, error) {
	size := len(userCodeAlphabet)
	// Largest multiple of size that fits in a byte; values at or above it are
	// rejected to avoid modulo bias.
	limit := 256 - (256 % size)
	var b [1]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if int(b[0]) < limit {
			return userCodeAlphabet[int(b[0])%size], nil
		}
	}
}
