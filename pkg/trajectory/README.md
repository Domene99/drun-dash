# pkg/trajectory

Go schema for **drun-dash trajectory events** — the records the CLI emits during
governed drun sessions and POSTs to the control-plane ingest API.

```
import "github.com/Domene99/drun-dash/pkg/trajectory"
```

drun-dash captures each agent run as a **checkpoint tree**, much like git
branches. A `TrajectoryEvent` is one node/edge in that tree plus the metadata
describing what happened.

## Residency boundary (product design §5.2)

The control plane ingests trajectory **structure and metadata** by default —
**not file contents**. This boundary is modeled by the `field_class` field:

| `field_class` | Carries | Default ingest |
| ------------- | ------- | -------------- |
| `structure`   | tree shape + metadata only (no raw content) | **accepted** |
| `payload`     | additionally carries raw content (e.g. file bytes) | **rejected** |

`payload` events cross the residency boundary, so the default ingest path
rejects them. They are accepted only when the caller explicitly opts in — for an
authorized, payload-bearing ingest flow.

```go
// Default ingest path — structure only.
err := trajectory.ValidateForIngest(&ev, false)
// err == trajectory.ErrPayloadNotAllowed if ev.FieldClass == "payload"

// Explicitly authorized payload ingest.
err = trajectory.ValidateForIngest(&ev, true)
```

Two invariants are always enforced by `Validate` (and therefore by
`ValidateForIngest`):

1. A `structure` event **must not** include a `payload` field
   (`ErrStructureHasPayload`).
2. On the default path, a `payload` event is rejected
   (`ErrPayloadNotAllowed`).

## Event schema

| Field | Type | Notes |
| ----- | ---- | ----- |
| `schema_version` | int | Current version is `trajectory.SchemaVersion`. |
| `event_id` | UUID string | Idempotency key for ingest. |
| `trajectory_id` | string | The checkpoint tree this event belongs to. |
| `session_id` | string | drun sandbox session id. |
| `node_id` | string | This event's node in the tree. |
| `parent_node_id` | string | Tree edge for the fork/rollback graph; empty for the root. |
| `checkpoint_id` | string | drun checkpoint id. |
| `tool_name` | string | drun MCP tool (`session_bash`, `session_fetch`, …); required for `tool_call`/`policy_block`. |
| `actor_member_id` | string | Governed member who acted. |
| `timestamp` | RFC3339 string | When the event occurred. |
| `event_type` | string | `session_start` \| `tool_call` \| `policy_block` \| `fork` \| `rollback` (open set). |
| `field_class` | string | `structure` \| `payload` — the residency tier. |
| `metadata` | object | `domains`, `packages`, `token_counts`, `policy_exception`, … Unknown keys are preserved. |
| `payload` | object | Present **only** when `field_class = payload`. |

### drun concept mapping

- `session_id` → drun sandbox session
- `checkpoint_id` → point in checkpoint history
- `parent_node_id` → tree edge for the fork/rollback graph
- `tool_name` → drun MCP tool name (`session_bash`, `session_write_file`, …)

## Validation

- `(*TrajectoryEvent).Validate()` — structural validation: required fields, UUID
  `event_id`, RFC3339 `timestamp`, known `field_class`, and the
  `field_class`/`payload` invariant. Independent of ingest policy.
- `ValidateForIngest(ev, allowPayload)` — runs `Validate`, then enforces the
  residency policy above.

Failure modes are exported sentinels (`ErrMissingField`, `ErrInvalidEventID`,
`ErrInvalidTimestamp`, `ErrInvalidFieldClass`, `ErrUnsupportedSchema`,
`ErrStructureHasPayload`, `ErrPayloadNotAllowed`) — match them with
`errors.Is`.

## Fixtures

`fixtures/` holds one canonical JSON example per event type:
`session_start`, `tool_call`, `policy_block`, `fork`, `rollback`. All are
`structure` events and pass the default ingest path; the test suite validates
each one.

## Tests

```
go test ./...
```
