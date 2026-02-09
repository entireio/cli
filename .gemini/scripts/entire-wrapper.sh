#!/usr/bin/env bash
# Wrapper script for Entire CLI hooks that supports local development mode
#
# Usage: entire-wrapper.sh <args...>
#
# By default, uses the 'entire' binary in PATH.
# Set ENTIRE_LOCAL_DEV=1 to use 'go run' for local development.
#
# You can set ENTIRE_LOCAL_DEV via:
#   1. Shell profile: export ENTIRE_LOCAL_DEV=1
#   2. .env file in project root (variable will be extracted automatically)

set -euo pipefail

# Determine project directory
PROJECT_DIR="${GEMINI_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# Extract ENTIRE_LOCAL_DEV from .env file if it exists
# We only extract this specific variable to avoid issues with undefined variables
# in .env files (e.g., ${DB_USER} expansions that might fail with set -u)
if [ -f "${PROJECT_DIR}/.env" ]; then
    # Use grep to find the line, then extract the value
    # Format: ENTIRE_LOCAL_DEV=1 or ENTIRE_LOCAL_DEV="1"
    ENTIRE_LOCAL_DEV_FROM_FILE=$(grep -E "^ENTIRE_LOCAL_DEV=" "${PROJECT_DIR}/.env" 2>/dev/null | cut -d'=' -f2- | tr -d '"' || echo "")
    if [ -z "${ENTIRE_LOCAL_DEV:-}" ] && [ -n "${ENTIRE_LOCAL_DEV_FROM_FILE}" ]; then
        export ENTIRE_LOCAL_DEV="${ENTIRE_LOCAL_DEV_FROM_FILE}"
    fi
fi

if [ "${ENTIRE_LOCAL_DEV:-}" = "1" ]; then
    # Local dev mode: use go run
    exec go run "${PROJECT_DIR}/cmd/entire/main.go" "$@"
else
    # Production mode: use entire binary in PATH
    exec entire "$@"
fi
