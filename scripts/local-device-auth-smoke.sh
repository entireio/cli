#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

: "${ENTIRE_API_BASE_URL:=http://localhost:8787}"
# The login server is chosen at login time via --server
# (ENTIRE_AUTH_BASE_URL is retired); default to the same host that
# serves the local data API.
: "${ENTIRE_LOGIN_SERVER:=${ENTIRE_API_BASE_URL}}"

LOG_FILE="$(mktemp -t entire-device-auth-smoke.XXXXXX.log)"
cleanup() {
  if [[ -n "${LOGIN_PID:-}" ]] && kill -0 "${LOGIN_PID}" 2>/dev/null; then
    kill "${LOGIN_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

cd "${REPO_ROOT}"

echo "Starting device auth login against ${ENTIRE_LOGIN_SERVER}"
ENTIRE_TEST_TTY=0 ENTIRE_API_BASE_URL="${ENTIRE_API_BASE_URL}" go run ./cmd/entire login --server "${ENTIRE_LOGIN_SERVER}" --insecure-http-auth >"${LOG_FILE}" 2>&1 &
LOGIN_PID=$!

for _ in {1..100}; do
  if grep -q '^Login URL:' "${LOG_FILE}" && grep -q '^Device code: ' "${LOG_FILE}"; then
    break
  fi
  sleep 0.1
done

if ! grep -q '^Login URL:' "${LOG_FILE}"; then
  cat "${LOG_FILE}"
  echo "Failed to capture login URL from login output" >&2
  exit 1
fi

APPROVAL_URL="$(python3 - <<'PY' "${LOG_FILE}"
import pathlib
import sys

for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    if line.startswith("Login URL:"):
        print(line.split(":", 1)[1].strip())
        break
PY
)"

DEVICE_CODE="$(python3 - <<'PY' "${LOG_FILE}"
import pathlib
import sys

for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    if line.startswith("Device code: "):
        print(line.split(": ", 1)[1])
        break
PY
)"

echo "Device code: ${DEVICE_CODE}"
echo "Login URL: ${APPROVAL_URL}"

if command -v open >/dev/null 2>&1; then
  open "${APPROVAL_URL}"
elif command -v xdg-open >/dev/null 2>&1; then
  xdg-open "${APPROVAL_URL}"
else
  echo "No browser opener found. Open this URL manually:" >&2
  echo "  ${APPROVAL_URL}" >&2
fi

echo "Approve the request in your browser. Waiting for CLI login to finish..."

if ! wait "${LOGIN_PID}"; then
  cat "${LOG_FILE}"
  echo "Login command failed" >&2
  exit 1
fi

echo "Verifying the current login from the credential store"
ENTIRE_API_BASE_URL="${ENTIRE_API_BASE_URL}" \
  go run ./cmd/entire auth status --insecure-http-auth

cat "${LOG_FILE}"
