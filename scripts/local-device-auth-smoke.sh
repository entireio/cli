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

# A --server login is recorded as a contexts.json context (the legacy
# keyring/auth.json entry is only written for the default login server).
CONTEXTS_FILE="${ENTIRE_CONFIG_DIR:-${HOME}/.config/entire}/contexts.json"

python3 - <<'PY' "${CONTEXTS_FILE}" "${ENTIRE_LOGIN_SERVER}"
import json
import pathlib
import sys

contexts_file = pathlib.Path(sys.argv[1])
server = sys.argv[2].rstrip("/")

if not contexts_file.exists():
    raise SystemExit(f"Contexts file not found: {contexts_file}")

data = json.loads(contexts_file.read_text())
if not any(c.get("core_url", "").rstrip("/") == server for c in data.get("contexts", [])):
    raise SystemExit(f"No login context for {server} in {contexts_file}")

print(f"Verified login context for {server} in {contexts_file}")
PY

cat "${LOG_FILE}"
