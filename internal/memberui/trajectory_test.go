package memberui

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/agentrun"
)

// seqIDs returns a deterministic id generator for tests.
func seqIDs() func() string {
	n := 0
	return func() string {
		n++
		return "ev" + strconv.Itoa(n)
	}
}

func TestTrajectoryEmitterMapsStepsStructureOnly(t *testing.T) {
	fixed := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	em := newTrajectoryEmitter(func() time.Time { return fixed }, seqIDs())
	em.begin()
	em.onStep(agentrun.Step{Kind: agentrun.StepModelCall})
	em.onStep(agentrun.Step{Kind: agentrun.StepToolCall, ToolName: "session_bash", ArgsMeta: &agentrun.FieldMeta{ByteSize: 12, ValueType: "json"}})
	em.onStep(agentrun.Step{Kind: agentrun.StepToolResult, ToolName: "session_bash", ResultMeta: &agentrun.FieldMeta{ByteSize: 40, ValueType: "text"}})
	em.onStep(agentrun.Step{Kind: agentrun.StepFinal, Text: "secret model text"})

	events := em.drain()
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5 (turn + 4 steps)", len(events))
	}

	turn := events[0]
	if turn.Type != trajTypeTurn || turn.ParentEventID != "" {
		t.Errorf("first event should be the root turn: %+v", turn)
	}
	for _, ev := range events[1:] {
		if ev.ParentEventID != turn.EventID {
			t.Errorf("step %q not nested under turn: parent=%q", ev.Type, ev.ParentEventID)
		}
		if ev.FieldClass != fieldClassStructure {
			t.Errorf("event %q must be structure-class, got %q", ev.Type, ev.FieldClass)
		}
		if !ev.OccurredAt.Equal(fixed) {
			t.Errorf("event %q occurred_at = %v, want %v", ev.Type, ev.OccurredAt, fixed)
		}
	}

	// The tool_call carries the tool name as a field key and only the arg shape.
	toolCall := events[2]
	if toolCall.Type != trajTypeToolCall || len(toolCall.Fields) != 1 {
		t.Fatalf("unexpected tool_call event: %+v", toolCall)
	}
	f := toolCall.Fields[0]
	if f.Key != "session_bash" || f.FieldClass != fieldClassStructure || f.ValueType != "json" || f.ByteSize == nil || *f.ByteSize != 12 {
		t.Errorf("tool_call field is not structure-only shape: %+v", f)
	}

	// No event may carry a raw payload value. Round-trip to JSON and confirm the
	// model's final text and any raw field value never leak off-box.
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "secret model text") {
		t.Error("final answer text leaked into trajectory")
	}
	if strings.Contains(string(raw), `"value"`) {
		t.Error("a raw field value leaked into trajectory")
	}
}

func TestIngestTrajectory(t *testing.T) {
	var gotBody struct {
		Events []trajectoryEvent `json:"events"`
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions/sess-1/trajectory" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(IngestAck{Accepted: len(gotBody.Events), Rejected: 0})
	})

	events := []trajectoryEvent{{
		EventID:    "ev1",
		Type:       trajTypeTurn,
		OccurredAt: time.Now(),
		FieldClass: fieldClassStructure,
	}}
	ack, err := c.IngestTrajectory(context.Background(), "tok", "sess-1", events)
	if err != nil {
		t.Fatalf("IngestTrajectory: %v", err)
	}
	if ack.Accepted != 1 {
		t.Errorf("accepted = %d, want 1", ack.Accepted)
	}
	if len(gotBody.Events) != 1 || gotBody.Events[0].EventID != "ev1" {
		t.Errorf("server received unexpected events: %+v", gotBody.Events)
	}
}
