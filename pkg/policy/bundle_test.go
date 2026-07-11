package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixtureFiles are the shipped example bundles in testdata/policy. Keep in sync
// with the directory (guarded by TestAllFixturesCovered).
var fixtureFiles = []string{"strict.json", "standard.json", "permissive.json"}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "policy", name)
}

func loadFixture(t *testing.T, name string) *PolicyBundle {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	b, err := Load(data)
	if err != nil {
		t.Fatalf("load fixture %s: %v", name, err)
	}
	return b
}

// referenceNow is a timestamp inside every fixture's validity window (all
// fixtures are issued 2026-07-11 and expire between 2026-07-18 and 2026-10-11).
var referenceNow = time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)

func TestFixturesLoadAndHashVerifies(t *testing.T) {
	for _, name := range fixtureFiles {
		t.Run(name, func(t *testing.T) {
			b := loadFixture(t, name)
			if err := b.VerifyContentHash(); err != nil {
				t.Fatalf("content_hash did not verify: %v", err)
			}
			if err := b.CheckFreshness(referenceNow); err != nil {
				t.Fatalf("fixture should be fresh at %s: %v", referenceNow, err)
			}
			if b.OrgID != "org_acme" {
				t.Errorf("org_id = %q, want org_acme", b.OrgID)
			}
		})
	}
}

func TestAllFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "testdata", "policy"))
	if err != nil {
		t.Fatalf("read fixtures dir: %v", err)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			onDisk[e.Name()] = true
		}
	}
	if len(onDisk) != len(fixtureFiles) {
		t.Fatalf("fixtures on disk = %d, tracked = %d", len(onDisk), len(fixtureFiles))
	}
	for _, name := range fixtureFiles {
		if !onDisk[name] {
			t.Errorf("tracked fixture %s missing from disk", name)
		}
	}
}

// TestContentHashMatchesStructCanonicalization checks that hashing the received
// drun_config bytes and hashing the re-serialized DrunConfig struct agree — the
// property a later signing batch relies on to hash configs it builds itself.
func TestContentHashMatchesStructCanonicalization(t *testing.T) {
	for _, name := range fixtureFiles {
		t.Run(name, func(t *testing.T) {
			b := loadFixture(t, name)
			fromStruct, err := b.DrunConfig.CanonicalJSON()
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			got, err := ComputeContentHash(fromStruct)
			if err != nil {
				t.Fatalf("ComputeContentHash: %v", err)
			}
			if got != b.ContentHash {
				t.Errorf("struct-canonical hash = %s, want %s", got, b.ContentHash)
			}
		})
	}
}

func TestParsedFieldsStandard(t *testing.T) {
	b := loadFixture(t, "standard.json")
	if b.PolicyID != "pol_standard_dev" {
		t.Errorf("policy_id = %q", b.PolicyID)
	}
	if b.PolicyVersion != 7 {
		t.Errorf("policy_version = %d, want 7", b.PolicyVersion)
	}
	if len(b.DrunConfig.DomainAllowlist) != 7 {
		t.Errorf("domain_allowlist len = %d, want 7", len(b.DrunConfig.DomainAllowlist))
	}
	// present but empty selects denylist mode; must be non-nil.
	if b.DrunConfig.BashCommandAllowlist == nil {
		t.Error("bash_command_allowlist should be present (non-nil) and empty")
	}
	if len(b.DrunConfig.BashCommandAllowlist) != 0 {
		t.Errorf("bash_command_allowlist should be empty, got %v", b.DrunConfig.BashCommandAllowlist)
	}
	if b.DrunConfig.BashTimeoutMs == nil || *b.DrunConfig.BashTimeoutMs != 120000 {
		t.Errorf("bash_timeout_ms = %v, want 120000", b.DrunConfig.BashTimeoutMs)
	}
}

// TestToTOMLRoundTrip compiles each fixture's config to TOML, parses it back
// with a minimal TOML reader, and checks the reconstructed values equal the
// originals — proving the output is well-formed TOML for the config subset.
func TestToTOMLRoundTrip(t *testing.T) {
	for _, name := range fixtureFiles {
		t.Run(name, func(t *testing.T) {
			b := loadFixture(t, name)
			toml := b.DrunConfig.ToTOML()
			parsed := parseFlatTOML(t, toml)

			checkArray(t, parsed, "domain_allowlist", b.DrunConfig.DomainAllowlist)
			checkArray(t, parsed, "mount_allowlist", b.DrunConfig.MountAllowlist)
			checkArray(t, parsed, "mount_overlay_paths", b.DrunConfig.MountOverlayPaths)
			checkArray(t, parsed, "env_allowlist", b.DrunConfig.EnvAllowlist)
			checkArray(t, parsed, "bash_command_denylist", b.DrunConfig.BashCommandDenylist)
			checkArray(t, parsed, "bash_command_allowlist", b.DrunConfig.BashCommandAllowlist)
			checkInt(t, parsed, "bash_timeout_ms", b.DrunConfig.BashTimeoutMs)
			checkInt(t, parsed, "fetch_timeout_ms", b.DrunConfig.FetchTimeoutMs)
			checkInt(t, parsed, "connect_timeout_ms", b.DrunConfig.ConnectTimeoutMs)
			checkInt(t, parsed, "max_workspace_mb", b.DrunConfig.MaxWorkspaceMb)
			checkInt(t, parsed, "max_sessions", b.DrunConfig.MaxSessions)
			checkInt(t, parsed, "max_checkpoints", b.DrunConfig.MaxCheckpoints)
			checkInt(t, parsed, "session_idle_timeout_secs", b.DrunConfig.SessionIdleTimeoutSecs)
		})
	}
}

// TestToTOMLEmptyVsAbsent verifies a present-but-empty slice compiles to `= []`
// while an absent (nil) field is omitted entirely so drun applies its default.
func TestToTOMLEmptyVsAbsent(t *testing.T) {
	cfg := DrunConfig{
		BashCommandAllowlist: []string{}, // present, empty -> "[]"
		// BashCommandDenylist left nil -> omitted
	}
	out := string(cfg.ToTOML())
	if !strings.Contains(out, "bash_command_allowlist = []") {
		t.Errorf("expected empty array line, got:\n%s", out)
	}
	if strings.Contains(out, "bash_command_denylist") {
		t.Errorf("absent field should be omitted, got:\n%s", out)
	}
}

func TestRejectUnknownTopLevelField(t *testing.T) {
	data := mustReadFixture(t, "standard.json")
	tampered := injectTopLevel(t, data, "rogue_field", `"x"`)
	if _, err := Load(tampered); err == nil {
		t.Fatal("expected Load to reject unknown top-level field")
	}
}

func TestRejectUnknownDrunConfigField(t *testing.T) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustReadFixture(t, "standard.json"), &m); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(m["drun_config"], &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["rogue"] = json.RawMessage(`true`)
	m["drun_config"], _ = json.Marshal(cfg)
	data, _ := json.Marshal(m)
	if _, err := Load(data); err == nil {
		t.Fatal("expected Load to reject unknown drun_config field")
	}
}

func TestRejectMissingField(t *testing.T) {
	for _, field := range []string{
		"schema_version", "org_id", "policy_id", "policy_version",
		"drun_config", "issued_at", "expires_at", "content_hash", "signature",
	} {
		t.Run(field, func(t *testing.T) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(mustReadFixture(t, "standard.json"), &m); err != nil {
				t.Fatal(err)
			}
			delete(m, field)
			data, _ := json.Marshal(m)
			_, err := Load(data)
			if !errors.Is(err, ErrMissingField) {
				t.Fatalf("err = %v, want ErrMissingField", err)
			}
		})
	}
}

func TestRejectExpired(t *testing.T) {
	b := loadFixture(t, "strict.json") // expires 2026-07-18
	afterExpiry := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := b.CheckFreshness(afterExpiry); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestRejectNotYetValid(t *testing.T) {
	b := loadFixture(t, "strict.json") // issued 2026-07-11
	beforeIssue := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := b.CheckFreshness(beforeIssue); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("err = %v, want ErrNotYetValid", err)
	}
}

func TestFreshnessClockSkew(t *testing.T) {
	b := loadFixture(t, "strict.json") // issued 2026-07-11T00:00:00Z
	// 30s before issued_at is inside the 60s default skew allowance.
	justBefore := time.Date(2026, 7, 10, 23, 59, 30, 0, time.UTC)
	if err := b.CheckFreshness(justBefore); err != nil {
		t.Fatalf("within skew should pass, got %v", err)
	}
}

func TestRejectTamperedConfig(t *testing.T) {
	// Mutating drun_config without recomputing content_hash must be detected.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustReadFixture(t, "standard.json"), &m); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(m["drun_config"], &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["max_sessions"] = json.RawMessage(`9999`) // was 10
	m["drun_config"], _ = json.Marshal(cfg)
	data, _ := json.Marshal(m)

	b, err := Load(data) // structurally fine
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := b.VerifyContentHash(); !errors.Is(err, ErrContentHashMismatch) {
		t.Fatalf("err = %v, want ErrContentHashMismatch", err)
	}
}

func TestRejectBadSchemaMajor(t *testing.T) {
	data := setTopLevelString(t, mustReadFixture(t, "standard.json"), "schema_version", "2.0")
	if _, err := Load(data); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("err = %v, want ErrUnsupportedSchema", err)
	}
}

func TestRejectExpiresBeforeIssued(t *testing.T) {
	data := setTopLevelString(t, mustReadFixture(t, "standard.json"), "expires_at", "2026-07-10T00:00:00Z")
	if _, err := Load(data); !errors.Is(err, ErrExpiryBeforeIssue) {
		t.Fatalf("err = %v, want ErrExpiryBeforeIssue", err)
	}
}

func TestRejectBadTimestamp(t *testing.T) {
	data := setTopLevelString(t, mustReadFixture(t, "standard.json"), "issued_at", "11 July 2026")
	if _, err := Load(data); !errors.Is(err, ErrInvalidTimestamp) {
		t.Fatalf("err = %v, want ErrInvalidTimestamp", err)
	}
}

func TestSigningInputFormat(t *testing.T) {
	b := loadFixture(t, "strict.json")
	want := strings.Join([]string{
		"drun-policy-bundle/v1",
		"1.0",
		"org_acme",
		"pol_strict_ci",
		"3",
		"2026-07-11T00:00:00Z",
		"2026-07-18T00:00:00Z",
		b.ContentHash,
	}, "\n")
	if got := string(b.SigningInput()); got != want {
		t.Errorf("signing input mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestSignatureVerifyRoundTrip(t *testing.T) {
	// Signing keys arrive in a later batch, so the fixtures carry a
	// non-verifying placeholder. Exercise verify against a locally generated
	// keypair by signing a fixture's signing input.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := loadFixture(t, "standard.json")
	sig := ed25519.Sign(priv, b.SigningInput())
	b.Signature = "ed25519:" + base64.StdEncoding.EncodeToString(sig)

	if err := b.VerifySignature(pub); err != nil {
		t.Fatalf("valid signature should verify: %v", err)
	}

	// Wrong key must fail.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := b.VerifySignature(otherPub); !errors.Is(err, ErrSignatureVerify) {
		t.Fatalf("wrong key err = %v, want ErrSignatureVerify", err)
	}

	// Tampering with a signed field breaks verification.
	b.PolicyVersion = 999
	if err := b.VerifySignature(pub); !errors.Is(err, ErrSignatureVerify) {
		t.Fatalf("tampered field err = %v, want ErrSignatureVerify", err)
	}
}

func TestPlaceholderSignatureDoesNotVerify(t *testing.T) {
	// The shipped placeholder is well-formed base64 of 64 zero bytes; it must
	// not verify against any real key.
	pub, _, _ := ed25519.GenerateKey(nil)
	b := loadFixture(t, "strict.json")
	if err := b.VerifySignature(pub); !errors.Is(err, ErrSignatureVerify) {
		t.Fatalf("placeholder err = %v, want ErrSignatureVerify", err)
	}
}

func TestVerifyBadPublicKey(t *testing.T) {
	b := loadFixture(t, "strict.json")
	if err := b.VerifySignature([]byte{1, 2, 3}); !errors.Is(err, ErrBadPublicKey) {
		t.Fatalf("err = %v, want ErrBadPublicKey", err)
	}
}

func TestVerifyPipeline(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	b := loadFixture(t, "standard.json")
	b.Signature = "ed25519:" + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, b.SigningInput()))

	if err := b.Verify(referenceNow, pub); err != nil {
		t.Fatalf("full verify should pass: %v", err)
	}
	// Expired -> pipeline fails at the freshness step.
	if err := b.Verify(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), pub); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired err = %v, want ErrExpired", err)
	}
}

// --- helpers -------------------------------------------------------------

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func injectTopLevel(t *testing.T, data []byte, key, rawValue string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m[key] = json.RawMessage(rawValue)
	out, _ := json.Marshal(m)
	return out
}

func setTopLevelString(t *testing.T, data []byte, key, value string) []byte {
	t.Helper()
	quoted, _ := json.Marshal(value)
	return injectTopLevel(t, data, key, string(quoted))
}

// parseFlatTOML is a minimal reader for the flat `key = value` grammar ToTOML
// emits: values are either a bare integer or a single-line array of basic
// strings. It is deliberately small — just enough to prove the output parses.
func parseFlatTOML(t *testing.T, data []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			t.Fatalf("malformed TOML line (no '='): %q", line)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		switch {
		case strings.HasPrefix(val, "["):
			if !strings.HasSuffix(val, "]") {
				t.Fatalf("malformed TOML array: %q", line)
			}
			inner := strings.TrimSpace(val[1 : len(val)-1])
			arr := []string{}
			if inner != "" {
				for _, part := range strings.Split(inner, ",") {
					s, err := strconv.Unquote(strings.TrimSpace(part))
					if err != nil {
						t.Fatalf("unquote %q: %v", part, err)
					}
					arr = append(arr, s)
				}
			}
			out[key] = arr
		default:
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				t.Fatalf("parse int %q: %v", val, err)
			}
			out[key] = n
		}
	}
	return out
}

func checkArray(t *testing.T, parsed map[string]any, key string, want []string) {
	t.Helper()
	v, ok := parsed[key]
	if want == nil {
		if ok {
			t.Errorf("%s should be omitted (nil), but TOML has it", key)
		}
		return
	}
	if !ok {
		t.Errorf("%s missing from TOML", key)
		return
	}
	got := v.([]string)
	if len(got) != len(want) {
		t.Errorf("%s len = %d, want %d", key, len(got), len(want))
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", key, i, got[i], want[i])
		}
	}
}

func checkInt(t *testing.T, parsed map[string]any, key string, want *int64) {
	t.Helper()
	v, ok := parsed[key]
	if want == nil {
		if ok {
			t.Errorf("%s should be omitted, but TOML has it", key)
		}
		return
	}
	if !ok {
		t.Errorf("%s missing from TOML", key)
		return
	}
	if got := v.(int64); got != *want {
		t.Errorf("%s = %d, want %d", key, got, *want)
	}
}
