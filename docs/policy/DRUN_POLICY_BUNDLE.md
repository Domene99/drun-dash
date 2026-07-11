# drun Policy Bundle Format (v1.0)

drun-dash distributes **signed policy bundles** to fleets of drun sandboxes.
A bundle is a JSON document that compiles down to a drun `~/.drun/config.toml`
and carries the metadata the control plane needs to verify authenticity and
freshness.

The control plane is **fail-closed**: a session only receives proxy
credentials (the upstream tokens brokered by drun-dash for `session_fetch`,
package registries, etc.) after its bundle validates. No valid bundle → no
credentials → the sandbox runs fully offline. See
[Fail-closed behavior](#fail-closed-behavior).

> **Scope of this document.** This batch specifies the *format* and ships
> validated [test fixtures](../../testdata/policy/). Signing and verification
> code, and the Go types that load these bundles, land in later batches. The
> `signature` field is documented here but the fixtures carry a non-verifying
> placeholder (see [Signature](#signature)).

## 1. Top-level structure

A bundle is a single JSON object with exactly these fields:

| Field            | Type    | Required | Description |
|------------------|---------|----------|-------------|
| `schema_version` | string  | yes | Bundle-format version. `"1.0"` for this spec. Consumers MUST reject a bundle whose major version they do not understand. |
| `org_id`         | string  | yes | Stable identifier of the owning organization (e.g. `org_acme`). |
| `policy_id`      | string  | yes | Stable identifier of the policy within the org (e.g. `pol_standard_dev`). A given `policy_id` may be re-issued many times with increasing `policy_version`. |
| `policy_version` | integer | yes | Monotonically increasing version for this `policy_id`. Consumers reject a bundle older than the newest they have already accepted (rollback protection). |
| `drun_config`    | object  | yes | The compiled drun configuration. See [§2](#2-the-drun_config-object). |
| `issued_at`      | string  | yes | RFC 3339 / ISO 8601 UTC timestamp (`Z`) when the bundle was signed. |
| `expires_at`     | string  | yes | RFC 3339 / ISO 8601 UTC timestamp after which the bundle is invalid. MUST be strictly after `issued_at`. |
| `content_hash`   | string  | yes | `sha256:` + hex SHA-256 of the canonical `drun_config` JSON. See [§3](#3-content_hash). |
| `signature`      | string  | yes | `ed25519:` + base64 Ed25519 signature over the signing input. See [§4](#4-signature). |

Unknown top-level fields MUST be rejected (bundles are strict; extra fields are
treated as tampering).

## 2. The `drun_config` object

These fields map one-to-one onto keys in drun's `~/.drun/config.toml`. Every
field is optional inside `drun_config`; an omitted field means "drun default".
When present, a field fully replaces the corresponding drun default (arrays are
not merged).

| Field                    | Type              | drun config.toml key       | Purpose |
|--------------------------|-------------------|----------------------------|---------|
| `domain_allowlist`       | array of string   | `domain_allowlist`         | Domains reachable via `session_fetch`. Entries may be exact hosts (`github.com`) or leading-wildcard patterns (`*.githubusercontent.com`). |
| `mount_allowlist`        | array of string   | `mount_allowlist`          | Absolute host paths that `session_mount` is permitted to read. |
| `mount_overlay_paths`    | array of string   | `mount_overlay_paths`      | Directory names symlinked read-only into the workspace (e.g. `node_modules`, `.venv`). |
| `env_allowlist`          | array of string   | `env_allowlist`            | Host environment variable names exposed via `session_get_env`. |
| `bash_command_denylist`  | array of string   | `bash_command_denylist`    | Command basenames that `session_bash` must refuse. Takes precedence over the allowlist. |
| `bash_command_allowlist` | array of string   | `bash_command_allowlist`   | If non-empty, only these command basenames may run (allowlist mode). If empty/omitted, all commands are allowed except those in the denylist (denylist mode). |
| `bash_timeout_ms`        | integer (ms)      | `bash_timeout_ms`          | Max wall-clock time for a single `session_bash` command. |
| `fetch_timeout_ms`       | integer (ms)      | `fetch_timeout_ms`         | Max total time for a `session_fetch` request. |
| `connect_timeout_ms`     | integer (ms)      | `connect_timeout_ms`       | Max TCP connect time for outbound requests. |
| `max_workspace_mb`       | integer (MB)      | `max_workspace_mb`         | Hard cap on total workspace size. |
| `max_sessions`           | integer           | `max_sessions`             | Max concurrent sessions per host. |
| `max_checkpoints`        | integer           | `max_checkpoints`          | Max retained checkpoints per session. |
| `session_idle_timeout_secs` | integer (s)    | `session_idle_timeout_secs`| Idle time before a session is reaped. |

### Command policy precedence

`bash_command_denylist` and `bash_command_allowlist` combine as follows:

1. If the command basename is in `bash_command_denylist` → **denied**.
2. Otherwise, if `bash_command_allowlist` is non-empty and the basename is
   **not** in it → **denied**.
3. Otherwise → **allowed**.

So the denylist always wins, and a non-empty allowlist switches the sandbox
into strict whitelist mode. The `strict` fixture uses whitelist mode; the
`standard` and `permissive` fixtures use denylist-only mode.

## 3. `content_hash`

`content_hash` is `sha256:` followed by the lowercase hex SHA-256 digest of the
**canonical JSON serialization of the `drun_config` object only** (not the whole
bundle).

Canonicalization follows JCS (RFC 8785) essentials:

- Object keys sorted lexicographically, recursively.
- No insignificant whitespace (`,` and `:` separators only).
- UTF-8 encoding.
- **Array order is preserved** — it is semantically significant and never
  sorted.

Reference (Python):

```python
import hashlib, json
canon = json.dumps(drun_config, sort_keys=True,
                   separators=(",", ":"), ensure_ascii=False).encode("utf-8")
content_hash = "sha256:" + hashlib.sha256(canon).hexdigest()
```

A consumer MUST recompute the hash from the received `drun_config` and reject
the bundle if it does not match `content_hash`. This binds the config to the
signature (which signs the hash, not the pretty-printed bytes), so bundles can
be re-formatted in transit without breaking verification, while any change to
config *content* is detected.

## 4. `signature`

`signature` is `ed25519:` followed by the base64 (standard, with padding)
encoding of a 64-byte Ed25519 signature.

**Signing input.** The signature is computed over the ASCII bytes of the
following string, which binds the identity, freshness, and content of the
bundle together:

```
drun-policy-bundle/v1\n
<schema_version>\n
<org_id>\n
<policy_id>\n
<policy_version>\n
<issued_at>\n
<expires_at>\n
<content_hash>
```

(One field per line, no trailing newline after `content_hash`.) Because the
signing input includes `content_hash`, and `content_hash` covers the canonical
`drun_config`, a valid signature authenticates the entire bundle.

The signing keypair is per-org; the control plane distributes the Ed25519
**public** key out of band. **Key management, signing, and verification are out
of scope for this batch** and will be implemented later. The shipped fixtures
carry a structurally-shaped but deliberately non-verifying placeholder
signature so that field-parsing can be exercised without a real key.

## 5. Validation order (fail-closed)

A consumer MUST perform these checks, in order, and treat *any* failure as a
hard reject:

1. `schema_version` major version is understood.
2. All required fields present; no unknown fields.
3. `issued_at` ≤ now ≤ `expires_at` (with a small clock-skew allowance).
4. `policy_version` ≥ the newest previously accepted version for this
   `policy_id` (rollback protection).
5. Recomputed canonical hash of `drun_config` equals `content_hash`.
6. `signature` verifies against the org public key over the signing input.

Only if all checks pass is the bundle compiled to `config.toml` and are proxy
credentials released.

## Fail-closed behavior

drun-dash never degrades open. If a bundle is missing, expired, malformed,
tampered (hash mismatch), rolled back, or carries an invalid signature, the
control plane:

- withholds all brokered proxy credentials, and
- refuses to compile a `config.toml`.

The practical effect is that the sandbox has **no network egress and no host
mounts beyond drun's own built-in defaults** until a valid bundle is presented.
There is no "allow on error" path: an unverifiable policy is treated exactly
like the most restrictive policy possible.

## Example

See [`testdata/policy/`](../../testdata/policy/) for three complete, valid
example bundles (`strict`, `standard`, `permissive`) and a
[README](../../testdata/policy/README.md) explaining how each maps to a drun
`config.toml`.
