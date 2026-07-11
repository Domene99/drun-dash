# drun-dash Security Threat Model

> **Status:** Living document. This file is intended to be *self-contained*: an
> engineer or auditor reading only this document should come away with an
> accurate picture of drun-dash's security posture, what it defends against,
> and — just as importantly — what it does **not**.

## 1. What drun-dash is

drun-dash is enterprise management ("governance") software for **drun**, a
sandbox that executes autonomous AI agents. Developers run coding/agentic
sessions locally inside the drun sandbox; drun-dash sits alongside those
sessions to provide credential custody, policy enforcement, entitlements, and
audit for an organization's fleet of agents.

The single most important design fact for reasoning about security:

> **drun-dash's governance leverage comes from credential custody, not from
> controlling the developer's machine.** Master LLM provider keys never reach
> developer machines. A proxy holds the keys and releases only short-lived,
> narrowly-scoped tokens to sessions that are currently entitled. Revoke or
> withhold the token and the agent cannot reach the model — regardless of what
> else is running on the laptop.

Everything below follows from that leverage point.

---

## 2. Assets

The assets drun-dash is responsible for protecting, in rough priority order:

| Asset | Description | Primary risk if compromised |
|-------|-------------|-----------------------------|
| **LLM credentials** | Master API keys for LLM providers (Anthropic, OpenAI, etc.). Custodied by the proxy; never distributed to endpoints. | Direct financial loss (token spend), data exfiltration to a provider under an attacker's account, loss of the central governance chokepoint. |
| **Signed policies** | Machine-readable governance rules (allowed models, spend caps, tool/permission constraints) that are cryptographically signed by the control plane. | Forged or downgraded policy lets an agent operate outside its intended envelope. |
| **Trajectory metadata** | Structural record of what agents did: session identifiers, timestamps, model/tool invocations, token counts, policy decisions — **not** source code contents by default. | Loss of audit integrity; leakage of organizational activity patterns. |
| **Member identity** | The mapping of human org members ↔ entitlements ↔ active sessions. | Impersonation, privilege escalation, entitlement theft. |

Note that **source code contents are not, by default, an asset held by
drun-dash** — see §6 (Data residency) for why this matters and how it is
bounded.

---

## 3. Trust boundaries

Data crosses four principal boundaries. Each boundary is where an attacker's
capabilities change, and therefore where controls are placed.

```
  ┌──────────────────┐   (A)   ┌──────────────────┐   (B)   ┌────────────────┐
  │ Developer laptop │ <-----> │  Control plane   │ <-----> │  LLM providers │
  │  (drun sandbox + │  scoped │  (proxy + policy │  master │  (Anthropic,   │
  │   agent + local  │  token  │   + entitlement  │   key   │   OpenAI, ...) │
  │   source code)   │         │   + audit)       │         │                │
  └──────────────────┘         └──────────────────┘         └────────────────┘
             │                          │
             │ (C) deep capture (opt-in)│
             ▼                          ▼
  ┌──────────────────────────────────────────────┐
  │  Customer-controlled storage (deep capture)   │
  │  Full trajectories / code context land HERE,   │
  │  in the customer's own bucket — not ours.      │
  └──────────────────────────────────────────────┘
```

**(A) Developer laptop ↔ control plane.**
The laptop is *semi-trusted*. It runs the drun sandbox and the agent, and it
holds a **session token** (short-lived, scoped) — not a master key. The control
plane trusts a laptop only as far as the current entitlement and token allow.
The laptop authenticates as a member/session; the control plane authenticates
itself to the laptop (policy signatures, TLS).

**(B) Control plane ↔ LLM providers.**
This is the only place master LLM keys exist at rest and in use. The proxy
brokers every model call: it attaches the master credential, enforces
per-request policy, and records metadata. Providers see traffic from *our*
account, never from the developer directly.

**(C) Control plane / laptop ↔ customer storage (deep capture).**
"Deep capture" (full trajectory including code context) is an **opt-in** feature
that writes to **customer-controlled storage**. This boundary exists so that the
sensitive payload never has to reside in drun-dash-operated infrastructure.

**(D) Agent ↔ drun sandbox (intra-laptop).**
Inside the laptop there is a further boundary between the agent process and the
host, enforced by drun itself — see §7.

---

## 4. Threat actors and scenarios

| Actor | Capability assumed | In scope? |
|-------|--------------------|-----------|
| **Compromised / misbehaving agent** | Arbitrary behavior *inside* the drun sandbox: prompt-injected, buggy, or actively hostile agent code. | **Yes — primary threat.** |
| **Stolen laptop / stolen session token** | Attacker obtains a laptop or exfiltrates its session token. | **Yes.** |
| **Insider admin** | A drun-dash org administrator abusing legitimate access. | **Yes (partial) — bounded by audit + least privilege.** |
| **Proxy bypass attempt** | Agent/laptop tries to reach LLM providers directly, circumventing the proxy. | **Yes.** |
| **Malicious developer with root on their own machine** | Full control of their own endpoint, including the drun process. | **No — explicit non-goal. See §5.** |

### 4.1 Compromised / misbehaving agent (primary)
The agent is assumed potentially hostile. It may attempt to: exfiltrate data,
call disallowed models, exceed spend, invoke unauthorized tools, or escape the
sandbox. This is the actor the entire architecture is optimized against, because
autonomous agents are exactly the untrusted-code-you-chose-to-run case.

### 4.2 Stolen laptop / stolen session token
An attacker with a laptop or its token can act only as that session, only for
as long as the token is valid, and only within the token's scope. Tokens are
short-lived and per-session; revocation at the control plane cuts off model
access even if the token has not yet expired.

### 4.3 Insider admin
An org admin can change policy and entitlements. This power is real and is
bounded, not eliminated: every policy issuance and entitlement change is
signed and recorded in the audit trail, so admin action is *attributable* and
*reviewable* rather than silent.

### 4.4 Proxy bypass attempt
Because the laptop never holds a master key, "bypassing the proxy" gains the
attacker nothing usable against the *provider account*: a direct call to the
provider has no valid master credential. Bypass at best lets an agent use its
own personal keys — which is outside drun-dash's custody and outside its
governance claims (and can be constrained by drun's own network permissioning,
§7).

---

## 5. Non-goals (explicit)

drun-dash deliberately does **not** attempt to defend against:

- **A malicious developer with root on their own machine.** If a human owns the
  endpoint, they can inspect memory, tamper with local processes, and read
  anything their scoped session token can currently fetch. drun-dash's guarantee
  is *not* "the developer cannot misuse their own live session"; it is
  **"the developer never holds the master keys, their access is short-lived and
  scoped, and everything they do is entitled and audited."** Custody + scoping +
  attribution, not endpoint invincibility.
- **Confidentiality of data the developer is themselves entitled to see.** A
  scoped token releases exactly what the session is entitled to; a root
  developer can read that. Containment is via *scope and TTL*, not endpoint DRM.
- **Provider-side compromise.** We trust the LLM providers to the extent of the
  master account; defending the provider's own infrastructure is out of scope.

Stating these plainly is itself a control: it prevents the false-security
failure mode where operators assume protections that were never designed in.

---

## 6. Data residency (this is *not* zero-knowledge)

**Default posture:** the control plane receives trajectory **structure and
metadata** — session IDs, timestamps, which model/tool was called, token
counts, policy decisions — **not source code contents**. This keeps sensitive
IP off drun-dash-operated infrastructure in the common case.

**Deep capture:** when an org opts into full-trajectory capture (including code
context), that payload is written to **customer-controlled storage** (§3, boundary C),
not to drun-dash's own stores.

> **Do not describe this as zero-knowledge.** It is **data residency /
> data-minimization**, which is a categorically weaker and more honest claim:
>
> - **Data residency / minimization (what we have):** by default we *do not
>   collect* code contents; when deep capture is enabled, the data *resides in
>   the customer's own storage*. The proxy still, by construction, sees model
>   request/response traffic in cleartext at the moment it brokers a call —
>   that is how it enforces policy and attaches credentials.
> - **Zero-knowledge (what we do NOT claim):** a ZK claim would assert that
>   drun-dash is *cryptographically incapable* of observing prompt/response
>   contents. That is false. The proxy is a man-in-the-middle by design; it
>   handles cleartext to broker credentials and enforce per-request policy.
>
> Any documentation, sales material, or UI copy that implies drun-dash "cannot
> see" agent traffic is incorrect and should be corrected. The correct framing
> is: *we minimize what we retain and let customers control where deep-capture
> data lives* — not *we are unable to see it*.

---

## 7. The drun engine boundary

drun-dash **governs** drun; it does **not modify** drun. The sandbox's isolation
properties are provided by drun itself, and drun-dash relies on — rather than
reimplements — them:

- **Deno-based permissioning.** drun builds on Deno's capability model: an agent
  process has no ambient authority to the filesystem, network, or environment.
  Access must be granted explicitly (e.g. read/write specific paths, reach
  specific hosts). This is the first line of containment for a compromised
  agent (§4.1) and the mechanism that can blunt a proxy-bypass attempt's network
  reach (§4.4).
- **Process-level isolation.** The agent runs as an isolated process; it does
  not share drun-dash's or the host's trust context. Sandbox escape is a drun
  concern, and its limits are drun's limits — drun-dash inherits both the
  protection and the residual risk.

**Policy → drun configuration mapping.** drun-dash policy does not fork or patch
the engine. Instead, governance policy is expressed as, and maps onto, drun's
own **`config.toml`** fields (permission grants, allowed hosts, resource
limits, etc.). The control plane's job is to (a) decide what the `config.toml`
for an entitled session should contain, (b) sign that decision, and (c) ensure
the session cannot obtain model credentials unless it is running under the
sanctioned configuration.

Consequences of this boundary:

- drun-dash's containment guarantees are **upper-bounded by drun's sandbox
  guarantees.** If drun's Deno permissioning or process isolation has a gap,
  drun-dash does not close it — it is layered governance on top, not a
  replacement sandbox.
- Conversely, drun-dash adds the controls drun alone does not: **who** may run,
  with **which credentials**, under **what policy**, and **recorded how**.

---

## 8. Mitigations mapped to platform features

| Threat (§4) | Primary mitigation | Platform feature |
|-------------|--------------------|------------------|
| Compromised agent exfiltrating via model calls | No master key on endpoint; every call brokered and policy-checked | **Proxy credential brokering** + **per-request entitlement check** |
| Compromised agent escaping to host/filesystem/network | Capability-scoped execution | **drun Deno permissioning + process isolation** (§7) |
| Stolen laptop / token | Short-lived, per-session scoped tokens; central revocation | **Scoped token issuance**, **entitlement checks per request** |
| Forged or downgraded policy | Signature verification before enforcement | **Policy signing** |
| Control plane unreachable (offline) | Deny by default when policy/entitlement cannot be verified | **Fail-closed offline** |
| Proxy bypass to provider | Endpoint has no usable master credential; provider account unreachable without the proxy | **Credential custody** + drun network permissioning |
| Insider admin abuse | Attribution and reviewability of every privileged action | **Signed policy issuance + audit trail** |
| Replay / stale entitlement | Entitlement evaluated at the moment of each request, not once per session | **Entitlement checks per request** |

### Key mitigation properties

- **Proxy brokering.** The proxy is the single point where master credentials
  are applied. This concentrates trust deliberately: it is a chokepoint for
  enforcement *and* the highest-value target, so it is the component that most
  warrants defense-in-depth, monitoring, and blast-radius limiting.
- **Policy signing.** Policies are signed by the control plane and verified
  before enforcement, so an agent or tampered laptop cannot substitute a
  weaker policy without detection.
- **Fail-closed offline.** If the control plane cannot be reached to verify
  entitlement or refresh a token, the safe default is to **deny** — no
  credential release, no model access. Availability is intentionally traded for
  safety here.
- **Per-request entitlement checks.** Entitlement is not a one-time gate at
  session start; each model request is checked against current entitlement, so
  revocation and policy changes take effect promptly and stale sessions cannot
  coast.

---

## 9. Summary of the security posture

- **We defend against the untrusted agent, not the untrusted human owner of the
  endpoint.** (§4, §5)
- **Our leverage is custody of LLM credentials**, released as short-lived scoped
  tokens only to entitled sessions and brokered through a proxy. (§1, §8)
- **We minimize and localize data**: metadata by default, deep capture into
  customer-controlled storage — **data residency, explicitly not
  zero-knowledge.** (§6)
- **We govern drun; we do not modify it.** Containment is inherited from drun's
  Deno permissioning and process isolation; policy maps onto drun's
  `config.toml`. (§7)
- **Fail-closed, signed, and audited** are the defaults: deny when unsure, verify
  before enforcing, and record for attribution. (§8)
