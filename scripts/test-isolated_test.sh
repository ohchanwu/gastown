#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
LAUNCHER="$SCRIPT_DIR/test-isolated.sh"
TMPDIR="$(mktemp -d)"
CAPTURE="$TMPDIR/go.env"
PASS=0
FAIL=0
LISTENER_PID=""
BLOCK_PID=""
LAUNCHER_DOLT_PID=""

cleanup() {
  if [[ -n "$LISTENER_PID" ]] && kill -0 "$LISTENER_PID" 2>/dev/null; then
    kill "$LISTENER_PID"
    wait "$LISTENER_PID" 2>/dev/null || true
  fi
  if [[ -n "$BLOCK_PID" ]] && kill -0 "$BLOCK_PID" 2>/dev/null; then
    kill "$BLOCK_PID"
    wait "$BLOCK_PID" 2>/dev/null || true
  fi
  if [[ -n "$LAUNCHER_DOLT_PID" ]] && kill -0 "$LAUNCHER_DOLT_PID" 2>/dev/null; then
    kill "$LAUNCHER_DOLT_PID"
    wait "$LAUNCHER_DOLT_PID" 2>/dev/null || true
  fi
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

mkdir -p "$TMPDIR/bin"
cat > "$TMPDIR/bin/go" <<'FAKE_GO'
#!/usr/bin/env bash
if [[ "${1:-}" == build && "${2:-}" == -o ]]; then
  cat > "${3:?guard output required}" <<'FAKE_GUARD'
#!/usr/bin/env bash
case "${1:-}" in
    custody)
      printf 'fake-pre-identity-custody\n'
      ;;
    stop-custody)
      [[ "${4:-}" == fake-pre-identity-custody ]] || exit 6
      kill "${2:?launcher PID required}"
      ;;
    identity)
      [[ -z "${FAKE_GUARD_IDENTITY_EXIT:-}" ]] || exit "$FAKE_GUARD_IDENTITY_EXIT"
      printf 'fake-launcher-identity\n'
      ;;
    stop)
      [[ "${5:-}" == fake-launcher-identity ]] || exit 6
      [[ -z "${FAKE_GUARD_STOP_EXIT:-}" ]] || exit "$FAKE_GUARD_STOP_EXIT"
      kill "${2:?launcher PID required}"
      ;;
    snapshot)
      printf 'baseline\n' > "${2:?baseline receipt required}"
      [[ -n "${GUARD_CAPTURE:-}" ]] && printf 'snapshot\n' >> "$GUARD_CAPTURE"
      ;;
    cleanup)
      [[ -n "${GUARD_CAPTURE:-}" ]] && printf 'cleanup\n' >> "$GUARD_CAPTURE"
      if [[ -n "${FAKE_GUARD_CLEANUP_EXIT:-}" ]]; then
        exit "$FAKE_GUARD_CLEANUP_EXIT"
      fi
      [[ -n "${FAKE_LEAK:-}" ]] && rm -f -- "$FAKE_LEAK"
      ;;
esac
exit 0
FAKE_GUARD
  chmod +x "${3}"
  exit 0
fi

if [[ -n "${FAIL_IF_LEAK_PRESENT:-}" && -e "${FAKE_LEAK:-}" ]]; then
  exit 42
fi
[[ -n "${CREATE_FAKE_LEAK:-}" ]] && touch "${FAKE_LEAK:?}"
if [[ -n "${FAKE_GO_BLOCK:-}" ]]; then
  printf '%s\n' "$$" > "${FAKE_GO_PID_FILE:?}"
  trap 'exit 143' TERM
  while :; do sleep 1; done
fi
printf '%s\n' \
  "GT_DOLT_PORT=$GT_DOLT_PORT" \
  "BEADS_DOLT_PORT=$BEADS_DOLT_PORT" \
  "BEADS_DOLT_SERVER_PORT=$BEADS_DOLT_SERVER_PORT" \
  "GT_TEST_DOLT_PORT=${GT_TEST_DOLT_PORT-}" \
	"GT_TEST_ISOLATED=${GT_TEST_ISOLATED-}" \
	"GIT_CONFIG_GLOBAL=${GIT_CONFIG_GLOBAL-}" \
	"GIT_CONFIG_SYSTEM=${GIT_CONFIG_SYSTEM-}" \
  "umask=$(umask)" \
  "args=$*" > "$CAPTURE"
exit "${FAKE_GO_EXIT:-0}"
FAKE_GO
chmod +x "$TMPDIR/bin/go"

cat > "$TMPDIR/bin/dolt" <<'FAKE_DOLT'
#!/usr/bin/env bash
[[ -n "${FAKE_DOLT_PID_FILE:-}" ]] && printf '%s\n' "$$" > "$FAKE_DOLT_PID_FILE"
if [[ -n "${FAKE_DOLT_NEVER_LISTENS:-}" ]]; then
  trap 'exit 143' TERM
  while :; do sleep 1; done
fi
port=""
while (( $# )); do
  case "$1" in
    --port) port="${2:-}"; shift 2 ;;
    *) shift ;;
  esac
done
exec python3 - "$port" <<'PY'
import signal
import socket
import sys

listener = socket.socket()
listener.bind(("127.0.0.1", int(sys.argv[1])))
listener.listen()
signal.pause()
PY
FAKE_DOLT
chmod +x "$TMPDIR/bin/dolt"

pass() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

fail() {
  echo "  FAIL: $1"
  FAIL=$((FAIL + 1))
}

echo "=== isolated test launcher tests ==="

if (
  umask 077
  PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
    GT_DOLT_PORT=45123 BEADS_DOLT_PORT=45123 BEADS_DOLT_SERVER_PORT=45123 \
    GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER"
); then
  expected=$'GT_DOLT_PORT=44001\nBEADS_DOLT_PORT=44001\nBEADS_DOLT_SERVER_PORT=44001\nGT_TEST_DOLT_PORT=\nGT_TEST_ISOLATED=1\nGIT_CONFIG_GLOBAL=/dev/null\nGIT_CONFIG_SYSTEM=/dev/null\numask=0022\nargs=test -timeout=15m -p 1 ./...'
  [[ "$(cat "$CAPTURE")" == "$expected" ]] && \
    pass "quarantines inherited Dolt selectors" || \
    fail "quarantines inherited Dolt selectors"
else
  fail "launches with an explicit isolated Dolt port"
fi

if PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" GT_TEST_DOLT_PORT=44001 \
  bash "$LAUNCHER" -race ./internal/doltserver ./internal/testguard -count=1; then
  expected=$'GT_DOLT_PORT=44001\nBEADS_DOLT_PORT=44001\nBEADS_DOLT_SERVER_PORT=44001\nGT_TEST_DOLT_PORT=\nGT_TEST_ISOLATED=1\nGIT_CONFIG_GLOBAL=/dev/null\nGIT_CONFIG_SYSTEM=/dev/null\numask=0022\nargs=test -race ./internal/doltserver ./internal/testguard -count=1'
  [[ "$(cat "$CAPTURE")" == "$expected" ]] && \
    pass "passes focused go test arguments through isolation" || \
    fail "passes focused go test arguments through isolation"
else
  fail "launches focused go test arguments"
fi

rm -f "$CAPTURE"
status=0
output="$(env -u GT_TEST_DOLT_PORT PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  GT_DOLT_PORT=45123 BEADS_DOLT_PORT=45123 \
  bash "$LAUNCHER" 2>&1)" || status=$?
if [[ "$status" -eq 78 && ! -e "$CAPTURE" && \
      "$output" == *"test-isolation: configuration"* ]]; then
  pass "fails closed without an isolated Dolt port"
else
  fail "fails closed without an isolated Dolt port"
fi

for invalid_port in 0 65536; do
  rm -f "$CAPTURE"
  status=0
  output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
    GT_TEST_DOLT_PORT="$invalid_port" bash "$LAUNCHER" 2>&1)" || status=$?
  if [[ "$status" -eq 78 && ! -e "$CAPTURE" && \
        "$output" == *"test-isolation: configuration"* ]]; then
    pass "rejects out-of-range test port $invalid_port"
  else
    fail "rejects out-of-range test port $invalid_port"
  fi
done

rm -f "$CAPTURE"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  GT_DOLT_PORT=45123 BEADS_DOLT_PORT=45123 GT_TEST_DOLT_PORT=45123 \
  bash "$LAUNCHER" 2>&1)" || status=$?
if [[ "$status" -eq 78 && ! -e "$CAPTURE" && \
      "$output" == *"test-isolation: configuration"* ]]; then
  pass "rejects an isolated port that aliases an inherited listener"
else
  fail "rejects an isolated port that aliases an inherited listener"
fi

rm -f "$CAPTURE"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  BEADS_DOLT_PORT=3306 GT_TEST_DOLT_PORT=03306 \
  bash "$LAUNCHER" 2>&1)" || status=$?
if [[ "$status" -eq 78 && ! -e "$CAPTURE" && \
      "$output" == *"test-isolation: configuration"* ]]; then
  pass "rejects a numerically equivalent inherited listener port"
else
  fail "rejects a numerically equivalent inherited listener port"
fi

rm -f "$CAPTURE"
listener_ready="$TMPDIR/listener.port"
python3 - "$listener_ready" <<'PY' &
import signal
import socket
import sys

listener = socket.socket()
listener.bind(("127.0.0.1", 0))
listener.listen()
with open(sys.argv[1], "w", encoding="utf-8") as ready:
    ready.write(str(listener.getsockname()[1]))
signal.pause()
PY
LISTENER_PID=$!
for _ in {1..100}; do
  [[ -s "$listener_ready" ]] && break
  kill -0 "$LISTENER_PID" 2>/dev/null || break
  sleep 0.02
done
occupied_port="$(cat "$listener_ready")"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  GT_TEST_DOLT_PORT="$occupied_port" bash "$LAUNCHER" 2>&1)" || status=$?
kill "$LISTENER_PID"
wait "$LISTENER_PID" 2>/dev/null || true
LISTENER_PID=""
if [[ "$status" -eq 78 && ! -e "$CAPTURE" && \
      "$output" == *"test-isolation: configuration"* ]]; then
  pass "rejects a preexisting listener without inherited selectors"
else
  fail "rejects a preexisting listener without inherited selectors"
fi

status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" FAKE_GO_EXIT=9 \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER" 2>&1)" || status=$?
if [[ "$status" -eq 9 && "$output" == *"test-isolation: suite"* ]]; then
  pass "classifies suite failures and preserves their status"
else
  fail "classifies suite failures and preserves their status"
fi

rm -f "$CAPTURE"
guard_capture="$TMPDIR/guard.calls"
fake_leak="$TMPDIR/orphaned-test-listener"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" GUARD_CAPTURE="$guard_capture" \
  FAKE_LEAK="$fake_leak" CREATE_FAKE_LEAK=1 FAKE_GO_EXIT=9 \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER" 2>&1)" || status=$?
retry_status=0
PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" GUARD_CAPTURE="$guard_capture" \
  FAKE_LEAK="$fake_leak" FAIL_IF_LEAK_PRESENT=1 \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER" >/dev/null 2>&1 || retry_status=$?
expected_guard_calls=$'snapshot\ncleanup\nsnapshot\ncleanup'
if [[ "$status" -eq 9 && "$retry_status" -eq 0 && ! -e "$fake_leak" && \
      "$(cat "$guard_capture")" == "$expected_guard_calls" ]]; then
  pass "reaps a failed run before a clean retry"
else
  fail "reaps a failed run before a clean retry"
fi

rm -f "$guard_capture" "$fake_leak"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" GUARD_CAPTURE="$guard_capture" \
  FAKE_LEAK="$fake_leak" CREATE_FAKE_LEAK=1 FAKE_GO_EXIT=9 FAKE_GUARD_CLEANUP_EXIT=7 \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER" 2>&1)" || status=$?
if [[ "$status" -eq 1 && "$output" == *"test-isolation: cleanup"* ]]; then
  pass "fails visibly when outer cleanup fails"
else
  fail "fails visibly when outer cleanup fails"
fi

never_pid_file="$TMPDIR/never-listens.pid"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  FAKE_DOLT_NEVER_LISTENS=1 FAKE_DOLT_PID_FILE="$never_pid_file" \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER" 2>&1)" || status=$?
LAUNCHER_DOLT_PID="$(cat "$never_pid_file")"
remaining_run_dir="$(find "$TMPDIR" -maxdepth 1 -type d -name 'gastown-test-dolt.*' -print -quit)"
if [[ "$status" -eq 78 && -z "$remaining_run_dir" ]] && \
      ! kill -0 "$LAUNCHER_DOLT_PID" 2>/dev/null; then
  LAUNCHER_DOLT_PID=""
  pass "stops a launcher that stays alive without listening"
else
  fail "stops a launcher that stays alive without listening"
fi

identity_pid_file="$TMPDIR/identity-failure.pid"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  FAKE_DOLT_PID_FILE="$identity_pid_file" FAKE_GUARD_IDENTITY_EXIT=7 \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER" 2>&1)" || status=$?
LAUNCHER_DOLT_PID="$(cat "$identity_pid_file")"
remaining_run_dir="$(find "$TMPDIR" -maxdepth 1 -type d -name 'gastown-test-dolt.*' -print -quit)"
if [[ "$status" -eq 78 && -z "$remaining_run_dir" ]] && \
      ! kill -0 "$LAUNCHER_DOLT_PID" 2>/dev/null; then
  LAUNCHER_DOLT_PID=""
  pass "stops a launcher when listener identity capture fails"
else
  fail "stops a launcher when listener identity capture fails"
fi

fallback_pid_file="$TMPDIR/custody-fallback.pid"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  FAKE_DOLT_PID_FILE="$fallback_pid_file" FAKE_GUARD_STOP_EXIT=7 \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER" 2>&1)" || status=$?
LAUNCHER_DOLT_PID="$(cat "$fallback_pid_file")"
remaining_run_dir="$(find "$TMPDIR" -maxdepth 1 -type d -name 'gastown-test-dolt.*' -print -quit)"
if [[ "$status" -eq 1 && -z "$remaining_run_dir" ]] && \
      ! kill -0 "$LAUNCHER_DOLT_PID" 2>/dev/null; then
  LAUNCHER_DOLT_PID=""
  pass "falls back to direct-child custody when identity stop fails"
else
  fail "falls back to direct-child custody when identity stop fails"
fi

rm -f "$guard_capture" "$fake_leak"
block_pid_file="$TMPDIR/block.pid"
PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" GUARD_CAPTURE="$guard_capture" \
  FAKE_LEAK="$fake_leak" CREATE_FAKE_LEAK=1 FAKE_GO_BLOCK=1 \
  FAKE_GO_PID_FILE="$block_pid_file" GT_TEST_DOLT_PORT=44001 \
  bash "$LAUNCHER" >/dev/null 2>&1 &
launcher_pid=$!
for _ in {1..100}; do
  [[ -s "$block_pid_file" ]] && break
  kill -0 "$launcher_pid" 2>/dev/null || break
  sleep 0.02
done
BLOCK_PID="$(cat "$block_pid_file")"
kill -TERM "$BLOCK_PID" "$launcher_pid"
status=0
wait "$launcher_pid" || status=$?
wait "$BLOCK_PID" 2>/dev/null || true
BLOCK_PID=""
if [[ "$status" -eq 143 && ! -e "$fake_leak" && \
      "$(cat "$guard_capture")" == $'snapshot\ncleanup' ]]; then
  pass "reaps test-owned leaks when an active suite is terminated"
else
  fail "reaps test-owned leaks when an active suite is terminated"
fi

make_plan="$(make --no-print-directory -n -C "$REPO_ROOT" test)"
if [[ "$make_plan" == *"bash scripts/test-isolated.sh"* ]] && \
   [[ "$make_plan" != *$'\ngo test ./...'* ]]; then
  pass "routes the repository-wide Make target through isolation"
else
  fail "routes the repository-wide Make target through isolation"
fi

echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
