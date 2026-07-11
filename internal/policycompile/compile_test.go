package policycompile

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Domene99/drun-dash/pkg/policy"
)

func mustParse(t *testing.T, s string) Boundary {
	t.Helper()
	b, err := ParseBoundary(json.RawMessage(s))
	if err != nil {
		t.Fatalf("ParseBoundary(%s): %v", s, err)
	}
	return b
}

func mustCompile(t *testing.T, b Boundary) policy.DrunConfig {
	t.Helper()
	cfg, err := Compile(b)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return cfg
}

// TestCompileNetworkNone proves network=none denies all egress with a present,
// empty domain allowlist (not an omitted one).
func TestCompileNetworkNone(t *testing.T) {
	cfg := mustCompile(t, mustParse(t, `{"network":"none"}`))
	if cfg.DomainAllowlist == nil {
		t.Fatal("domain_allowlist = nil, want [] (deny all)")
	}
	if len(cfg.DomainAllowlist) != 0 {
		t.Errorf("domain_allowlist = %v, want []", cfg.DomainAllowlist)
	}
}

// TestCompileNetworkAllowlist proves the allowlist posture uses the domains
// listed under rules.network_allowlist.
func TestCompileNetworkAllowlist(t *testing.T) {
	cfg := mustCompile(t, mustParse(t, `{"network":"allowlist","rules":{"network_allowlist":["api.example.com","cdn.example.com"]}}`))
	want := []string{"api.example.com", "cdn.example.com"}
	if !reflect.DeepEqual(cfg.DomainAllowlist, want) {
		t.Errorf("domain_allowlist = %v, want %v", cfg.DomainAllowlist, want)
	}
}

// TestCompileNetworkAllowlistEmpty proves the allowlist posture with no listed
// domains still yields a present, empty allowlist (deny all until listed).
func TestCompileNetworkAllowlistEmpty(t *testing.T) {
	cfg := mustCompile(t, mustParse(t, `{"network":"allowlist"}`))
	if cfg.DomainAllowlist == nil || len(cfg.DomainAllowlist) != 0 {
		t.Errorf("domain_allowlist = %v, want []", cfg.DomainAllowlist)
	}
}

// TestCompileNetworkUnrestricted proves the unrestricted posture omits the
// domain allowlist so drun applies its open default.
func TestCompileNetworkUnrestricted(t *testing.T) {
	cfg := mustCompile(t, mustParse(t, `{"network":"unrestricted"}`))
	if cfg.DomainAllowlist != nil {
		t.Errorf("domain_allowlist = %v, want nil (omitted)", cfg.DomainAllowlist)
	}
}

// TestCompileUnknownNetworkFailsClosed proves an absent or unrecognised posture
// denies egress rather than widening to the drun default.
func TestCompileUnknownNetworkFailsClosed(t *testing.T) {
	for _, s := range []string{`{}`, `{"network":""}`, `{"network":"wide-open"}`} {
		cfg := mustCompile(t, mustParse(t, s))
		if cfg.DomainAllowlist == nil || len(cfg.DomainAllowlist) != 0 {
			t.Errorf("%s: domain_allowlist = %v, want [] (fail closed)", s, cfg.DomainAllowlist)
		}
	}
}

// TestCompileAllowedTools proves allowed_tools becomes bash_command_allowlist.
func TestCompileAllowedTools(t *testing.T) {
	cfg := mustCompile(t, mustParse(t, `{"network":"none","allowed_tools":["bash","read"]}`))
	want := []string{"bash", "read"}
	if !reflect.DeepEqual(cfg.BashCommandAllowlist, want) {
		t.Errorf("bash_command_allowlist = %v, want %v", cfg.BashCommandAllowlist, want)
	}
}

// TestCompileRulesKnobs proves the recognised rule keys map onto the remaining
// DrunConfig knobs and unknown keys are dropped.
func TestCompileRulesKnobs(t *testing.T) {
	cfg := mustCompile(t, mustParse(t, `{
		"network":"none",
		"rules":{
			"mount_allowlist":["/work"],
			"env_allowlist":["PATH"],
			"bash_command_denylist":["rm"],
			"bash_timeout_ms":5000,
			"max_workspace_mb":2048,
			"session_idle_timeout_secs":900,
			"unknown_key":"ignored"
		}
	}`))
	if !reflect.DeepEqual(cfg.MountAllowlist, []string{"/work"}) {
		t.Errorf("mount_allowlist = %v", cfg.MountAllowlist)
	}
	if !reflect.DeepEqual(cfg.EnvAllowlist, []string{"PATH"}) {
		t.Errorf("env_allowlist = %v", cfg.EnvAllowlist)
	}
	if !reflect.DeepEqual(cfg.BashCommandDenylist, []string{"rm"}) {
		t.Errorf("bash_command_denylist = %v", cfg.BashCommandDenylist)
	}
	if cfg.BashTimeoutMs == nil || *cfg.BashTimeoutMs != 5000 {
		t.Errorf("bash_timeout_ms = %v, want 5000", cfg.BashTimeoutMs)
	}
	if cfg.MaxWorkspaceMb == nil || *cfg.MaxWorkspaceMb != 2048 {
		t.Errorf("max_workspace_mb = %v, want 2048", cfg.MaxWorkspaceMb)
	}
	if cfg.SessionIdleTimeoutSecs == nil || *cfg.SessionIdleTimeoutSecs != 900 {
		t.Errorf("session_idle_timeout_secs = %v, want 900", cfg.SessionIdleTimeoutSecs)
	}
}

// TestCompileDeterministic proves identical boundaries compile to identical
// content hashes regardless of rules-object key order (the ADR-0003 §2
// requirement that content_hash is reproducible).
func TestCompileDeterministic(t *testing.T) {
	a := mustParse(t, `{"network":"allowlist","allowed_tools":["a","b"],"rules":{"bash_timeout_ms":1000,"mount_allowlist":["/x"],"network_allowlist":["h"]}}`)
	b := mustParse(t, `{"rules":{"network_allowlist":["h"],"mount_allowlist":["/x"],"bash_timeout_ms":1000},"allowed_tools":["a","b"],"network":"allowlist"}`)

	ha := hashOf(t, mustCompile(t, a))
	hb := hashOf(t, mustCompile(t, b))
	if ha != hb {
		t.Errorf("content hashes differ: %s vs %s", ha, hb)
	}
}

func hashOf(t *testing.T, cfg policy.DrunConfig) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h, err := policy.ComputeContentHash(raw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// TestCompiledBundleVerifies proves a compiled config yields a bundle whose
// content hash the pkg/policy verifier accepts after a JSON round-trip through
// policy.Load (the client's path).
func TestCompiledBundleVerifies(t *testing.T) {
	cfg := mustCompile(t, mustParse(t, `{"network":"allowlist","allowed_tools":["bash"],"rules":{"network_allowlist":["api.example.com"]}}`))
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	hash, err := policy.ComputeContentHash(raw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// A syntactically valid (64-byte) but non-verifying signature: Load only
	// checks the prefix and length, not the signature itself.
	placeholderSig := "ed25519:" + base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	b := policy.PolicyBundle{
		SchemaVersion: "1.0",
		OrgID:         "org-1",
		PolicyID:      "pol-1",
		PolicyVersion: 1,
		DrunConfig:    cfg,
		IssuedAt:      "2026-07-11T00:00:00Z",
		ExpiresAt:     "2026-07-11T01:00:00Z",
		ContentHash:   hash,
		Signature:     placeholderSig,
	}
	wire, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	loaded, err := policy.Load(wire)
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	if err := loaded.VerifyContentHash(); err != nil {
		t.Errorf("VerifyContentHash: %v", err)
	}
}

// TestParseBoundaryInvalid proves a syntactically invalid content_json is an
// error, and an empty blob is rejected.
func TestParseBoundaryInvalid(t *testing.T) {
	if _, err := ParseBoundary(json.RawMessage(`{not json`)); err == nil {
		t.Error("expected error for malformed JSON")
	}
	if _, err := ParseBoundary(nil); err == nil {
		t.Error("expected error for empty content_json")
	}
}

// TestParseBoundaryFanOut proves the fan-out ceiling is decoded and preserves
// the absent-vs-zero distinction.
func TestParseBoundaryFanOut(t *testing.T) {
	withCeiling := mustParse(t, `{"network":"none","fan_out_ceiling":4}`)
	if withCeiling.FanOutCeiling == nil || *withCeiling.FanOutCeiling != 4 {
		t.Errorf("fan_out_ceiling = %v, want 4", withCeiling.FanOutCeiling)
	}
	absent := mustParse(t, `{"network":"none"}`)
	if absent.FanOutCeiling != nil {
		t.Errorf("fan_out_ceiling = %v, want nil (absent)", absent.FanOutCeiling)
	}
}
