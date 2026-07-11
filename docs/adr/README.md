# Architecture Decision Records

This directory holds **Architecture Decision Records (ADRs)** for drun-dash —
short, immutable documents that capture a single significant decision, the
context that forced it, and the consequences the team accepts by making it.

ADRs complement the foundation docs in [`docs/`](../): where
[`TECH_STACK.md`](../TECH_STACK.md) locks *what* we build with and
[`ARCHITECTURE.md`](../ARCHITECTURE.md) describes *how the planes fit together*,
an ADR records *why a specific cross-cutting decision was made* at a point in
time, so that later work streams stay consistent without re-litigating it.

## Conventions

- **Filename:** `NNNN-kebab-case-title.md`, zero-padded 4-digit sequence
  (`0001-...`). Numbers are never reused.
- **Immutability:** an accepted ADR is not rewritten. To change a decision,
  add a new ADR that **supersedes** the old one and update both `Status` lines.
- **Status:** one of `Proposed`, `Accepted`, `Superseded by NNNN`, `Deprecated`.
- **Scope:** one decision per record. Markdown only; wrap prose at ~80 columns
  to match the rest of `docs/`.

## Index

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-authentication-and-session-model.md) | Authentication and session model | Proposed |
| [0002](0002-rest-api-conventions.md) | REST API conventions (org scoping, error mapping, pagination, DTO mapping) | Proposed |
