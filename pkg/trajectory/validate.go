package trajectory

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Validation sentinel errors. Callers can match specific failure modes with
// errors.Is.
var (
	// ErrMissingField indicates a required field was empty. The error message
	// names the field.
	ErrMissingField = errors.New("trajectory: missing required field")
	// ErrUnsupportedSchema indicates schema_version is outside the supported
	// range.
	ErrUnsupportedSchema = errors.New("trajectory: unsupported schema_version")
	// ErrInvalidEventID indicates event_id is not a UUID.
	ErrInvalidEventID = errors.New("trajectory: event_id must be a UUID")
	// ErrInvalidTimestamp indicates timestamp is not RFC3339.
	ErrInvalidTimestamp = errors.New("trajectory: timestamp must be RFC3339")
	// ErrInvalidFieldClass indicates field_class is not a known value.
	ErrInvalidFieldClass = errors.New("trajectory: invalid field_class")
	// ErrStructureHasPayload indicates a structure event included a payload.
	ErrStructureHasPayload = errors.New("trajectory: structure event must not include a payload")
	// ErrPayloadNotAllowed indicates a payload event was rejected by the
	// ingest policy (default ingest path).
	ErrPayloadNotAllowed = errors.New("trajectory: payload event rejected by ingest policy")
)

// uuidRE matches the canonical 8-4-4-4-12 hex UUID form (any version/variant).
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Validate checks that the event is structurally well-formed and internally
// consistent, independent of any ingest policy.
//
// It enforces required fields, a UUID event_id, an RFC3339 timestamp, a known
// field_class, and the field_class/payload invariant: structure events must
// not carry a payload. It does NOT decide whether a payload event is
// acceptable to ingest — use ValidateForIngest for that.
func (e *TrajectoryEvent) Validate() error {
	if e.SchemaVersion < 1 || e.SchemaVersion > SchemaVersion {
		return fmt.Errorf("%w: %d (supported 1..%d)", ErrUnsupportedSchema, e.SchemaVersion, SchemaVersion)
	}

	required := []struct {
		name  string
		value string
	}{
		{"event_id", e.EventID},
		{"trajectory_id", e.TrajectoryID},
		{"session_id", e.SessionID},
		{"node_id", e.NodeID},
		{"checkpoint_id", e.CheckpointID},
		{"actor_member_id", e.ActorMemberID},
		{"timestamp", e.Timestamp},
		{"event_type", string(e.EventType)},
		{"field_class", string(e.FieldClass)},
	}
	for _, f := range required {
		if f.value == "" {
			return fmt.Errorf("%w: %s", ErrMissingField, f.name)
		}
	}

	if !uuidRE.MatchString(e.EventID) {
		return fmt.Errorf("%w: %q", ErrInvalidEventID, e.EventID)
	}

	if _, err := time.Parse(time.RFC3339, e.Timestamp); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidTimestamp, e.Timestamp)
	}

	if !e.FieldClass.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidFieldClass, e.FieldClass)
	}

	// tool_call and policy_block events describe a specific tool invocation, so
	// tool_name is required for them.
	if e.EventType == EventToolCall || e.EventType == EventPolicyBlock {
		if e.ToolName == "" {
			return fmt.Errorf("%w: tool_name (required for %s events)", ErrMissingField, e.EventType)
		}
	}

	// Residency invariant: structure events carry no raw payload.
	if e.FieldClass == FieldClassStructure && e.HasPayload() {
		return ErrStructureHasPayload
	}

	return nil
}

// ValidateForIngest validates the event and enforces the control plane's
// residency policy.
//
// It first runs Validate. Then, on the default ingest path (allowPayload
// false), it rejects events with field_class=payload — the control plane
// ingests trajectory structure and metadata only, not file contents (§5.2).
// Set allowPayload true only for explicitly authorized payload-bearing ingest.
func ValidateForIngest(e *TrajectoryEvent, allowPayload bool) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.FieldClass == FieldClassPayload && !allowPayload {
		return ErrPayloadNotAllowed
	}
	return nil
}
