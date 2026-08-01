#!/usr/bin/env bash

set -uo pipefail

server_pid=""
data_dir=""
guard=""
guard_receipt=""
launcher_identity=""
launcher_custody=""

cleanup() {
	status=$?
	trap - EXIT
	cleanup_failed=false
	if [[ -n "$guard_receipt" ]] && ! "$guard" cleanup "$guard_receipt" "$server_pid" "$data_dir"; then
		echo "test-isolation: cleanup: test-owned Dolt listener cleanup failed" >&2
		cleanup_failed=true
	fi
	if [[ -n "$launcher_identity" ]]; then
		if "$guard" stop "$server_pid" "$test_port" "$data_dir" "$launcher_identity"; then
			wait "$server_pid" 2>/dev/null || true
			server_pid=""
		else
			echo "test-isolation: cleanup: launcher Dolt identity changed or could not be stopped" >&2
			cleanup_failed=true
		fi
	elif [[ -n "$launcher_custody" ]]; then
		if "$guard" stop-custody "$server_pid" "$$" "$launcher_custody"; then
			wait "$server_pid" 2>/dev/null || true
			server_pid=""
		else
			echo "test-isolation: cleanup: pre-identity launcher custody changed or could not be stopped" >&2
			cleanup_failed=true
		fi
	elif [[ -n "$server_pid" ]]; then
		echo "test-isolation: cleanup: launcher ownership was never captured; preserving its data directory" >&2
		cleanup_failed=true
	fi
	[[ -n "$data_dir" && -z "$server_pid" ]] && rm -rf -- "$data_dir"
	if [[ "$cleanup_failed" == true ]]; then
		exit 1
	fi
	exit "$status"
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

for dependency in dolt go lsof; do
  if ! command -v "$dependency" >/dev/null 2>&1; then
    echo "test-isolation: configuration: required command '$dependency' is unavailable" >&2
    exit 78
  fi
done

data_dir="$(mktemp -d "${TMPDIR:-/tmp}/gastown-test-dolt.XXXXXX")" || {
  echo "test-isolation: configuration: could not create isolated Dolt data directory" >&2
  exit 78
}
guard="$data_dir/testguard"
if ! go build -o "$guard" ./internal/testguard; then
	echo "test-isolation: configuration: could not build the Dolt listener guard" >&2
	exit 78
fi
dolt sql-server \
  --host 127.0.0.1 \
  --port "$test_port" \
  --data-dir "$data_dir" \
  --socket "$data_dir/mysql.sock" \
  --loglevel error >"$data_dir/server.log" 2>&1 &
server_pid=$!

if ! launcher_custody="$("$guard" custody "$server_pid" "$$")" || [[ -z "$launcher_custody" ]]; then
	echo "test-isolation: configuration: could not capture pre-identity launcher custody" >&2
	exit 78
fi

owns_listener=false
for _ in {1..200}; do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    wait "$server_pid" 2>/dev/null || true
    server_pid=""
		launcher_custody=""
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

if ! launcher_identity="$("$guard" identity "$server_pid" "$test_port" "$data_dir")" || [[ -z "$launcher_identity" ]]; then
	echo "test-isolation: configuration: could not capture launcher Dolt identity" >&2
	exit 78
fi

baseline_receipt="$data_dir/listener-baseline.json"
if ! "$guard" snapshot "$baseline_receipt" "$server_pid" "$data_dir"; then
	echo "test-isolation: configuration: could not snapshot the Dolt listener baseline" >&2
	exit 78
fi
guard_receipt="$baseline_receipt"

mkdir "$data_dir/tmp"
if env \
	TMPDIR="$data_dir/tmp" \
  GT_DOLT_HOST=127.0.0.1 \
  GT_DOLT_PORT="$test_port" \
  DOLT_PORT="$test_port" \
  BEADS_DOLT_HOST=127.0.0.1 \
  BEADS_DOLT_PORT="$test_port" \
  BEADS_DOLT_SERVER_HOST=127.0.0.1 \
  BEADS_DOLT_SERVER_PORT="$test_port" \
  go test ./...; then
  exit 0
else
  status=$?
  echo "test-isolation: suite: isolated test run failed; classify custody errors before retrying" >&2
  exit "$status"
fi
