# cmd/policyctl

`policyctl` is the operator-side CLI for **drun-dash signed policy bundles**. It
generates Ed25519 keypairs, signs bundles, and verifies signed bundles. It is
the signing counterpart to the verification-only
[`pkg/policy`](../../pkg/policy) library — `policyctl` produces exactly what the
control plane later verifies with `policy.Load` + `policy.Verify`.

```
go build -o policyctl ./cmd/policyctl
```

Keys are **base64-encoded raw Ed25519 bytes**: a 32-byte public key and a
64-byte private key. Timestamps are RFC 3339 UTC (e.g. `2026-07-11T00:00:00Z`).

## Subcommands

### `keygen` — generate a keypair

```
policyctl keygen [--pub <file>] [--priv <file>]
```

Generates a fresh Ed25519 keypair (`crypto/ed25519` + `crypto/rand`). Each key
is written base64-encoded to the given file (public `0644`, private `0600`); if a
flag is omitted, that key is printed to stdout instead.

```
policyctl keygen --pub org.pub --priv org.key
```

### `sign` — sign a bundle

```
policyctl sign --key <priv> --in <bundle.json> --out <signed.json> \
  [--issued-at <ts>] [--expires-at <ts>]
```

Reads the input bundle (its `content_hash` and `signature` may be empty
placeholders), then:

1. recomputes `content_hash` with `policy.ComputeContentHash` over the **exact**
   `drun_config` bytes from the input;
2. applies `--issued-at` / `--expires-at` if given, otherwise keeps the input's;
3. computes `signature = "ed25519:" + base64(ed25519.Sign(priv, SigningInput()))`;
4. writes the assembled bundle to `--out`;
5. re-`policy.Load`s and fully `policy.Verify`s the result (against the public
   half of the signing key, at the current time) before exiting `0`, so a bundle
   that would not verify never ships silently.

```
policyctl sign --key org.key --in bundle.json --out signed.json \
  --issued-at 2026-07-11T00:00:00Z --expires-at 2026-07-18T00:00:00Z
```

### `verify` — verify a signed bundle

```
policyctl verify --pub <pub> --in <signed.json>
```

Runs the full `pkg/policy` fail-closed pipeline — structural validation,
freshness at the current time, `content_hash`, and the Ed25519 signature —
against the supplied public key. Exits `0` on success, non-zero on any failure.

```
policyctl verify --pub org.pub --in signed.json
```

## Notes

- **stdlib only.** `crypto/ed25519`, `crypto/rand`, `encoding/base64`,
  `encoding/json`, and `flag`, plus `github.com/Domene99/drun-dash/pkg/policy`.
  No external CLI framework and no new module dependencies.
- **`drun_config` is preserved verbatim.** `policyctl` keeps `drun_config` as raw
  JSON through signing, so fields it does not model are neither dropped nor
  reordered, and the recomputed `content_hash` matches what a consumer verifies.
- The `sign` freshness check in step 5 uses the current time, so signing a bundle
  whose validity window does not include "now" fails fast — set `--issued-at` /
  `--expires-at` (or the input's timestamps) to a window that brackets signing.
