#!/usr/bin/env bash

set -uo pipefail

# Tests assert repository-default permissions; do not inherit a restrictive
# interactive-shell umask into the hermetic suite.
umask 022

server_pid=""
data_dir=""

cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill "$server_pid"
    wait "$server_pid" 2>/dev/null || true
  fi
  [[ -n "$data_dir" ]] && rm -rf -- "$data_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

test_port="${GT_TEST_DOLT_PORT:-}"
case "$test_port" in
  ''|*[!0-9]*)
    echo "test-isolation: configuration: GT_TEST_DOLT_PORT must name a test-owned listener" >&2
    exit 78
    ;;
esac

if (( ${#test_port} > 5 || 10#$test_port < 1 || 10#$test_port > 65535 )); then
  echo "test-isolation: configuration: GT_TEST_DOLT_PORT is outside the TCP port range" >&2
  exit 78
fi
test_port=$((10#$test_port))

for selector in GT_DOLT_PORT BEADS_DOLT_PORT BEADS_DOLT_SERVER_PORT; do
  if [[ -n "${!selector:-}" && "${!selector}" == "$test_port" ]]; then
    echo "test-isolation: configuration: test port aliases an inherited Dolt listener" >&2
    exit 78
  fi
done

for dependency in dolt lsof; do
  if ! command -v "$dependency" >/dev/null 2>&1; then
    echo "test-isolation: configuration: required command '$dependency' is unavailable" >&2
    exit 78
  fi
done

data_dir="$(mktemp -d "${TMPDIR:-/tmp}/gastown-test-dolt.XXXXXX")" || {
  echo "test-isolation: configuration: could not create isolated Dolt data directory" >&2
  exit 78
}
dolt sql-server \
  --host 127.0.0.1 \
  --port "$test_port" \
  --data-dir "$data_dir" \
  --socket "$data_dir/mysql.sock" \
  --loglevel error >"$data_dir/server.log" 2>&1 &
server_pid=$!

owns_listener=false
for _ in {1..200}; do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    wait "$server_pid" 2>/dev/null || true
    server_pid=""
    break
  fi
  if lsof -nP -a -p "$server_pid" -iTCP:"$test_port" -sTCP:LISTEN \
      >/dev/null 2>&1; then
    owns_listener=true
    break
  fi
  sleep 0.05
done

if [[ "$owns_listener" != true ]]; then
  echo "test-isolation: configuration: launcher could not own the isolated Dolt listener" >&2
  exit 78
fi

if env -u GT_TEST_DOLT_PORT \
	GT_TEST_ISOLATED=1 \
	GIT_CONFIG_GLOBAL=/dev/null \
	GIT_CONFIG_SYSTEM=/dev/null \
  GT_DOLT_HOST=127.0.0.1 \
  GT_DOLT_PORT="$test_port" \
  DOLT_PORT="$test_port" \
  BEADS_DOLT_HOST=127.0.0.1 \
  BEADS_DOLT_PORT="$test_port" \
  BEADS_DOLT_SERVER_HOST=127.0.0.1 \
  BEADS_DOLT_SERVER_PORT="$test_port" \
  go test -p 1 ./...; then
  exit 0
else
  status=$?
  echo "test-isolation: suite: isolated test run failed; classify custody errors before retrying" >&2
  exit "$status"
fi
