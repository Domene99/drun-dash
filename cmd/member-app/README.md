# cmd/member-app — member standalone desktop app

`member-app` is the drun-dash member surface as a **standalone desktop
application**: a native OS window (a webview) over the exact same
`internal/memberui` server that [`cmd/member-console`](../member-console) serves,
run **in-process on an ephemeral loopback port**. The window renders the existing
templ + HTMX pages verbatim — agent picker, governed conversation (SSE-streamed
turns), checkpoint timeline, diff-before-commit, and approvals — as a real app
instead of a browser tab.

It implements [ADR-0005 — *Member standalone desktop app*](../../docs/adr/0007-member-standalone-desktop-app.md)
(the tracking issue calls it ADR-0007; see the ADR's numbering note). Everything
governance-related is unchanged from ADR-0004: the server is local (loopback,
ephemeral port), the drun engine is local, and only the same
identity/model/governance traffic already leaves the machine.

## How it works

1. Build the control-plane `memberui.Client` and `memberui.Server` exactly like
   `cmd/member-console` (same device flow, same run view, same
   checkpoints/diff/approvals), reading the same environment (below).
2. Bind `127.0.0.1:0` — an **OS-assigned ephemeral loopback port** — and serve
   the member UI in-process on that listener. Loopback-only keeps it reachable
   solely from this machine; the fresh port never collides with a separately-run
   `cmd/member-console`.
3. Open a native OS window with
   [`github.com/webview/webview_go`](https://github.com/webview/webview_go) and
   navigate it to `http://127.0.0.1:<port>/`. `webview.Run()` blocks on the main
   OS thread for the window's life.
4. When the window closes, **shut the in-process server down gracefully**
   (`http.Server.Shutdown`) and exit — no orphaned server or listener.

**drun engine & auth are unchanged.** The app reads `DRUN_MCP_ENDPOINT`
(default `http://127.0.0.1:7273/mcp`) and detects the local engine exactly as
`internal/memberui` already does. Sign-in is the RFC 8628 device flow: the
member surface shows the `user_code` and `verification_uri`, and OIDC login is
completed in the member's **default browser** (per ADR-0005 §3), distinct from
the app's own window. The desktop app introduces no new credential store — it
runs the same server, so it inherits the shipped secure-cookie session.

## Configuration

Same environment as `cmd/member-console`, **minus the listen address** — the
desktop app always binds an ephemeral loopback port, so there is nothing to
configure there:

| Variable | Default | Purpose |
| --- | --- | --- |
| `CONTROL_PLANE_BASE_URL` | `http://localhost:8080` | control-plane REST base URL |
| `MEMBER_CONSOLE_CLIENT_ID` | `drun-dash-member-console` | device-flow `client_id` |
| `MEMBER_CONSOLE_MODEL` | *(unset)* | model id passed to the governed proxy (required for runs) |
| `DRUN_MCP_ENDPOINT` | `http://127.0.0.1:7273/mcp` | local drun engine endpoint |
| `MEMBER_CONSOLE_APPROVAL_TIMEOUT` | `5m` | how long a policy-blocked step waits before failing closed |
| `MEMBER_CONSOLE_APPROVAL_POLL_INTERVAL` | `3s` | escalation-queue poll interval while waiting |
| `ENV` | `development` | `production` enables secure (HTTPS-only) cookies |

## Building

The desktop window is **cgo** and links the platform web engine
(macOS **WebKit**, Windows **WebView2**, Linux **WebKitGTK**). That toolchain is
a packaging-time requirement, **not** a build-time one, so the webview code lives
behind the `desktop` build tag.

- **Default build (no cgo, no webview) — what CI runs:**

  ```sh
  go build ./...            # compiles the cmd/member-app stub; stays green
  ```

  The default build compiles [`main.go`](main.go) (`//go:build !desktop`), a tiny
  stub that prints how to build the real app and exits 0. No cgo, no platform
  web engine — CI and `go build ./...` never need the webview toolchain.

- **Desktop build (the real app)** — on a machine with cgo and the platform web
  engine's dev headers:

  ```sh
  go build -tags desktop ./cmd/member-app
  ```

  This compiles [`app_desktop.go`](app_desktop.go) (`//go:build desktop`), which
  imports `github.com/webview/webview_go`. The two files carry mutually
  exclusive tags, so exactly one `main()` compiles per tag set.

`github.com/webview/webview_go` appears in `go.mod` because `go mod tidy` treats
all build tags as enabled and sees the tagged import; it is only *compiled* under
`-tags desktop`.

> There is deliberately **no `make member-app` target** — adding one would edit
> the root `Makefile`, outside this component's file ownership. The one-line
> command above is the supported entry point; wire it into `make` in a later
> change if desired.

### macOS `.app` packaging (v1 target)

Packaging is a **local/release step**, not required by CI. To assemble a
double-clickable `.app` bundle on macOS:

```sh
APP="drun Member.app"
BIN="member-app"

# 1. Build the desktop binary against system WebKit.
CGO_ENABLED=1 go build -tags desktop -o "$BIN" ./cmd/member-app

# 2. Lay out the bundle.
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/$BIN"

# 3. Info.plist (minimal).
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>            <string>drun Member</string>
  <key>CFBundleDisplayName</key>     <string>drun Member</string>
  <key>CFBundleIdentifier</key>      <string>dev.drun.member-app</string>
  <key>CFBundleVersion</key>         <string>1.0.0</string>
  <key>CFBundleShortVersionString</key> <string>1.0.0</string>
  <key>CFBundlePackageType</key>     <string>APPL</string>
  <key>CFBundleExecutable</key>      <string>member-app</string>
  <key>NSHighResolutionCapable</key> <true/>
</dict>
</plist>
PLIST

# 4. (Optional) drop an icon at Contents/Resources/AppIcon.icns and add
#    <key>CFBundleIconFile</key><string>AppIcon</string> to Info.plist.
# 5. (Optional, for local distribution) codesign:
#    codesign --deep --force --sign - "$APP"
```

Windows (WebView2 + `.exe`/installer) and Linux (WebKitGTK + `.desktop` entry)
packaging are **named follow-ups** in ADR-0005 §5, not part of v1. On those
platforms use the cgo-free `cmd/member-console` web binary until then.

## Relationship to `cmd/member-console`

`cmd/member-console` stays as the **plain, cgo-free web binary** — buildable
everywhere, ideal for development, headless/remote use, and CI. `member-app`
**reuses that server in-process** rather than replacing it, so the two share one
code path and behavior; only the shell (native window vs. browser tab) differs.
