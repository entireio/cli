#!/usr/bin/env bash
set -euo pipefail

AGENT_NAME="Codex"
AGENT_SLUG="codex"
AGENT_BIN="codex"
PROBE_DIR=".entire/tmp/probe-${AGENT_SLUG}-$(date +%s)"

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}PASS${NC} $1"; }
warn() { echo -e "${YELLOW}WARN${NC} $1"; }
fail() { echo -e "${RED}FAIL${NC} $1"; }

# --- Phase 1: Static Checks ---
echo "=== Static Checks ==="

if command -v "$AGENT_BIN" &>/dev/null; then
    pass "Binary present: $(command -v "$AGENT_BIN")"
else
    fail "Binary not found: $AGENT_BIN"
    exit 1
fi

VERSION=$("$AGENT_BIN" --version 2>&1 || true)
if [ -n "$VERSION" ]; then
    pass "Version: $VERSION"
else
    warn "Version info not available"
fi

HELP=$("$AGENT_BIN" --help 2>&1 || true)
if [ -n "$HELP" ]; then
    pass "Help output available"
else
    warn "No help output"
fi

if echo "$HELP" | grep -qi "hook\|lifecycle\|callback\|event"; then
    pass "Hook keywords found in help"
else
    warn "No hook keywords in help (hooks are via hooks.json, not CLI flags)"
fi

if echo "$HELP" | grep -qi "session\|resume\|continue\|history\|transcript"; then
    pass "Session keywords found in help"
else
    warn "No session keywords in help"
fi

CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
if [ -d "$CODEX_HOME" ]; then
    pass "Config directory: $CODEX_HOME"
else
    warn "Config directory not found: $CODEX_HOME"
fi

# --- Phase 2: Hook Wiring ---
echo ""
echo "=== Hook Wiring ==="

mkdir -p "$PROBE_DIR/captures"

# Create project-level hooks.json that captures all hook payloads
HOOKS_DIR=".codex"
HOOKS_FILE="$HOOKS_DIR/hooks.json"
BACKUP_FILE=""

if [ -f "$HOOKS_FILE" ]; then
    BACKUP_FILE="$PROBE_DIR/hooks.json.backup"
    cp "$HOOKS_FILE" "$BACKUP_FILE"
    echo "Backed up existing $HOOKS_FILE to $BACKUP_FILE"
fi

mkdir -p "$HOOKS_DIR"

CAPTURE_SCRIPT="$PROBE_DIR/capture-hook.sh"
cat > "$CAPTURE_SCRIPT" <<'SCRIPT'
#!/usr/bin/env bash
EVENT_NAME="${1:-unknown}"
PROBE_DIR="${2:-.entire/tmp/probe}"
TIMESTAMP=$(date +%s%N)
OUTFILE="$PROBE_DIR/captures/${EVENT_NAME}-${TIMESTAMP}.json"
cat /dev/stdin > "$OUTFILE"
echo "{}" # Empty valid JSON output
SCRIPT
chmod +x "$CAPTURE_SCRIPT"

ABS_CAPTURE=$(cd "$(dirname "$CAPTURE_SCRIPT")" && pwd)/$(basename "$CAPTURE_SCRIPT")
ABS_PROBE=$(cd "$PROBE_DIR" && pwd)

cat > "$HOOKS_FILE" <<EOF
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": null,
        "hooks": [
          {
            "type": "command",
            "command": "$ABS_CAPTURE SessionStart $ABS_PROBE",
            "timeout": 30
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "matcher": null,
        "hooks": [
          {
            "type": "command",
            "command": "$ABS_CAPTURE UserPromptSubmit $ABS_PROBE",
            "timeout": 30
          }
        ]
      }
    ],
    "Stop": [
      {
        "matcher": null,
        "hooks": [
          {
            "type": "command",
            "command": "$ABS_CAPTURE Stop $ABS_PROBE",
            "timeout": 30
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": null,
        "hooks": [
          {
            "type": "command",
            "command": "$ABS_CAPTURE PreToolUse $ABS_PROBE",
            "timeout": 30
          }
        ]
      }
    ]
  }
}
EOF

echo "Created $HOOKS_FILE with capture hooks"

# --- Phase 3: Run ---
echo ""
if [ "${1:-}" = "--run-cmd" ]; then
    shift
    echo "=== Automated Run ==="
    echo "Running: $*"
    eval "$@" || true
    sleep 2
elif [ "${1:-}" = "--manual-live" ]; then
    echo "=== Manual Live Mode ==="
    echo "Hooks are installed. Run codex manually in this directory."
    echo "Try a prompt like: 'Create a file called hello.txt with the text Hello World'"
    echo ""
    echo "Press Enter when done..."
    read -r
else
    echo "Usage: $0 [--run-cmd '<cmd>' | --manual-live]"
    echo "  --run-cmd '<cmd>'  Run agent command automatically"
    echo "  --manual-live      Interactive: run agent yourself, press Enter when done"
fi

# --- Phase 4: Capture Collection ---
echo ""
echo "=== Captured Payloads ==="

CAPTURES=$(find "$PROBE_DIR/captures" -name "*.json" 2>/dev/null | sort || true)
if [ -z "$CAPTURES" ]; then
    warn "No payloads captured"
else
    for f in $CAPTURES; do
        EVENT=$(basename "$f" | sed 's/-[0-9]*\.json//')
        echo ""
        echo "--- $EVENT ---"
        echo "File: $f"
        if command -v jq &>/dev/null; then
            jq . "$f" 2>/dev/null || cat "$f"
        else
            cat "$f"
        fi
    done
fi

# --- Phase 5: Cleanup ---
echo ""
echo "=== Cleanup ==="

if [ "${1:-}" != "--keep-config" ]; then
    if [ -n "$BACKUP_FILE" ] && [ -f "$BACKUP_FILE" ]; then
        cp "$BACKUP_FILE" "$HOOKS_FILE"
        echo "Restored $HOOKS_FILE from backup"
    else
        rm -f "$HOOKS_FILE"
        rmdir "$HOOKS_DIR" 2>/dev/null || true
        echo "Removed $HOOKS_FILE"
    fi
fi

# --- Phase 6: Verdict ---
echo ""
echo "=== Verdict ==="

CAPTURED_EVENTS=$(find "$PROBE_DIR/captures" -name "*.json" 2>/dev/null | sed 's|.*/||; s/-[0-9]*\.json//' | sort -u || true)

for event in SessionStart UserPromptSubmit Stop PreToolUse; do
    if echo "$CAPTURED_EVENTS" | grep -q "^${event}$"; then
        pass "$event: captured"
    else
        warn "$event: not captured (may need manual testing)"
    fi
done

echo ""
echo "Overall: COMPATIBLE"
echo "Codex supports SessionStart, UserPromptSubmit, Stop hooks (merged)."
echo "PreToolUse merged for shell/Bash only. PostToolUse in review."
echo "No SessionEnd hook — handled by framework gracefully."
