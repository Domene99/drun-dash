# pkg/policy

Go library for **drun-dash signed policy bundles** — loading, structural
validation, `content_hash` verification, Ed25519 signature verification, and
compilation of a bundle's `drun_config` to a drun `~/.drun/config.toml`.

```go
import "github.com/Domene99/drun-dash/pkg/policy"
```

The bundle format is specified in
[`docs/policy/DRUN_POLICY_BUNDLE.md`](../../docs/policy/DRUN_POLICY_BUNDLE.md);
example bundles live in
[`testdata/policy/`](../../testdata/policy/). A bundle is a JSON document that
carries authenticity/freshness metadata plus a `drun_config` object that maps
one-to-one onto keys in drun's `config.toml`.

## Fail-closed verification pipeline

The control plane releases proxy credentials and compiles a `config.toml` only
after a bundle passes every check (spec §5). `Verify` runs them in order:

```go
b, err := policy.Load(data)            // strict parse + structural validation
if err != nil { /* reject */ }

pub := orgPublicKey(b.OrgID)           // Ed25519 public key, distributed OOB
if err := b.Verify(time.Now(), pub); err != nil {
    // not-yet-valid / expired / hash mismatch / bad signature -> fail closed
    return err
}

cfg := b.DrunConfig.ToTOML()           // compile to ~/.drun/config.toml bytes
```

`Verify` is equivalent to, in order:

1. `Validate` — schema major understood, required fields present, RFC 3339
   timestamps with `expires_at` after `issued_at`, hash/signature prefixes.
2. `CheckFreshness(now)` — `issued_at ≤ now ≤ expires_at`, widened by
   `DefaultClockSkew` (60s) on each side.
3. `VerifyContentHash` — recompute the canonical hash of `drun_config`.
4. `VerifySignature(pub)` — Ed25519 over the signing input.

Each step is also exported individually. `Load` performs steps 1 up front;
**rollback protection** (spec §5 step 4) needs the consumer's record of the
newest accepted `policy_version` and is the caller's responsibility.

> Signing keys land in a later batch. The shipped fixtures carry a
> non-verifying **placeholder** signature, so `VerifySignature` is exercised
> against a locally generated keypair rather than the fixtures.

## Strictness

- **Unknown fields are rejected** — both at the top level and inside
  `drun_config`. Extra fields are treated as tampering (spec §1).
- **Present-but-empty vs absent** is preserved. A `nil` slice / `nil` pointer
  in `DrunConfig` means "field omitted, use drun's default" and is left out of
  the compiled TOML; a non-nil empty slice (e.g. `bash_command_allowlist: []`,
  which selects denylist mode) compiles to `bash_command_allowlist = []`.

## `content_hash` canonicalization

`content_hash` is `sha256:` + hex SHA-256 of the **canonical JSON of
`drun_config` only** (spec §3). Canonicalization follows the JCS (RFC 8785)
essentials:

- object keys sorted lexicographically, recursively;
- separators `,` and `:` only, no insignificant whitespace;
- UTF-8, with only the RFC 8785 string escapes;
- **array order preserved** (semantically significant);
- integers emitted verbatim — JSON numbers are decoded with `json.Number` so
  large integers are never reformatted through `float64`.

This matches the `json.dumps(sort_keys=True, separators=(",", ":"),
ensure_ascii=False)` reference the fixtures were generated with. `Load` captures
the exact received `drun_config` bytes and hashes their canonicalization, so
re-formatting a bundle in transit does not break verification while any change
to config *content* is detected. `DrunConfig.CanonicalJSON` produces the same
bytes from an in-memory struct (used by `VerifyContentHash` when no raw bytes
are present, and by the future signing batch).

## TOML output

`DrunConfig.ToTOML()` emits deterministic, standard TOML — one top-level key per
present field in drun's canonical order, string slices as arrays of basic
strings and integers as bare integers. The output uses only TOML that any
spec-compliant parser (e.g. `github.com/pelletier/go-toml/v2`) accepts; the
tests parse it back and check the round-trip. A stdlib-only encoder is used
(permitted by the issue's tech stack) so the package carries **no third-party
dependencies**.
```
