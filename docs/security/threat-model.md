# drun-dash Threat Model & Security Review Checklist

> Status: **Draft for review** · Owner: Platform Security · Epic: `epic/deployment`
> Tracker: [Domene99/drun-dash#103](https://github.com/Domene99/drun-dash/issues/103)

This document describes the threat model for the **drun-dash hosted control plane** — the
enterprise management surface for the `drun` agent sandbox platform — and provides a
security review checklist to be run before each production deployment.

The document is deliberately scoped to the *control plane* (drun-dash) and its trust
boundary with the *data plane* (`drun` sandbox sessions). It is not a general security
policy for the whole organization; see [§6 Non-goals](#6-non-goals) for what is explicitly
out of scope.

---

## 1. Purpose and scope

### 1.1 What we are protecting

drun-dash is the hosted control plane that lets an enterprise operate `drun`: it creates
and supervises sandboxed agent sessions, brokers LLM traffic, enforces policy, and
collects telemetry. The primary security goal is that **an agent running arbitrary,
untrusted code inside a `drun` session cannot escape the sandbox, tamper with policy, or
exfiltrate credentials or another tenant's data**, and that **a human operator cannot
silently abuse the platform without leaving an audit trail**.

The assets we care about, in rough priority order:

| # | Asset | Why it matters |
|---|-------|----------------|
| A1 | Provider credentials (LLM API keys, cloud IAM, registry tokens) | Broadest blast radius; a leak enables spend abuse, lateral movement, and data theft. |
| A2 | Customer/tenant source code and data inside sessions | Confidentiality and, for regulated tenants, data-residency obligations. |
| A3 | Policy definitions (fetch allowlist, resource limits, egress rules) | If tampered, every downstream control is void. |
| A4 | Session state — checkpoints, snapshots, mounts, exports | Contains secrets and code; also the substrate for tenant isolation. |
| A5 | Audit log / telemetry integrity | Detection and non-repudiation depend on it. |
| A6 | Control-plane availability | Outage stops all tenants; also a lever for attacks that rely on disabling controls. |

### 1.2 In scope

- The drun-dash control plane services: session orchestrator, policy engine, LLM proxy,
  telemetry ingest worker, and admin UI (see sibling issues #98–#102).
- The trust boundary between the control plane and `drun` data-plane sessions.
- Credential handling, policy tamper resistance, and data residency (the three checklist
  domains called out in #103).

### 1.3 Out of scope (summary)

Physical security, endpoint security of operator laptops, and the internal security of the
LLM providers themselves are out of scope. Full list in [§6](#6-non-goals).

---

## 2. System model and trust boundaries

### 2.1 Components

```
                    ┌──────────────────────── drun-dash control plane ───────────────────────┐
                    │                                                                          │
   operator ──TLS──▶│  Admin UI ──▶ API gateway ──▶ Policy engine ◀──▶ Secrets manager (A1)   │
   (human)          │                    │                │                                   │
                    │                    ▼                ▼                                    │
   agent ───────────┼──▶ Session orchestrator ──enforces policy──▶ LLM proxy ──▶ providers    │
   (in session)     │            │                                    │                        │
                    │            ▼                                    ▼                        │
                    │    Telemetry ingest ◀──── audit events ──── all services (A5)            │
                    └───────────┬──────────────────────────────────────────────────────────────┘
                                │  trust boundary  (control plane │ data plane)
                    ┌───────────▼──────────────────────────────────────────────────────────────┐
                    │  drun sandbox session:  session_bash · session_fetch (allowlisted)         │
                    │                          checkpoints · snapshots · mounts · export         │
                    └───────────────────────────────────────────────────────────────────────────┘
```

### 2.2 Trust boundaries (TB)

- **TB1 — Session ↔ control plane.** Code inside a `drun` session is **untrusted**. The
  session may run arbitrary code chosen by an agent or a developer. Everything crossing
  this boundary (tool calls, fetch requests, checkpoint/export operations) must be
  authenticated, authorized, policy-checked, and logged.
- **TB2 — Operator ↔ control plane.** Human operators are **semi-trusted**. They are
  authenticated and authorized, but a single operator must not be able to unilaterally
  disable audit, exfiltrate secrets in bulk, or weaken policy without a second control
  (review, approval, or alerting).
- **TB3 — Control plane ↔ LLM/cloud providers.** Providers are **trusted for
  availability but not for confidentiality of tenant data beyond contractual terms**;
  egress to them is the main data-residency concern.
- **TB4 — Tenant ↔ tenant.** Different tenants (and different sessions) must be isolated;
  one session must never read another's state, secrets, or telemetry.

### 2.3 Assumptions

1. Transport is TLS everywhere; certificate validation is enforced (no `--insecure`).
2. The sandbox runtime provides OS-level isolation (namespaces/VM) as designed; a kernel
   or hypervisor escape is a dependency risk tracked separately, not re-derived here.
3. Secrets are never delivered into a session as plaintext environment values that the
   agent can read back; they are brokered (see [§4.1](#41-credential-handling)).
4. Clocks are roughly synchronized so audit-log ordering is meaningful.

---

## 3. Threat model: misbehaving agent vs. malicious developer

This is the core distinction the platform is designed around. Both actors operate *from
inside or adjacent to a `drun` session*, but their intent, capability, and the controls
that stop them differ. Conflating them leads to controls that stop neither.

### 3.1 Actor definitions

| | **Misbehaving agent** | **Malicious developer** |
|---|---|---|
| **Intent** | None (no adversarial intent). Fails through hallucination, prompt injection, buggy tool use, or reward-hacking a task. | Deliberate. Wants to exfiltrate data/credentials, abuse spend, or evade detection. |
| **Capability** | Can call any tool the session exposes and run arbitrary code, but does not strategize against the controls; typically does not know the control topology. | Understands the platform, can craft inputs, chain tool calls, and probe for weaknesses; may hold legitimate operator credentials. |
| **Persistence** | Bounded to the session; stops when the task/turn ends. | Will retry, pivot, and use legitimate access over time. |
| **Trust level** | Untrusted, but *contained by design*. | Semi-trusted (has an account) — an **insider** threat. |

The guiding principle: **contain the agent with mechanism; deter and detect the developer
with policy, least privilege, and audit.** Mechanism alone won't stop a determined
insider; policy alone won't stop a confused agent that ignores instructions.

### 3.2 Misbehaving-agent threats and controls

| ID | Threat | Vector | Primary control |
|----|--------|--------|-----------------|
| MA-1 | Agent exfiltrates secrets it was given | Prints env/`$OPENAI_API_KEY`, POSTs to attacker URL | Credential brokering — secrets never enter session env (A1); egress restricted to the fetch allowlist. |
| MA-2 | Agent reaches unintended network endpoints | `session_fetch` / `curl` to arbitrary host | Default-deny egress; only `get_fetch_allowlist` hosts permitted; deny logged. |
| MA-3 | Prompt-injection from fetched content redirects behavior | Malicious README/webpage instructs agent to leak data | Content from `session_fetch` is data, never elevated to policy; egress allowlist bounds the blast radius even if the agent is convinced. |
| MA-4 | Agent consumes unbounded resources | Fork bomb, infinite LLM loop, disk fill via snapshots | Per-session CPU/mem/disk/time quotas; LLM spend budget per session (see #99). |
| MA-5 | Agent writes to a mount that persists outside the session | `session_mount` on a shared/writable path | Mounts read-only by default; writable mounts require explicit policy grant, scoped per session. |
| MA-6 | Agent leaks data cross-session | Reads another session's checkpoint/snapshot | Checkpoints/snapshots namespaced per session+tenant; access mediated by orchestrator (TB4). |

Key insight: the misbehaving agent is **not** trying to defeat these controls — so
default-deny, quotas, and brokering are highly effective. The residual risk is a control
*gap* (a tool that bypasses policy), not a control *bypass*.

### 3.3 Malicious-developer threats and controls

| ID | Threat | Vector | Primary control |
|----|--------|--------|-----------------|
| MD-1 | Bulk credential exfiltration | Uses legitimate session/API access to pull secrets manager or many keys | Least privilege: sessions get brokered, short-lived, scoped tokens — never the raw vault; bulk access rate-limited and alerted. |
| MD-2 | Policy tampering to weaken controls | Edits fetch allowlist, disables quotas, widens egress | Policy changes require authz + are versioned, signed, and audited; sensitive changes need second-party review (TB2). |
| MD-3 | Audit/telemetry tampering to hide activity | Deletes or edits logs, disables ingest | Audit log append-only and shipped off-box to a store the operator cannot mutate; ingest failures alert (A5). |
| MD-4 | Egress to unapproved region to break residency | Adds an out-of-region host/provider to allowlist | Data-residency policy pins allowed regions/providers; residency-affecting policy edits flagged and reviewed ([§4.3](#43-data-residency)). |
| MD-5 | Privilege escalation via the control plane | Grants self broader scope, impersonates another tenant | RBAC with separation of duties; no self-approval of privilege grants; tenant boundary enforced server-side, not by client claims. |
| MD-6 | Malicious `session_export` / snapshot to smuggle data out | Exports tenant data to a personal location | Exports authorized, logged, and (for regulated tenants) restricted to approved destinations; large/unusual exports alerted. |
| MD-7 | Supply-chain / dependency tamper in the sandbox image | Poisons a base image or dependency | Out of this doc's remediation scope but noted as a residual — pinned/verified images tracked in the deployment epic. |

Key insight: the malicious developer *will* attempt bypass, so the controls that matter
are **separation of duties, least privilege, and tamper-evident audit** — not
mechanism-only containment. A control the same person can both use and disable is not a
control against this actor.

### 3.4 Why the distinction changes the design

- Controls that only *deter or detect* (audit, alerting, review) are close to useless
  against a misbehaving agent (it has no intent to deter) but essential against a
  malicious developer.
- Controls that *contain by mechanism* (brokering, default-deny egress, quotas,
  namespacing) stop the misbehaving agent cleanly and *also* raise the cost for the
  developer — but the developer can still legitimately reconfigure them, which is why
  every mechanism must sit behind tamper-evident policy (MD-2, MD-3).
- Therefore the invariant: **no single actor — human or agent — can both weaken a control
  and erase the evidence.**

---

## 4. Security review checklist

Run this checklist before every production deployment of drun-dash. Each item is
`PASS` / `FAIL` / `N/A` with a link to evidence. A `FAIL` on any **must** item blocks the
deploy.

### 4.1 Credential handling

- [ ] **(must)** No provider secret (A1) is injected into a session as a readable env var
  or file; sessions receive only brokered, short-lived, scope-limited tokens.
- [ ] **(must)** LLM/provider keys live only in the secrets manager; no key appears in
  source, container images, config maps, or logs. (Automated secret-scan is green.)
- [ ] **(must)** Brokered session tokens are short-lived (≤ session lifetime) and
  automatically revoked on session close/`session_close`.
- [ ] **(must)** Secrets are never written to telemetry/audit payloads; log pipelines
  redact known key patterns.
- [ ] **(should)** Bulk secret access is rate-limited and alerts on anomaly (MD-1).
- [ ] **(should)** Key rotation is possible without downtime; last rotation date recorded.
- [ ] **(should)** TLS certificate validation enforced on all egress; no `--insecure`
  paths in the LLM proxy or fetch client.

### 4.2 Policy tamper resistance

- [ ] **(must)** All policy objects (fetch allowlist, quotas, egress rules, RBAC) are
  versioned; every change has an author, timestamp, and diff in an append-only store.
- [ ] **(must)** Policy is enforced server-side by the orchestrator/policy engine; a
  session cannot supply or override its own policy (TB1).
- [ ] **(must)** Sensitive policy changes (widening egress, disabling quotas, granting
  privilege) require a second-party review or approval; **no self-approval** (MD-2, MD-5).
- [ ] **(must)** The fetch allowlist is default-deny; an empty/missing allowlist denies
  all egress rather than allowing all.
- [ ] **(should)** Policy engine fails closed: if it is unreachable, sessions are denied
  new privileged actions rather than defaulting to allow.
- [ ] **(should)** Policy definitions are signed or checksummed and verified at load.

### 4.3 Data residency

- [ ] **(must)** Each tenant has a declared allowed region set; session storage
  (checkpoints, snapshots, mounts, exports) is pinned to those regions.
- [ ] **(must)** LLM proxy routes tenant traffic only to providers/endpoints in approved
  regions; cross-region fallback is disabled for residency-bound tenants (TB3, MD-4).
- [ ] **(must)** Telemetry and audit data for a tenant are stored within the tenant's
  allowed region(s).
- [ ] **(must)** `session_export` and snapshot destinations are constrained to approved
  regions/destinations for regulated tenants; violations are blocked, not just logged
  (MD-6).
- [ ] **(should)** A policy change that affects residency (new provider, new region, new
  export target) is flagged for explicit review before it takes effect.
- [ ] **(should)** Data-flow map is current and shows no residency-crossing path.

### 4.4 Tenant isolation & audit (cross-cutting)

- [ ] **(must)** Tenant/session boundary (TB4) enforced server-side; no cross-session
  read of checkpoints, snapshots, mounts, secrets, or telemetry.
- [ ] **(must)** Audit log (A5) is append-only and shipped off-box to storage the
  operating account cannot mutate or delete (MD-3).
- [ ] **(must)** Audit-pipeline failure or gap raises an alert; silent loss is treated as
  an incident.
- [ ] **(should)** Every action crossing TB1/TB2 is attributable to an actor (agent
  session id or operator id).

---

## 5. Residual risks

These are accepted or externally-owned risks, recorded so review can re-weigh them:

- **R1 — Sandbox/hypervisor escape.** A kernel or VM escape in the sandbox runtime would
  break TB1. Owned by the runtime dependency; mitigated by keeping the runtime patched
  and by not co-locating high-value secrets with session hosts.
- **R2 — LLM-provider side data handling.** Once tenant data crosses TB3 to a provider, we
  rely on contractual terms, not technical controls. Bounded by the residency checklist
  ([§4.3](#43-data-residency)) and provider selection.
- **R3 — Supply-chain compromise of the sandbox image (MD-7).** Poisoned base images or
  dependencies; mitigated by pinned/verified images in the deployment epic, tracked
  separately.
- **R4 — Colluding operators.** Separation of duties assumes reviewers are independent;
  two colluding privileged operators can defeat second-party review. Detected only by
  audit review, not prevented.

---

## 6. Non-goals

The following are **explicitly out of scope** for this document and this control plane's
threat model. Listing them prevents scope creep and false assurance:

1. **Not** a defense against a sandbox-runtime (kernel/hypervisor) escape — see R1; that
   is the runtime's responsibility, tracked as a dependency.
2. **Not** endpoint security for operator or developer workstations (malware, keyloggers
   on a laptop that holds legitimate credentials).
3. **Not** physical/data-center security of the hosting provider.
4. **Not** the internal security posture of upstream LLM or cloud providers beyond
   contractual and residency controls (see R2).
5. **Not** a general application-security review of arbitrary code that developers *choose*
   to run inside their own sessions — the sandbox contains it, but its correctness is the
   developer's responsibility.
6. **Not** DoS/abuse protection for the public network edge beyond per-session quotas;
   edge-layer rate limiting and WAF are owned by the infrastructure epic (#98).
7. **Not** a compliance certification (SOC 2 / ISO 27001) — this informs such an effort
   but does not substitute for an audit.

---

## 7. Review & sign-off

| Reviewer | Role | Date | Result |
|----------|------|------|--------|
|          | Platform Security | | |
|          | Deployment / SRE  | | |
|          | Eng lead (data plane) | | |

**Acceptance (per #103):** this document is reviewed and signed off above, and explicit
non-goals are documented in [§6](#6-non-goals).
