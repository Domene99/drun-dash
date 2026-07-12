#!/usr/bin/env bash
#
# member-console.sh — a convenience guide for bringing up the drun-dash member
# console against the org stack, a seeded demo org, and a local drun engine.
#
# It DOES NOT reimplement any deployment logic: it checks prerequisites, probes
# the running stack, and prints the exact ordered commands to run — every one of
# which delegates to the existing compose file (deploy/docker/docker-compose.app.yml)
# and Makefile targets. Nothing here duplicates compose or migration logic.
#
# It is safe to run repeatedly (it only reads and prints) and degrades
# gracefully: a missing prerequisite (Docker, a drun engine, OIDC config) is
# reported with clear guidance instead of an obscure failure.
#
# See docs/MEMBER_CONSOLE.md for the walkthrough and docs/RUNBOOK.md for the
# full stack reference.

set -euo pipefail

# ---------------------------------------------------------------------------
# Locate the repository root so the script works from any working directory.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)"

# ---------------------------------------------------------------------------
# Configuration (all overridable from the environment; matches the defaults in
# cmd/member-console, docs/RUNBOOK.md, and pkg/drunmcp).
# ---------------------------------------------------------------------------
COMPOSE_FILE="deploy/docker/docker-compose.app.yml"
ENV_FILE="deploy/docker/.env.app"
ENV_EXAMPLE="deploy/docker/.env.app.example"
SEED_FILE="deploy/seed/seed.sql"

CONTROL_PLANE_URL="${CONTROL_PLANE_BASE_URL:-http://localhost:8080}"
PROXY_URL="${PROXY_URL:-http://localhost:8081}"
ADMIN_UI_URL="${ADMIN_UI_URL:-http://localhost:8082}"
MEMBER_CONSOLE_ADDR="${MEMBER_CONSOLE_HTTP_ADDR:-:8083}"
# Turn a ":8083" style listen address into a clickable localhost URL.
CONSOLE_URL="http://localhost:${MEMBER_CONSOLE_ADDR##*:}"
DRUN_MCP_ENDPOINT="${DRUN_MCP_ENDPOINT:-http://127.0.0.1:7273/mcp}"

# ---------------------------------------------------------------------------
# Pretty output helpers (colour only when writing to a terminal).
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
  BOLD="$(printf '\033[1m')"; DIM="$(printf '\033[2m')"; RESET="$(printf '\033[0m')"
  GREEN="$(printf '\033[32m')"; YELLOW="$(printf '\033[33m')"; RED="$(printf '\033[31m')"; BLUE="$(printf '\033[34m')"
else
  BOLD=""; DIM=""; RESET=""; GREEN=""; YELLOW=""; RED=""; BLUE=""
fi

section() { printf '\n%s== %s ==%s\n' "${BOLD}${BLUE}" "$1" "${RESET}"; }
ok()      { printf '  %s✓%s %s\n' "${GREEN}" "${RESET}" "$1"; }
warn()    { printf '  %s!%s %s\n' "${YELLOW}" "${RESET}" "$1"; }
bad()     { printf '  %s✗%s %s\n' "${RED}" "${RESET}" "$1"; }
step()    { printf '  %s$%s %s\n' "${DIM}" "${RESET}" "$1"; }
note()    { printf '    %s%s%s\n' "${DIM}" "$1" "${RESET}"; }

usage() {
  cat <<EOF
${BOLD}member-console.sh${RESET} — prereq check + ordered bring-up guide for the drun-dash member console.

Usage: scripts/demo/member-console.sh [-h|--help]

It only reads and prints; it never mutates your stack, so it is safe to run
repeatedly. Copy-paste the commands it prints (each delegates to the existing
compose file / make targets — no logic is duplicated here).

Overridable env vars: CONTROL_PLANE_BASE_URL, PROXY_URL, ADMIN_UI_URL,
MEMBER_CONSOLE_HTTP_ADDR, DRUN_MCP_ENDPOINT.

Docs: docs/MEMBER_CONSOLE.md (walkthrough), docs/RUNBOOK.md (full stack).
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  "") ;;
  *) printf 'Unknown argument: %s\n\n' "$1" >&2; usage >&2; exit 2 ;;
esac

cd "${REPO_ROOT}"

# have <cmd> — true if the command exists on PATH.
have() { command -v "$1" >/dev/null 2>&1; }

# http_ok <url> — true if an HTTP GET returns any response (2xx/3xx) within 2s.
http_ok() { have curl && curl -fsS --max-time 2 -o /dev/null "$1" 2>/dev/null; }

# reachable <url> — true if something answers at all (even a 4xx), used for the
# drun engine, which speaks MCP/JSON-RPC and has no plain GET health route.
reachable() { have curl && curl -sS --max-time 2 -o /dev/null "$1" 2>/dev/null; }

MISSING_HARD=0   # blocks bring-up (Docker, curl)
STACK_UP=0       # control-plane reachable

# ---------------------------------------------------------------------------
# 1. Tooling prerequisites
# ---------------------------------------------------------------------------
section "Tooling"

if have docker; then
  ok "docker found"
  if docker compose version >/dev/null 2>&1; then
    ok "docker compose (v2) plugin found"
  else
    bad "docker compose v2 plugin not found (need 'docker compose', not the legacy 'docker-compose')"
    MISSING_HARD=1
  fi
else
  bad "docker not found — install Docker Engine with the Compose v2 plugin (see docs/RUNBOOK.md §1)"
  MISSING_HARD=1
fi

have curl && ok "curl found" || { bad "curl not found — needed for health probes"; MISSING_HARD=1; }
have go   && ok "go toolchain found (to run the console from source)" \
          || warn "go toolchain not found — needed to 'go run ./cmd/member-console' or 'make build'"
have migrate && ok "migrate found on PATH (optional — the compose 'migrate' service applies migrations for you)" \
             || note "migrate not on PATH — that's fine: the compose 'migrate' service runs migrations inside the stack"

# ---------------------------------------------------------------------------
# 2. Repository artifacts
# ---------------------------------------------------------------------------
section "Repository artifacts"

[ -f "${COMPOSE_FILE}" ] && ok "app compose file present (${COMPOSE_FILE})" \
                         || bad "missing ${COMPOSE_FILE} — are you in the drun-dash repo?"

# ENV_FILE_FLAG is the optional "--env-file <path>" argument, kept as a scalar
# string (not an array) so it stays safe under `set -u` on bash 3.2 (macOS).
ENV_FILE_FLAG=""
if [ -f "${ENV_FILE}" ]; then
  ok "env file present (${ENV_FILE})"
  ENV_FILE_FLAG="--env-file ${ENV_FILE}"
else
  warn "no ${ENV_FILE} yet — compose has inline dev defaults, so it still runs, but create one for a real setup:"
  note "cp ${ENV_EXAMPLE} ${ENV_FILE}"
fi

[ -f "${SEED_FILE}" ] && ok "seed data present (${SEED_FILE})" \
                      || warn "missing ${SEED_FILE} — the demo org will not exist until it is added; step 2 below will no-op"

# The compose invocation every printed command reuses (delegates to the existing
# compose file; reimplements nothing). Built as a copy-pasteable string.
COMPOSE_STR="docker compose"
[ -n "${ENV_FILE_FLAG}" ] && COMPOSE_STR="${COMPOSE_STR} ${ENV_FILE_FLAG}"
COMPOSE_STR="${COMPOSE_STR} -f ${COMPOSE_FILE}"

# ---------------------------------------------------------------------------
# 3. Is the org stack already up?
# ---------------------------------------------------------------------------
section "Org stack health"

if http_ok "${CONTROL_PLANE_URL}/healthz"; then
  ok "control-plane live (${CONTROL_PLANE_URL}/healthz)"
  STACK_UP=1
  http_ok "${CONTROL_PLANE_URL}/readyz" && ok "control-plane ready (DB reachable)" \
                                        || warn "control-plane not ready (${CONTROL_PLANE_URL}/readyz) — DB not migrated/reachable yet"
else
  warn "control-plane not reachable at ${CONTROL_PLANE_URL} — the stack is probably down"
fi

http_ok "${PROXY_URL}/healthz"    && ok "llm-proxy live (${PROXY_URL})"  || warn "llm-proxy not reachable at ${PROXY_URL}"
http_ok "${ADMIN_UI_URL}/healthz" && ok "admin-ui live (${ADMIN_UI_URL})" || warn "admin-ui not reachable at ${ADMIN_UI_URL} — needed to approve the device login"

# ---------------------------------------------------------------------------
# 4. Local drun engine (needed for the governed run; not for the login shell)
# ---------------------------------------------------------------------------
section "Local drun engine"

if reachable "${DRUN_MCP_ENDPOINT}"; then
  ok "something is answering at ${DRUN_MCP_ENDPOINT} (drun engine likely up)"
else
  warn "no drun engine reachable at ${DRUN_MCP_ENDPOINT}"
  note "The conversational run executes tools in a LOCAL drun sandbox over this endpoint."
  note "Start your local drun engine so ${DRUN_MCP_ENDPOINT} is reachable, or override DRUN_MCP_ENDPOINT."
  note "Note: on 'main' today the console is still the login/launch shell and does not dial the engine yet"
  note "(see docs/MEMBER_CONSOLE.md §6), so this is not fatal for bringing the console up."
fi

# ---------------------------------------------------------------------------
# 5. OIDC (device-login approval happens on the admin UI, behind OIDC)
# ---------------------------------------------------------------------------
section "OIDC / login"

note "Device-flow login is approved on the admin UI's device-approval page, which sits behind OIDC."
note "The control plane must have CP_OIDC_ISSUER and CP_OIDC_CLIENT_ID set (docs/RUNBOOK.md §8);"
note "with them unset the control plane serves only health probes and login cannot complete."
note "This script cannot verify the IdP for you — confirm those vars in ${ENV_FILE} before signing in."

# ---------------------------------------------------------------------------
# 6. Ordered bring-up steps
# ---------------------------------------------------------------------------
section "Ordered bring-up"

if [ "${MISSING_HARD}" -ne 0 ]; then
  bad "Resolve the missing tooling above before continuing."
fi

if [ "${STACK_UP}" -eq 1 ]; then
  ok "The org stack is already up — skip step 1."
else
  printf '%s1.%s Build and start the org stack (Postgres + Redis + migrate + control-plane + llm-proxy + admin-ui):\n' "${BOLD}" "${RESET}"
  [ -f "${ENV_FILE}" ] || step "cp ${ENV_EXAMPLE} ${ENV_FILE}      # first run only"
  step "${COMPOSE_STR} up --build -d"
  step "${COMPOSE_STR} ps                 # watch health"
fi

printf '%s2.%s Seed the demo org (idempotent — safe to re-run):\n' "${BOLD}" "${RESET}"
if [ -f "${SEED_FILE}" ]; then
  step "${COMPOSE_STR} exec -T postgres psql -U drundash -d drundash < ${SEED_FILE}"
else
  note "(${SEED_FILE} not present yet — nothing to seed)"
fi

printf '%s3.%s (For the governed run, once it lands) start your local drun engine so\n' "${BOLD}" "${RESET}"
printf '   %s is reachable.\n' "${DRUN_MCP_ENDPOINT}"

printf '%s4.%s Start the member console (a LOCAL binary — not part of the docker stack):\n' "${BOLD}" "${RESET}"
step "go run ./cmd/member-console          # or: make build && ./bin/member-console"
note "Listens on ${MEMBER_CONSOLE_ADDR} (override with MEMBER_CONSOLE_HTTP_ADDR)."

printf '%s5.%s Open the console and sign in:\n' "${BOLD}" "${RESET}"
printf '   %sConsole:%s   %s\n' "${BOLD}" "${RESET}" "${CONSOLE_URL}"
printf '   %sApprove at:%s %s  (device-flow approval page)\n' "${BOLD}" "${RESET}" "${ADMIN_UI_URL}"
note "Then: pick an entitled agent → launch a governed session. The conversational"
note "run view arrives in a later batch (docs/MEMBER_CONSOLE.md §4)."

section "Done"
note "Full walkthrough: docs/MEMBER_CONSOLE.md   Full stack reference: docs/RUNBOOK.md"
