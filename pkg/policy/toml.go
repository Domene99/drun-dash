package policy

import (
	"fmt"
	"strconv"
	"strings"
)

// ToTOML compiles the drun_config into ~/.drun/config.toml bytes.
//
// Each present field becomes one top-level key in drun's canonical order (spec
// §2): string slices become TOML arrays of basic strings, integers become
// bare integers. Omitted fields (nil slice / nil pointer) are not emitted, so
// drun applies its own defaults; a present-but-empty slice is emitted as an
// empty array (e.g. bash_command_allowlist = [] to select denylist mode). The
// output is deterministic and uses only TOML that any spec-compliant parser
// (such as github.com/pelletier/go-toml/v2) accepts.
func (c *DrunConfig) ToTOML() []byte {
	var b strings.Builder
	writeTOMLStringArray(&b, "domain_allowlist", c.DomainAllowlist)
	writeTOMLStringArray(&b, "mount_allowlist", c.MountAllowlist)
	writeTOMLStringArray(&b, "mount_overlay_paths", c.MountOverlayPaths)
	writeTOMLStringArray(&b, "env_allowlist", c.EnvAllowlist)
	writeTOMLStringArray(&b, "bash_command_denylist", c.BashCommandDenylist)
	writeTOMLStringArray(&b, "bash_command_allowlist", c.BashCommandAllowlist)
	writeTOMLInt(&b, "bash_timeout_ms", c.BashTimeoutMs)
	writeTOMLInt(&b, "fetch_timeout_ms", c.FetchTimeoutMs)
	writeTOMLInt(&b, "connect_timeout_ms", c.ConnectTimeoutMs)
	writeTOMLInt(&b, "max_workspace_mb", c.MaxWorkspaceMb)
	writeTOMLInt(&b, "max_sessions", c.MaxSessions)
	writeTOMLInt(&b, "max_checkpoints", c.MaxCheckpoints)
	writeTOMLInt(&b, "session_idle_timeout_secs", c.SessionIdleTimeoutSecs)
	return []byte(b.String())
}

func writeTOMLStringArray(b *strings.Builder, key string, vals []string) {
	if vals == nil {
		return
	}
	b.WriteString(key)
	b.WriteString(" = [")
	for i, v := range vals {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlBasicString(v))
	}
	b.WriteString("]\n")
}

func writeTOMLInt(b *strings.Builder, key string, v *int64) {
	if v == nil {
		return
	}
	b.WriteString(key)
	b.WriteString(" = ")
	b.WriteString(strconv.FormatInt(*v, 10))
	b.WriteByte('\n')
}

// tomlBasicString renders s as a TOML basic string (double-quoted), escaping
// the characters TOML requires: backslash, double-quote, and control
// characters (using the short escapes where defined, \uXXXX otherwise).
func tomlBasicString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
