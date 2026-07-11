// Package policycompile turns a declarative governance boundary (the
// OpenAPI PolicyBoundary stored in policy_versions.content_json) into the
// enforceable sandbox configuration the drun engine consumes
// (pkg/policy.DrunConfig).
//
// These are two different vocabularies. A Boundary states governance intent —
// which tools an agent may use, its network egress posture, its fan-out
// ceiling, and free-form engine rules. A DrunConfig is the sandbox knob set
// that compiles one-to-one into ~/.drun/config.toml. ADR-0003 §2 fixes that
// this translation happens once, at session launch, and is deterministic:
// identical Boundary → identical DrunConfig → identical content_hash, so the
// signed bundle a client verifies is reproducible from the stored boundary.
//
// The concrete field mapping is owned by this batch (the ADR deliberately
// leaves it here). Compile is fail-closed on the one axis that matters for
// custody — network egress: an unrecognised or absent posture yields an empty
// (deny-all) domain allowlist rather than an omitted, drun-default one.
package policycompile

import (
	"encoding/json"
	"fmt"

	"github.com/Domene99/drun-dash/pkg/policy"
)

// Network egress postures (PolicyBoundary.network). The zero value ("") and any
// value outside this set compile fail-closed to a deny-all domain allowlist.
const (
	NetworkNone         = "none"
	NetworkAllowlist    = "allowlist"
	NetworkUnrestricted = "unrestricted"
)

// Boundary is the declarative governance artifact stored in
// policy_versions.content_json — the OpenAPI PolicyBoundary. It is decoded
// leniently: the schema is free-form by design (rules carries arbitrary
// engine-specific knobs), so unknown top-level keys are ignored rather than
// rejected.
type Boundary struct {
	// AllowedTools are the tools the agent may invoke. They compile to the
	// sandbox's bash_command_allowlist.
	AllowedTools []string `json:"allowed_tools"`
	// Network is the egress posture: none, allowlist, or unrestricted.
	Network string `json:"network"`
	// FanOutCeiling is the boundary's ceiling on concurrent sub-agents. It is a
	// pointer so an absent ceiling (no bound) is distinguishable from an
	// explicit 0; the launch handler folds it into the fan-out clamp.
	FanOutCeiling *int `json:"fan_out_ceiling"`
	// Rules carries the remaining engine-specific knobs (mount/env allowlists,
	// timeouts, resource limits, the network allowlist itself). Compile reads
	// the keys it understands via rulesKnobs and ignores the rest.
	Rules map[string]any `json:"rules"`
}

// rulesKnobs is the typed view of Boundary.Rules that Compile understands. The
// boundary's rules object is round-tripped through this struct so the
// translation is deterministic regardless of map iteration order, and unknown
// rule keys are simply dropped. Every field is optional; an absent key leaves
// the corresponding DrunConfig knob at the drun default.
type rulesKnobs struct {
	// NetworkAllowlist is the set of domains permitted under the "allowlist"
	// egress posture. Ignored for the other postures.
	NetworkAllowlist       []string `json:"network_allowlist"`
	MountAllowlist         []string `json:"mount_allowlist"`
	MountOverlayPaths      []string `json:"mount_overlay_paths"`
	EnvAllowlist           []string `json:"env_allowlist"`
	BashCommandDenylist    []string `json:"bash_command_denylist"`
	BashTimeoutMs          *int64   `json:"bash_timeout_ms"`
	FetchTimeoutMs         *int64   `json:"fetch_timeout_ms"`
	ConnectTimeoutMs       *int64   `json:"connect_timeout_ms"`
	MaxWorkspaceMb         *int64   `json:"max_workspace_mb"`
	MaxSessions            *int64   `json:"max_sessions"`
	MaxCheckpoints         *int64   `json:"max_checkpoints"`
	SessionIdleTimeoutSecs *int64   `json:"session_idle_timeout_secs"`
}

// ParseBoundary decodes a policy_versions.content_json blob into a [Boundary].
// It is lenient (unknown top-level keys are ignored) because the boundary
// schema is free-form; a syntactically invalid blob is an error the caller
// treats as a server-side misconfiguration.
func ParseBoundary(contentJSON json.RawMessage) (Boundary, error) {
	var b Boundary
	if len(contentJSON) == 0 {
		return Boundary{}, fmt.Errorf("policycompile: empty content_json")
	}
	if err := json.Unmarshal(contentJSON, &b); err != nil {
		return Boundary{}, fmt.Errorf("policycompile: decode boundary: %w", err)
	}
	return b, nil
}

// Compile deterministically translates a [Boundary] into a
// [policy.DrunConfig]. The same boundary always yields the same config (and
// therefore the same content_hash), as ADR-0003 §2 requires.
//
// Mapping:
//   - allowed_tools    → bash_command_allowlist (passed through as-is)
//   - network=none     → domain_allowlist = []            (deny all egress)
//   - network=allowlist→ domain_allowlist = rules.network_allowlist (or [])
//   - network=unrestricted → domain_allowlist omitted     (drun default: open)
//   - unknown/absent network → domain_allowlist = []      (fail closed)
//   - rules.*          → the remaining mount/env/timeout/limit knobs
//
// fan_out_ceiling is NOT compiled into the DrunConfig: it is a launch-time
// clamp the control plane applies to the session and grant, not a sandbox knob.
func Compile(b Boundary) (policy.DrunConfig, error) {
	knobs, err := decodeRules(b.Rules)
	if err != nil {
		return policy.DrunConfig{}, err
	}

	cfg := policy.DrunConfig{
		BashCommandAllowlist: cloneStrings(b.AllowedTools),
		MountAllowlist:       cloneStrings(knobs.MountAllowlist),
		MountOverlayPaths:    cloneStrings(knobs.MountOverlayPaths),
		EnvAllowlist:         cloneStrings(knobs.EnvAllowlist),
		BashCommandDenylist:  cloneStrings(knobs.BashCommandDenylist),

		BashTimeoutMs:          cloneInt(knobs.BashTimeoutMs),
		FetchTimeoutMs:         cloneInt(knobs.FetchTimeoutMs),
		ConnectTimeoutMs:       cloneInt(knobs.ConnectTimeoutMs),
		MaxWorkspaceMb:         cloneInt(knobs.MaxWorkspaceMb),
		MaxSessions:            cloneInt(knobs.MaxSessions),
		MaxCheckpoints:         cloneInt(knobs.MaxCheckpoints),
		SessionIdleTimeoutSecs: cloneInt(knobs.SessionIdleTimeoutSecs),
	}

	switch b.Network {
	case NetworkUnrestricted:
		// Leave domain_allowlist nil (omitted) so drun applies its open
		// default.
	case NetworkAllowlist:
		// Present-and-explicit: the named domains, or an empty allowlist when
		// the posture is "allowlist" but no domains were listed.
		if knobs.NetworkAllowlist != nil {
			cfg.DomainAllowlist = cloneStrings(knobs.NetworkAllowlist)
		} else {
			cfg.DomainAllowlist = []string{}
		}
	default:
		// none, "", or any unrecognised posture → deny all egress. Fail closed:
		// an unknown posture must never widen to the drun default.
		cfg.DomainAllowlist = []string{}
	}

	return cfg, nil
}

// decodeRules round-trips the free-form rules object through the typed
// [rulesKnobs] view. A nil rules object yields a zero rulesKnobs (all defaults).
func decodeRules(rules map[string]any) (rulesKnobs, error) {
	var knobs rulesKnobs
	if len(rules) == 0 {
		return knobs, nil
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return rulesKnobs{}, fmt.Errorf("policycompile: encode rules: %w", err)
	}
	if err := json.Unmarshal(raw, &knobs); err != nil {
		return rulesKnobs{}, fmt.Errorf("policycompile: decode rules: %w", err)
	}
	return knobs, nil
}

// cloneStrings returns a copy of s, preserving the nil/empty distinction: a nil
// input stays nil (the DrunConfig field is omitted) and a non-nil empty slice
// stays a non-nil empty slice (emitted as []).
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// cloneInt returns a copy of the pointed-to int64, or nil when v is nil, so the
// compiled config never aliases the caller's memory.
func cloneInt(v *int64) *int64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
