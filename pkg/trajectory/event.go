// Package trajectory defines the on-the-wire schema for drun-dash trajectory
// events.
//
// drun-dash captures agent runs as a checkpoint tree (like git branches).
// During a governed session the CLI emits TrajectoryEvents and POSTs them to
// the control plane ingest API. By default the control plane ingests only
// trajectory *structure and metadata* — never raw file contents. This
// residency boundary (product design §5.2) is modeled by FieldClass and
// enforced by the validators in validate.go.
package trajectory

import "encoding/json"

// SchemaVersion is the current TrajectoryEvent schema version. Producers set
// TrajectoryEvent.SchemaVersion to this value.
const SchemaVersion = 1

// FieldClass classifies what residency tier of data an event carries.
//
// A structure event carries only trajectory shape and metadata and is safe for
// the default ingest path. A payload event additionally carries raw content
// (e.g. file bytes) and crosses the residency boundary, so it is rejected by
// the default ingest path unless explicitly allowed.
type FieldClass string

const (
	// FieldClassStructure marks an event carrying only trajectory structure
	// and metadata — never raw payload. Accepted on the default ingest path.
	FieldClassStructure FieldClass = "structure"
	// FieldClassPayload marks an event that additionally carries a raw
	// payload. Rejected on the default ingest path (see ValidateForIngest).
	FieldClassPayload FieldClass = "payload"
)

// Valid reports whether c is a known field class.
func (c FieldClass) Valid() bool {
	return c == FieldClassStructure || c == FieldClassPayload
}

// EventType names the semantic kind of a trajectory event. The set is open for
// forward compatibility — validation only requires a non-empty value.
type EventType string

const (
	// EventSessionStart marks the creation of a drun sandbox session and the
	// root node of a trajectory.
	EventSessionStart EventType = "session_start"
	// EventToolCall marks the invocation of a drun MCP tool (session_bash,
	// session_fetch, session_write_file, ...).
	EventToolCall EventType = "tool_call"
	// EventPolicyBlock marks a tool call denied by a governance policy.
	EventPolicyBlock EventType = "policy_block"
	// EventFork marks a fork of the checkpoint tree (a new branch node whose
	// parent_node_id points at the forked-from node).
	EventFork EventType = "fork"
	// EventRollback marks a rollback to an earlier checkpoint.
	EventRollback EventType = "rollback"
)

// TokenCounts summarizes model token usage associated with an event.
type TokenCounts struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
	Total  int `json:"total,omitempty"`
}

// PolicyException records a governance policy decision attached to an event,
// such as a blocked tool call or an approved exception.
type PolicyException struct {
	Rule   string `json:"rule,omitempty"`
	Action string `json:"action,omitempty"` // e.g. "block", "allow", "warn"
	Reason string `json:"reason,omitempty"`
}

// Metadata carries structural, non-residency-sensitive descriptors about an
// event. Every field is optional. The known fields are typed; any additional
// keys are preserved verbatim in Extra so the schema can evolve without
// dropping data on round-trips.
type Metadata struct {
	// Domains lists network domains touched by the event (e.g. session_fetch
	// targets).
	Domains []string `json:"domains,omitempty"`
	// Packages lists package identifiers referenced by the event.
	Packages []string `json:"packages,omitempty"`
	// TokenCounts summarizes model token usage.
	TokenCounts *TokenCounts `json:"token_counts,omitempty"`
	// PolicyException records a governance decision.
	PolicyException *PolicyException `json:"policy_exception,omitempty"`
	// Extra captures forward-compatible metadata keys not yet modeled above.
	Extra map[string]json.RawMessage `json:"-"`
}

// metadataKnown mirrors Metadata's typed fields for (un)marshalling without
// recursing into Metadata's custom JSON methods.
type metadataKnown struct {
	Domains         []string         `json:"domains,omitempty"`
	Packages        []string         `json:"packages,omitempty"`
	TokenCounts     *TokenCounts     `json:"token_counts,omitempty"`
	PolicyException *PolicyException `json:"policy_exception,omitempty"`
}

// knownMetadataKeys are the JSON keys owned by Metadata's typed fields.
var knownMetadataKeys = []string{"domains", "packages", "token_counts", "policy_exception"}

// MarshalJSON renders the typed fields and folds Extra keys into the same
// object. Typed fields win if a key collides with Extra.
func (m Metadata) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(m.Extra)+len(knownMetadataKeys))
	for k, v := range m.Extra {
		out[k] = v
	}
	known, err := json.Marshal(metadataKnown{
		Domains:         m.Domains,
		Packages:        m.Packages,
		TokenCounts:     m.TokenCounts,
		PolicyException: m.PolicyException,
	})
	if err != nil {
		return nil, err
	}
	var typed map[string]json.RawMessage
	if err := json.Unmarshal(known, &typed); err != nil {
		return nil, err
	}
	for k, v := range typed {
		out[k] = v
	}
	return json.Marshal(out)
}

// UnmarshalJSON populates the typed fields and stashes any remaining keys in
// Extra.
func (m *Metadata) UnmarshalJSON(data []byte) error {
	var known metadataKnown
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	m.Domains = known.Domains
	m.Packages = known.Packages
	m.TokenCounts = known.TokenCounts
	m.PolicyException = known.PolicyException

	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, k := range knownMetadataKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		m.Extra = raw
	} else {
		m.Extra = nil
	}
	return nil
}

// TrajectoryEvent is a single event in a drun-dash trajectory checkpoint tree.
//
// Structure fields (node_id/parent_node_id/checkpoint_id) describe the shape of
// the tree; metadata describes the event; payload — present only when
// FieldClass is FieldClassPayload — carries raw content that crosses the
// residency boundary.
type TrajectoryEvent struct {
	// SchemaVersion is the schema version this event conforms to.
	SchemaVersion int `json:"schema_version"`
	// EventID is a UUID that uniquely identifies the event for idempotent
	// ingest.
	EventID string `json:"event_id"`
	// TrajectoryID identifies the trajectory (checkpoint tree) this event
	// belongs to.
	TrajectoryID string `json:"trajectory_id"`
	// SessionID is the drun sandbox session id.
	SessionID string `json:"session_id"`
	// NodeID identifies this event's node in the checkpoint tree.
	NodeID string `json:"node_id"`
	// ParentNodeID is the tree edge for the fork/rollback graph. Empty for the
	// root node (session_start).
	ParentNodeID string `json:"parent_node_id,omitempty"`
	// CheckpointID is the drun checkpoint id this event corresponds to.
	CheckpointID string `json:"checkpoint_id"`
	// ToolName is the drun MCP tool name for tool_call / policy_block events
	// (e.g. session_bash). Empty for events not tied to a tool.
	ToolName string `json:"tool_name,omitempty"`
	// ActorMemberID identifies the governed member who performed the action.
	ActorMemberID string `json:"actor_member_id"`
	// Timestamp is when the event occurred, in RFC3339 format.
	Timestamp string `json:"timestamp"`
	// EventType is the semantic kind of the event.
	EventType EventType `json:"event_type"`
	// FieldClass is the residency tier of the event's data.
	FieldClass FieldClass `json:"field_class"`
	// Metadata carries structural, non-residency-sensitive descriptors.
	Metadata *Metadata `json:"metadata,omitempty"`
	// Payload carries raw content and must be present only when FieldClass is
	// FieldClassPayload.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// HasPayload reports whether the event carries a non-empty payload.
func (e *TrajectoryEvent) HasPayload() bool {
	return len(e.Payload) > 0 && string(e.Payload) != "null"
}
