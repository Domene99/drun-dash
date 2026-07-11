# internal/cli

Command tree and dispatch for the `drun-dash` developer CLI. The
`cmd/drun-dash` binary is a one-liner that calls [`Run`](cli.go); all logic
lives here so tests exercise every subcommand in-process with buffers.

## Design

- **stdlib only** — dispatch is a `switch` on the first argument with a
  per-command `flag.FlagSet`, mirroring `cmd/policyctl`. No cobra/urfave or any
  CLI framework, and no new dependencies.
- **`Run(args, stdout, stderr) int`** — returns a process exit code and never
  calls `os.Exit`, so `main` stays trivial and tests need no subprocess.
- **Version** — `Version` defaults to `"dev"` and is overridable at build time:
  `-ldflags "-X github.com/Domene99/drun-dash/internal/cli.Version=v1.2.3"`.

## Commands

| Command             | Status | Exit | Notes |
|---------------------|--------|------|-------|
| `login`             | stub   | 1    | Describes the planned OAuth 2.0 device-authorization flow. Real auth + token storage land in **batch 5**. |
| `whoami`            | stub   | 1    | Prints "not logged in"; depends on `login` storing a token (**batch 5**). |
| `config` (`env`)    | real   | 0    | Loads `config.Load()` and prints the resolved runtime configuration. |
| `version` / `--version` | real | 0 | Prints `Version`. |
| `help` / `-h` / `--help` | real | 0 | Prints usage listing every subcommand. |
| no args / unknown   | —      | 2    | Prints usage on stderr. |

## Not implemented yet

Real authentication (device-authorization flow), token storage / keychain
access, policy sync, and drun session control are deferred to later batches and
marked with `// TODO(batch/…)` comments in the stub files.
