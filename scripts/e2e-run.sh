#!/bin/sh
# Shared execution for one agent and a Go test regex. Run from the repo root.
# Credentials come from the agent's native environment variables.
set -eu

if [ "$#" -gt 2 ]; then
  echo "usage: sh scripts/e2e-run.sh <agent> [test-regex]; use the agent's native credential environment variable" >&2
  exit 2
fi
agent="${1:-${E2E_AGENT:-}}"
filter="${2:-}"
case "$agent" in
  ''|claude-code|opencode|gemini-cli|codex|cursor-cli|factoryai-droid|copilot-cli|pi|vogon|roger-roger) ;;
  *) echo "error: unknown E2E agent: $agent" >&2; exit 2 ;;
esac
export E2E_AGENT="$agent"
export E2E_CHECKPOINT_STORE="${E2E_CHECKPOINT_STORE:-git-refs}"

# CI supplies API keys; local runs may use the agent's stored login instead.
if [ "${E2E_REQUIRE_CREDENTIALS:-0}" = 1 ]; then
  case "$agent" in
    claude-code|opencode) : "${ANTHROPIC_API_KEY:?ANTHROPIC_API_KEY is required for $agent}" ;;
    gemini-cli) : "${GEMINI_API_KEY:?GEMINI_API_KEY is required for $agent}" ;;
    codex) : "${OPENAI_API_KEY:?OPENAI_API_KEY is required for $agent}" ;;
    cursor-cli) : "${CURSOR_API_KEY:?CURSOR_API_KEY is required for $agent}" ;;
    factoryai-droid) : "${FACTORY_API_KEY:?FACTORY_API_KEY is required for $agent}" ;;
    copilot-cli) : "${COPILOT_GITHUB_TOKEN:?COPILOT_GITHUB_TOKEN is required for $agent}" ;;
  esac
fi

if [ -z "${E2E_ENTIRE_BIN:-}" ]; then
  mise run build
  if [ -f "$PWD/entire.exe" ]; then
    export E2E_ENTIRE_BIN="$PWD/entire.exe"
  else
    export E2E_ENTIRE_BIN="$PWD/entire"
  fi
fi

case "$agent" in
  vogon)
    vogon_binary=vogon
    if [ "${OS:-}" = Windows_NT ]; then vogon_binary=vogon.exe; fi
    go build -o "$vogon_binary" ./e2e/vogon
    export PATH="$PWD:$PATH"
    ;;
  roger-roger)
    for binary in roger-roger entire-agent-roger-roger; do
      if ! command -v "$binary" >/dev/null 2>&1; then
        echo "error: $binary not found on PATH; install it with mise install" >&2
        exit 1
      fi
    done
    ;;
esac

if [ -z "${E2E_ARTIFACT_DIR:-}" ]; then
  E2E_ARTIFACT_DIR="$PWD/e2e/artifacts/$(date +%Y-%m-%dT%H-%M-%S)-${agent:-all}"
fi
export E2E_ARTIFACT_DIR
mkdir -p "$E2E_ARTIFACT_DIR"
echo "artifacts: $E2E_ARTIFACT_DIR"

# Canary keeps its existing per-lane filenames in the shared artifact directory.
suffix="${E2E_REPORT_SUFFIX:-}"
events="$E2E_ARTIFACT_DIR/test-events$suffix.json"
report="$E2E_ARTIFACT_DIR/report$suffix.txt"
# A failed bootstrap must not leave a previous run's events looking current.
: > "$events"
rc=0
if [ "${E2E_BOOTSTRAP:-0}" = 1 ]; then
  go run ./e2e/bootstrap "$agent" || rc=$?
fi

if [ "$rc" -eq 0 ]; then
  set -- --format dots --packages=./e2e/tests --jsonfile "$events"
  timeout=30m
  case "$agent" in
    vogon|roger-roger) timeout=10m ;;
    *) set -- "$@" --rerun-fails=1 ;;
  esac
  # Explicit package path avoids Go's Windows ARM64 wildcard failures.
  # Deterministic agents never retry a failing test.
  set -- "$@" -- -tags=e2e -count=1 "-timeout=$timeout"
  if [ -n "$filter" ]; then
    set -- "$@" -run "$filter"
  fi
  gotestsum "$@" || rc=$?
fi

report_rc=0
# CI supplies the current reporter when testing a separate nightly checkout.
if [ -n "${E2E_TESTREPORT_BIN:-}" ]; then
  set -- "$E2E_TESTREPORT_BIN"
else
  set -- go run ./e2e/cmd/testreport
fi
"$@" -fail-on-empty -color -o "$report" "$events" || report_rc=$?
echo ""
cat "$E2E_ARTIFACT_DIR/entire-version.txt" 2>/dev/null || true
echo "artifacts: $E2E_ARTIFACT_DIR"
if [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
exit "$report_rc"
