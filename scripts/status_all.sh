#!/usr/bin/env bash
# Status helper: shows docker compose services, Redis availability, local process and last logs

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PID_DIR="$ROOT_DIR/build"
LOG_FILE="$ROOT_DIR/screner.log"

REDIS_HOST="${REDIS_HOST:-localhost}"
REDIS_PORT="${REDIS_PORT:-6379}"

ts() { date +"%Y-%m-%dT%H:%M:%S%z"; }
info() { echo "$(ts) [INFO] $*"; }
warn() { echo "$(ts) [WARN] $*"; }

section() { echo "\n=== $* ==="; }

docker_status() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    section "Docker Compose Services"
    (cd "$ROOT_DIR" && docker compose ps || true)
  else
    warn "Docker compose not available"
  fi
}

redis_status() {
  section "Redis"
  if command -v redis-cli >/dev/null 2>&1; then
    if redis-cli -h "$REDIS_HOST" -p "$REDIS_PORT" --no-auth-warning ping >/dev/null 2>&1; then
      info "Redis PING ok at ${REDIS_HOST}:${REDIS_PORT}"
      local cnt_raw cnt_canon
      cnt_raw=$(redis-cli -h "$REDIS_HOST" -p "$REDIS_PORT" --scan --pattern 'price:*' | wc -l || echo 0)
      cnt_canon=$(redis-cli -h "$REDIS_HOST" -p "$REDIS_PORT" --scan --pattern 'price_canon:*' | wc -l || echo 0)
      echo "keys: price:* = $cnt_raw, price_canon:* = $cnt_canon"
    else
      warn "Redis not responding at ${REDIS_HOST}:${REDIS_PORT}"
    fi
  else
    warn "redis-cli not found"
  fi
}

process_status() {
  section "Local process"
  local pid_file="$PID_DIR/screener-core.pid"
  if [[ -f "$pid_file" ]]; then
    local pid
    pid="$(cat "$pid_file" 2>/dev/null || true)"
    if [[ -n "$pid" ]] && ps -p "$pid" >/dev/null 2>&1; then
      echo "screener-core running, PID=$pid"
    else
      echo "screener-core not running (stale pid file?)"
    fi
  else
    echo "No local PID file"
  fi
}

tail_logs() {
  section "Last logs"
  if [[ -f "$LOG_FILE" ]]; then
    tail -n 40 "$LOG_FILE" || true
  else
    echo "No log file yet: $LOG_FILE"
  fi
}

main() {
  docker_status
  redis_status
  process_status
  tail_logs
}

main "$@"
