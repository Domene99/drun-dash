# drun-dash Control Plane API

This directory holds the OpenAPI contract for the drun-dash control plane REST
API. The spec is the **source of truth** for the interface shared by the CLI,
the admin UI server, and (for auth/grant semantics) the LLM proxy.

- [`v1.yaml`](./v1.yaml) — OpenAPI 3.1 contract for the `v1` API.

> Status: this is a **contract skeleton**. Every operation is described with
> request/response schemas, but the reference server is not implemented yet, so
> unimplemented operations are documented as returning `501 Not Implemented`.

## Domains

| Tag       | Purpose |
|-----------|---------|
| Auth      | Device flow for the CLI, OIDC session exchange for the admin UI, `whoami`. |
| Agents    | Catalog CRUD, immutable versions, per-principal entitlements. |
| Policies  | Versioned, signed policy boundaries. |
| Sessions  | Launch a governed session (returns the signed policy bundle + scoped proxy credentials) and manage its lifecycle. |
| Telemetry | Trajectory event ingest — structure-only by default, payload capture opt-in per field. |
| Approvals | Human-in-the-loop escalation queue. |
| Analytics | Spend and budget queries. |

## Validate & lint

The spec validates in the [Swagger Editor](https://editor-next.swagger.io/)
(paste the contents of `v1.yaml`) and lints with Spectral. No Node application
code is committed to this repo — Spectral is run ad hoc via `npx`:

```sh
# From the repository root:
npx @stoplight/spectral-cli lint openapi/v1.yaml
```

The lint ruleset lives in [`.spectral.yaml`](../.spectral.yaml) at the repo
root; it extends `spectral:oas` and adds documentation and path-style rules.

## Versioning policy

The API is versioned by **major version in the URL path** (`/v1`, `/v2`, ...).
Each major version is a separate document in this directory (`v1.yaml`,
`v2.yaml`, ...).

**Within a major version we only make backward-compatible changes:**

- ✅ Add a new path, operation, optional request field, or response field.
- ✅ Add a new optional query parameter or a new enum value the client can
  safely ignore.
- ✅ Relax a validation constraint (e.g. raise a maximum).
- ✅ Document a previously undocumented error response.

**Breaking changes require a new major version (`/v2`):**

- ❌ Remove or rename a path, operation, field, or enum value.
- ❌ Make an optional field required, or tighten a validation constraint.
- ❌ Change a field's type or the semantics of an existing field.
- ❌ Change authentication or authorization requirements for an operation.

### `info.version` vs. the URL version

- The URL segment (`/v1`) is the **major** contract version and changes only on
  a breaking change.
- `info.version` in `v1.yaml` uses **semver** (`MAJOR.MINOR.PATCH`) to track the
  document itself: bump the **minor** for additive changes and the **patch** for
  editorial/documentation-only changes. The `MAJOR` stays in lockstep with the
  URL version.

### Deprecation

Before removing anything in a future major version, mark it `deprecated: true`
in the current version and keep it working for at least one release cycle.
Announce deprecations in release notes with a migration path.

## Consumers

- **`drun-dash` CLI (Go)** — authenticates via the OAuth 2.0 device flow
  (`/auth/device/*`) and calls `/sessions/launch` to obtain a policy bundle and
  scoped proxy credentials.
- **Admin UI server (Go + templ)** — authenticates operators via OIDC, exchanges
  the session at `/auth/session`, and drives the approval inbox and analytics.
- **LLM proxy** — a separate process that shares the auth/grant model; it
  validates the `proxy_token` minted at session launch.
