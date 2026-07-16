//go:build !desktop

// Command member-app is the drun-dash member standalone desktop application: a
// native OS window (a webview) over the same internal/memberui server that
// cmd/member-console serves, but run in-process on an ephemeral loopback port
// (ADR-0005, "member standalone desktop app"). The window renders the existing
// templ+HTMX member surface verbatim — agent picker, governed conversation,
// checkpoint timeline, diff-before-commit, approvals — as a real app instead of
// a browser tab. Governance is unchanged: the server is local (loopback,
// ephemeral port), the drun engine is local, and only the same
// identity/model/governance traffic ADR-0004 already sends leaves the machine.
//
// The desktop window is cgo: it links the platform web engine (macOS WebKit,
// Windows WebView2, Linux WebKitGTK) through github.com/webview/webview_go. That
// toolchain is a packaging-time requirement, not a build-time one, so the real
// app lives behind the "desktop" build tag in app_desktop.go and is compiled
// only by:
//
//	go build -tags desktop ./cmd/member-app
//
// This file is the default (non-desktop) build. It carries the mutually
// exclusive "!desktop" tag so exactly one main() compiles per tag set, and it
// keeps `go build ./...` and CI cgo-free and green by compiling a tiny stub that
// tells the user how to build the real app. See cmd/member-app/README.md for
// the macOS .app packaging recipe.
package main

import "fmt"

func main() {
	fmt.Println("member-app: this binary was built without the desktop UI.")
	fmt.Println("Build the desktop app with: go build -tags desktop ./cmd/member-app")
	fmt.Println("(needs cgo and the platform web engine; see cmd/member-app/README.md)")
}
