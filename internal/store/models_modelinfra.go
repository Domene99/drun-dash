package store

import (
	"encoding/json"
	"time"
)

// The structs below map one-to-one to the model-infrastructure tables created
// by migration 000003. They follow the conventions in models.go: UUID columns
// are strings, TIMESTAMPTZ columns are time.Time, nullable columns are pointer
// fields, and jsonb columns are json.RawMessage.

// ModelEndpoint is a registered upstream LLM endpoint the proxy can route to
// (table model_endpoints; unique per (org_id, name)). secret_ref is an opaque
// handle into an external secret store — the raw provider key is never stored.
type ModelEndpoint struct {
	ID        string    // id (uuid)
	OrgID     string    // org_id → organizations.id
	Name      string    // name
	BaseURL   string    // base_url
	Provider  string    // provider
	SecretRef string    // secret_ref
	Active    bool      // active
	CreatedAt time.Time // created_at
}

// RoutingRule maps brokered traffic to a [ModelEndpoint] by a match predicate,
// evaluated in ascending priority order (table routing_rules).
type RoutingRule struct {
	ID         string          // id (uuid)
	OrgID      string          // org_id → organizations.id
	Priority   int             // priority (lower is evaluated first)
	MatchJSON  json.RawMessage // match_json (jsonb)
	EndpointID string          // endpoint_id → model_endpoints.id
	CreatedAt  time.Time       // created_at
}
