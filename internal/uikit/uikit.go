// Package uikit is the shared design system for drun-dash — the single source
// of truth for design tokens and UI components that BOTH the admin web app
// (internal/ui) and the member app (internal/memberui) compose, so their
// visuals cannot drift. It implements ADR-0005.
//
// The package is deliberately presentation-only: like internal/ui/views it
// imports no control-plane packages and holds no business logic, so the
// dependency edge runs one way (each app -> uikit) and never the reverse.
// Components receive fully-resolved view models (plain strings the apps
// assemble) and render markup that references design tokens only.
//
// What it provides:
//
//   - An embedded stylesheet (assets/uikit.css) — tokens (:root light +
//     [data-theme="dark"] override) plus the component styles — served by each
//     app at a stable, content-hashed path via [Handler].
//   - A minimal vanilla theme-toggle script (assets/theme-toggle.js), embedded
//     and linked by the [Head] component.
//   - A templ component library (see components.templ): [Shell], [Sidebar],
//     [TopBar], [PageHeader], [Card], [Table], [Badge], [Button], form
//     controls, [EmptyState], [Alert], [Modal], [IDChip], [RelativeTime], …
//   - Status vocabulary helpers ([StatusVariant]) mapping domain status strings
//     to the shared semantic tokens, so "running"/"approved"/"denied" render
//     identically in both apps.
//
// Serving the assets. Each app mounts [Handler] under [MountPrefix]:
//
//	mux.Handle(uikit.MountPrefix, uikit.Handler())
//
// and renders the page head with [Head], which links [StylesheetURL] (a
// content-hashed URL for cache-busting) and the theme script. The generated
// *_templ.go files are committed; regenerate with `templ generate` after
// editing any *.templ (see README.md). Edit the .templ, never the generated Go.
package uikit

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

// assetsFS holds the embedded stylesheet and theme script. The files under
// assets/ are the design system's runtime payload; everything else in the
// package renders markup that points at them.
//
//go:embed assets/uikit.css assets/theme-toggle.js
var assetsFS embed.FS

// Asset filenames within the embedded assets/ directory.
const (
	stylesheetName = "uikit.css"
	scriptName     = "theme-toggle.js"
)

// MountPrefix is the URL prefix each app mounts [Handler] under. The stylesheet
// and theme script are served beneath it (see [StylesheetPath], [ScriptPath]).
const MountPrefix = "/uikit/"

// StylesheetPath and ScriptPath are the canonical request paths for the two
// embedded assets, served by [Handler]. Use [StylesheetURL] / [ScriptURL] when
// linking them from a page so the URL carries a content hash for cache-busting.
const (
	StylesheetPath = MountPrefix + stylesheetName
	ScriptPath     = MountPrefix + scriptName
)

// htmxScriptURL is the pinned HTMX runtime linked by [Head]. Both apps drive
// their interactivity with HTMX (ADR-0005 locks "HTMX only, no JS framework"),
// so the design system links one pinned version for consistency. Swap for a
// vendored asset if serving from a CDN is undesirable in your deployment.
const htmxScriptURL = "https://cdn.jsdelivr.net/npm/htmx.org@2.0.4/dist/htmx.min.js"

// contentHashes maps an asset filename to a short hex content hash, computed
// once at init from the embedded bytes. The hash is appended as a ?v= query so
// a changed asset gets a fresh URL while an unchanged one stays cacheable.
var contentHashes = buildContentHashes()

func buildContentHashes() map[string]string {
	hashes := make(map[string]string, 2)
	for _, name := range []string{stylesheetName, scriptName} {
		b, err := assetsFS.ReadFile("assets/" + name)
		if err != nil {
			// Embedded files are compiled in; a miss is a build-time bug.
			panic("uikit: embedded asset missing: " + name)
		}
		sum := sha256.Sum256(b)
		hashes[name] = hex.EncodeToString(sum[:])[:12]
	}
	return hashes
}

// StylesheetURL returns the cache-busting URL for the stylesheet: its serve
// path plus a ?v=<hash> query. Link this from a page head (Head does so).
func StylesheetURL() string { return StylesheetPath + "?v=" + contentHashes[stylesheetName] }

// ScriptURL returns the cache-busting URL for the theme script.
func ScriptURL() string { return ScriptPath + "?v=" + contentHashes[scriptName] }

// Handler serves the embedded assets under [MountPrefix]. Mount it with:
//
//	mux.Handle(uikit.MountPrefix, uikit.Handler())
//
// It serves only the known asset files (any other path 404s), sets a correct
// Content-Type, and — because linked URLs carry a content hash — marks the
// responses immutably cacheable. The ?v= query is ignored when resolving the
// file, so both the bare path and the hashed URL serve the same bytes.
func Handler() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic("uikit: cannot open embedded assets sub-fs: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, MountPrefix)
		if name != stylesheetName && name != scriptName {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch name {
		case stylesheetName:
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case scriptName:
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		if r.URL.RawQuery != "" {
			// Hashed URL: safe to cache forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		//nolint:errcheck // best-effort write to the response body
		w.Write(b)
	})
}
