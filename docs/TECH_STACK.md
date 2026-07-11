# Technology Stack

This document locks the technology choices for **drun-dash**, the commercial
governance overlay for the open-source [drun](https://github.com/dmosc/drun)
engine. Every service, CLI, and shared library in this monorepo **must** use the
stack below so that pull requests from parallel work streams compose cleanly.

> These decisions are binding. Do not introduce alternatives without a documented
> change to this file agreed across the team.

## Locked decisions

| Layer | Choice | Rationale |
|-------|--------|-----------|
| Language | **Go 1.22+** | One language across API services, the LLM proxy, the CLI, and shared libraries. Static binaries, fast builds, strong stdlib for HTTP/crypto, and a single toolchain for every agent working the repo. |
| Admin UI | **Go + [templ](https://templ.guide) + [HTMX](https://htmx.org)** | Server-rendered HTML. No SPA, no client bundle, no separate frontend build. `templ` gives type-safe Go templates; HTMX adds interactivity via hypermedia. Keeps the entire product in one language and one deploy artifact. |
| HTTP routing | **chi** (`github.com/go-chi/chi/v5`) | Lightweight, idiomatic `net/http` router with middleware composition. No framework lock-in; handlers stay standard `http.HandlerFunc`. |
| Database | **PostgreSQL 16** | Relational store for tenants, policies, spend records, and trajectory metadata. Schema migrations managed with [`golang-migrate/migrate`](https://github.com/golang-migrate/migrate). |
| Cache | **Redis 7** | Budget counters, rate/spend metering, and session cache. Atomic counters and TTLs fit the metering workload. |
| Config | **[koanf](https://github.com/knadh/koanf) or environment variables** | Layered config (env + file). The exact precedence is settled per implementation issue. |
| OpenAPI | **Hand-written YAML in `openapi/`** | The API contract lives in `openapi/v1.yaml`. Handlers are written by hand — no `oapi-codegen` in v1 unless explicitly added later. |
| drun integration | **External `drun-mcp` binary** | The Rust `drun-mcp` binary (from GitHub releases) is invoked out-of-process and integrated over **MCP Streamable HTTP**. The upstream drun engine is **never modified**. |
| IaC (later) | **Terraform** | Infrastructure as code, introduced in a later batch. Not in scope for the foundation docs. |

## Explicitly forbidden

The following are **not permitted** anywhere in application or frontend code:

- **TypeScript** and **JavaScript** — no application logic, no frontend bundles,
  no build-time JS tooling.
- **Python** — no application code.
- **Node.js / npm / pnpm** — not for build, not for runtime, not for tooling.
- **Electron / Tauri** with a JavaScript frontend.

The **only** JS-adjacent dependency allowed is **HTMX loaded from a CDN** inside
HTML templates. No bundler, no `package.json`, no `node_modules`.

## Consequences

- A single Go module (`github.com/Domene99/drun-dash`) spans the whole repo — see
  [MONOREPO.md](./MONOREPO.md).
- `go build ./...`, `go test ./...`, and `gofmt` are the canonical checks.
- Every artifact ships as a self-contained Go binary; the admin UI is served by
  the control plane, not by a separate frontend server.
