#!/usr/bin/env bash
set -euo pipefail

# Copilot CLI Agent Integration Probe Script
# Tests hook mechanism and captures payloads for the Entire CLI integration.

AGENT_NAME="Copilot CLI"
AGENT_SLUG="copilot-cli"
AGENT_BIN="copilot"
PROBE_DIR=".entire/tmp/probe-${AGENT_SLUG}-$(date +%s)"
HOOK_FILE=".github/hooks/entire-probe.json"
KEEP_CONFIG=false
MANUAL_LIVE=false
RUN_CMD=""

usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Probe Copilot CLI hook mechanism and capture payloads.

Options:
    --run-cmd '<cmd>'   Automated: launch agent with given command, collect captures
    --manual-live       Interactive: user runs agent manually, presses Enter when done
    --keep-config       Don't remove probe hook config after completion
    --help              Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --run-cmd)
            RUN_CMD="$2"
            shift 2
            ;;
        --manual-live)
            MANUAL_LIVE=true
            shift
            ;;
        --keep-config)
            KEEP_CONFIG=true
            shift
            ;;
        --help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            usage
            exit 1
            ;;
    esac
done

if [[ -z "$RUN_CMD" ]] && [[ "$MANUAL_LIVE" != "true" ]]; then
    echo "Error: Specify --run-cmd '<cmd>' or --manual-live"
    usage
    exit 1
fi

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

pass() { echo -e "${GREEN}PASS${NC} $1"; }
warn() { echo -e "${YELLOW}WARN${NC} $1"; }
fail() { echo -e "${RED}FAIL${NC} $1"; }
info() { echo -e "${BLUE}INFO${NC} $1"; }

echo "============================================"
echo "  ${AGENT_NAME} Integration Probe"
echo "============================================"
echo ""

# --- Phase 1: Static Checks ---
echo "--- Static Checks ---"

# Binary present
if command -v "$AGENT_BIN" &>/dev/null; then
    AGENT_PATH=$(command -v "$AGENT_BIN")
    pass "Binary present: $AGENT_PATH"
else
    fail "Binary not found: $AGENT_BIN (blocker)"
    exit 1
fi

# Version info
VERSION=$("$AGENT_BIN" --version 2>/dev/null || echo "unknown")
if [[ "$VERSION" != "unknown" ]]; then
    pass "Version: $VERSION"
else
    warn "Could not determine version"
fi

# Help output
if "$AGENT_BIN" --help &>/dev/null; then
    pass "Help available"
else
    warn "Help not available"
fi

# Hook keywords in help
HELP_OUTPUT=$("$AGENT_BIN" --help 2>&1 || true)
HOOK_KEYWORDS=0
for kw in hook lifecycle callback event trigger plugin extension; do
    if echo "$HELP_OUTPUT" | grep -qi "$kw"; then
        HOOK_KEYWORDS=$((HOOK_KEYWORDS + 1))
    fi
done
if [[ $HOOK_KEYWORDS -gt 0 ]]; then
    pass "Hook keywords found in help ($HOOK_KEYWORDS matches)"
else
    warn "No hook keywords found in help"
fi

# Session keywords
SESSION_KEYWORDS=0
for kw in session resume continue history transcript context; do
    if echo "$HELP_OUTPUT" | grep -qi "$kw"; then
        SESSION_KEYWORDS=$((SESSION_KEYWORDS + 1))
    fi
done
if [[ $SESSION_KEYWORDS -gt 0 ]]; then
    pass "Session keywords found in help ($SESSION_KEYWORDS matches)"
else
    warn "No session keywords found in help"
fi

# Config directory
if [[ -d "$HOME/.copilot" ]]; then
    pass "Config directory: ~/.copilot/"
else
    warn "Config directory ~/.copilot/ not found"
fi

echo ""

# --- Phase 2: Hook Wiring ---
echo "--- Hook Wiring ---"

# Create capture directory
mkdir -p "$PROBE_DIR/captures"
info "Capture directory: $PROBE_DIR/captures/"

# Determine absolute path for captures
CAPTURE_DIR="$(cd "$PROBE_DIR/captures" && pwd)"

# Create hook config that dumps stdin JSON to capture files
mkdir -p .github/hooks

# Build the hook config
# Each hook captures its stdin payload to a timestamped file
cat > "$HOOK_FILE" <<HOOKEOF
{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/sessionStart-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ],
    "sessionEnd": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/sessionEnd-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ],
    "userPromptSubmitted": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/userPromptSubmitted-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ],
    "agentStop": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/agentStop-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ],
    "subagentStop": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/subagentStop-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ],
    "preToolUse": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/preToolUse-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ],
    "postToolUse": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/postToolUse-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ],
    "errorOccurred": [
      {
        "type": "command",
        "bash": "cat > '${CAPTURE_DIR}/errorOccurred-\$(date +%s)-\$\$-\$RANDOM.json'",
        "comment": "Entire CLI integration probe"
      }
    ]
  }
}
HOOKEOF

pass "Hook config written to $HOOK_FILE"
echo ""

# --- Phase 3: Run ---
echo "--- Execution ---"

if [[ "$MANUAL_LIVE" == "true" ]]; then
    echo ""
    echo "Manual live mode. Instructions:"
    echo "  1. Open another terminal in this directory"
    echo "  2. Run: copilot"
    echo "  3. Send a prompt (e.g., 'create a file called hello.txt with hello world')"
    echo "  4. Wait for the agent to complete"
    echo "  5. Exit the agent (Ctrl+C or Ctrl+D)"
    echo "  6. Come back here and press Enter"
    echo ""
    read -rp "Press Enter when the agent session is complete... "
elif [[ -n "$RUN_CMD" ]]; then
    info "Running: $RUN_CMD"
    eval "$RUN_CMD" || true
    info "Command completed"
fi

echo ""

# --- Phase 4: Capture Collection ---
echo "--- Captured Payloads ---"

CAPTURE_COUNT=0
for f in "$CAPTURE_DIR"/*.json; do
    [[ -e "$f" ]] || continue
    CAPTURE_COUNT=$((CAPTURE_COUNT + 1))
    BASENAME=$(basename "$f")
    EVENT_NAME="${BASENAME%%-*}"
    echo ""
    info "Event: $EVENT_NAME"
    info "File: $f"
    if command -v jq &>/dev/null; then
        jq . "$f" 2>/dev/null || cat "$f"
    else
        cat "$f"
    fi
done

if [[ $CAPTURE_COUNT -eq 0 ]]; then
    warn "No payloads captured. Make sure the agent was run from this directory."
fi

echo ""
echo "Total captures: $CAPTURE_COUNT"
echo ""

# --- Phase 5: Cleanup ---
echo "--- Cleanup ---"

if [[ "$KEEP_CONFIG" == "true" ]]; then
    info "Keeping hook config at $HOOK_FILE (--keep-config)"
else
    rm -f "$HOOK_FILE"
    # Remove .github/hooks/ if empty
    rmdir .github/hooks 2>/dev/null || true
    rmdir .github 2>/dev/null || true
    pass "Removed probe hook config"
fi

echo ""

# --- Phase 6: Verdict ---
echo "--- Lifecycle Event Verdict ---"

check_event() {
    local event_name="$1"
    local entire_event="$2"
    local found=false
    for f in "$CAPTURE_DIR"/${event_name}-*.json; do
        [[ -e "$f" ]] && found=true && break
    done
    if $found; then
        pass "$event_name → $entire_event"
    else
        warn "$event_name → $entire_event (not captured)"
    fi
}

check_event "sessionStart" "SessionStart"
check_event "userPromptSubmitted" "TurnStart"
check_event "agentStop" "TurnEnd"
check_event "sessionEnd" "SessionEnd"
check_event "subagentStop" "SubagentEnd"
check_event "preToolUse" "(pass-through)"
check_event "postToolUse" "(pass-through)"
check_event "errorOccurred" "(pass-through)"

echo ""

# Overall verdict
REQUIRED_EVENTS=("sessionStart" "userPromptSubmitted" "agentStop" "sessionEnd")
REQUIRED_FOUND=0
for evt in "${REQUIRED_EVENTS[@]}"; do
    for f in "$CAPTURE_DIR"/${evt}-*.json; do
        [[ -e "$f" ]] && REQUIRED_FOUND=$((REQUIRED_FOUND + 1)) && break
    done
done

echo "--- Overall Verdict ---"
if [[ $REQUIRED_FOUND -eq ${#REQUIRED_EVENTS[@]} ]]; then
    echo -e "${GREEN}COMPATIBLE${NC} — All 4 required lifecycle events captured"
elif [[ $REQUIRED_FOUND -ge 2 ]]; then
    echo -e "${YELLOW}PARTIAL${NC} — $REQUIRED_FOUND of ${#REQUIRED_EVENTS[@]} required events captured"
else
    echo -e "${RED}INCOMPATIBLE${NC} — Only $REQUIRED_FOUND of ${#REQUIRED_EVENTS[@]} required events captured"
fi

echo ""
echo "Probe artifacts: $PROBE_DIR/"
echo "Done."
