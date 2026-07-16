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
| [0003](0003-session-launch-and-credential-grant.md) | Session launch and scoped credential-grant model | Proposed |
| [0004](0004-member-conversational-console-architecture.md) | Member conversational console architecture | Proposed |
| [0005](0005-design-system-and-information-architecture.md) | Design system and information architecture | Proposed |
| [0006](0006-rbac-and-access-model.md) | RBAC and access model (roles, custom roles, permissions) | Proposed |
| [0007](0007-member-standalone-desktop-app.md) | Member standalone desktop app | Proposed |
| [0008](0008-local-authentication.md) | Local authentication (email/password, org self-signup, member invitations) | Proposed |
