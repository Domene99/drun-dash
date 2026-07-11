package store

import (
	"encoding/json"
	"time"
)

// The structs below map one-to-one to the agent-catalog tables created by
// migration 000004: policies and their versions, agents and their versions,
// and the entitlements that grant agents to principals. Conventions match
// models.go — UUIDs are strings, TIMESTAMPTZ is time.Time, nullable columns are
// pointers, and jsonb columns are json.RawMessage.

// Policy is a named governance boundary with an append-only version history
// (table policies; unique per (org_id, name)). CurrentVersion points at the
// newest version number, or 0 until the first version lands.
type Policy struct {
	ID             string    // id (uuid)
	OrgID          string    // org_id → organizations.id
	Name           string    // name
	Description    *string   // description (nullable)
	CurrentVersion int       // current_version
	CreatedAt      time.Time // created_at
}

// PolicyVersion is one immutable revision of a [Policy] (table policy_versions;
// unique per (policy_id, version), version > 0). content_hash is a
// deterministic digest of content_json; signing_json is optional detached
// signing metadata.
type PolicyVersion struct {
	ID          string          // id (uuid)
	PolicyID    string          // policy_id → policies.id
	Version     int             // version (> 0)
	ContentJSON json.RawMessage // content_json (jsonb)
	ContentHash string          // content_hash
	SigningJSON json.RawMessage // signing_json (nullable jsonb; nil when absent)
	CreatedAt   time.Time       // created_at
}

// Agent is a named catalog entry with an append-only version history (table
// agents; unique per (org_id, name)). CurrentVersion points at the newest
// version number, or 0 until the first version lands.
type Agent struct {
	ID             string    // id (uuid)
	OrgID          string    // org_id → organizations.id
	Name           string    // name
	Description    *string   // description (nullable)
	CurrentVersion int       // current_version
	CreatedAt      time.Time // created_at
	UpdatedAt      time.Time // updated_at
}

// AgentVersion is one immutable revision of an [Agent] (table agent_versions;
// unique per (agent_id, version), version > 0). Each version pins governance
// building blocks by id; the pins are optional (nullable).
type AgentVersion struct {
	ID              string          // id (uuid)
	AgentID         string          // agent_id → agents.id
	Version         int             // version (> 0)
	SystemPrompt    string          // system_prompt
	AllowedTools    json.RawMessage // allowed_tools (jsonb array)
	PolicyVersionID *string         // policy_version_id → policy_versions.id (nullable)
	EndpointID      *string         // endpoint_id → model_endpoints.id (nullable)
	BudgetID        *string         // budget_id → budgets.id (nullable)
	FanOutCeiling   int             // fan_out_ceiling
	CreatedAt       time.Time       // created_at
}

// Entitlement grants an [Agent] to exactly one principal — a member XOR a team
// (table entitlements). PolicyVersionID and MaxFanOut are optional overrides
// for the grant.
type Entitlement struct {
	ID              string    // id (uuid)
	AgentID         string    // agent_id → agents.id
	MemberID        *string   // member_id → members.id (nullable; XOR team_id)
	TeamID          *string   // team_id → teams.id (nullable; XOR member_id)
	PolicyVersionID *string   // policy_version_id → policy_versions.id (nullable)
	MaxFanOut       *int      // max_fan_out (nullable)
	CreatedAt       time.Time // created_at
}
