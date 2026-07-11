package store

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// The structs and enums below map one-to-one to the runtime + metering tables
// created by migration 000005: launched sessions, the scoped proxy credentials
// minted for them, append-only token usage, and the human approval queue.
// Conventions match models.go — UUIDs are strings, TIMESTAMPTZ is time.Time,
// nullable columns are pointers, jsonb columns are json.RawMessage, and
// numeric(20,6) amounts are pgtype.Numeric.

// SessionStatus mirrors the Postgres session_status enum: the lifecycle state
// of a launched [Session].
type SessionStatus string

// The session_status enum values.
const (
	SessionStatusPending    SessionStatus = "pending"
	SessionStatusActive     SessionStatus = "active"
	SessionStatusTerminated SessionStatus = "terminated"
	SessionStatusFailed     SessionStatus = "failed"
)

// Valid reports whether s is one of the defined session_status values.
func (s SessionStatus) Valid() bool {
	switch s {
	case SessionStatusPending, SessionStatusActive, SessionStatusTerminated, SessionStatusFailed:
		return true
	default:
		return false
	}
}

// ApprovalStatus mirrors the Postgres approval_status enum: the state of an
// [ApprovalRequest] in the human escalation queue.
type ApprovalStatus string

// The approval_status enum values.
const (
	ApprovalStatusPending  ApprovalStatus = "pending"
	ApprovalStatusApproved ApprovalStatus = "approved"
	ApprovalStatusDenied   ApprovalStatus = "denied"
	ApprovalStatusExpired  ApprovalStatus = "expired"
)

// Valid reports whether s is one of the defined approval_status values.
func (s ApprovalStatus) Valid() bool {
	switch s {
	case ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusDenied, ApprovalStatusExpired:
		return true
	default:
		return false
	}
}

// Session is a governed run of a specific agent version (table sessions).
// agent_id uses ON DELETE RESTRICT, so a session always keeps a valid reference
// to its agent. The remaining foreign keys are nullable and detach (SET NULL)
// if their target is removed.
type Session struct {
	ID              string          // id (uuid)
	OrgID           string          // org_id → organizations.id
	AgentID         string          // agent_id → agents.id (RESTRICT)
	AgentVersion    *int            // agent_version (nullable)
	PolicyVersionID *string         // policy_version_id → policy_versions.id (nullable)
	EnvironmentID   *string         // environment_id → environments.id (nullable)
	LaunchedBy      *string         // launched_by → members.id (nullable)
	Status          SessionStatus   // status (session_status enum)
	FanOutCeiling   int             // fan_out_ceiling
	Labels          json.RawMessage // labels (jsonb object)
	CreatedAt       time.Time       // created_at
	TerminatedAt    *time.Time      // terminated_at (nullable)
}

// CredentialGrant is a scoped, expiring proxy token minted for a [Session]
// (table credential_grants). Only the hash of the token is stored, so a
// database leak does not expose usable credentials.
type CredentialGrant struct {
	ID             string          // id (uuid)
	SessionID      string          // session_id → sessions.id (CASCADE)
	ProxyTokenHash string          // proxy_token_hash (unique)
	EndpointID     *string         // endpoint_id → model_endpoints.id (nullable)
	Scopes         json.RawMessage // scopes (jsonb array)
	IssuedAt       time.Time       // issued_at
	ExpiresAt      time.Time       // expires_at
	RevokedAt      *time.Time      // revoked_at (nullable)
}

// UsageEvent is one append-only token-metering record (table usage_events).
// Rows are keyed by a bigint identity and never updated in place. TotalTokens
// is database-generated (prompt_tokens + completion_tokens) and read-only.
type UsageEvent struct {
	ID               int64          // id (bigint identity)
	OrgID            string         // org_id → organizations.id
	SessionID        *string        // session_id → sessions.id (nullable)
	AgentID          *string        // agent_id → agents.id (nullable)
	EndpointID       *string        // endpoint_id → model_endpoints.id (nullable)
	Model            *string        // model (nullable)
	PromptTokens     int            // prompt_tokens
	CompletionTokens int            // completion_tokens
	TotalTokens      int            // total_tokens (DB-generated, read-only)
	CostAmount       pgtype.Numeric // cost_amount (numeric(20,6))
	Currency         string         // currency (defaults to 'USD')
	OccurredAt       time.Time      // occurred_at
}

// ApprovalRequest is one entry in the human escalation queue for gated actions
// (table approval_requests). It starts pending and is resolved by a decision.
type ApprovalRequest struct {
	ID          string          // id (uuid)
	OrgID       string          // org_id → organizations.id
	SessionID   string          // session_id → sessions.id (CASCADE)
	Action      string          // action
	Reason      *string         // reason (nullable)
	ContextJSON json.RawMessage // context_json (jsonb object)
	Status      ApprovalStatus  // status (approval_status enum)
	DecidedBy   *string         // decided_by → members.id (nullable)
	DecidedAt   *time.Time      // decided_at (nullable)
	CreatedAt   time.Time       // created_at
}
