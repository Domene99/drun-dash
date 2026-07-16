# 0009 — Writable, encrypted-at-rest secret vault

- **Status:** Accepted
- **Date:** 2026-07-16
- **Deciders:** drun-dash control-plane / llm-proxy work streams
- **Batch:** 29

## Context

A freshly signed-up org (Batch 27/28 local auth) can create agents, but a
created agent **cannot run**: launching a session
(`internal/httpapi/launch.go`, ADR-0003 §1.5) requires the pinned agent
version to name a **non-nil `endpoint_id`**, and the only credential backend on
`main` is the **read-only `EnvVault`** (`internal/vault/envvault.go`): a
`vault://dev/<name>` ref resolves to the process environment variable
`DRUNDASH_SECRET_<NAME>`. There is therefore **no in-product way** for an org to
register a model endpoint and supply its real Anthropic/OpenAI key — the key can
only be injected as an environment variable set by whoever operates the process.

The custody model this must preserve is the whole point of the vault package
(`internal/vault/vault.go`): master provider keys never reach a developer's
machine or the sandbox; the control plane holds them and the llm-proxy brokers
every upstream call, resolving `model_endpoints.secret_ref` → a live `Secret`
per request (`internal/proxy/broker.go`). `secret_ref` is opaque — the raw key
is never persisted in Postgres — and `vault.Secret` redacts itself under every
`fmt` verb so it cannot leak through logs.

What already exists and constrains this decision:

- **`model_endpoints`** (migration `000003`): `secret_ref TEXT` already exists
  and is opaque; unique per `(org_id, name)`. The store
  (`internal/store/model_endpoints.go`) has `Create`/`GetByID`/`ListByOrg`/
  `SetActive`.
- **`vault.Vault`**: `Resolve(ctx, secretRef) (Secret, error)`, with sentinels
  `ErrNotFound` and `ErrInvalidRef`. `EnvVault` serves `vault://dev/*`.
- **The proxy** constructs `vault.NewEnvVault()` internally and resolves a
  grant's endpoint → `secret_ref` → vault per call.
- **RBAC**: `store.PermEndpointsManage` (`"endpoints.manage"`) is defined and
  seeded to Owner/Admin; org scoping and error mapping follow ADR-0002.

This ADR closes the gap for **local / self-hosted** deployments: a REST API to
manage `model_endpoints`, and a **writable, encrypted-at-rest** secret backend
so a key entered once (via the admin UI, sibling issue) actually reaches the
proxy — without env-var gymnastics and without ever weakening custody.

## Decision drivers

- **Configure keys through the product**, not by editing process environment
  variables — the self-hosting workflow the owner asked for.
- **Never store plaintext.** Keys are encrypted at rest; only sealed bytes hit
  Postgres. The existing custody guarantees hold: keys never go to the browser,
  `Secret` stays redacted, nothing logs the plaintext.
- **Additive, no rewrite, no new dependencies.** `EnvVault` and `vault://dev/*`
  keep working; encryption uses only Go's standard library
  (`crypto/aes`, `crypto/cipher`, `crypto/rand`, `encoding/base64`).
- **Fail loud, boot anyway.** With no master key the service still starts;
  db-backed secrets are simply unavailable and the API says so clearly, rather
  than crashing or silently storing plaintext.

## Decision

### 1. A writable DB-backed vault, encrypted with AES-256-GCM

Add a new backend, **`DBVault`**, that stores provider credentials **encrypted
at rest** in a new `secrets` table and references them by a
**`vault://db/<uuid>`** ref (the `<uuid>` is the secret row's id). Sealing uses
**AES-256-GCM** (authenticated encryption) with a **fresh random 12-byte
nonce** per secret; the row stores the `ciphertext` and `nonce` as `bytea`. The
raw key exists only transiently in memory during a seal or open and is returned
wrapped in the redacting `vault.Secret`.

`DBVault` implements `vault.Vault` and adds the write side the control plane
needs:

- `Resolve(ctx, "vault://db/<uuid>")` — load the row, GCM-open under the master
  key, return `Secret{Value: plaintext}`. Malformed ref → `ErrInvalidRef`;
  missing row → `ErrNotFound`; no key configured → `ErrKeyNotConfigured`.
- `Seal(ctx, orgID, plaintext)` — GCM-seal with a fresh nonce, insert, return
  `"vault://db/" + id`.
- `Reseal(ctx, secretRef, plaintext)` — overwrite an existing ref's ciphertext
  (rotate a credential in place without changing `secret_ref`).
- `Delete(ctx, secretRef)` — drop the backing row when its endpoint is removed
  (no master key required — the ciphertext is discarded, not opened).

### 2. Key management — `DRUNDASH_VAULT_KEY`

The master key is read from the environment variable **`DRUNDASH_VAULT_KEY`**,
a **base64-encoded 32-byte** value (AES-256). It is decoded once at boot in each
binary. If it is **unset or malformed**, the DB vault is constructed in a
**disabled** state: `Seal`/`Reseal`/`Resolve` return **`ErrKeyNotConfigured`**,
and the API surfaces that on a key-storing request as a **400** telling the
operator to set the variable. The service still boots and `vault://dev/*` keeps
working, so a missing key degrades exactly one capability rather than the whole
plane. The same key must be configured on **both** the control plane (which
seals) and the proxy (which opens), or db-backed refs will not resolve.

The key is never persisted, never logged, and never sent to any client. Rotating
the key is out of scope here (a later ADR): the ciphertext is not
self-describing, so a key change requires resealing existing secrets, for which
`Reseal` is the building block.

### 3. `CompositeVault` routes by ref scheme

A **`CompositeVault`** implements `vault.Vault.Resolve` by routing on the ref's
backend segment: `vault://db/*` → `DBVault`, `vault://dev/*` → `EnvVault`,
anything else → `ErrInvalidRef`. It is the single vault the **llm-proxy** holds,
so a key sealed by the control plane (`vault://db/*`) and a dev key
(`vault://dev/*`) both resolve at call time with no per-call branching in the
broker. The proxy's hardcoded `vault.NewEnvVault()` is replaced by an
**injected** `vault.Vault` (a new parameter on `NewRouter`/`newBroker`); a nil
vault falls back to `EnvVault` for backward compatibility.

### 4. Endpoints REST API

New handlers under the protected router, org-scoped per ADR-0002, with the
`ModelEndpoint` response carrying **`secret_set` (bool)** and **never** the key
or `secret_ref`:

- `GET /model-endpoints`, `GET /model-endpoints/{id}` — member-visible.
- `POST /model-endpoints` — requires `endpoints.manage`. A non-empty `api_key`
  is `Seal`ed → `secret_ref`; empty means "no key yet". Dup `(org, name)` → 409;
  `api_key` with no master key → 400.
- `PATCH /model-endpoints/{id}` — requires `endpoints.manage`. A non-empty
  `api_key` rotates the sealed secret (`Reseal` when a `vault://db/*` ref exists,
  else `Seal`); `active` toggles via `SetActive`.
- `DELETE /model-endpoints/{id}` — requires `endpoints.manage`. Deletes the
  endpoint and its backing `secrets` row when the ref is `vault://db/*`.

### 5. Schema — new `secrets` table (migration `000009`)

```
CREATE TABLE secrets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    ciphertext BYTEA NOT NULL,   -- AES-256-GCM sealed bytes; never plaintext
    nonce      BYTEA NOT NULL,   -- the 12-byte GCM nonce
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX secrets_org_id_idx ON secrets (org_id);
```

Additive and reversible; `model_endpoints` is untouched (`secret_ref` already
exists). `down` drops the table.

## Consequences

**Positive**

- An org can register a model endpoint and supply its **real** provider key
  through the product; the key is sealed once and resolves at proxy time, so a
  pinned agent version can actually launch and run.
- Keys are **encrypted at rest** (AES-256-GCM, per-secret nonce, authenticated),
  never stored or logged in plaintext, and never returned to a client. The
  existing custody model is preserved end to end.
- **No new dependencies** and **no rewrite**: `EnvVault` / `vault://dev/*` keep
  working, and the proxy gains the DB backend via a `CompositeVault` behind the
  same `Vault` interface.
- A missing/invalid master key **degrades gracefully**: the service boots,
  dev-backed secrets work, and a key-storing request gets a clear 400.

**Negative / trade-offs**

- **A new operational secret** (`DRUNDASH_VAULT_KEY`) must be configured
  identically on the control plane and the proxy; a mismatch makes db-backed
  refs fail to open (surfaced as a fail-closed credential error at call time,
  never a plaintext leak).
- **No key rotation flow** in this batch: `Reseal` exists as the primitive, but
  re-encrypting every secret under a new key is a later ADR. The ciphertext is
  not algorithm-tagged, so a future KDF/cipher change needs a migration.
- **A single symmetric key per deployment**, held in the environment, is the
  weakest link — appropriate for local/self-hosted use, not a substitute for a
  cloud KMS. Swapping in a KMS backend behind the same `Vault` interface remains
  a later batch.

## Alternatives considered

- **Keep only `EnvVault`.** Rejected: it forces every real key through a process
  environment variable set out-of-band, which is exactly the gap this issue
  closes — an org cannot configure a key through the product.
- **A cloud KMS / secret-manager backend now.** Deferred: it adds a dependency
  and infrastructure the self-hosted target does not require. The `Vault`
  interface and `CompositeVault` make it a drop-in later.
- **Store the key in Postgres too (envelope-encrypted).** Rejected for v1:
  without a separate key-encryption key held elsewhere it adds complexity
  without changing the trust boundary; the master key stays in the environment.
- **Per-org master keys.** Rejected for v1: one deployment key is simpler and
  matches the self-hosted single-tenant-per-instance target; per-org keys are a
  later refinement if multi-tenant isolation of the KEK is required.

## References

- [ADR-0002](0002-rest-api-conventions.md) — org scoping, error mapping,
  DTO mapping the endpoints API follows.
- [ADR-0003](0003-session-launch-and-credential-grant.md) — the launch/grant
  model and the per-call credential resolution the proxy performs.
- [ADR-0006](0006-rbac-and-access-model.md) — `endpoints.manage` and the
  permission gates on the write routes.
- `internal/vault/vault.go` — the `Vault` interface, `Secret` redaction, and the
  `ErrNotFound` / `ErrInvalidRef` sentinels; `envvault.go` — the `vault://dev/*`
  backend this ADR leaves intact.
- `internal/store/model_endpoints.go`, `migrations/000003_*` — the
  `model_endpoints` table and `secret_ref` this builds on.
- `internal/proxy/broker.go` — the per-call resolve the injected
  `CompositeVault` now serves.
