package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakeTrajectoryStore is an in-memory trajectoryStore: it returns a canned
// find/create result and records the create params, so the handler tests can
// assert on the find-or-create branch and org/session scoping without a
// database.
type fakeTrajectoryStore struct {
	getTraj    store.Trajectory
	getErr     error
	createTraj store.Trajectory
	createErr  error

	getCalls    int
	createCalls int
	gotCreate   store.CreateTrajectoryParams
}

func (f *fakeTrajectoryStore) GetBySessionID(_ context.Context, _ string) (store.Trajectory, error) {
	f.getCalls++
	return f.getTraj, f.getErr
}

func (f *fakeTrajectoryStore) Create(_ context.Context, p store.CreateTrajectoryParams) (store.Trajectory, error) {
	f.createCalls++
	f.gotCreate = p
	return f.createTraj, f.createErr
}

// fakeTrajectoryNodeStore is a stateful in-memory trajectoryNodeStore: Create
// appends nodes (rejecting a duplicate (trajectory_id, event_id) with
// ErrConflict), GetByEventID and ListByTrajectory read them back. Being
// stateful lets a single test exercise parent resolution, idempotent re-ingest,
// and list round-trips against the same node set.
type fakeTrajectoryNodeStore struct {
	nodes     []store.TrajectoryNode
	createErr error
	getErr    error
	listErr   error

	seq        int
	gotCreates []store.CreateTrajectoryNodeParams
}

func (f *fakeTrajectoryNodeStore) Create(_ context.Context, p store.CreateTrajectoryNodeParams) (store.TrajectoryNode, error) {
	f.gotCreates = append(f.gotCreates, p)
	if f.createErr != nil {
		return store.TrajectoryNode{}, f.createErr
	}
	for _, n := range f.nodes {
		if n.TrajectoryID == p.TrajectoryID && n.EventID == p.EventID {
			return store.TrajectoryNode{}, store.ErrConflict
		}
	}
	f.seq++
	fc := p.FieldClass
	if fc == "" {
		fc = store.TrajectoryFieldClassStructure
	}
	n := store.TrajectoryNode{
		ID:           fmt.Sprintf("n%d", f.seq),
		TrajectoryID: p.TrajectoryID,
		ParentID:     p.ParentID,
		EventID:      p.EventID,
		EventType:    p.EventType,
		ToolName:     p.ToolName,
		FieldClass:   fc,
		MetadataJSON: p.MetadataJSON,
		OccurredAt:   p.OccurredAt,
	}
	f.nodes = append(f.nodes, n)
	return n, nil
}

func (f *fakeTrajectoryNodeStore) GetByEventID(_ context.Context, trajectoryID, eventID string) (store.TrajectoryNode, error) {
	if f.getErr != nil {
		return store.TrajectoryNode{}, f.getErr
	}
	for _, n := range f.nodes {
		if n.TrajectoryID == trajectoryID && n.EventID == eventID {
			return n, nil
		}
	}
	return store.TrajectoryNode{}, store.ErrNotFound
}

func (f *fakeTrajectoryNodeStore) ListByTrajectory(_ context.Context, trajectoryID string) ([]store.TrajectoryNode, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.TrajectoryNode
	for _, n := range f.nodes {
		if n.TrajectoryID == trajectoryID {
			out = append(out, n)
		}
	}
	return out, nil
}

// setTrajectoryStores points the trajectory store seams at the fakes for the
// duration of the test, restoring the production factories afterwards. Because
// it mutates package-level vars, callers must not run in parallel.
func setTrajectoryStores(t *testing.T, ts trajectoryStore, ns trajectoryNodeStore) {
	t.Helper()
	origTS, origNS := newTrajectoryStore, newTrajectoryNodeStore
	newTrajectoryStore = func(store.Querier) trajectoryStore { return ts }
	newTrajectoryNodeStore = func(store.Querier) trajectoryNodeStore { return ns }
	t.Cleanup(func() { newTrajectoryStore, newTrajectoryNodeStore = origTS, origNS })
}

// trajectoryHarness wires a Deps, points the trajectory + session store seams at
// the fakes, mounts the router, and returns a ready Authorization header for
// orgID.
func trajectoryHarness(t *testing.T, ts trajectoryStore, ns trajectoryNodeStore, sess sessionStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setTrajectoryStores(t, ts, ns)
	if sess == nil {
		sess = &fakeSessionStore{}
	}
	setSessionStore(t, sess)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

func storeTrajectory(id, org, sessionID string) store.Trajectory {
	sid := sessionID
	return store.Trajectory{ID: id, OrgID: org, SessionID: &sid, MetadataJSON: json.RawMessage("{}")}
}

func storeTrajNode(id, trajID string, parentID *string, eventID, eventType string, fc store.TrajectoryFieldClass, metadata string, occurred time.Time) store.TrajectoryNode {
	return store.TrajectoryNode{
		ID:           id,
		TrajectoryID: trajID,
		ParentID:     parentID,
		EventID:      eventID,
		EventType:    eventType,
		FieldClass:   fc,
		MetadataJSON: json.RawMessage(metadata),
		OccurredAt:   occurred,
	}
}

func sessionForTraj(id, org string) store.Session {
	return storeSession(id, org, "a1", nil, store.SessionStatusActive, 1, time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC), nil)
}

// TestIngestTrajectoryCreatesTrajectory proves the first ingest finds no
// trajectory, creates one scoped to the caller's org and the URL session, and
// stores each structure event — resolving a child's parent_event_id to the
// parent node's id.
func TestIngestTrajectoryCreatesTrajectory(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 10, 1, 0, 0, time.UTC)
	ts := &fakeTrajectoryStore{getErr: store.ErrNotFound, createTraj: storeTrajectory("t1", "org-1", "s1")}
	ns := &fakeTrajectoryNodeStore{}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
	h, hdr := trajectoryHarness(t, ts, ns, sess, "org-1")

	body := fmt.Sprintf(`{"events":[
		{"event_id":"e1","type":"session_start","occurred_at":%q,"field_class":"structure"},
		{"event_id":"e2","type":"tool_call","occurred_at":%q,"field_class":"structure","parent_event_id":"e1",
		 "fields":[{"key":"cmd","field_class":"structure","value_type":"string","byte_size":12}]}
	]}`, t1.Format(time.RFC3339), t2.Format(time.RFC3339))

	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/trajectory", body, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}

	var ack apiIngestAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ack.Accepted != 2 || ack.Rejected != 0 {
		t.Fatalf("ack = %+v, want accepted 2 rejected 0", ack)
	}
	if ts.createCalls != 1 {
		t.Fatalf("trajectory create calls = %d, want 1", ts.createCalls)
	}
	if ts.gotCreate.OrgID != "org-1" {
		t.Errorf("trajectory org = %q, want org-1 (from token)", ts.gotCreate.OrgID)
	}
	if ts.gotCreate.SessionID == nil || *ts.gotCreate.SessionID != "s1" {
		t.Errorf("trajectory session = %v, want s1 (from URL)", ts.gotCreate.SessionID)
	}
	if len(ns.gotCreates) != 2 {
		t.Fatalf("node creates = %d, want 2", len(ns.gotCreates))
	}
	if ns.gotCreates[0].ParentID != nil {
		t.Errorf("root node parent = %v, want nil", ns.gotCreates[0].ParentID)
	}
	if ns.gotCreates[1].ParentID == nil || *ns.gotCreates[1].ParentID != "n1" {
		t.Errorf("child node parent = %v, want n1 (e1's node id)", ns.gotCreates[1].ParentID)
	}
	if ns.gotCreates[1].EventType != "tool_call" {
		t.Errorf("child node type = %q, want tool_call", ns.gotCreates[1].EventType)
	}
	// The child's fields are captured as structure metadata.
	var meta trajectoryNodeMetadata
	if err := json.Unmarshal(ns.gotCreates[1].MetadataJSON, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if len(meta.Fields) != 1 || meta.Fields[0].Key != "cmd" || meta.Fields[0].ByteSize == nil || *meta.Fields[0].ByteSize != 12 {
		t.Errorf("metadata fields = %+v, want one cmd/byte_size=12 field", meta.Fields)
	}
}

// TestIngestTrajectoryReusesTrajectory proves a session that already has a
// trajectory is not re-created: GetBySessionID hits and Create is never called.
func TestIngestTrajectoryReusesTrajectory(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	ts := &fakeTrajectoryStore{getTraj: storeTrajectory("t1", "org-1", "s1")}
	ns := &fakeTrajectoryNodeStore{}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
	h, hdr := trajectoryHarness(t, ts, ns, sess, "org-1")

	body := fmt.Sprintf(`{"events":[{"event_id":"e1","type":"fork","occurred_at":%q,"field_class":"structure"}]}`, t1.Format(time.RFC3339))
	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/trajectory", body, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if ts.createCalls != 0 {
		t.Errorf("trajectory create calls = %d, want 0 (reused existing)", ts.createCalls)
	}
	if len(ns.gotCreates) != 1 || ns.gotCreates[0].TrajectoryID != "t1" {
		t.Errorf("node create trajectory = %+v, want t1", ns.gotCreates)
	}
}

// TestIngestTrajectoryResidencyRejections proves the v1 structure-only policy:
// a payload-class event, a structure event carrying a raw field value, and an
// invalid field_class are each counted rejected without aborting the batch,
// while the valid structure events around them are stored.
func TestIngestTrajectoryResidencyRejections(t *testing.T) {
	ttime := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	ts := &fakeTrajectoryStore{getErr: store.ErrNotFound, createTraj: storeTrajectory("t1", "org-1", "s1")}
	ns := &fakeTrajectoryNodeStore{}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
	h, hdr := trajectoryHarness(t, ts, ns, sess, "org-1")

	body := fmt.Sprintf(`{"events":[
		{"event_id":"ok1","type":"tool_call","occurred_at":%q,"field_class":"structure"},
		{"event_id":"pay","type":"tool_call","occurred_at":%q,"field_class":"payload"},
		{"event_id":"leak","type":"tool_call","occurred_at":%q,"field_class":"structure",
		 "fields":[{"key":"blob","field_class":"payload","value":"secret"}]},
		{"event_id":"bad","type":"tool_call","occurred_at":%q,"field_class":"bogus"},
		{"event_id":"ok2","type":"rollback","occurred_at":%q,"field_class":"structure"}
	]}`, ttime, ttime, ttime, ttime, ttime)

	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/trajectory", body, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var ack apiIngestAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ack.Accepted != 2 || ack.Rejected != 3 {
		t.Fatalf("ack = %+v, want accepted 2 rejected 3", ack)
	}
	// Only the two valid structure events reached the store.
	if len(ns.gotCreates) != 2 {
		t.Errorf("node creates = %d, want 2 (rejections never stored)", len(ns.gotCreates))
	}
}

// TestIngestTrajectoryDuplicateRejected proves an event whose (trajectory,
// event_id) already exists is a rejected (idempotent) re-ingest, not an error.
func TestIngestTrajectoryDuplicateRejected(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	ts := &fakeTrajectoryStore{getTraj: storeTrajectory("t1", "org-1", "s1")}
	ns := &fakeTrajectoryNodeStore{nodes: []store.TrajectoryNode{
		storeTrajNode("n1", "t1", nil, "e1", "session_start", store.TrajectoryFieldClassStructure, "{}", t1),
	}}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
	h, hdr := trajectoryHarness(t, ts, ns, sess, "org-1")

	body := fmt.Sprintf(`{"events":[
		{"event_id":"e1","type":"session_start","occurred_at":%q,"field_class":"structure"},
		{"event_id":"e2","type":"fork","occurred_at":%q,"field_class":"structure"}
	]}`, t1.Format(time.RFC3339), t1.Add(time.Minute).Format(time.RFC3339))

	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/trajectory", body, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var ack apiIngestAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ack.Accepted != 1 || ack.Rejected != 1 {
		t.Errorf("ack = %+v, want accepted 1 (e2) rejected 1 (duplicate e1)", ack)
	}
}

// TestIngestTrajectoryCrossOrgSession proves a session owned by another org is a
// 404 (not 403) and nothing is recorded.
func TestIngestTrajectoryCrossOrgSession(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	ts := &fakeTrajectoryStore{}
	ns := &fakeTrajectoryNodeStore{}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-2")}
	h, hdr := trajectoryHarness(t, ts, ns, sess, "org-1")

	body := fmt.Sprintf(`{"events":[{"event_id":"e1","type":"fork","occurred_at":%q,"field_class":"structure"}]}`, t1)
	rec := doJSON(t, h, http.MethodPost, "/sessions/s1/trajectory", body, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if ts.getCalls != 0 || ts.createCalls != 0 || len(ns.gotCreates) != 0 {
		t.Error("trajectory work happened for a cross-org session")
	}
}

func TestIngestTrajectoryMissingSession(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	sess := &fakeSessionStore{getErr: store.ErrNotFound}
	h, hdr := trajectoryHarness(t, &fakeTrajectoryStore{}, &fakeTrajectoryNodeStore{}, sess, "org-1")

	body := fmt.Sprintf(`{"events":[{"event_id":"e1","type":"fork","occurred_at":%q,"field_class":"structure"}]}`, t1)
	rec := doJSON(t, h, http.MethodPost, "/sessions/ghost/trajectory", body, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestIngestTrajectoryBadBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty events", `{"events":[]}`},
		{"missing events", `{}`},
		{"malformed json", `{`},
		{"unknown field", `{"events":[],"bogus":1}`},
		{"bad occurred_at", `{"events":[{"event_id":"e1","type":"fork","occurred_at":"nope","field_class":"structure"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
			h, hdr := trajectoryHarness(t, &fakeTrajectoryStore{}, &fakeTrajectoryNodeStore{}, sess, "org-1")
			rec := doJSON(t, h, http.MethodPost, "/sessions/s1/trajectory", tc.body, hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestListTrajectory proves the read path maps stored nodes back to API events
// in replay order and renders a child's parent_event_id from its parent_id.
func TestListTrajectory(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 10, 1, 0, 0, time.UTC)
	n1 := storeTrajNode("n1", "t1", nil, "e1", "session_start", store.TrajectoryFieldClassStructure, "{}", t1)
	n2 := storeTrajNode("n2", "t1", strptr("n1"), "e2", "tool_call", store.TrajectoryFieldClassStructure,
		`{"fields":[{"key":"cmd","field_class":"structure","value_type":"string","byte_size":7}]}`, t2)
	ts := &fakeTrajectoryStore{getTraj: storeTrajectory("t1", "org-1", "s1")}
	// Deliberately out of order to prove the handler sorts by (occurred_at, id).
	ns := &fakeTrajectoryNodeStore{nodes: []store.TrajectoryNode{n2, n1}}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
	h, hdr := trajectoryHarness(t, ts, ns, sess, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions/s1/trajectory", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body apiTrajectoryEventList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[0].EventID != "e1" || body.Items[1].EventID != "e2" {
		t.Errorf("order = %q,%q, want e1,e2", body.Items[0].EventID, body.Items[1].EventID)
	}
	if body.Items[0].ParentEventID != nil {
		t.Errorf("root parent_event_id = %v, want nil", body.Items[0].ParentEventID)
	}
	if body.Items[1].ParentEventID == nil || *body.Items[1].ParentEventID != "e1" {
		t.Errorf("child parent_event_id = %v, want e1", body.Items[1].ParentEventID)
	}
	if body.Items[1].Type != "tool_call" || len(body.Items[1].Fields) != 1 || body.Items[1].Fields[0].Key != "cmd" {
		t.Errorf("child event = %+v, want tool_call with cmd field", body.Items[1])
	}
	if body.NextCursor != nil {
		t.Errorf("next_cursor = %v, want null on a full read", body.NextCursor)
	}
}

// TestListTrajectoryPage proves the in-memory keyset lookahead: with limit=1 the
// first page returns one event and a next_cursor, and paging with that cursor
// returns the following event.
func TestListTrajectoryPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 10, 1, 0, 0, time.UTC)
	n1 := storeTrajNode("n1", "t1", nil, "e1", "session_start", store.TrajectoryFieldClassStructure, "{}", t1)
	n2 := storeTrajNode("n2", "t1", nil, "e2", "fork", store.TrajectoryFieldClassStructure, "{}", t2)
	ts := &fakeTrajectoryStore{getTraj: storeTrajectory("t1", "org-1", "s1")}
	ns := &fakeTrajectoryNodeStore{nodes: []store.TrajectoryNode{n1, n2}}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
	h, hdr := trajectoryHarness(t, ts, ns, sess, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions/s1/trajectory?limit=1", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var page1 apiTrajectoryEventList
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page1.Items) != 1 || page1.Items[0].EventID != "e1" {
		t.Fatalf("page1 items = %+v, want just e1", page1.Items)
	}
	if page1.NextCursor == nil {
		t.Fatal("page1 next_cursor = nil, want a cursor")
	}
	cur, err := DecodeCursor(*page1.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if cur.ID != "n1" || !cur.CreatedAt.Equal(t1) {
		t.Errorf("cursor = (%v,%s), want (%v,n1)", cur.CreatedAt, cur.ID, t1)
	}

	rec = doJSON(t, h, http.MethodGet, "/sessions/s1/trajectory?limit=1&cursor="+*page1.NextCursor, "", hdr)
	var page2 apiTrajectoryEventList
	if err := json.Unmarshal(rec.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].EventID != "e2" {
		t.Fatalf("page2 items = %+v, want just e2", page2.Items)
	}
	if page2.NextCursor != nil {
		t.Errorf("page2 next_cursor = %v, want null (last page)", page2.NextCursor)
	}
}

// TestListTrajectoryNoTrajectory proves a session with nothing ingested yet is
// an empty (but valid) page, not a 404, and items serialises as [] not null.
func TestListTrajectoryNoTrajectory(t *testing.T) {
	ts := &fakeTrajectoryStore{getErr: store.ErrNotFound}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
	h, hdr := trajectoryHarness(t, ts, &fakeTrajectoryNodeStore{}, sess, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions/s1/trajectory", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !containsItemsArray(got) {
		t.Errorf("body = %s, want items:[] present", got)
	}
}

func TestListTrajectoryCrossOrgSession(t *testing.T) {
	ts := &fakeTrajectoryStore{}
	sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-2")}
	h, hdr := trajectoryHarness(t, ts, &fakeTrajectoryNodeStore{}, sess, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions/s1/trajectory", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if ts.getCalls != 0 {
		t.Error("trajectory lookup happened for a cross-org session")
	}
}

func TestListTrajectoryBadParams(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"non-integer limit", "/sessions/s1/trajectory?limit=abc"},
		{"malformed cursor", "/sessions/s1/trajectory?cursor=notacursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := &fakeSessionStore{getSession: sessionForTraj("s1", "org-1")}
			h, hdr := trajectoryHarness(t, &fakeTrajectoryStore{}, &fakeTrajectoryNodeStore{}, sess, "org-1")
			rec := doJSON(t, h, http.MethodGet, tc.path, "", hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTrajectoryRoutesRequireAuth proves both telemetry routes sit behind
// RequireAuth: an unauthenticated request is a 401 and never reaches the store.
func TestTrajectoryRoutesRequireAuth(t *testing.T) {
	tests := []struct {
		method, path string
	}{
		{http.MethodPost, "/sessions/s1/trajectory"},
		{http.MethodGet, "/sessions/s1/trajectory"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setTrajectoryStores(t, &fakeTrajectoryStore{}, &fakeTrajectoryNodeStore{})
			setSessionStore(t, &fakeSessionStore{})
			h := mountRouter(deps)
			rec := doJSON(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}
