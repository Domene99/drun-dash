# Monorepo Layout

drun-dash is a **single Go module**. Every future issue must place code according
to the layout below so that parallel pull requests compose without collisions.

## Module path

```
github.com/Domene99/drun-dash
```

There is exactly **one** `go.mod` at the repository root declaring this module
path. Do **not** create nested modules.

> The root `go.mod` is created in the batch **after** these foundation docs merge.
> This document defines only the directory conventions.

## Directory layout

```
drun-dash/
├── cmd/
│   ├── control-plane/    # API + admin UI server
│   ├── llm-proxy/        # OpenAI-compatible broker
│   └── drun-dash/        # CLI
├── internal/             # private app packages (per-binary)
├── pkg/                  # importable libraries (drunmcp, trajectory, policy, …)
├── openapi/v1.yaml       # hand-written API contract
├── migrations/           # golang-migrate SQL migrations
├── deploy/               # deployment manifests / IaC (Terraform, later)
├── docs/                 # project documentation (this directory)
└── go.mod                # single module: github.com/Domene99/drun-dash
```

## Conventions

- **`cmd/<binary>/`** — one directory per executable, each with a `main` package.
  The three v1 binaries are `control-plane`, `llm-proxy`, and `drun-dash` (the CLI).
- **`internal/`** — private application packages that cannot be imported by other
  modules. Organize per-binary or per-domain; nothing here is part of the public
  surface. Import path prefix: `github.com/Domene99/drun-dash/internal/...`.
- **`pkg/`** — importable libraries that may be shared across binaries (for
  example `drunmcp`, `trajectory`, `policy`). Import path prefix:
  `github.com/Domene99/drun-dash/pkg/...`.
- **`openapi/`** — the hand-written OpenAPI YAML (`v1.yaml`). Handlers are written
  by hand; no code generation in v1.
- **`migrations/`** — SQL migration files applied with `golang-migrate/migrate`.
- **`deploy/`** — deployment and infrastructure manifests (Terraform arrives in a
  later batch).
- **`docs/`** — project documentation, including
  [TECH_STACK.md](./TECH_STACK.md) and [ARCHITECTURE.md](./ARCHITECTURE.md).

## Rules

1. All application code is Go — see the forbidden languages in
   [TECH_STACK.md](./TECH_STACK.md).
2. New executables go under `cmd/`; shared libraries under `pkg/`; private code
   under `internal/`.
3. Keep the single module path `github.com/Domene99/drun-dash` — no submodules.
