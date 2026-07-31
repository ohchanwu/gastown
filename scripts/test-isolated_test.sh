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

cleanup() {
  if [[ -n "$LISTENER_PID" ]] && kill -0 "$LISTENER_PID" 2>/dev/null; then
    kill "$LISTENER_PID"
    wait "$LISTENER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

mkdir -p "$TMPDIR/bin"
cat > "$TMPDIR/bin/go" <<'FAKE_GO'
#!/usr/bin/env bash
printf '%s\n' \
  "GT_DOLT_PORT=$GT_DOLT_PORT" \
  "BEADS_DOLT_PORT=$BEADS_DOLT_PORT" \
  "BEADS_DOLT_SERVER_PORT=$BEADS_DOLT_SERVER_PORT" \
  "args=$*" > "$CAPTURE"
exit "${FAKE_GO_EXIT:-0}"
FAKE_GO
chmod +x "$TMPDIR/bin/go"

cat > "$TMPDIR/bin/dolt" <<'FAKE_DOLT'
#!/usr/bin/env bash
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

if PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
  GT_DOLT_PORT=45123 BEADS_DOLT_PORT=45123 BEADS_DOLT_SERVER_PORT=45123 \
  GT_TEST_DOLT_PORT=44001 bash "$LAUNCHER"; then
  expected=$'GT_DOLT_PORT=44001\nBEADS_DOLT_PORT=44001\nBEADS_DOLT_SERVER_PORT=44001\nargs=test ./...'
  [[ "$(cat "$CAPTURE")" == "$expected" ]] && \
    pass "quarantines inherited Dolt selectors" || \
    fail "quarantines inherited Dolt selectors"
else
  fail "launches with an explicit isolated Dolt port"
fi

rm -f "$CAPTURE"
status=0
output="$(PATH="$TMPDIR/bin:$PATH" CAPTURE="$CAPTURE" \
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

make_plan="$(make --no-print-directory -n -C "$REPO_ROOT" test)"
if [[ "$make_plan" == *"bash scripts/test-isolated.sh"* ]] && \
   [[ "$make_plan" != *$'\ngo test ./...'* ]]; then
  pass "routes the repository-wide Make target through isolation"
else
  fail "routes the repository-wide Make target through isolation"
fi

echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
