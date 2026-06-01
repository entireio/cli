#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

: "${ENTIRE_API_BASE_URL:=http://localhost:8787}"
# Pin auth at the same host: AuthBaseURL defaults to the production
# us.auth.entire.io and no longer inherits from ENTIRE_API_BASE_URL.
: "${ENTIRE_AUTH_BASE_URL:=${ENTIRE_API_BASE_URL}}"

LOG_FILE="$(mktemp -t entire-device-auth-smoke.XXXXXX.log)"
cleanup() {
  if [[ -n "${LOGIN_PID:-}" ]] && kill -0 "${LOGIN_PID}" 2>/dev/null; then
    kill "${LOGIN_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

cd "${REPO_ROOT}"

echo "Starting device auth login against ${ENTIRE_API_BASE_URL}"
ENTIRE_TEST_TTY=0 ENTIRE_API_BASE_URL="${ENTIRE_API_BASE_URL}" ENTIRE_AUTH_BASE_URL="${ENTIRE_AUTH_BASE_URL}" go run ./cmd/entire login --insecure-http-auth >"${LOG_FILE}" 2>&1 &
LOGIN_PID=$!

for _ in {1..100}; do
  if grep -q '^Approval URL: ' "${LOG_FILE}" && grep -q '^Device code: ' "${LOG_FILE}"; then
    break
  fi
  sleep 0.1
done

if ! grep -q '^Approval URL: ' "${LOG_FILE}"; then
  cat "${LOG_FILE}"
  echo "Failed to capture approval URL from login output" >&2
  exit 1
fi

APPROVAL_URL="$(python3 - <<'PY' "${LOG_FILE}"
import pathlib
import sys

for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    if line.startswith("Approval URL: "):
        print(line.split(": ", 1)[1])
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
echo "Approval URL: ${APPROVAL_URL}"

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

AUTH_FILE="${HOME}/.config/entire/auth.json"

python3 - <<'PY' "${AUTH_FILE}" "${ENTIRE_API_BASE_URL}"
import json
import pathlib
import sys

auth_file = pathlib.Path(sys.argv[1])
base_url = sys.argv[2]

if not auth_file.exists():
    raise SystemExit(f"Auth file not found: {auth_file}")

data = json.loads(auth_file.read_text())
token = data.get("tokens", {}).get(base_url, {}).get("value", "")
if not token:
    raise SystemExit(f"No token saved for {base_url} in {auth_file}")

print(f"Verified token saved for {base_url} in {auth_file}")
PY

cat "${LOG_FILE}"
