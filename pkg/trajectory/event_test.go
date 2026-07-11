package trajectory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fixtureFiles are the canonical example events shipped in fixtures/. Every one
// is a structure event and must pass the default (payload-rejecting) ingest
// path.
var fixtureFiles = []string{
	"session_start.json",
	"tool_call.json",
	"policy_block.json",
	"fork.json",
	"rollback.json",
}

func loadFixture(t *testing.T, name string) *TrajectoryEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var e TrajectoryEvent
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	return &e
}

func TestFixturesValidateForDefaultIngest(t *testing.T) {
	for _, name := range fixtureFiles {
		t.Run(name, func(t *testing.T) {
			e := loadFixture(t, name)
			if err := ValidateForIngest(e, false); err != nil {
				t.Fatalf("expected fixture to pass default ingest, got: %v", err)
			}
			if e.FieldClass != FieldClassStructure {
				t.Fatalf("fixture field_class = %q, want structure", e.FieldClass)
			}
		})
	}
}

// TestAllFixturesCovered guards against a fixture file being added or removed
// without updating fixtureFiles, and confirms every listed fixture parses.
func TestAllFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir("fixtures")
	if err != nil {
		t.Fatalf("read fixtures dir: %v", err)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			onDisk[e.Name()] = true
		}
	}
	if len(onDisk) != len(fixtureFiles) {
		t.Fatalf("fixtures on disk = %d, tracked = %d", len(onDisk), len(fixtureFiles))
	}
	for _, name := range fixtureFiles {
		if !onDisk[name] {
			t.Errorf("tracked fixture %s missing from disk", name)
		}
	}
}

func TestFixtureFieldsRoundTrip(t *testing.T) {
	// tool_call carries the richest metadata; verify a full decode + re-encode
	// preserves the typed fields.
	e := loadFixture(t, "tool_call.json")
	if e.ToolName != "session_bash" {
		t.Errorf("tool_name = %q, want session_bash", e.ToolName)
	}
	if e.ParentNodeID != "node-0" {
		t.Errorf("parent_node_id = %q, want node-0", e.ParentNodeID)
	}
	if e.Metadata == nil || e.Metadata.TokenCounts == nil {
		t.Fatalf("expected token_counts metadata")
	}
	if got := e.Metadata.TokenCounts.Total; got != 612 {
		t.Errorf("token_counts.total = %d, want 612", got)
	}

	out, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back TrajectoryEvent
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if back.Metadata.TokenCounts.Total != 612 {
		t.Errorf("round-trip token total = %d, want 612", back.Metadata.TokenCounts.Total)
	}
}

func TestPolicyBlockMetadata(t *testing.T) {
	e := loadFixture(t, "policy_block.json")
	if e.Metadata == nil || e.Metadata.PolicyException == nil {
		t.Fatalf("expected policy_exception metadata")
	}
	if e.Metadata.PolicyException.Action != "block" {
		t.Errorf("policy action = %q, want block", e.Metadata.PolicyException.Action)
	}
}

func TestMetadataExtraRoundTrip(t *testing.T) {
	// Unknown metadata keys must survive a decode/encode round-trip.
	raw := `{
		"domains": ["a.example.com"],
		"custom_key": {"nested": true},
		"another": 42
	}`
	var m Metadata
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Domains) != 1 || m.Domains[0] != "a.example.com" {
		t.Errorf("domains not decoded: %+v", m.Domains)
	}
	if len(m.Extra) != 2 {
		t.Fatalf("extra keys = %d, want 2 (%v)", len(m.Extra), m.Extra)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	for _, k := range []string{"domains", "custom_key", "another"} {
		if _, ok := back[k]; !ok {
			t.Errorf("key %q dropped on round-trip", k)
		}
	}
}

func payloadEvent() *TrajectoryEvent {
	return &TrajectoryEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "66666666-6666-4666-8666-666666666666",
		TrajectoryID:  "traj-7f3c",
		SessionID:     "b0392a4d-48e9-4a59-882b-1fd15dede731",
		NodeID:        "node-5",
		ParentNodeID:  "node-1",
		CheckpointID:  "1",
		ToolName:      "session_write_file",
		ActorMemberID: "member-alice",
		Timestamp:     "2026-07-11T11:10:00Z",
		EventType:     EventToolCall,
		FieldClass:    FieldClassPayload,
		Payload:       json.RawMessage(`{"path":"src/main.go","content":"package main"}`),
	}
}

func TestPayloadIngestPolicy(t *testing.T) {
	e := payloadEvent()

	// Structurally valid on its own.
	if err := e.Validate(); err != nil {
		t.Fatalf("payload event should be structurally valid: %v", err)
	}

	// Rejected on the default ingest path.
	if err := ValidateForIngest(e, false); !errors.Is(err, ErrPayloadNotAllowed) {
		t.Fatalf("default ingest err = %v, want ErrPayloadNotAllowed", err)
	}

	// Accepted when explicitly allowed.
	if err := ValidateForIngest(e, true); err != nil {
		t.Fatalf("allowed ingest err = %v, want nil", err)
	}
}

func TestStructureEventWithPayloadRejected(t *testing.T) {
	e := payloadEvent()
	e.FieldClass = FieldClassStructure // structure must not carry payload
	if err := e.Validate(); !errors.Is(err, ErrStructureHasPayload) {
		t.Fatalf("err = %v, want ErrStructureHasPayload", err)
	}
	// And therefore also fails ingest regardless of the allow flag.
	if err := ValidateForIngest(e, true); !errors.Is(err, ErrStructureHasPayload) {
		t.Fatalf("ingest err = %v, want ErrStructureHasPayload", err)
	}
}

func TestValidateRequiredFields(t *testing.T) {
	base := func() *TrajectoryEvent { return loadFixture(t, "tool_call.json") }

	cases := []struct {
		name    string
		mutate  func(*TrajectoryEvent)
		wantErr error
	}{
		{"missing event_id", func(e *TrajectoryEvent) { e.EventID = "" }, ErrMissingField},
		{"missing trajectory_id", func(e *TrajectoryEvent) { e.TrajectoryID = "" }, ErrMissingField},
		{"missing session_id", func(e *TrajectoryEvent) { e.SessionID = "" }, ErrMissingField},
		{"missing node_id", func(e *TrajectoryEvent) { e.NodeID = "" }, ErrMissingField},
		{"missing checkpoint_id", func(e *TrajectoryEvent) { e.CheckpointID = "" }, ErrMissingField},
		{"missing actor_member_id", func(e *TrajectoryEvent) { e.ActorMemberID = "" }, ErrMissingField},
		{"missing timestamp", func(e *TrajectoryEvent) { e.Timestamp = "" }, ErrMissingField},
		{"missing event_type", func(e *TrajectoryEvent) { e.EventType = "" }, ErrMissingField},
		{"missing field_class", func(e *TrajectoryEvent) { e.FieldClass = "" }, ErrMissingField},
		{"tool_call without tool_name", func(e *TrajectoryEvent) { e.ToolName = "" }, ErrMissingField},
		{"bad uuid", func(e *TrajectoryEvent) { e.EventID = "not-a-uuid" }, ErrInvalidEventID},
		{"bad timestamp", func(e *TrajectoryEvent) { e.Timestamp = "11 July 2026" }, ErrInvalidTimestamp},
		{"bad field_class", func(e *TrajectoryEvent) { e.FieldClass = "secret" }, ErrInvalidFieldClass},
		{"schema too high", func(e *TrajectoryEvent) { e.SchemaVersion = SchemaVersion + 1 }, ErrUnsupportedSchema},
		{"schema zero", func(e *TrajectoryEvent) { e.SchemaVersion = 0 }, ErrUnsupportedSchema},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base()
			tc.mutate(e)
			err := e.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestForkAndRollbackTreeEdges(t *testing.T) {
	fork := loadFixture(t, "fork.json")
	if fork.EventType != EventFork {
		t.Errorf("event_type = %q, want fork", fork.EventType)
	}
	if fork.ParentNodeID == "" {
		t.Error("fork event must carry a parent_node_id (the forked-from node)")
	}

	rb := loadFixture(t, "rollback.json")
	if rb.EventType != EventRollback {
		t.Errorf("event_type = %q, want rollback", rb.EventType)
	}
	if rb.ParentNodeID == "" {
		t.Error("rollback event must carry a parent_node_id")
	}
}

func TestSessionStartIsRoot(t *testing.T) {
	e := loadFixture(t, "session_start.json")
	if e.ParentNodeID != "" {
		t.Errorf("session_start parent_node_id = %q, want empty (root node)", e.ParentNodeID)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("root event should validate without a parent: %v", err)
	}
}
