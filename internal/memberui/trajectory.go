package memberui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"sync"
	"time"

	"github.com/Domene99/drun-dash/internal/agentrun"
)

// Trajectory field/event classes. The console only ever emits structure-class
// data (ADR-0004 §5): shape and metadata about what the agent did, never the
// raw command, file contents, or model text. Promotion to payload is a policy
// decision the console never makes on its own initiative, so it is deliberately
// not offered here.
const (
	fieldClassStructure = "structure"

	// Event types the console emits, one per meaningful turn-loop step. The
	// control plane treats the type as an open string (only non-empty is
	// required), so these names are the console's own vocabulary, chosen to read
	// clearly in the admin trajectory view.
	trajTypeTurn       = "turn"
	trajTypeModelCall  = "model_call"
	trajTypeToolCall   = "tool_call"
	trajTypeToolResult = "tool_result"
	trajTypeFinal      = "final"
	trajTypeBlocked    = "blocked"
	trajTypeEscalated  = "escalated"
)

// trajectoryField mirrors the OpenAPI TrajectoryField schema. The console emits
// only structure-class fields, so value is always omitted — the control plane's
// default ingest path rejects any field carrying a raw value. The tool a step
// touched is recorded in Key (structure metadata: which tool ran), while
// ValueType/ByteSize summarize the shape and size of the arguments or result
// without shipping their bytes.
type trajectoryField struct {
	Key        string `json:"key"`
	FieldClass string `json:"field_class"`
	ValueType  string `json:"value_type,omitempty"`
	ByteSize   *int   `json:"byte_size,omitempty"`
}

// trajectoryEvent mirrors the OpenAPI TrajectoryEvent request shape (the one
// the control plane's POST /sessions/{id}/trajectory actually decodes, which is
// leaner than pkg/trajectory's CLI wire event). ParentEventID nests tool
// calls/results under their turn so the admin view can group a turn's steps.
type trajectoryEvent struct {
	EventID       string            `json:"event_id"`
	Type          string            `json:"type"`
	OccurredAt    time.Time         `json:"occurred_at"`
	FieldClass    string            `json:"field_class"`
	ParentEventID string            `json:"parent_event_id,omitempty"`
	Fields        []trajectoryField `json:"fields,omitempty"`
}

// IngestAck mirrors the OpenAPI IngestAck response: how many of the posted
// events the control plane stored versus turned away (invalid, payload-class,
// or a duplicate re-ingest).
type IngestAck struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

// IngestTrajectory posts a batch of structure-only trajectory events for a
// session to POST /sessions/{sessionId}/trajectory. The control plane returns
// 202 with an accepted/rejected tally; ingest is idempotent by event_id, so a
// retried batch double-counts nothing. It is the residency-preserving telemetry
// channel: only event shape and metadata leave the member's machine (ADR-0004
// §5).
func (c *Client) IngestTrajectory(ctx context.Context, token, sessionID string, events []trajectoryEvent) (*IngestAck, error) {
	body := struct {
		Events []trajectoryEvent `json:"events"`
	}{Events: events}
	var ack IngestAck
	path := "/sessions/" + url.PathEscape(sessionID) + "/trajectory"
	if err := c.postJSON(ctx, token, path, body, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// randomEventID returns a 128-bit random hex id for idempotent trajectory
// ingest. crypto/rand is used so ids never collide across turns or consoles.
func randomEventID() string {
	var b [16]byte
	// rand.Read never returns a short read on the crypto source; on the
	// impossible error path the (mostly zero) id still yields a valid, if less
	// unique, event id rather than panicking a governed run.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// trajectoryEmitter converts the turn loop's [agentrun.Step] stream into
// structure-only [trajectoryEvent]s and buffers them for a single batched,
// off-the-critical-path POST (ADR-0004 §5). Each turn opens with a parent
// "turn" event; every subsequent step nests under it via parent_event_id.
//
// The emitter is written from the turn's OnStep callback (one goroutine) but
// buffers under a mutex so a concurrent flush reads a consistent snapshot.
type trajectoryEmitter struct {
	now   func() time.Time
	newID func() string

	mu     sync.Mutex
	parent string
	events []trajectoryEvent
}

// newTrajectoryEmitter builds an emitter. now and newID default to time.Now and
// [randomEventID] when nil, so tests can inject deterministic clocks/ids.
func newTrajectoryEmitter(now func() time.Time, newID func() string) *trajectoryEmitter {
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = randomEventID
	}
	return &trajectoryEmitter{now: now, newID: newID}
}

// begin records the turn's root event and returns its id. Later steps nest
// under it. The directive text is deliberately not recorded — only that a turn
// began — keeping the member's prompt on their machine.
func (e *trajectoryEmitter) begin() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.parent = e.newID()
	e.events = append(e.events, trajectoryEvent{
		EventID:    e.parent,
		Type:       trajTypeTurn,
		OccurredAt: e.now(),
		FieldClass: fieldClassStructure,
	})
}

// onStep maps one loop step to a structure-only event nested under the turn.
// Tool arguments and results appear only as their coarse shape/size (via
// [agentrun.FieldMeta]); the tool name is recorded as the field key. Model and
// block text is never emitted — the console renders it locally and would
// promote it to payload only under policy, which this batch does not do.
func (e *trajectoryEmitter) onStep(s agentrun.Step) {
	ev := trajectoryEvent{
		EventID:    e.newID(),
		OccurredAt: e.now(),
		FieldClass: fieldClassStructure,
	}
	switch s.Kind {
	case agentrun.StepModelCall:
		ev.Type = trajTypeModelCall
	case agentrun.StepToolCall:
		ev.Type = trajTypeToolCall
		ev.Fields = fieldsFor(s.ToolName, s.ArgsMeta)
	case agentrun.StepToolResult:
		ev.Type = trajTypeToolResult
		ev.Fields = fieldsFor(s.ToolName, s.ResultMeta)
	case agentrun.StepFinal:
		ev.Type = trajTypeFinal
	case agentrun.StepBlocked:
		ev.Type = trajTypeBlocked
		ev.Fields = fieldsFor(s.ToolName, nil)
	case agentrun.StepEscalated:
		ev.Type = trajTypeEscalated
		ev.Fields = fieldsFor(s.ToolName, nil)
	default:
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	ev.ParentEventID = e.parent
	e.events = append(e.events, ev)
}

// drain returns the buffered events and resets the buffer, so a flush POSTs each
// event at most once even if called more than once.
func (e *trajectoryEmitter) drain() []trajectoryEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.events
	e.events = nil
	return out
}

// fieldsFor builds the structure-only field record for a step: the tool name as
// the field key, and the argument/result shape and size when known. A nil meta
// (a block/escalation, which has no value) records just the tool identity.
func fieldsFor(toolName string, meta *agentrun.FieldMeta) []trajectoryField {
	if toolName == "" {
		return nil
	}
	f := trajectoryField{Key: toolName, FieldClass: fieldClassStructure}
	if meta != nil {
		size := meta.ByteSize
		f.ByteSize = &size
		f.ValueType = meta.ValueType
	}
	return []trajectoryField{f}
}
