package uikit

import (
	"strconv"

	"github.com/a-h/templ"
)

// This file holds the plain-Go configuration types and class helpers for the
// templ component library in components.templ. Keeping them here (rather than
// in the .templ file) means they compile and review independently of `templ
// generate`, while the templ markup stays focused on structure.
//
// The components are presentational and slot-based: callers pass fully-resolved
// display strings and, where a slot is needed, a templ.Component. A nil slot is
// simply omitted, so both apps compose the same parts into their own shells.

// --- App shell -------------------------------------------------------------

// ShellConfig configures [Shell], the outer page frame. Head is required (use
// [Head]); Sidebar is optional — omit it for the member app's minimal variant.
// The page's main content is supplied as the component's children.
type ShellConfig struct {
	// Lang is the <html lang> value; defaults to "en" when empty.
	Lang string
	// Head renders the document <head>; build it with [Head].
	Head templ.Component
	// TopBar renders the sticky top bar; build it with [TopBar]. Optional.
	TopBar templ.Component
	// Sidebar renders the left navigation; build it with [Sidebar]. When nil
	// the shell is full-width (the member "minimal" variant).
	Sidebar templ.Component
}

// HeadConfig configures [Head], the document <head> helper. It links the
// embedded stylesheet (content-hashed), applies the initial theme before first
// paint, and pulls in HTMX and the theme script.
type HeadConfig struct {
	// Title is the page title; rendered as "<Title> · drun-dash".
	Title string
	// Extra renders any additional <head> markup (app-specific meta/links).
	Extra templ.Component
}

// --- Sidebar ---------------------------------------------------------------

// SidebarConfig configures [Sidebar].
type SidebarConfig struct {
	// Brand is the product/word-mark shown at the top of the sidebar.
	Brand string
	// BrandHref is where the brand links; defaults to "/" when empty.
	BrandHref string
	// Groups are the navigation sections, rendered top to bottom.
	Groups []NavGroup
	// Footer renders optional pinned content at the bottom (e.g. a build tag).
	Footer templ.Component
}

// NavGroup is one labeled section of sidebar navigation.
type NavGroup struct {
	// Label is the uppercase section heading; omitted when empty.
	Label string
	// Items are the links in this section.
	Items []NavItem
}

// NavItem is a single sidebar link.
type NavItem struct {
	// Label is the human item name.
	Label string
	// Href is the link target.
	Href string
	// Icon renders an optional leading icon (e.g. an inline <svg>).
	Icon templ.Component
	// Active marks the current section (accent-tinted).
	Active bool
	// Disabled renders a non-interactive item (e.g. a nav slot reserved for a
	// not-yet-shipped feature, per ADR-0005's Members/Roles reservation).
	Disabled bool
	// Attrs are extra attributes merged onto the link (e.g. hx-* or target).
	Attrs templ.Attributes
}

// --- Top bar ---------------------------------------------------------------

// TopBarConfig configures [TopBar].
type TopBarConfig struct {
	// OrgName is the organization/word-mark on the left.
	OrgName string
	// Environment is an optional environment indicator (e.g. "staging").
	Environment string
	// HideThemeToggle omits the dark-mode toggle when true.
	HideThemeToggle bool
	// UserMenu renders the profile/user menu on the right (identity, sign-out).
	UserMenu templ.Component
	// Leading renders optional content immediately after the org name (e.g. a
	// mobile menu button or breadcrumb).
	Leading templ.Component
}

// --- Page header -----------------------------------------------------------

// PageHeaderConfig configures [PageHeader]: the title + description + primary
// action every content page opens with (ADR-0005 §5).
type PageHeaderConfig struct {
	// Title is the page heading.
	Title string
	// Description is optional supporting text under the title.
	Description string
	// Action renders the optional primary-action slot on the right (one per
	// page — typically a [Button]).
	Action templ.Component
}

// --- Card ------------------------------------------------------------------

// CardConfig configures [Card].
type CardConfig struct {
	// Title renders an optional card header heading.
	Title string
	// Action renders optional content on the right of the header (e.g. a
	// Refresh [Button]); only shown when Title is set.
	Action templ.Component
	// Class appends extra classes to the card element.
	Class string
	// Attrs are extra attributes on the card element (e.g. hx-* for a
	// self-refreshing fragment).
	Attrs templ.Attributes
}

// Metric is one figure in the [Metrics] row (Overview summary).
type Metric struct {
	// Label is the metric name (e.g. "Active sessions").
	Label string
	// Value is the pre-formatted display value (e.g. "12" or "$1,204.50").
	Value string
}

// --- Badge -----------------------------------------------------------------

// BadgeConfig configures [Badge]. Prefer [StatusBadge] when the variant comes
// from a domain status string.
type BadgeConfig struct {
	// Variant selects the semantic color; empty defaults to neutral.
	Variant Variant
	// Label is the badge text (always present — status is text + color, never
	// color alone, per ADR-0005 §6).
	Label string
}

// --- Buttons ---------------------------------------------------------------

// ButtonVariant is one of the four locked button styles (ADR-0005 §5): the
// single hierarchy used the same way in both apps.
type ButtonVariant string

const (
	// ButtonPrimary is the accent action; at most one per page header.
	ButtonPrimary ButtonVariant = "primary"
	// ButtonSecondary is the neutral-outline default action.
	ButtonSecondary ButtonVariant = "secondary"
	// ButtonGhost is a low-emphasis text button that fills on hover.
	ButtonGhost ButtonVariant = "ghost"
	// ButtonDanger is a destructive action (gets the danger focus ring).
	ButtonDanger ButtonVariant = "danger"
)

// ButtonConfig configures the [Button] component. Its content is the button's
// children; attach HTMX or other behavior via Attrs.
type ButtonConfig struct {
	// Variant selects the style; empty defaults to secondary.
	Variant ButtonVariant
	// Type is the HTML button type ("button", "submit", "reset"); defaults to
	// "button".
	Type string
	// Small renders the compact size (e.g. for table row actions).
	Small bool
	// Disabled renders a non-interactive button.
	Disabled bool
	// Attrs are extra attributes (e.g. hx-post, hx-target, name, value).
	Attrs templ.Attributes
}

// ButtonLinkConfig configures [ButtonLink], an <a> styled as a button.
type ButtonLinkConfig struct {
	// Variant selects the style; empty defaults to secondary.
	Variant ButtonVariant
	// Href is the link target.
	Href string
	// Small renders the compact size.
	Small bool
	// Disabled renders a non-interactive, dimmed link.
	Disabled bool
	// Attrs are extra attributes (e.g. target, hx-*).
	Attrs templ.Attributes
}

// --- Form controls ---------------------------------------------------------

// FieldConfig configures [Field], the label + control + help/error wrapper. The
// control itself is the component's children.
type FieldConfig struct {
	// Label is the field label text.
	Label string
	// For is the id of the control the label points at.
	For string
	// Required marks the field with a required indicator.
	Required bool
	// Help is optional supporting text under the control.
	Help string
	// Error is an optional error message; when set it replaces Help and marks
	// the field invalid.
	Error string
}

// InputConfig configures [Input].
type InputConfig struct {
	Type        string // defaults to "text"
	Name        string
	ID          string
	Value       string
	Placeholder string
	Invalid     bool
	Disabled    bool
	Attrs       templ.Attributes
}

// TextareaConfig configures [Textarea]. Value is rendered as the text content.
type TextareaConfig struct {
	Name        string
	ID          string
	Value       string
	Placeholder string
	Rows        int // defaults to 4
	Invalid     bool
	Disabled    bool
	Attrs       templ.Attributes
}

// SelectConfig configures [Select]. The <option> elements are the children.
type SelectConfig struct {
	Name     string
	ID       string
	Invalid  bool
	Disabled bool
	Attrs    templ.Attributes
}

// CheckConfig configures [Check], a checkbox or radio with an inline label.
type CheckConfig struct {
	// Radio renders a radio instead of a checkbox.
	Radio    bool
	Name     string
	ID       string
	Value    string
	Label    string
	Checked  bool
	Disabled bool
	Attrs    templ.Attributes
}

// --- Empty state -----------------------------------------------------------

// EmptyStateConfig configures [EmptyState]: the "no data yet" pattern.
type EmptyStateConfig struct {
	// Icon renders an optional illustrative icon (inline <svg>).
	Icon templ.Component
	// Title is the short headline (e.g. "No sessions yet").
	Title string
	// Message is optional supporting text.
	Message string
	// Action renders an optional call-to-action (e.g. a primary [Button]).
	Action templ.Component
}

// --- Alert -----------------------------------------------------------------

// AlertConfig configures [Alert], an inline message. Its body is the children.
type AlertConfig struct {
	// Variant selects the semantic color; empty defaults to neutral.
	Variant Variant
	// Title is an optional bold lead line above the body.
	Title string
}

// --- Modal -----------------------------------------------------------------

// ModalConfig configures [Modal], a native <dialog> (focus-trapped, Esc to
// close). Its body is the component's children.
type ModalConfig struct {
	// ID is the dialog element id; open it from a control carrying
	// data-uk-modal-open="<ID>".
	ID string
	// Title is the modal heading.
	Title string
	// Footer renders the action row (e.g. cancel + confirm buttons).
	Footer templ.Component
}

// ConfirmConfig configures [ConfirmDialog], a ready-made confirmation modal.
type ConfirmConfig struct {
	// ID is the dialog element id (open via data-uk-modal-open="<ID>").
	ID string
	// Title is the dialog heading (e.g. "Revoke session?").
	Title string
	// Message is the confirmation prompt body.
	Message string
	// ConfirmLabel is the confirm button text; defaults to "Confirm".
	ConfirmLabel string
	// ConfirmVariant selects the confirm button style; defaults to danger.
	ConfirmVariant ButtonVariant
	// ConfirmAttrs are attributes on the confirm button (e.g. hx-post to run
	// the action).
	ConfirmAttrs templ.Attributes
	// CancelLabel is the cancel button text; defaults to "Cancel".
	CancelLabel string
}

// --- ID chip & relative time ----------------------------------------------

// IDChipConfig configures [IDChip], the copyable monospaced UUID shown as
// secondary detail (never a primary label — ADR-0005 §4).
type IDChipConfig struct {
	// ID is the full identifier; copied verbatim to the clipboard on click.
	ID string
	// Display is the shown text; defaults to ID (truncated by CSS). Use a
	// shortened form (e.g. first 8 chars) when space is tight.
	Display string
	// Label is optional leading text (e.g. "Session").
	Label string
}

// RelativeTimeConfig configures [RelativeTime]. The app supplies both the
// human relative string and the absolute timestamp (uikit does no time math —
// it stays dependency-free and renders resolved view models).
type RelativeTimeConfig struct {
	// Relative is the shown text, e.g. "3 minutes ago".
	Relative string
	// Absolute is the machine/hover timestamp, e.g. an RFC-3339 stamp.
	Absolute string
}

// --- class helpers ---------------------------------------------------------

// buttonClass returns the class string for a button of the given variant/size.
func buttonClass(v ButtonVariant, small bool) string {
	cls := "uk-btn uk-btn--" + string(buttonVariantOrDefault(v))
	if small {
		cls += " uk-btn--sm"
	}
	return cls
}

// buttonVariantOrDefault defaults an empty variant to secondary so a
// zero-value config still renders a visible, styled button.
func buttonVariantOrDefault(v ButtonVariant) ButtonVariant {
	if v == "" {
		return ButtonSecondary
	}
	return v
}

// navItemClass returns the class string for a sidebar nav item.
func navItemClass(item NavItem) string {
	cls := "uk-nav-item"
	if item.Active {
		cls += " is-active"
	}
	if item.Disabled {
		cls += " is-disabled"
	}
	return cls
}

// stringOr returns v, or def when v is empty. Used to default optional config
// strings (type, lang, labels) at render time.
func stringOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// cardClass returns the class string for a card, appending any extra classes.
func cardClass(extra string) string {
	if extra == "" {
		return "uk-card"
	}
	return "uk-card " + extra
}

// shellClass returns the shell grid class, widening for the sidebar variant.
func shellClass(withSidebar bool) string {
	if withSidebar {
		return "uk-shell uk-shell--with-sidebar"
	}
	return "uk-shell"
}

// linkDisabledClass returns the class suffix that dims a disabled ButtonLink.
func linkDisabledClass(disabled bool) string {
	if disabled {
		return " is-disabled"
	}
	return ""
}

// ariaBool renders a boolean as an ARIA "true"/"false" string.
func ariaBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// checkType returns the input type for a [Check] (radio vs checkbox).
func checkType(radio bool) string {
	if radio {
		return "radio"
	}
	return "checkbox"
}

// textareaRows renders a rows count, defaulting to 4 for a non-positive value.
func textareaRows(rows int) string {
	if rows <= 0 {
		rows = 4
	}
	return strconv.Itoa(rows)
}

// confirmVariant defaults an empty confirm-button variant to danger, since a
// confirmation dialog usually gates a destructive action.
func confirmVariant(v ButtonVariant) ButtonVariant {
	if v == "" {
		return ButtonDanger
	}
	return v
}
