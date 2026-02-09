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
#   2. .env file in project root (will be sourced automatically)

set -euo pipefail

# Determine project directory
PROJECT_DIR="${GEMINI_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# Source .env file if it exists (allows ENTIRE_LOCAL_DEV=1 in .env)
if [ -f "${PROJECT_DIR}/.env" ]; then
    set -a  # Export all variables
    source "${PROJECT_DIR}/.env"
    set +a
fi

if [ "${ENTIRE_LOCAL_DEV:-}" = "1" ]; then
    # Local dev mode: use go run
    exec go run "${PROJECT_DIR}/cmd/entire/main.go" "$@"
else
    # Production mode: use entire binary in PATH
    exec entire "$@"
fi
