# Architecture

**drun-dash** is the commercial governance overlay for the open-source
[drun](https://github.com/dmosc/drun) engine. drun runs locally on developer
machines; drun-dash is a **hosted control plane** that brokers LLM credentials,
distributes signed policy, meters spend, and ingests trajectory metadata. The
drun engine itself is **never modified**.

## The three planes

```
┌───────────────────────────────────────────────────────────────┐
│  drun-dash CONTROL PLANE (hosted)                              │
│                                                               │
│  ┌────────────────┐   ┌──────────────┐   ┌─────────────────┐  │
│  │ control-plane  │   │  llm-proxy   │   │  Admin UI       │  │
│  │ API + admin UI │   │ OpenAI-compat│   │ (templ + HTMX)  │  │
│  └────────────────┘   │  broker      │   └─────────────────┘  │
│         │             └──────────────┘          │             │
│         │  PostgreSQL 16  ·  Redis 7            │             │
└─────────┼──────────────────────────────────────┼─────────────┘
          │ signed policy · brokered credentials  │ metadata ingest
          │ metered LLM traffic                    ▲
          ▼                                        │
┌───────────────────────────────────────────────────────────────┐
│  DEVELOPER CLIENT (local machine)                             │
│                                                               │
│   drun-dash CLI  ──►  drun engine (unmodified)                │
│                          │                                    │
│                          ▼                                    │
│                   drun-mcp binary (Rust)                      │
│                   MCP Streamable HTTP                         │
│                   http://127.0.0.1:7273/mcp                   │
└───────────────────────────────────────────────────────────────┘
```

### 1. Control plane (hosted)

The hosted services in this repository:

- **control-plane** — REST API (contract in `openapi/v1.yaml`) plus the
  server-rendered admin UI. Owns tenants, users, policy, and spend records.
  Backed by **PostgreSQL 16**; uses **Redis 7** for session cache.
- **llm-proxy** — an OpenAI-compatible broker. Developer clients point their LLM
  traffic here; the proxy injects brokered provider credentials, enforces policy,
  and meters spend using Redis budget counters.
- **Admin UI** — Go + templ + HTMX, served directly by control-plane. No SPA.

The control plane **brokers LLM credentials** (developers never hold raw provider
keys), **distributes signed policy** to clients, **meters spend**, and **ingests
trajectory metadata** emitted by local runs.

### 2. Local drun runtime (developer machine)

The open-source **drun engine** runs entirely on the developer's machine and is
used as-is. drun exposes its capabilities through the external **`drun-mcp`**
binary — a Rust program distributed via GitHub releases — over **MCP Streamable
HTTP** at:

```
http://127.0.0.1:7273/mcp
```

drun-dash integrates with drun **only** through this MCP endpoint. It does not
patch, fork, or wrap the engine's source.

### 3. Developer clients

The **drun-dash CLI** is the developer-facing client. It authenticates to the
control plane, pulls signed policy, routes LLM calls through the llm-proxy, and
talks to the local drun runtime via the `drun-mcp` endpoint above.

## Trust boundaries

- Provider API keys live **only** in the control plane; clients receive brokered,
  scoped access through the llm-proxy.
- Policy is **signed** by the control plane and verified by clients before use.
- Local drun ↔ drun-dash communication stays on `127.0.0.1` via MCP; trajectory
  metadata flows **outbound** to the control plane for metering and audit.

See [TECH_STACK.md](./TECH_STACK.md) for the locked stack and
[MONOREPO.md](./MONOREPO.md) for where each component lives.
