#!/usr/bin/env bash
#
# test.sh — manual unit/integration test runner for the request tracking,
# rate limiting, audit logging and safe body reading work.
#
# Usage:
#   ./test.sh                 Run the full Go unit test suite (with -race).
#   ./test.sh --no-race       Run without the race detector (faster).
#   ./test.sh --pkg PKG...    Test only the given package path(s), e.g.
#                             ./test.sh --pkg ./pkg/api
#   ./test.sh --build         Also build the hetty binary (requires the admin
#                             frontend to be present under cmd/hetty/admin).
#   ./test.sh -h | --help     Show this help.
#
# This script is intended to be run MANUALLY by a developer. It requires
# network access the first time so `go mod download` can fetch dependencies.
set -eo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

RACE=1
DO_BUILD=0
PKGS=()

# --- colors ----------------------------------------------------------------
if [[ -t 1 ]]; then
  RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[0;33m'; BOLD=$'\033[1m'; OFF=$'\033[0m'
else
  RED=""; GREEN=""; YELLOW=""; BOLD=""; OFF=""
fi

info() { printf '%s==>%s %s\n' "$BOLD" "$OFF" "$*"; }
ok()   { printf '%s ok %s %s\n' "$GREEN" "$OFF" "$*"; }
warn() { printf '%s warn%s %s\n' "$YELLOW" "$OFF" "$*"; }
die()  { printf '%sFAIL%s %s\n' "$RED" "$OFF" "$*" >&2; exit 1; }

usage() { sed -n '2,21p' "${BASH_SOURCE[0]}"; }

# --- args ------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --no-race) RACE=0; shift ;;
    --build)   DO_BUILD=1; shift ;;
    --pkg)
      shift
      [[ $# -gt 0 ]] || die "--pkg requires at least one package path"
      while [[ $# -gt 0 && "$1" != --* ]]; do PKGS+=("$1"); shift; done
      ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

command -v go >/dev/null 2>&1 || die "go toolchain not found in PATH"

# --- dependencies ----------------------------------------------------------
info "Ensuring modules are downloaded (needs network on first run)..."
if ! go mod download; then
  warn "go mod download failed; if you are offline, pre-populate the module cache."
fi

RACE_FLAG=()
if [[ "$RACE" -eq 1 ]]; then
  RACE_FLAG=(-race)
fi

# --- 1. focused tests for the new protection packages ----------------------
info "Running focused tests: reqid, ratelimit, httpio..."
go test "${RACE_FLAG[@]}" -count=1 \
  ./pkg/reqid/... ./pkg/ratelimit/... ./pkg/httpio/...
ok "request id, rate limiter and safe body packages"

if [[ ${#PKGS[@]} -eq 0 ]]; then
  # --- 2. GraphQL protection layer ----------------------------------------
  info "Running GraphQL API protection tests (request id, complexity, rate limit, body cap, audit)..."
  go test "${RACE_FLAG[@]}" -count=1 -v ./pkg/api/... \
    -run 'RequestID|Complexity|RateLimit|MaxQueryBytes|Audit|Playground'
  ok "GraphQL API protection tests"

  # --- 3. proxy request id propagation -------------------------------------
  info "Running proxy request id propagation tests..."
  go test "${RACE_FLAG[@]}" -count=1 -v ./pkg/proxy/... -run 'RequestID|ServeHTTP'
  ok "proxy request id propagation"

  # --- 4. full suite -------------------------------------------------------
  info "Running the complete unit test suite..."
  go test "${RACE_FLAG[@]}" -count=1 ./...
  ok "full unit test suite"
else
  info "Running requested package(s) only: ${PKGS[*]}"
  go test "${RACE_FLAG[@]}" -count=1 "${PKGS[@]}"
  ok "requested packages"
fi

# --- 5. vet ----------------------------------------------------------------
if [[ ${#PKGS[@]} -eq 0 ]]; then
  info "Running go vet..."
  go vet ./...
  ok "go vet"
else
  info "Running go vet on requested package(s)..."
  go vet "${PKGS[@]}"
  ok "go vet (requested packages)"
fi

# --- 6. optional build -----------------------------------------------------
if [[ "$DO_BUILD" -eq 1 ]]; then
  info "Building hetty binary..."
  go build -o /tmp/hetty-test ./cmd/hetty
  ok "built /tmp/hetty-test"
fi

printf '\n%sAll manual test stages passed.%s\n' "$GREEN$BOLD" "$OFF"
