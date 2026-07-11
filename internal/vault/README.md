# internal/vault

Credential vault abstraction for drun-dash.

Credential custody is drun-dash's core governance lever: master LLM provider
keys never reach a developer's machine or the sandbox. The control plane holds
them and brokers every upstream model call. In the schema,
`model_endpoints.secret_ref` is an **opaque handle** into a secret store — the
raw provider key is never persisted in Postgres. This package resolves such a
handle to a live provider credential.

## The `Vault` interface

```go
type Vault interface {
    Resolve(ctx context.Context, secretRef string) (Secret, error)
}
```

`Resolve` maps a `secret_ref` to a [`Secret`](vault.go). It is total on its
input and returns one of two sentinel errors (compare with `errors.Is`):

| Error           | Meaning                                                              |
| --------------- | ------------------------------------------------------------------- |
| `ErrInvalidRef` | The ref is malformed / does not match the backend's ref grammar.    |
| `ErrNotFound`   | The ref is well-formed but no credential is registered for it.      |

### `Secret` redaction

`Secret` wraps the raw credential in its `Value` field. To keep credentials
out of logs by accident, `Secret` redacts itself under both `fmt.Stringer`
(`%s`, `%v`) and `fmt.GoStringer` (`%#v`), rendering `Secret(***)`. Read
`Value` explicitly when the raw credential is genuinely needed — never log a
`Secret`'s value.

## The `vault://` ref scheme

A `secret_ref` is a URI:

```
vault://<backend>/<name>
```

Backends interpret the space after the scheme. This package ships the
development backend, which serves the `dev` namespace only:

```
vault://dev/<name>
```

## `EnvVault` — the development backend

`EnvVault` (constructed with `NewEnvVault()`) is the documented dev backend. It
exists so local runs work without a real KMS or secret manager. It resolves
`vault://dev/<name>` by reading an environment variable:

```
vault://dev/<name>  ->  DRUNDASH_SECRET_<NAME>
```

where `<NAME>` is `<name>` **uppercased** with **hyphens replaced by
underscores**. Examples:

| Ref                     | Environment variable          |
| ----------------------- | ----------------------------- |
| `vault://dev/anthropic` | `DRUNDASH_SECRET_ANTHROPIC`   |
| `vault://dev/openai-us` | `DRUNDASH_SECRET_OPENAI_US`   |

Names must be non-empty and use only `[A-Za-z0-9_-]`; anything else (a missing
scheme, a non-`dev` backend, a nested or illegal name) returns `ErrInvalidRef`.
A valid ref whose environment variable is unset returns `ErrNotFound`.

The seed data uses `vault://dev/anthropic`, so a local stack resolves it by
exporting:

```sh
export DRUNDASH_SECRET_ANTHROPIC='sk-ant-...'
```

`EnvVault` holds no state, is safe for concurrent use, and never persists or
logs the credential.

## Production backends

Production deployments implement the **same `Vault` interface** with a cloud
KMS / secret manager backend — that lands in a later batch. Nothing outside
this package changes: callers depend on `Vault`, not on `EnvVault`. This
package deliberately does **not** wire the vault into the llm-proxy or control
plane; that integration is a later batch. It delivers the interface, the
`Secret` type, and the dev backend only.
