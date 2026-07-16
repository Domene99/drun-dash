package uikit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// render is a small helper that renders a templ component to a string.
func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func TestHandlerServesStylesheet(t *testing.T) {
	// httptest.NewRecorder does not bind a socket, so this runs in the sandbox.
	req := httptest.NewRequest(http.MethodGet, StylesheetURL(), nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	body := rec.Body.String()
	// Tokens: the light palette and the dark override must both be present,
	// since theming is a value swap on the same names (ADR-0005 §1).
	for _, want := range []string{":root", `[data-theme="dark"]`, "--accent", "--focus-ring", "--neutral-900", ".uk-btn--primary"} {
		if !strings.Contains(body, want) {
			t.Errorf("stylesheet missing %q", want)
		}
	}
	// Cache-busting query => immutable caching.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
}

func TestHandlerServesScript(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, ScriptURL(), nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "uikit-theme") {
		t.Error("theme script missing localStorage key")
	}
}

func TestHandlerUnknownPath404(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, MountPrefix+"nope.css", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAssetURLsCarryContentHash(t *testing.T) {
	if !strings.HasPrefix(StylesheetURL(), StylesheetPath+"?v=") {
		t.Errorf("StylesheetURL = %q, want %s?v=<hash>", StylesheetURL(), StylesheetPath)
	}
	if !strings.HasPrefix(ScriptURL(), ScriptPath+"?v=") {
		t.Errorf("ScriptURL = %q, want %s?v=<hash>", ScriptURL(), ScriptPath)
	}
	if !strings.HasPrefix(CoreScriptURL(), CoreScriptPath+"?v=") {
		t.Errorf("CoreScriptURL = %q, want %s?v=<hash>", CoreScriptURL(), CoreScriptPath)
	}
	if !strings.HasPrefix(SSEExtensionURL(), SSEExtensionPath+"?v=") {
		t.Errorf("SSEExtensionURL = %q, want %s?v=<hash>", SSEExtensionURL(), SSEExtensionPath)
	}
}

// TestHandlerServesVendoredHTMX proves the governed desktop app depends on no
// CDN: the HTMX runtime and its SSE extension are embedded and served locally,
// content-hashed and immutably cacheable like the other assets.
func TestHandlerServesVendoredHTMX(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string // a marker the real vendored file contains
	}{
		{"htmx core v2.0.4", CoreScriptURL(), "htmx"},
		{"htmx-ext-sse v2.2.2", SSEExtensionURL(), "defineExtension('sse'"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.url, nil)
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("%s: Content-Type = %q, want text/javascript", tc.name, ct)
		}
		if body := rec.Body.String(); !strings.Contains(body, tc.want) {
			t.Errorf("%s: served asset missing marker %q (wrong file vendored?)", tc.name, tc.want)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control = %q, want immutable", tc.name, cc)
		}
	}
}

// TestHeadLinksVendoredHTMXNotCDN guards the no-CDN rule at the head: HTMX core
// is linked from the local content-hashed path, never from a third-party origin.
func TestHeadLinksVendoredHTMXNotCDN(t *testing.T) {
	out := render(t, Head(HeadConfig{Title: "Chat"}))
	if !strings.Contains(out, CoreScriptPath) {
		t.Errorf("head should link the local HTMX runtime %q: %s", CoreScriptPath, out)
	}
	if strings.Contains(out, "jsdelivr") || strings.Contains(out, "cdn.") || strings.Contains(out, "//unpkg") {
		t.Errorf("head must not link HTMX from a CDN: %s", out)
	}
}

func TestStatusVariant(t *testing.T) {
	cases := map[string]Variant{
		"completed":   VariantSuccess,
		"APPROVED":    VariantSuccess, // case-insensitive
		"  active  ":  VariantSuccess, // trimmed
		"pending":     VariantWarning,
		"failed":      VariantDanger,
		"denied":      VariantDanger,
		"running":     VariantInfo,
		"in_progress": VariantInfo,
		"structure":   VariantInfo,
		"payload":     VariantDanger,
		"wat":         VariantNeutral, // unknown falls back
		"":            VariantNeutral,
	}
	for status, want := range cases {
		if got := StatusVariant(status); got != want {
			t.Errorf("StatusVariant(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestButtonRendersVisibleVariant(t *testing.T) {
	// The invisible-button fix: a button always renders a real variant class,
	// even from a zero-value config (defaults to secondary).
	out := render(t, Button(ButtonConfig{}))
	if !strings.Contains(out, "uk-btn uk-btn--secondary") {
		t.Errorf("zero-value button missing default variant class: %s", out)
	}
	out = render(t, Button(ButtonConfig{Variant: ButtonPrimary, Type: "submit", Attrs: templ.Attributes{"hx-post": "/x"}}))
	for _, want := range []string{"uk-btn--primary", `type="submit"`, `hx-post="/x"`} {
		if !strings.Contains(out, want) {
			t.Errorf("button missing %q: %s", want, out)
		}
	}
}

func TestStatusBadgeMapsColorAndKeepsLabel(t *testing.T) {
	out := render(t, StatusBadge("running"))
	if !strings.Contains(out, "uk-badge--info") {
		t.Errorf("running badge should be info: %s", out)
	}
	// Status is text + color, never color alone (ADR-0005 §6).
	if !strings.Contains(out, ">running<") {
		t.Errorf("badge should carry its label text: %s", out)
	}
}

func TestIDChipIsCopyableSecondaryDetail(t *testing.T) {
	id := "018f9a2b-0000-7000-8000-000000000000"
	out := render(t, IDChip(IDChipConfig{ID: id, Display: "018f9a2b"}))
	if !strings.Contains(out, `data-uk-copy="`+id+`"`) {
		t.Errorf("id chip should carry full id for copy: %s", out)
	}
	if !strings.Contains(out, ">018f9a2b<") {
		t.Errorf("id chip should show the truncated display: %s", out)
	}
}

func TestShellComposesSlots(t *testing.T) {
	page := Shell(ShellConfig{
		Head:    Head(HeadConfig{Title: "Overview"}),
		Sidebar: Sidebar(SidebarConfig{Brand: "drun-dash", Groups: []NavGroup{{Items: []NavItem{{Label: "Overview", Href: "/", Active: true}}}}}),
		TopBar:  TopBar(TopBarConfig{OrgName: "Acme"}),
	})
	out := render(t, page)
	for _, want := range []string{
		"<!doctype html>",
		"uk-shell--with-sidebar",
		"uk-skip-link",
		`href="#uk-content"`,
		StylesheetPath,         // stylesheet linked
		"Overview · drun-dash", // title
		"data-uk-theme-toggle", // theme toggle present
		`aria-current="page"`,  // active nav item
	} {
		if !strings.Contains(out, want) {
			t.Errorf("shell missing %q", want)
		}
	}
}

func TestShellWithoutSidebarIsFullWidth(t *testing.T) {
	// The member app uses the minimal (no-sidebar) variant.
	out := render(t, Shell(ShellConfig{Head: Head(HeadConfig{Title: "Chat"})}))
	if strings.Contains(out, "uk-shell--with-sidebar") {
		t.Errorf("sidebarless shell must not use the sidebar grid: %s", out)
	}
	if !strings.Contains(out, `class="uk-shell"`) {
		t.Errorf("expected full-width shell class: %s", out)
	}
}

func TestEmptyStateReplacesBareText(t *testing.T) {
	out := render(t, EmptyState(EmptyStateConfig{Title: "No sessions yet", Message: "Sessions appear here once a member runs an agent."}))
	if !strings.Contains(out, "uk-empty-title") || !strings.Contains(out, "No sessions yet") {
		t.Errorf("empty state missing title: %s", out)
	}
}

func TestInputMarksInvalid(t *testing.T) {
	out := render(t, Input(InputConfig{Name: "token", Invalid: true}))
	if !strings.Contains(out, `aria-invalid="true"`) {
		t.Errorf("invalid input should set aria-invalid=true: %s", out)
	}
	out = render(t, Input(InputConfig{Name: "token"}))
	if !strings.Contains(out, `aria-invalid="false"`) {
		t.Errorf("valid input should set aria-invalid=false: %s", out)
	}
}

func TestConfirmDialogHasCancelAndConfirm(t *testing.T) {
	out := render(t, ConfirmDialog(ConfirmConfig{
		ID:           "revoke",
		Title:        "Revoke session?",
		Message:      "This ends the session immediately.",
		ConfirmLabel: "Revoke",
		ConfirmAttrs: templ.Attributes{"hx-post": "/sessions/x/revoke"},
	}))
	for _, want := range []string{
		`<dialog id="revoke"`,
		"uk-modal",
		"data-uk-modal-close",          // cancel + close button
		`hx-post="/sessions/x/revoke"`, // confirm action wired
		"uk-btn--danger",               // confirm defaults to danger
		"Revoke",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("confirm dialog missing %q: %s", want, out)
		}
	}
}
