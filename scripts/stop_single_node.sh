#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${WORK_DIR:-$ROOT_DIR/.single-node}"
PID_FILE="$WORK_DIR/gobeansdb.pid"
RUN_SCRIPT="$ROOT_DIR/scripts/run_single_node.sh"

usage() {
  cat <<EOF
Usage: $(basename "$0") [stop|restart]

Commands:
  stop     Stop running single-node gobeansdb service
  restart  Stop first, then start again via run_single_node.sh

Environment overrides:
  WORK_DIR (default: $ROOT_DIR/.single-node)
EOF
}

is_running() {
  if [[ ! -f "$PID_FILE" ]]; then
    return 1
  fi
  local pid
  pid="$(cat "$PID_FILE")"
  if [[ -z "$pid" ]]; then
    return 1
  fi
  kill -0 "$pid" >/dev/null 2>&1
}

stop_service() {
  if ! is_running; then
    echo "gobeansdb is not running"
    rm -f "$PID_FILE"
    return 0
  fi

  local pid
  pid="$(cat "$PID_FILE")"
  echo "Stopping gobeansdb (pid=$pid)..."
  kill "$pid" >/dev/null 2>&1 || true

  for _ in {1..20}; do
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      echo "Stopped"
      rm -f "$PID_FILE"
      return 0
    fi
    sleep 0.5
  done

  echo "Process did not exit in time, sending SIGKILL..."
  kill -9 "$pid" >/dev/null 2>&1 || true
  rm -f "$PID_FILE"
  echo "Stopped (forced)"
}

restart_service() {
  stop_service
  "$RUN_SCRIPT" start
}

main() {
  local cmd="${1:-stop}"
  case "$cmd" in
    stop)
      stop_service
      ;;
    restart)
      restart_service
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      echo "unknown command: $cmd" >&2
      usage
      exit 2
      ;;
  esac
}

main "$@"
