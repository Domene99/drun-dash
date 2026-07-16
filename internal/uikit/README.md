# internal/uikit

The shared **design system** for drun-dash — the single source of truth for
design **tokens** and UI **components** that BOTH the admin web app
(`internal/ui`) and the member app (`internal/memberui`) compose, so their
visuals cannot drift. It implements
[ADR-0005](../../docs/adr/0005-design-system-and-information-architecture.md).

Stack: **Go + [templ](https://templ.guide) + hand-authored CSS**, `go:embed`,
HTMX (consumed by the apps). **No CSS/JS framework, no build step, no new deps.**

> This batch builds the package only. Restyling `internal/ui` and
> `internal/memberui` onto it is a **later batch** — they are not touched here.

## What's here

```
internal/uikit/
├── assets/uikit.css          # design tokens (:root light + [data-theme=dark]) + component styles
├── assets/theme-toggle.js    # the only JS: theme toggle, modal open/close, copy-to-clipboard
├── uikit.go                  # go:embed, Handler(), mount paths, content-hashed URLs, docs
├── status.go                 # shared status vocabulary → semantic Variant (ADR-0005 §5)
├── components.go             # component config types + class helpers
├── components.templ          # the templ component library (source of truth)
├── components_templ.go       # generated from components.templ (committed) — DO NOT EDIT
└── uikit_test.go             # serving + rendering + status-mapping tests
```

The package is **presentation-only**: it imports no control-plane packages and
holds no business logic, so the dependency edge runs one way (each app →
`uikit`). Components receive fully-resolved display strings and, where a slot is
needed, a `templ.Component`; a nil slot is simply omitted.

## Design tokens

All tokens are CSS custom properties in `assets/uikit.css`. Dark mode reassigns
the **same** token names under `[data-theme="dark"]` (and, absent an explicit
choice, `@media (prefers-color-scheme: dark)`), so theming is a value swap with
zero component changes. Both themes are held to WCAG-AA contrast.

Token groups (see the ADR for intent): `--neutral-50…900`;
`--accent` / `--accent-hover` / `--accent-subtle` / `--accent-contrast`;
semantic `--success-*` / `--warning-*` / `--danger-*` / `--info-*` (each
`-fg`/`-bg`/`-border`); surface + text aliases (`--surface`, `--surface-raised`,
`--surface-sunken`, `--bg`, `--border`, `--text`, `--text-muted`, `--link`, …);
a spacing scale (`--space-1…8`); a radius scale (`--radius-sm/md/lg/pill`); a
typography scale (`--font-sans`/`--font-mono`, `--text-xs…2xl`, weights,
leading); shadows (`--shadow-sm/md/lg`); and a visible focus ring
(`--focus-ring`, `--focus-ring-danger`).

**Every** focusable control gets a visible resting style and a `:focus-visible`
ring — the fix for the invisible-button bug.

The hex values are indicative and tuned to WCAG-AA; the token **names and
intent** are the contract components rely on.

## Serving the assets

Each app mounts the embedded assets under `uikit.MountPrefix` and renders its
page head with the `Head` component:

```go
import "github.com/Domene99/drun-dash/internal/uikit"

// wiring:
mux.Handle(uikit.MountPrefix, uikit.Handler()) // serves /uikit/uikit.css and /uikit/theme-toggle.js
```

```templ
// a page = Shell(head, top bar, optional sidebar) wrapping the content:
@uikit.Shell(uikit.ShellConfig{
    Head:    uikit.Head(uikit.HeadConfig{Title: "Overview"}),
    TopBar:  uikit.TopBar(uikit.TopBarConfig{OrgName: org, Environment: env, UserMenu: profileMenu()}),
    Sidebar: uikit.Sidebar(uikit.SidebarConfig{Brand: "drun-dash", Groups: nav}),
}) {
    @uikit.PageHeader(uikit.PageHeaderConfig{Title: "Overview", Description: "..."})
    @uikit.Metrics(metrics)
    // ...cards, tables, badges...
}
```

`Head` links a **content-hashed** stylesheet URL (`uikit.StylesheetURL()`,
e.g. `/uikit/uikit.css?v=<hash>`) so a changed asset busts caches while an
unchanged one stays cacheable, plus HTMX and the theme script. It also inlines a
tiny script that applies the stored theme **before first paint** (no flash). The
member app omits `Sidebar` for its minimal chat-first variant.

## Components

`Shell`, `Head`, `Sidebar`, `TopBar`, `ThemeToggle`, `PageHeader`, `Card`,
`Metrics`, `Table`, `Badge`/`StatusBadge`, `Button`/`ButtonLink` (primary /
secondary / ghost / danger), form controls (`Field`, `Input`, `Textarea`,
`Select`, `Check`), `EmptyState`, `Alert`, `Modal`/`ConfirmDialog`, `IDChip`,
`RelativeTime`. Config types and per-component docs are in `components.go`.

**Human-readable rule (ADR-0005 §4):** never show a raw UUID as a primary label.
Use `IDChip` (copyable, monospaced, truncated) for a UUID as *secondary* detail,
`RelativeTime` for timestamps, and `StatusBadge` for status — never a bare enum
string.

The `theme-toggle.js` handlers are all delegated on `document`, so they work for
markup HTMX adds later: `[data-uk-theme-toggle]`, `[data-uk-modal-open="<id>"]`,
`[data-uk-modal-close]`, `[data-uk-copy="<text>"]`.

## Regenerating

The `*.templ` files are the source of truth; the committed `*_templ.go` are
generated. After editing any `.templ`, regenerate and commit both:

```sh
templ generate ./internal/uikit
go test ./internal/uikit/
```

Never hand-edit `components_templ.go`. Keep the `templ` binary version in sync
with the `github.com/a-h/templ` version in `go.mod` so regenerated output stays
byte-stable.
