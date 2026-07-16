package uikit

import "strings"

// Variant is a semantic status color from the shared vocabulary (ADR-0005 §5).
// It selects a badge/alert style whose tokens are defined once in uikit.css, so
// the same domain status renders as the same color in the admin audit view and
// the member conversation.
type Variant string

const (
	// VariantNeutral is the default, un-emphasized style (no status meaning).
	VariantNeutral Variant = "neutral"
	// VariantSuccess marks completed, healthy, approved, active states.
	VariantSuccess Variant = "success"
	// VariantWarning marks pending, needs-attention, expiring, throttled states.
	VariantWarning Variant = "warning"
	// VariantDanger marks failed, denied, error, revoked, destructive states.
	VariantDanger Variant = "danger"
	// VariantInfo marks running/in-progress, structure-class, informational states.
	VariantInfo Variant = "info"
	// VariantAccent is the brand-tinted style (selected/highlighted, not a status).
	VariantAccent Variant = "accent"
)

// statusVariants maps known domain status strings to their semantic variant.
// The keys cover the vocabulary ADR-0005 §5 lists plus the close synonyms the
// control plane emits; unknown statuses fall back to [VariantNeutral].
var statusVariants = map[string]Variant{
	// success
	"completed": VariantSuccess,
	"complete":  VariantSuccess,
	"succeeded": VariantSuccess,
	"success":   VariantSuccess,
	"approved":  VariantSuccess,
	"active":    VariantSuccess,
	"healthy":   VariantSuccess,
	"done":      VariantSuccess,
	"ok":        VariantSuccess,
	"granted":   VariantSuccess,

	// warning
	"pending":         VariantWarning,
	"expiring":        VariantWarning,
	"throttled":       VariantWarning,
	"needs-attention": VariantWarning,
	"needs_attention": VariantWarning,
	"waiting":         VariantWarning,
	"queued":          VariantWarning,
	"paused":          VariantWarning,
	"degraded":        VariantWarning,

	// danger
	"failed":    VariantDanger,
	"failure":   VariantDanger,
	"denied":    VariantDanger,
	"error":     VariantDanger,
	"errored":   VariantDanger,
	"revoked":   VariantDanger,
	"cancelled": VariantDanger,
	"canceled":  VariantDanger,
	"expired":   VariantDanger,
	"rejected":  VariantDanger,
	"payload":   VariantDanger, // trajectory event class (ADR-0005 §4)

	// info
	"running":       VariantInfo,
	"in-progress":   VariantInfo,
	"in_progress":   VariantInfo,
	"inprogress":    VariantInfo,
	"structure":     VariantInfo, // trajectory event class (ADR-0005 §4)
	"info":          VariantInfo,
	"informational": VariantInfo,
	"new":           VariantInfo,
	"starting":      VariantInfo,
}

// StatusVariant maps a domain status string to its semantic [Variant] using the
// shared vocabulary. Matching is case-insensitive and ignores surrounding
// whitespace. Unrecognized statuses return [VariantNeutral] so a badge still
// renders (readably) rather than falling back to a bare enum string.
func StatusVariant(status string) Variant {
	if v, ok := statusVariants[strings.ToLower(strings.TrimSpace(status))]; ok {
		return v
	}
	return VariantNeutral
}

// badgeClass returns the CSS class for a badge of the given variant.
func badgeClass(v Variant) string { return "uk-badge uk-badge--" + string(variantOrNeutral(v)) }

// alertClass returns the CSS class for an alert of the given variant.
func alertClass(v Variant) string { return "uk-alert uk-alert--" + string(variantOrNeutral(v)) }

// alertRole returns the ARIA live role for an alert: assertive "alert" for
// warning/danger, polite "status" otherwise, so screen readers announce
// problems promptly without nagging on informational messages.
func alertRole(v Variant) string {
	switch v {
	case VariantWarning, VariantDanger:
		return "alert"
	default:
		return "status"
	}
}

// variantOrNeutral defaults an empty variant to neutral so a zero-value config
// still produces a valid, styled component.
func variantOrNeutral(v Variant) Variant {
	if v == "" {
		return VariantNeutral
	}
	return v
}
