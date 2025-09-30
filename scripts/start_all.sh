#!/usr/bin/env bash
# Unified starter for Screner project: Redis + screener-core (and optional Docker stack)
# - Default: local binary of screener-core, Redis via docker-compose if not up
# - --docker-all: run everything via docker compose (redis + screener-core)
#
# Environment overrides:
#   REDIS_HOST (default: localhost)
#   REDIS_PORT (default: 6379)
#
# Exit codes: 0 ok, non-zero on failure. Verbose logging always on.

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PID_DIR="$ROOT_DIR/build"
PID_FILE="$PID_DIR/screener-core.pid"
WINPID_FILE="$PID_DIR/screener-core.winpid"
LOG_FILE="$ROOT_DIR/screner.log"
APP_BIN="$ROOT_DIR/build/screener-core"
APP_SRC="./cmd/screener-core"

REDIS_HOST="${REDIS_HOST:-localhost}"
REDIS_PORT="${REDIS_PORT:-6379}"

DOCKER_ALL=0
NO_BUILD=0
CLEAN_LOG=0
FORCE_RESTART=0

ts() { date +"%Y-%m-%dT%H:%M:%S%z"; }
info() { echo "$(ts) [INFO] $*"; }
warn() { echo "$(ts) [WARN] $*" >&2; }
err()  { echo "$(ts) [ERROR] $*" >&2; }

usage() {
  cat <<USAGE
Usage: $(basename "$0") [options]
  --docker-all        Run full stack in Docker Compose (redis + screener-core)
  --no-build          Skip go build (use existing build/screener-core)
  --clean-log         Truncate screner.log before start
  --force-restart     Stop existing local screener-core before launching
  -h, --help          Show help

Env:
  REDIS_HOST (default: localhost)
  REDIS_PORT (default: 6379)
USAGE
}

for arg in "$@"; do
  case "$arg" in
  --docker-all)     DOCKER_ALL=1 ;;
  --no-build)       NO_BUILD=1 ;;
  --clean-log)      CLEAN_LOG=1 ;;
  --force-restart)  FORCE_RESTART=1 ;;
    -h|--help)    usage; exit 0 ;;
    *) warn "Unknown option: $arg" ;;
  esac
done

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    err "Command not found: $1"
    exit 1
  fi
}

docker_compose_ok() {
  if command -v docker >/dev/null 2>&1; then
    if docker compose version >/dev/null 2>&1; then
      return 0
    fi
  fi
  return 1
}

wait_redis() {
  local host="$1" port="$2" timeout_s=20
  if ! command -v redis-cli >/dev/null 2>&1; then
    warn "redis-cli not found; skipping PING check"
    return 0
  fi
  for attempt in $(seq 1 $timeout_s); do
    if redis-cli -h "$host" -p "$port" --no-auth-warning ping >/dev/null 2>&1; then
      info "Redis is up at ${host}:${port}"
      return 0
    fi
    sleep 1
    if (( attempt % 5 == 0 )); then
      warn "Redis not responding yet (attempt ${attempt}/${timeout_s})"
    fi
  done
  err "Redis didn't respond to PING within ${timeout_s}s at ${host}:${port}"
  return 1
}

ensure_dirs() {
  mkdir -p "$PID_DIR"
}

read_pid_file() {
  if [[ -f "$PID_FILE" ]]; then
    cat "$PID_FILE"
  fi
}

read_winpid_file() {
  if [[ -f "$WINPID_FILE" ]]; then
    cat "$WINPID_FILE"
  fi
}

write_pid_files() {
  local msys_pid="$1"
  echo "$msys_pid" > "$PID_FILE"
  if [[ -r "/proc/$msys_pid/winpid" ]]; then
    cat "/proc/$msys_pid/winpid" > "$WINPID_FILE"
  else
    rm -f "$WINPID_FILE"
  fi
}

clear_pid_files() {
  rm -f "$PID_FILE" "$WINPID_FILE"
}

truncate_log_if_needed() {
  if [[ "$CLEAN_LOG" == "1" ]]; then
    : > "$LOG_FILE" || true
    info "Truncated $LOG_FILE"
  fi
}

start_compose_stack() {
  require_cmd docker
  if ! docker compose version >/dev/null 2>&1; then
    err "Docker Compose plugin not available (docker compose)."
    exit 1
  fi
  info "Starting Docker stack: redis + screener-core"
  (cd "$ROOT_DIR" && docker compose up -d redis screener-core)
  info "Waiting Redis health..."
  wait_redis "$REDIS_HOST" "$REDIS_PORT" || true
}

ensure_redis_local() {
  info "Checking Redis at ${REDIS_HOST}:${REDIS_PORT}..."
  if redis-cli -h "$REDIS_HOST" -p "$REDIS_PORT" --no-auth-warning ping >/dev/null 2>&1; then
    info "Redis is already running."
    return 0
  fi
  if docker_compose_ok; then
    info "Bringing up Redis via docker compose..."
    (cd "$ROOT_DIR" && docker compose up -d redis)
    wait_redis "$REDIS_HOST" "$REDIS_PORT" || true
  else
    err "Redis is not reachable and Docker Compose is unavailable to start it."
    exit 1
  fi
}

build_app() {
  if [[ "$NO_BUILD" == "1" ]]; then
    info "Skipping go build per --no-build"
    return 0
  fi
  require_cmd go
  info "Building screener-core binary..."
  (cd "$ROOT_DIR" && go build -o "$APP_BIN" "$APP_SRC")
  info "Build done: $APP_BIN"
}

msys_pid_alive() {
  local pid="$1"
  [[ -n "$pid" ]] || return 1
  if ps -p "$pid" >/dev/null 2>&1; then
    return 0
  fi
  if [[ -d "/proc/$pid" ]]; then
    return 0
  fi
  return 1
}

windows_pid_alive() {
  local winpid="$1"
  [[ -n "$winpid" ]] || return 1
  if command -v tasklist >/dev/null 2>&1; then
    if MSYS2_ARG_CONV_EXCL='*' tasklist /FI "PID eq $winpid" | grep -q "$winpid"; then
      return 0
    fi
  fi
  return 1
}

local_app_running() {
  local pid
  pid="$(read_pid_file)"
  local winpid
  winpid="$(read_winpid_file)"
  if msys_pid_alive "$pid"; then
    return 0
  fi
  if windows_pid_alive "$winpid"; then
    return 0
  fi
  return 1
}

stop_local_app() {
  local pid
  pid="$(read_pid_file)"
  local winpid
  winpid="$(read_winpid_file)"
  if msys_pid_alive "$pid"; then
    info "Stopping existing screener-core (pid=$pid)"
    kill "$pid" 2>/dev/null || true
    sleep 1
  fi
  if msys_pid_alive "$pid"; then
    warn "Process $pid still alive after SIGTERM, sending SIGKILL"
    kill -9 "$pid" 2>/dev/null || true
    sleep 1
  fi
  if windows_pid_alive "$winpid"; then
    warn "Using taskkill for Windows PID=$winpid"
    MSYS2_ARG_CONV_EXCL='*' taskkill /PID "$winpid" /T /F >/dev/null 2>&1 || true
    sleep 1
  fi
  clear_pid_files
}

start_local_app() {
  if local_app_running; then
    local pid
    pid="$(read_pid_file)"
    info "screener-core already running (PID $pid)"
    return 0
  fi
  if [[ "$FORCE_RESTART" == "1" ]]; then
    stop_local_app
  else
    # очищаем битый PID если процесс уже умер
    clear_pid_files
  fi

  info "Starting local screener-core in background..."
  nohup "$APP_BIN" >> "$LOG_FILE" 2>&1 &
  local msys_pid=$!
  sleep 0.2
  write_pid_files "$msys_pid"
  local winpid
  winpid="$(read_winpid_file)"
  info "screener-core started, msysPID=$msys_pid${winpid:+, winPID=$winpid} (log: $LOG_FILE)"
}

main() {
  ensure_dirs
  truncate_log_if_needed

  if [[ "$DOCKER_ALL" == "1" ]]; then
    info "Mode: Docker stack (redis + screener-core)"
    start_compose_stack
    info "Done. Use scripts/status_all.sh to inspect status."
    exit 0
  fi

  info "Mode: Local screener-core binary + Redis (docker if needed)"
  ensure_redis_local
  build_app
  if [[ "$FORCE_RESTART" == "1" ]]; then
    info "Force restart requested"
    stop_local_app
  fi
  start_local_app
  info "Done. Tail logs: tail -f $LOG_FILE"
}

main "$@"
