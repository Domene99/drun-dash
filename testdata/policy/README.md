# Policy bundle test fixtures

Three complete, valid [drun policy bundles](../../docs/policy/DRUN_POLICY_BUNDLE.md).
They exist so loaders, validators, and the config compiler have realistic input
to test against. All three:

- are valid JSON,
- share `org_id` `org_acme` and `issued_at` `2026-07-11T00:00:00Z`,
- carry a real `content_hash` (SHA-256 of the canonical `drun_config`), and
- carry a **placeholder** `signature` — signing is not implemented in this
  batch, so the signature does not verify. Do not use these to test signature
  verification success; they are for field-parsing and config-compilation
  tests.

| Fixture            | Intended use                          | Command policy   | `expires_at`  |
|--------------------|---------------------------------------|------------------|---------------|
| `strict.json`      | Locked-down CI runners                 | whitelist mode   | 2026-07-18    |
| `standard.json`    | Typical developer defaults             | denylist mode    | 2026-08-11    |
| `permissive.json`  | Trusted internal teams, wider access   | denylist mode    | 2026-10-11    |

## How a fixture maps to `config.toml`

The `drun_config` object compiles field-for-field into drun's
`~/.drun/config.toml`. Arrays become TOML arrays; integers stay integers. For
example, `standard.json`'s `drun_config` compiles to:

```toml
domain_allowlist = ["registry.npmjs.org", "pypi.org", "files.pythonhosted.org",
                    "github.com", "raw.githubusercontent.com",
                    "proxy.golang.org", "sum.golang.org"]
mount_allowlist = ["/home/dev/project", "/home/dev/.cache"]
mount_overlay_paths = ["node_modules", ".venv"]
env_allowlist = ["PATH", "HOME", "LANG", "LC_ALL", "TZ", "TERM"]
bash_command_denylist = ["ssh", "scp", "nc", "sudo"]
bash_command_allowlist = []      # empty → denylist mode (all but denied)
bash_timeout_ms = 120000
fetch_timeout_ms = 30000
connect_timeout_ms = 5000
max_workspace_mb = 2048
max_sessions = 10
max_checkpoints = 200
session_idle_timeout_secs = 3600
```

The other two fixtures follow the same mapping with tighter (`strict`) or wider
(`permissive`) values.

## How the tiers differ

| `drun_config` field         | `strict`                    | `standard`                  | `permissive`                    |
|-----------------------------|-----------------------------|-----------------------------|---------------------------------|
| `domain_allowlist`          | 2 hosts, no wildcards       | 7 common dev hosts          | 10 entries incl. wildcards      |
| `mount_allowlist`           | project dir only            | project + cache             | home + shared + datasets        |
| `mount_overlay_paths`       | none                        | `node_modules`, `.venv`     | + `vendor`, `target`, `.gradle` |
| `env_allowlist`             | `PATH`,`HOME`,`LANG`        | + `LC_ALL`,`TZ`,`TERM`      | + cloud/registry vars           |
| `bash_command_allowlist`    | 12-command whitelist        | empty (denylist mode)       | empty (denylist mode)           |
| `bash_command_denylist`     | net + `sudo`                | remote-shell + `sudo`       | `sudo` only                     |
| `bash_timeout_ms`           | 30 000                      | 120 000                     | 600 000                         |
| `fetch_timeout_ms`          | 10 000                      | 30 000                      | 60 000                          |
| `connect_timeout_ms`        | 3 000                       | 5 000                       | 10 000                          |
| `max_workspace_mb`          | 512                         | 2 048                       | 16 384                          |
| `max_sessions`              | 3                           | 10                          | 50                              |
| `max_checkpoints`           | 50                          | 200                         | 1 000                           |
| `session_idle_timeout_secs` | 900                         | 3 600                       | 14 400                          |

## Recomputing / verifying `content_hash`

`content_hash` is `sha256:` + the SHA-256 of the canonical `drun_config` JSON
(sorted keys, no whitespace, UTF-8, array order preserved — see the
[format spec §3](../../docs/policy/DRUN_POLICY_BUNDLE.md#3-content_hash)). To
verify a fixture is internally consistent:

```bash
python3 - <<'PY'
import hashlib, json, glob
for path in sorted(glob.glob("testdata/policy/*.json")):
    b = json.load(open(path))
    canon = json.dumps(b["drun_config"], sort_keys=True,
                       separators=(",", ":"), ensure_ascii=False).encode()
    want = "sha256:" + hashlib.sha256(canon).hexdigest()
    print(path, "OK" if want == b["content_hash"] else "MISMATCH")
PY
```
