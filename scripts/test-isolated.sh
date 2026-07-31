#!/usr/bin/env bash

set -uo pipefail

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

for selector in GT_DOLT_PORT BEADS_DOLT_PORT BEADS_DOLT_SERVER_PORT; do
  if [[ -n "${!selector:-}" && "${!selector}" == "$test_port" ]]; then
    echo "test-isolation: configuration: test port aliases an inherited Dolt listener" >&2
    exit 78
  fi
done

if env \
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
