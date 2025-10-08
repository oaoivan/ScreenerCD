#!/usr/bin/env bash
# Stop local screener-core and optionally docker stack

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PID_DIR="$ROOT_DIR/build"
PID_FILE="$PID_DIR/screener-core.pid"
WINPID_FILE="$PID_DIR/screener-core.winpid"
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
  local pid=""
  local winpid=""
  local found=0
  if [[ -f "$PID_FILE" ]]; then
    pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    found=1
  fi
  if [[ -f "$WINPID_FILE" ]]; then
    winpid="$(cat "$WINPID_FILE" 2>/dev/null || true)"
    found=1
  fi

  if [[ "$found" == "0" ]]; then
    info "PID file not found, nothing to stop (local)."
    return
  fi

  local stopped=0
  if [[ -n "$pid" ]] && ps -p "$pid" >/dev/null 2>&1; then
    info "Stopping screener-core (pid=$pid)"
    kill "$pid" 2>/dev/null || true
    sleep 1
    if ps -p "$pid" >/dev/null 2>&1; then
      warn "SIGTERM failed, sending SIGKILL"
      kill -9 "$pid" 2>/dev/null || true
      sleep 1
    fi
    stopped=1
  fi

  if [[ -n "$winpid" ]]; then
    local cmd="${COMSPEC:-cmd.exe}"
    if [[ -x "$cmd" ]]; then
      local task_output
      task_output="$(MSYS2_ARG_CONV_EXCL='*' "$cmd" /c "tasklist /FI \"PID eq $winpid\"" 2>/dev/null || true)"
      if [[ -n "$task_output" && "$task_output" != *"No tasks are running"* && "$task_output" == *" $winpid "* ]]; then
        warn "Using taskkill for Windows PID=$winpid"
        MSYS2_ARG_CONV_EXCL='*' "$cmd" /c "taskkill /PID $winpid /T /F" >/dev/null 2>&1 || true
        stopped=1
      fi
    fi
  fi

  if [[ "$stopped" == "1" ]]; then
    info "screener-core stopped."
  else
    info "No running screener-core found."
  fi

  rm -f "$PID_FILE" "$WINPID_FILE"
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
