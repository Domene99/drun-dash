package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// trajectoryStore is the narrow slice of *[store.TrajectoryStore] the telemetry
// handlers use. As with the other resources it is named as an interface so the
// tests can inject an in-memory fake in place of a live database while the
// production path runs the real SQL. Only the find-or-create pair the ingest
// handler needs is exposed here.
type trajectoryStore interface {
	GetBySessionID(ctx context.Context, sessionID string) (store.Trajectory, error)
	Create(ctx context.Context, p store.CreateTrajectoryParams) (store.Trajectory, error)
}

// trajectoryNodeStore is the narrow slice of *[store.TrajectoryNodeStore] the
// telemetry handlers use: append a node, resolve a parent by its event id, and
// read a trajectory's nodes back for the list endpoint.
type trajectoryNodeStore interface {
	Create(ctx context.Context, p store.CreateTrajectoryNodeParams) (store.TrajectoryNode, error)
	GetByEventID(ctx context.Context, trajectoryID, eventID string) (store.TrajectoryNode, error)
	ListByTrajectory(ctx context.Context, trajectoryID string) ([]store.TrajectoryNode, error)
}

// newTrajectoryStore and newTrajectoryNodeStore are the store-construction
// seams the telemetry handlers operate through — the same package-level-var
// pattern as newAgentStore/newSessionStore, so tests swap in a fake without a
// database and production constructs the real store over the request's pool.
var newTrajectoryStore = func(q store.Querier) trajectoryStore {
	return store.NewTrajectoryStore(q)
}

var newTrajectoryNodeStore = func(q store.Querier) trajectoryNodeStore {
	return store.NewTrajectoryNodeStore(q)
}

// apiTrajectoryField is the OpenAPI TrajectoryField schema
// (openapi/v1.yaml #/components/schemas/TrajectoryField): one captured field
// within an event. value is present only for a permitted payload field; on the
// default structure-only ingest path it is always absent, so only the structure
// descriptors (value_type, byte_size) survive to storage.
type apiTrajectoryField struct {
	Key        string          `json:"key"`
	FieldClass string          `json:"field_class"`
	Value      json.RawMessage `json:"value,omitempty"`
	ValueType  string          `json:"value_type,omitempty"`
	ByteSize   *int            `json:"byte_size,omitempty"`
}

// apiTrajectoryEvent is the OpenAPI TrajectoryEvent schema
// (openapi/v1.yaml #/components/schemas/TrajectoryEvent). parent_event_id and
// fields are omitted when absent; occurred_at maps to the node's occurred_at.
type apiTrajectoryEvent struct {
	EventID       string               `json:"event_id"`
	Type          string               `json:"type"`
	OccurredAt    time.Time            `json:"occurred_at"`
	FieldClass    string               `json:"field_class"`
	ParentEventID *string              `json:"parent_event_id,omitempty"`
	Fields        []apiTrajectoryField `json:"fields,omitempty"`
}

// apiTrajectoryEventBatch is the OpenAPI TrajectoryEventBatch request body: a
// non-empty list of events to ingest.
type apiTrajectoryEventBatch struct {
	Events []apiTrajectoryEvent `json:"events"`
}

// apiIngestAck is the OpenAPI IngestAck response: how many events of the batch
// were stored and how many were turned away (invalid, payload-class, or a
// duplicate re-ingest).
type apiIngestAck struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

// apiTrajectoryEventList is the OpenAPI TrajectoryEventList response (a Page
// envelope plus items). next_cursor is nullable — a string on a full page and
// null once the list is exhausted (ADR-0002 §3) — so it is a *string emitted
// without omitempty.
type apiTrajectoryEventList struct {
	Items      []apiTrajectoryEvent `json:"items"`
	NextCursor *string              `json:"next_cursor"`
}

// trajectoryNodeMetadata is the shape stored in a node's metadata_json: the
// event's per-field structure records. Ingest writes it and the list endpoint
// reads it back, keeping the two directions symmetric.
type trajectoryNodeMetadata struct {
	Fields []apiTrajectoryField `json:"fields,omitempty"`
}

// ingestTrajectoryEvents implements POST /sessions/{sessionId}/trajectory
// (operationId ingestTrajectoryEvents). It batch-ingests the *structure* of a
// governed run into the session's single trajectory (found-or-created on first
// ingest) and returns an accepted/rejected tally.
//
// Each event is validated against the v1 residency policy — structure only,
// payload not allowed — before it is stored (see validTrajectoryEvent). An
// invalid or payload-class event, and a duplicate (trajectory_id, event_id) on
// re-ingest, are counted as rejected without aborting the batch, so a partial
// or replayed batch still records everything it legitimately can (idempotent
// ingest). A malformed body or an empty events list is a 400; a cross-org or
// missing session is a 404.
func (d Deps) ingestTrajectoryEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	sessionID := chi.URLParam(r, "sessionId")

	var req apiTrajectoryEventBatch
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if len(req.Events) == 0 {
		WriteValidationError(w, "events", "events must contain at least one event")
		return
	}

	// Confirm the session belongs to the caller's org before recording anything
	// against it: a cross-org or missing session is a 404 (never 403) so
	// trajectory data cannot be seeded under, or leaked from, another tenant.
	session, err := newSessionStore(d.Pool).GetByID(ctx, sessionID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, session.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	traj, err := d.findOrCreateTrajectory(ctx, claims.OrgID, sessionID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	ns := newTrajectoryNodeStore(d.Pool)
	var accepted, rejected int
	for i := range req.Events {
		ev := req.Events[i]
		if !validTrajectoryEvent(ev) {
			rejected++
			continue
		}

		// Resolve parent_event_id to the parent node's id. A missing parent
		// (absent, or not yet ingested) leaves the node a root rather than
		// failing the event — the graph tolerates an as-yet-unknown parent.
		parentID, err := resolveTrajectoryParent(ctx, ns, traj.ID, ev.ParentEventID)
		if err != nil {
			WriteStoreError(w, err)
			return
		}

		_, err = ns.Create(ctx, store.CreateTrajectoryNodeParams{
			TrajectoryID: traj.ID,
			ParentID:     parentID,
			EventID:      ev.EventID,
			EventType:    ev.Type,
			FieldClass:   store.TrajectoryFieldClass(ev.FieldClass),
			MetadataJSON: trajectoryFieldsJSON(ev.Fields),
			OccurredAt:   ev.OccurredAt,
		})
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, store.ErrConflict):
			// Duplicate (trajectory_id, event_id): a re-ingest of an event
			// already recorded. Idempotent — count it rejected, keep going.
			rejected++
		default:
			WriteStoreError(w, err)
			return
		}
	}

	WriteAccepted(w, apiIngestAck{Accepted: accepted, Rejected: rejected})
}

// findOrCreateTrajectory returns the session's trajectory, creating it on the
// first ingest. It is the find-or-create the ingest handler opens with: a
// GetBySessionID miss (ErrNotFound) is not an error but the signal to Create.
func (d Deps) findOrCreateTrajectory(ctx context.Context, orgID, sessionID string) (store.Trajectory, error) {
	ts := newTrajectoryStore(d.Pool)
	traj, err := ts.GetBySessionID(ctx, sessionID)
	if err == nil {
		return traj, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Trajectory{}, err
	}
	sid := sessionID
	return ts.Create(ctx, store.CreateTrajectoryParams{OrgID: orgID, SessionID: &sid})
}

// resolveTrajectoryParent maps an event's parent_event_id to the stored parent
// node's id. It returns nil when parentEventID is absent/empty, or when no node
// with that event id exists yet in the trajectory (the node is stored as a
// root); any other store error is surfaced to the caller.
func resolveTrajectoryParent(ctx context.Context, ns trajectoryNodeStore, trajectoryID string, parentEventID *string) (*string, error) {
	if parentEventID == nil || *parentEventID == "" {
		return nil, nil
	}
	parent, err := ns.GetByEventID(ctx, trajectoryID, *parentEventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &parent.ID, nil
}

// validTrajectoryEvent reports whether ev may be ingested under the v1
// structure-only residency policy (product design §5.2). It mirrors
// pkg/trajectory.ValidateForIngest(e, allowPayload=false) for the public REST
// TrajectoryEvent shape: the required fields must be present, field_class must
// be a known value, and no raw payload may cross the boundary — a payload-class
// event is rejected outright, and a structure event may not smuggle a raw field
// value. (The pkg validator targets the richer CLI wire event, which carries
// fields the REST shape does not, so the equivalent invariants are enforced
// here rather than by calling it directly.)
func validTrajectoryEvent(ev apiTrajectoryEvent) bool {
	if ev.EventID == "" || ev.Type == "" || ev.OccurredAt.IsZero() {
		return false
	}
	fc := store.TrajectoryFieldClass(ev.FieldClass)
	if !fc.Valid() || fc == store.TrajectoryFieldClassPayload {
		return false
	}
	for _, f := range ev.Fields {
		ffc := store.TrajectoryFieldClass(f.FieldClass)
		if !ffc.Valid() || ffc == store.TrajectoryFieldClassPayload {
			return false
		}
		if fieldHasValue(f) {
			return false
		}
	}
	return true
}

// fieldHasValue reports whether a captured field carries a raw value that would
// cross the residency boundary. A JSON null is treated as no value.
func fieldHasValue(f apiTrajectoryField) bool {
	return len(f.Value) > 0 && string(f.Value) != "null"
}

// trajectoryFieldsJSON encodes an event's per-field structure records for the
// node's metadata_json. It returns nil (so the store writes its '{}' default)
// when the event carries no fields. Raw values were already rejected upstream,
// so only structure descriptors are ever serialized here.
func trajectoryFieldsJSON(fields []apiTrajectoryField) json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	// The fields are plain structure records at this point, so marshalling
	// cannot fail; ignore the error and fall back to nil on the impossible path.
	b, err := json.Marshal(trajectoryNodeMetadata{Fields: fields})
	if err != nil {
		return nil
	}
	return b
}

// listTrajectoryEvents implements GET /sessions/{sessionId}/trajectory
// (operationId listTrajectoryEvents). It reads back a keyset-paginated page of
// the session's recorded trajectory events in replay order.
//
// The node store exposes no keyset page method (and this batch does not add
// one), so the trajectory's nodes are loaded whole and paged in memory over the
// total order (occurred_at, id) — id, a UUID, breaks any occurred_at ties, so
// the order is stable and the opaque cursor seeks reliably. A cross-org or
// missing session is a 404; a session with nothing ingested yet is an empty
// (but valid) page rather than a 404.
func (d Deps) listTrajectoryEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	sessionID := chi.URLParam(r, "sessionId")

	limit, err := ParseLimit(r)
	if err != nil {
		WriteValidationError(w, "limit", "limit must be an integer")
		return
	}
	cursor, err := DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		WriteValidationError(w, "cursor", "invalid cursor")
		return
	}

	session, err := newSessionStore(d.Pool).GetByID(ctx, sessionID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, session.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	traj, err := newTrajectoryStore(d.Pool).GetBySessionID(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		// Nothing ingested for this session yet: a valid, empty page.
		WriteOK(w, apiTrajectoryEventList{Items: []apiTrajectoryEvent{}, NextCursor: nil})
		return
	}
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	nodes, err := newTrajectoryNodeStore(d.Pool).ListByTrajectory(ctx, traj.ID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	sort.Slice(nodes, func(i, j int) bool {
		if !nodes[i].OccurredAt.Equal(nodes[j].OccurredAt) {
			return nodes[i].OccurredAt.Before(nodes[j].OccurredAt)
		}
		return nodes[i].ID < nodes[j].ID
	})

	// event id keyed by node id, so a child's parent_event_id renders from its
	// parent_id. Built over every node (not just the page) so a parent that
	// falls on an earlier page still resolves.
	eventByID := make(map[string]string, len(nodes))
	for _, n := range nodes {
		eventByID[n.ID] = n.EventID
	}

	page := seekTrajectoryPage(nodes, cursor)
	var next *string
	if len(page) > limit {
		page = page[:limit]
		last := page[limit-1]
		c := EncodeCursor(Cursor{CreatedAt: last.OccurredAt, ID: last.ID})
		next = &c
	}

	items := make([]apiTrajectoryEvent, 0, len(page))
	for _, n := range page {
		items = append(items, toAPITrajectoryEvent(n, eventByID))
	}
	WriteOK(w, apiTrajectoryEventList{Items: items, NextCursor: next})
}

// seekTrajectoryPage returns the suffix of nodes strictly past the cursor's
// (occurred_at, id) position; a nil cursor returns the whole slice. nodes must
// already be sorted by (occurred_at, id) — the same order EncodeCursor captured
// — so the seek matches the keyset predicate the store applies for the other
// resources.
func seekTrajectoryPage(nodes []store.TrajectoryNode, cursor *Cursor) []store.TrajectoryNode {
	if cursor == nil {
		return nodes
	}
	start := 0
	for start < len(nodes) {
		n := nodes[start]
		if n.OccurredAt.After(cursor.CreatedAt) || (n.OccurredAt.Equal(cursor.CreatedAt) && n.ID > cursor.ID) {
			break
		}
		start++
	}
	return nodes[start:]
}

// toAPITrajectoryEvent maps a stored node back to its API event. parent_event_id
// is resolved through eventByID (omitted when the node is a root or its parent
// is unknown); fields are read back from metadata_json.
func toAPITrajectoryEvent(n store.TrajectoryNode, eventByID map[string]string) apiTrajectoryEvent {
	ev := apiTrajectoryEvent{
		EventID:    n.EventID,
		Type:       n.EventType,
		OccurredAt: n.OccurredAt,
		FieldClass: string(n.FieldClass),
	}
	if n.ParentID != nil {
		if parentEventID, ok := eventByID[*n.ParentID]; ok {
			ev.ParentEventID = &parentEventID
		}
	}
	if len(n.MetadataJSON) > 0 {
		var meta trajectoryNodeMetadata
		if err := json.Unmarshal(n.MetadataJSON, &meta); err == nil {
			ev.Fields = meta.Fields
		}
	}
	return ev
}
