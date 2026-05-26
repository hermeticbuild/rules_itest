#!/usr/bin/env bash
set -euo pipefail

cleanup_marker="${TEST_TMPDIR}/startup_failure_cleanup_marker"

cleanup() {
    echo "cleanup service received shutdown"
    echo "cleanup ran" >"${cleanup_marker}"
    exit 0
}

trap cleanup TERM INT

while true; do
    sleep 1
done
