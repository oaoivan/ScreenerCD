#!/usr/bin/env bash
# Stop local screener-core and optionally docker stack

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PID_DIR="$ROOT_DIR/build"
LOG_FILE="$ROOT_DIR/screner.log"

STOP_DOCKER=0

ts() { date +"%Y-%m-%dT%H:%M:%S%z"; }
info() { echo "$(ts) [INFO] $*"; }
warn() { echo "$(ts) [WARN] $*" >&2; }
err()  { echo "$(ts) [ERROR] $*" >&2; }

usage() {
  cat <<USAGE
Usage: $(basename "$0") [options]
  --docker-all   Stop docker compose services (redis + screener-core)
  -h, --help     Show help
USAGE
}

for arg in "$@"; do
  case "$arg" in
    --docker-all) STOP_DOCKER=1 ;;
    -h|--help)    usage; exit 0 ;;
    *) warn "Unknown option: $arg" ;;
  esac
done

stop_local() {
  local pid_file="$PID_DIR/screener-core.pid"
  if [[ -f "$pid_file" ]]; then
    local pid
    pid="$(cat "$pid_file" 2>/dev/null || true)"
    if [[ -n "$pid" ]] && ps -p "$pid" >/dev/null 2>&1; then
      info "Stopping screener-core PID=$pid"
      kill "$pid" || true
      sleep 1
      if ps -p "$pid" >/dev/null 2>&1; then
        warn "SIGTERM failed, sending SIGKILL"
        kill -9 "$pid" || true
      fi
      info "screener-core stopped."
    else
      info "No running screener-core found."
    fi
    rm -f "$pid_file"
  else
    info "PID file not found, nothing to stop (local)."
  fi
}

stop_docker() {
  if ! command -v docker >/dev/null 2>&1; then
    err "docker not found"
    exit 1
  fi
  if ! docker compose version >/dev/null 2>&1; then
    err "docker compose plugin not found"
    exit 1
  fi
  info "Stopping docker services: screener-core, redis"
  (cd "$ROOT_DIR" && docker compose stop screener-core redis || true)
}

main() {
  stop_local
  if [[ "$STOP_DOCKER" == "1" ]]; then
    stop_docker
  fi
  info "Done. Logs: $LOG_FILE"
}

main "$@"
