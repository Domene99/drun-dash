# 0005 — Design system and information architecture

- **Status:** Proposed
- **Date:** 2026-07-15
- **Deciders:** drun-dash admin-ui / member-console / design work streams
- **Batch:** 21 (decision artifact; no code)

## Context

drun-dash now has two user-facing surfaces on the roadmap, and neither looks
finished. The **admin dashboard** (`cmd/admin-ui`, `internal/ui`) exists on
`main` but is a **dev-grade skeleton**, and the **member conversational console**
([ADR-0004](0004-member-conversational-console-architecture.md), member-desktop
[ADR-0007]) is being built out batch by batch. The product owner reviewed the
admin UI and rejected its polish. The concrete complaints, all verifiable in the
current source:

- **The dashboard is a JWT-claims dump.** `views/dashboard.templ` renders a
  `<dl>` of `Subject`, `Organization`, and `Roles` — the raw contents of the
  access token — as the primary landing content. That answers "what is in my
  token," not "what is happening in my org."
- **Raw UUIDs are primary labels.** `views/sessions.templ` puts `{ s.ID }` (a
  session UUID) in the first column; `views/trajectory.templ` titles the page
  `Session { data.SessionID }` with the bare UUID. A human cannot tell which
  agent or member a row belongs to.
- **Invisible / inconsistent controls.** Interactive controls are unstyled
  `<button>` elements that inherit near-invisible defaults; the summary
  fragment's **Refresh** control has so little affordance that it reads as
  decoration. There is no single button hierarchy — some buttons are white on
  white, some are borderless.
- **No information architecture.** `views/layout.templ` gives every page the
  same flat four-link top bar (`Dashboard / Sessions / Spend / Approvals`) and a
  60rem centered column. There is no sidebar, no grouping, no page-header
  pattern, and the visual system is ~120 lines of inline `<style>` duplicated
  into the one layout — there is nothing for the member console to share.
- **Unreadable sessions and trajectories.** Tables show UUIDs and enum strings
  (`running`, `structure`) as plain text with no status color, alignment, empty
  states, or row affordances beyond a bare link.

The owner wants a **polished, consistent, professional experience** for two
audiences that must feel like **one product**:

- **Org admins** — a **web dashboard** for governing the system (agents,
  policies, members, roles, teams, runs, approvals, spend, audit).
- **Team members** — a **chat-first agent app** ([ADR-0004],
  [ADR-0007]) where the conversation is the surface.

This ADR **locks the shared design system and the information architecture** for
both surfaces before the UI-polish batches begin, so those batches share one
answer for tokens, components, layout, and the readable-not-UUID rule instead of
each inventing its own. The **high-level decisions below are already approved**;
this ADR documents and details them — it does not re-litigate them. It writes
**no code, no CSS, and no templates** (those are later batches) and adds **no
dependencies** and **no JS framework**. It aligns with the two ADRs that own the
surfaces it styles:

- [ADR-0004](0004-member-conversational-console-architecture.md) and
  **[ADR-0007]** own the **member console** — its local-client architecture, its
  auth, and (0007) its **desktop** shape. This ADR supplies the *look and IA*
  those batches render with; it does not change their architecture.
- **[ADR-0006]** owns **RBAC**; it adds the **Roles** and **Members** admin
  sections this IA lists. This ADR reserves the nav slots and the readable-label
  rules; ADR-0006 defines what those pages *do*.

> **Forward references.** ADR-0006 (RBAC) and ADR-0007 (member desktop) are
> named here as the owners of sections this ADR lays out; if their numbers shift
> when authored, update the links. They are cited as scope boundaries, not as
> already-merged contracts.

## Decision drivers

- **One product, two shells.** The admin dashboard and the member console must
  share type, color, spacing, status vocabulary, and controls so a member who
  becomes an admin (or an admin who opens the member app) sees the same system.
- **No visual drift.** Two frontends styled by copy-paste diverge on the first
  independent change. The system needs a **single source of truth** both import.
- **The locked stack.** `docs/TECH_STACK.md` and the existing UI fix the
  toolkit: **Go + templ + HTMX**, server-rendered, no SPA, no build step beyond
  `templ generate`. The design system must live *inside* that stack — CSS and
  templ, not a component framework.
- **Human-readable governance.** Governance is only useful if an admin can read
  it. Names, not UUIDs; relative times, not RFC-3339 stamps; colored status, not
  enum strings. This is the owner's top complaint and the core fix.
- **Accessibility is not optional.** Every control needs a visible default and
  focus state (this is literally the invisible-button bug), and text must meet
  **WCAG-AA** contrast in both light and dark mode.
- **Documents, not code.** Later batches implement `internal/uikit` and restyle
  both apps. This ADR must give an implementer concrete, honest choices — token
  names and intent, a component list, an IA — without shipping the pixels.

## Decision

### 1. Visual language — "professional & neutral"

The house style is **calm, neutral, and confident** — the visual register of a
governance console, not a consumer app. Concretely:

- **A neutral gray scale** carries almost all surfaces and text. **One
  restrained accent** color (a muted blue) marks primary actions, active nav,
  focus, and links — and *nothing decorative*. Accent is a tool for directing
  attention, used sparingly.
- **Generous whitespace** and a clear reading column; data-dense where the admin
  needs density, never cramped.
- **Crisp modern typography** from the **system font stack** (`system-ui,
  -apple-system, "Segoe UI", Roboto, …`) — no web-font download, fast and
  native-feeling. A **monospace stack** (`ui-monospace, SFMono-Regular, Menlo,
  …`) for IDs, code, and numeric detail.
- **Subtle borders and shadows**, **rounded-but-not-playful radii** (small
  radius on controls, slightly larger on cards; nothing pill-shaped except
  status badges).
- **Light and dark mode**, both first-class, implemented **entirely with CSS
  custom properties (design tokens)** — a `:root` light palette and a
  `[data-theme="dark"]` (and `@media (prefers-color-scheme: dark)`) override
  that reassigns the *same* token names. Components reference tokens only, so a
  theme is a value swap with zero component changes.
- **No JS framework.** Interactivity is **HTMX only** (already the pattern);
  theme toggling is a tiny inline script that flips `data-theme` and persists the
  choice. No React/Vue/Svelte, no bundler, no npm build.

**Design token catalog.** The names and intent are locked here; exact hex values
are tuned during implementation against WCAG-AA (values below are indicative,
not final). Tokens are grouped by role so both apps and every component draw
from one vocabulary.

_Neutral scale_ — the backbone; nine steps plus white/black anchors:

| Token | Intent |
|-------|--------|
| `--neutral-50` … `--neutral-100` | App background, subtle fills, zebra rows |
| `--neutral-200` … `--neutral-300` | Borders, dividers, disabled surfaces |
| `--neutral-400` … `--neutral-500` | Muted text, icons, placeholders |
| `--neutral-600` … `--neutral-700` | Secondary text, table headers |
| `--neutral-800` … `--neutral-900` | Primary text, headings, dark chrome |

_Accent_ — the single restrained brand color:

| Token | Intent |
|-------|--------|
| `--accent` | Primary buttons, active nav, links, selected state |
| `--accent-hover` | Hover/pressed of accent controls |
| `--accent-subtle` | Tinted accent background (selected row, active nav pill) |
| `--accent-contrast` | Text/icon color that sits legibly *on* `--accent` |

_Semantic status_ — one vocabulary shared by both apps (§4, §5). Each has a
`-fg` (text/icon), `-bg` (subtle fill for badges/alerts), and `-border`:

| Token family | Intent |
|--------------|--------|
| `--success-*` | Completed, healthy, approved, active |
| `--warning-*` | Pending, needs attention, expiring, throttled |
| `--danger-*` | Failed, denied, error, destructive action |
| `--info-*` | Neutral informational, running/in-progress, structure class |

_Surfaces & text_ — semantic aliases so components never hard-code a neutral:

| Token | Intent |
|-------|--------|
| `--surface` | Card / panel background |
| `--surface-raised` | Elevated surface (modal, popover, dropdown) |
| `--surface-sunken` | Insets, code blocks, table wells |
| `--bg` | Page background |
| `--border` | Default 1px border/divider |
| `--text` | Primary body text |
| `--text-muted` | Secondary/label text |
| `--text-on-accent` | Text on accent fills |
| `--link` | Link color (accent by default) |

_Spacing scale_ — a single step scale used for padding, gap, and margin so
rhythm is consistent (`--space-1` ≈ 4px through `--space-8` ≈ 48px, roughly a
4px base with a 1·2·3·4·6·8·12·16 progression).

_Radius_ — `--radius-sm` (controls, inputs), `--radius-md` (cards, panels),
`--radius-lg` (modals), `--radius-pill` (status badges only).

_Typography scale_ — `--font-sans` and `--font-mono` families; a size ramp
`--text-xs` … `--text-2xl`; weights `--weight-normal` / `--weight-medium` /
`--weight-bold`; `--leading-tight` / `--leading-normal`. Tabular numerals
(`font-variant-numeric: tabular-nums`) on all numeric/monetary columns.

_Elevation (shadows)_ — `--shadow-sm` (cards), `--shadow-md` (dropdowns,
popovers), `--shadow-lg` (modals). Shadows are soft and low-contrast; in dark
mode they lean on border + surface contrast rather than heavy drop shadows.

_Focus & motion_ — `--focus-ring` (a 2px accent outline with offset, applied to
**every** focusable control via `:focus-visible`), `--focus-ring-danger` for
destructive controls, and `--transition-fast` for hover/press states. No token
introduces motion that a `prefers-reduced-motion` user can't opt out of.

**Every interactive control has a visible default and a visible focus state.**
This is the direct fix for the invisible-button bug: buttons, links, inputs,
selects, tabs, and nav items all render with a real border/background at rest
and a `--focus-ring` on `:focus-visible`. All text/background pairings in both
themes meet **WCAG-AA** (4.5:1 body, 3:1 large text and UI borders); the token
values are chosen and verified to that bar during implementation.

### 2. Delivery — a shared `internal/uikit` package

Both frontends consume **one Go package, `internal/uikit`**, as the single
source of truth for the design system. It is deliberately **presentation-only**:
like `internal/ui/views` today, it imports no control-plane packages and holds
no business logic, so the dependency edge runs one way (each app → `uikit`) and
never the reverse.

`internal/uikit`'s responsibilities:

1. **The embedded stylesheet.** A single CSS file — **design tokens (§1) plus
   component styles** — `//go:embed`ed and served by each app at a stable path
   (e.g. `/assets/uikit.css`) with a content-hashed URL for cache-busting. This
   replaces the inline `<style>` block in `views/layout.templ`. It is the *only*
   place tokens and component CSS are defined.
2. **Base templ layout & component library.** Reusable `templ` components both
   apps compose. Each app keeps its **own** shell wiring (routes, nav contents,
   auth) but builds it from these shared parts, so visuals cannot drift.
3. **Shared status vocabulary helpers.** Small pure helpers that map a domain
   status string to its badge variant/token (§4, §5), so "running", "approved",
   "denied" render identically in both apps.

**Component inventory** (the list this ADR locks; a later batch implements it):

- **App shell** — the outer page frame: `<head>` with the embedded stylesheet
  and HTMX, theme wiring (`data-theme`), skip-link, and named slots for chrome +
  content. Both apps' layouts are thin wrappers over this.
- **Top bar** — org name, environment indicator, user/profile menu, and the
  **dark-mode toggle**.
- **Sidebar** — vertical nav with section groups, active-item state, and icons;
  used by the admin shell (the member shell uses a minimal variant).
- **Page header** — the consistent title + description + primary-action slot
  that every content page starts with.
- **Card / panel** — the standard surface container (`--surface`, `--radius-md`,
  `--shadow-sm`).
- **Table** — header row, alignment (numeric right, tabular numerals), zebra/
  hover rows, empty-state slot, and a row-actions affordance.
- **Badge / status pill** — the colored status vocabulary (§4/§5), one component,
  variants by semantic token.
- **Button variants** — the single hierarchy: **primary** (accent), **secondary**
  (neutral outline), **ghost** (text-only, hover fill), **danger** (destructive).
  Each has default, hover, active, focus, and disabled states.
- **Form controls** — text input, textarea, select, checkbox/radio, and label/
  field/help/error grouping, all with visible borders and focus rings.
- **Empty state** — the "no data yet" pattern (icon + message + optional action)
  replacing today's bare `<p class="muted">No sessions.</p>`.
- **Toast / inline alert** — transient toasts and inline `success/warning/
  danger/info` alerts using the semantic tokens.
- **Modal / confirm dialog** — the raised-surface overlay for confirmations
  (e.g. destructive actions), with focus trapping and `--focus-ring`.
- **ID chip (copyable)** — the monospaced, truncated, copy-to-clipboard element
  for showing a raw UUID as *secondary* detail (§4).
- **Relative-time** — renders a human "3 minutes ago" with the absolute
  timestamp on hover/title.

The package **specifies responsibility and the component list only**; the CSS
and templ implementation, and the migration of `internal/ui` onto it, are
**later batches**. This ADR does not ship them.

### 3. Information architecture — two tailored shells, one system

Both shells are built from the §2 components and share every token, so they are
one product wearing two layouts.

**Admin (web dashboard) — persistent left sidebar + top bar.** The flat top-nav
of four links is replaced by a **persistent left sidebar** (grouped sections)
plus a **top bar** carrying org name, **environment** indicator, **user menu**,
and **dark-mode toggle**. Content is **data-dense but readable**: each page opens
with the shared page-header (title + description + primary action) over cards and
tables. The sidebar sections, each described by *what it shows in human terms*:

| Section | What it shows (readable, not UUIDs) |
|---------|-------------------------------------|
| **Overview** | The meaningful landing summary (see §4): active sessions, spend this period, pending approvals, recent activity — **not** a JWT dump. |
| **Agents** | Entitled/available agents by **name + version + description**, with status. |
| **Policies** | Policy bundles by **policy name** and what they govern; version/status badges. |
| **Members** | Org members by **name and email**, their roles and teams. *(Owned by [ADR-0006] RBAC.)* |
| **Roles** | Role definitions and their grants. *(Owned by [ADR-0006] RBAC.)* |
| **Teams** | Teams by **team name**, membership and entitlements. |
| **Runs / Sessions** | Governed sessions by **agent name + member + status badge + relative start**, UUID as secondary detail; link into the trajectory. |
| **Approvals** | The escalation inbox: pending/decided approvals by **agent + member + action + reason**, status badges, approve/deny affordances. |
| **Spend** | Spend this period by **agent / member / team**, with tabular amounts and token counts. |
| **Audit (trajectories)** | The decision graph per session, readable event summaries with `structure`/`payload` class badges (§4). |

The sidebar reserves the **Members** and **Roles** slots for [ADR-0006]; until
RBAC lands they may be hidden or disabled, but the IA position is fixed now so
the nav does not reshuffle later.

**Member (chat-first, desktop — see [ADR-0007]) — conversation is the surface.**
Minimal chrome, tuned for "pick an agent, talk to it, watch it work":

- **Agent picker** — the member's entitled agents (`GET /me/agents`, per
  [ADR-0004]) by **name + description**, as the entry point.
- **Conversation** — the primary surface: the natural-language directive, the
  governed agent's streamed turns, tool calls, checkpoints, and escalations,
  rendered from the same tokens/components as the admin audit view so a run reads
  the same in both apps.
- **Session history** — prior conversations by **agent + a readable summary +
  relative time**, not a UUID list.
- **Minimal top chrome** — org/profile, theme toggle. No left admin sidebar.
- **No matrix.** The capability/permission matrix is **removed** from the member
  surface per the owner. (It is not part of the member IA.)

The member shell is the **minimal sidebar variant** of the same app shell; it
shares the top bar, tokens, badges, tables, and conversation rendering with the
admin app.

### 4. Human-readable everywhere — the core fix

**Rule: never show a raw UUID as a primary label.** Primary labels are always
the human name: **agent names, member names/emails, team names, policy names**,
**relative timestamps**, and **status as colored badges**. A raw UUID appears
**only** as *secondary, monospaced, truncated* detail on a **detail page**,
rendered with the **ID chip** component (§2) that offers **copy-to-clipboard**.
Tables get **real column headers, alignment, empty states, and row
affordances**. Status is **always** a colored badge from the shared vocabulary
(§5), never a bare enum string.

The **admin Overview replaces the JWT-claims dump** with a meaningful summary —
**active sessions, spend this period, pending approvals, recent activity**.
"Who I am" (subject, org, roles) moves into a **small profile menu** in the top
bar; it is not the page.

Concrete before → after (grounded in current source):

- **Dashboard** (`views/dashboard.templ`)
  - _Before:_ a `<dl>` of `Subject`, `Organization`, `Roles` — the token
    contents — as the landing content.
  - _After:_ the **Overview** — metric cards (active sessions, spend this
    period, pending approvals) over a **recent-activity** list of readable rows
    (agent name · member · relative time · status badge). Identity lives in the
    top-bar profile menu.
- **Sessions** (`views/sessions.templ`)
  - _Before:_ first column is `{ s.ID }`, the raw session UUID; status is the
    plain string `running`.
  - _After:_ primary column is the **agent name** (with member); columns are
    **Agent · Member · Status (badge) · Started (relative) · Fan-out**; the UUID
    is a copyable ID chip on the session's detail page, not the table's headline.
- **Trajectory / Audit** (`views/trajectory.templ`)
  - _Before:_ page titled `Session { data.SessionID }` (bare UUID); event rows
    are `type` + summary + class string + RFC-3339 time.
  - _After:_ page titled by **agent name + member + relative start**, with the
    session UUID as a secondary ID chip; event rows keep the indented decision
    graph but show `structure`/`payload` as **class badges** and **relative
    times**, with the raw timestamp on hover.
- **Buttons** (the invisible-button bug)
  - _Before:_ unstyled `<button>Refresh</button>` with no visible affordance.
  - _After:_ a **secondary** button variant (§2) with a real border, hover, and
    `--focus-ring` — visible at rest and on keyboard focus.

### 5. Consistency rules

The rules that keep the two shells one product, enforced by everyone building on
`internal/uikit`:

- **Shared spacing & typography.** All padding/gap/margin come from the spacing
  scale; all text from the type scale and system font stack. No ad-hoc pixel
  values in components.
- **One button hierarchy** — **primary / secondary / ghost / danger** — used the
  same way everywhere: one primary action per page header; destructive actions
  are always `danger`; low-emphasis actions are `ghost`.
- **Consistent page-header pattern** — every content page opens with **title +
  description + primary action** via the shared page-header component.
- **Consistent tables** — the same table component (headers, numeric alignment,
  tabular numerals, hover rows, empty state, row actions) across both apps; no
  bespoke tables.
- **One status vocabulary and color mapping** across **both** apps. The shared
  helper (§2) maps domain statuses to semantic tokens once:

| Status (examples) | Semantic | Token family |
|-------------------|----------|--------------|
| completed, approved, active, healthy | success | `--success-*` |
| pending, expiring, throttled, needs-attention | warning | `--warning-*` |
| failed, denied, error, revoked | danger | `--danger-*` |
| running, in-progress, `structure` class, informational | info | `--info-*` |

A status renders as the **same badge color in the admin audit view and the
member conversation**, because both call the same helper.

### 6. Accessibility & focus (locked)

- **Visible default + focus on every control** (buttons, links, inputs, selects,
  tabs, nav) via real resting styles and `:focus-visible` `--focus-ring`. This is
  the invisible-button fix and it is a *rule*, not a nicety.
- **WCAG-AA contrast** for all text and meaningful UI boundaries, verified in
  **both** light and dark themes when token hex values are set.
- **Keyboard operability** — logical tab order, a skip-to-content link in the app
  shell, focus trapping in modals, and status conveyed by **text + color**, never
  color alone (badges carry a label).
- **`prefers-reduced-motion`** honored for any transition; **`color-scheme`** set
  so native controls match the theme.

### 7. What this ADR does *not* decide

- **Final hex values** for tokens — set during implementation to the WCAG-AA bar;
  this ADR locks the token *names and intent*.
- **The CSS and templ implementation** of `internal/uikit`, and the migration of
  `internal/ui` onto it — later batches.
- **Member-console architecture, auth, and desktop packaging** — owned by
  [ADR-0004] and [ADR-0007]; this ADR only supplies their look and IA.
- **RBAC semantics** for the Members/Roles sections — owned by [ADR-0006]; this
  ADR reserves their nav slots and readable-label rules.

## Consequences

**Positive**

- **One coherent product.** A single token set and component library mean the
  admin dashboard and member console share type, color, spacing, status colors,
  and controls — they cannot drift because they compile from the same
  `internal/uikit` source.
- **The owner's complaints are answered by construction.** The Overview has
  meaning (not a token dump); every control is visible and focusable; UUIDs stop
  being primary labels; sessions and trajectories read in human terms; status is
  colored consistently.
- **Themes are cheap and correct.** Light/dark is a token value swap with no
  component churn, and both are held to WCAG-AA.
- **Stays inside the locked stack.** No JS framework, no bundler, no new
  dependency — CSS custom properties + templ + HTMX, exactly the toolkit
  `docs/TECH_STACK.md` fixes.
- **Later batches move fast.** Implementers get a named token catalog, a component
  inventory, and a settled IA, so UI-polish batches build rather than debate.

**Negative / costs**

- **A migration.** `internal/ui` must move off its inline `<style>` and bespoke
  markup onto `internal/uikit`; that is real work (a later batch) and touches
  every existing view.
- **Discipline required.** The system only holds if contributors use the
  components and tokens instead of one-off CSS; reviews must enforce "no ad-hoc
  pixel values, no bespoke tables/buttons."
- **A shared package is a coupling point.** A change to `internal/uikit` affects
  both apps; that is the point (consistency), but it means uikit changes need
  care and both surfaces re-checked.
- **Readable labels need data.** Showing agent/member/team **names** instead of
  UUIDs assumes those names are available to each view; where a name is missing,
  views must degrade gracefully (short ID chip) rather than fall back to the raw
  UUID as the headline.
- **Contrast tuning is iterative.** Hitting WCAG-AA in both themes for the full
  token set takes verification passes during implementation.

## Alternatives considered

- **Keep per-app inline styles (status quo).** Rejected: it is exactly what
  produced two skeletons and guarantees drift the moment either app changes. A
  shared source of truth is the whole point.
- **Adopt a CSS framework (Tailwind, Bootstrap) or a component library.**
  Rejected: Tailwind needs a build/pipeline step and litters utility classes
  through markup; Bootstrap imposes its own look and weight. Hand-authored design
  tokens + a small templ component set give a bespoke, on-brand system with **no
  build step** and total control of contrast — and stay within the locked stack.
- **Introduce a JS framework (React/Vue/Svelte) for the member chat UI.**
  Rejected and explicitly out of bounds: it breaks the server-rendered
  templ+HTMX architecture ([ADR-0004]), adds a bundler and a dependency tree, and
  splits the two apps onto different rendering models — the opposite of "one
  product." HTMX already covers the interactivity both surfaces need.
- **A theme via separate light/dark stylesheets.** Rejected: two stylesheets
  double the maintenance and drift. One stylesheet with CSS custom properties and
  a `data-theme` override is less code and cannot fall out of sync.
- **Ship tokens/components as a design doc only, no shared package.** Rejected:
  a document does not compile; without `internal/uikit` as importable code, both
  apps re-implement from prose and diverge. The package *is* the enforcement.
- **Keep UUIDs as primary labels with a tooltip name.** Rejected: it inverts the
  hierarchy the owner asked for. Names are primary; UUIDs are secondary,
  copyable detail on detail pages only.
- **Keep the flat top-nav for the admin app.** Rejected: it does not scale to the
  ten governance sections this IA needs; a grouped left sidebar is the readable,
  data-dense pattern for an admin console.

## Out of scope for this ADR (explicit)

- **Any code, CSS, or templates.** No `internal/uikit`, no restyle of
  `internal/ui`, no stylesheet, no `*.templ`. Implementation is later batches.
- **New dependencies, a JS framework, or a build step.** Explicitly forbidden by
  the brief and by `docs/TECH_STACK.md`.
- **Final token hex values, exact spacing/type numbers, and icon set.** Tuned in
  implementation to WCAG-AA; only names and intent are locked here.
- **Member-console architecture / auth / desktop packaging** — [ADR-0004],
  [ADR-0007].
- **RBAC model and the Members/Roles page behavior** — [ADR-0006].
- **Backend/API changes.** This ADR touches `openapi/`, `migrations/`, and Go
  source **not at all**; where a view needs a name it does not yet have, that is
  named as a data dependency for the implementing batch, not solved here.

## References

- `internal/ui` — the admin-ui pattern this ADR restyles: `views/layout.templ`
  (the inline `<style>` and flat top-nav being replaced), `views/dashboard.templ`
  (the JWT-claims `<dl>` → Overview), `views/sessions.templ` (`{ s.ID }` UUID
  column), `views/trajectory.templ` (`Session { data.SessionID }` title),
  `views/spend.templ`, `views/approvals.templ`, `README.md` (templ+HTMX,
  `templ generate`).
- `docs/TECH_STACK.md` — the locked **Go + templ + HTMX**, server-rendered,
  no-SPA stack this design system lives inside.
- `docs/ARCHITECTURE.md` — the planes and the admin vs member surfaces.
- [ADR-0002](0002-rest-api-conventions.md) — the DTO/naming conventions the
  readable view models draw names from.
- [ADR-0004](0004-member-conversational-console-architecture.md) — the member
  console architecture, `GET /me/agents`, trajectory and escalation rendering
  this design system styles; the templ+HTMX, no-JS-framework constraint.
- **ADR-0006** *(RBAC — to be authored)* — owns the **Members** and **Roles**
  admin sections this IA reserves.
- **ADR-0007** *(member desktop — to be authored)* — owns the member console's
  **desktop** shape; this ADR supplies its shared look and chat-first IA.
- WCAG 2.1 AA contrast (1.4.3 / 1.4.11) — the accessibility bar for token values
  in both themes.
