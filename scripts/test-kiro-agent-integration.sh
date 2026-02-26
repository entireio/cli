#!/bin/bash
# Test script for Kiro agent integration with Entire CLI.
# Captures hook payloads from kiro-cli to validate lifecycle mapping.
#
# Usage:
#   ./scripts/test-kiro-agent-integration.sh              # Static checks only
#   ./scripts/test-kiro-agent-integration.sh --manual-live # Interactive capture mode
#
# In --manual-live mode, the script:
#   1. Creates a temporary .kiro/agents/entire-probe.json agent config
#   2. Waits for you to run: kiro-cli chat --agent entire-probe
#   3. Captures all hook payloads to .entire/tmp/probe-kiro-*/captures/
#   4. Pretty-prints captured payloads and generates a compatibility report
#
# Prerequisites: kiro-cli must be installed and in PATH.

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

MANUAL_LIVE=false
if [[ "$1" == "--manual-live" ]]; then
    MANUAL_LIVE=true
fi

pass() { echo -e "${GREEN}PASS${NC}: $1"; }
warn() { echo -e "${YELLOW}WARN${NC}: $1"; }
fail() { echo -e "${RED}FAIL${NC}: $1"; }
info() { echo -e "${BLUE}INFO${NC}: $1"; }

PASS_COUNT=0
WARN_COUNT=0
FAIL_COUNT=0

check_pass() { ((PASS_COUNT++)); pass "$1"; }
check_warn() { ((WARN_COUNT++)); warn "$1"; }
check_fail() { ((FAIL_COUNT++)); fail "$1"; }

echo "=============================================="
echo "  Kiro Agent Integration - Compatibility Test"
echo "=============================================="
echo ""

# --- Static Checks ---
info "Phase 1: Static checks"

# Check kiro-cli binary
if command -v kiro-cli &>/dev/null; then
    KIRO_BIN=$(command -v kiro-cli)
    check_pass "kiro-cli found: $KIRO_BIN"
else
    check_fail "kiro-cli not found in PATH"
    echo "Install kiro-cli and try again."
    exit 1
fi

# Check version
KIRO_VERSION=$(kiro-cli --version 2>&1 || true)
if [[ -n "$KIRO_VERSION" ]]; then
    check_pass "kiro-cli version: $KIRO_VERSION"
else
    check_warn "Could not determine kiro-cli version"
fi

# Check help output for hook-related flags
KIRO_HELP=$(kiro-cli --help 2>&1 || true)
if echo "$KIRO_HELP" | grep -qi "agent"; then
    check_pass "kiro-cli help mentions 'agent'"
else
    check_warn "kiro-cli help does not mention 'agent'"
fi

if echo "$KIRO_HELP" | grep -qi "hook"; then
    check_pass "kiro-cli help mentions 'hook'"
else
    check_warn "kiro-cli help does not mention 'hook' (hooks may be agent-config-only)"
fi

# Check config directories
KIRO_CONFIG_DIR="$HOME/.kiro"
if [[ -d "$KIRO_CONFIG_DIR" ]]; then
    check_pass "Kiro config dir exists: $KIRO_CONFIG_DIR"
else
    check_warn "Kiro config dir not found: $KIRO_CONFIG_DIR"
fi

if [[ -d "$KIRO_CONFIG_DIR/agents" ]]; then
    check_pass "Kiro agents dir exists: $KIRO_CONFIG_DIR/agents"
else
    check_warn "Kiro agents dir not found: $KIRO_CONFIG_DIR/agents (will be created)"
fi

echo ""

# --- Manual Live Capture ---
if [[ "$MANUAL_LIVE" != "true" ]]; then
    info "Skipping live capture (use --manual-live to enable)"
    echo ""
    echo "=== Summary ==="
    echo -e "  ${GREEN}PASS${NC}: $PASS_COUNT"
    echo -e "  ${YELLOW}WARN${NC}: $WARN_COUNT"
    echo -e "  ${RED}FAIL${NC}: $FAIL_COUNT"
    exit 0
fi

info "Phase 2: Manual live capture mode"
echo ""

# Create capture directory
CAPTURE_DIR=".entire/tmp/probe-kiro-$(date +%s)"
mkdir -p "$CAPTURE_DIR/captures"
info "Capture directory: $CAPTURE_DIR"

# Create the hook capture script
CAPTURE_SCRIPT="$CAPTURE_DIR/capture-hook.sh"
cat > "$CAPTURE_SCRIPT" << 'HOOK_SCRIPT'
#!/bin/bash
# Capture hook payload from stdin
EVENT_NAME="${1:-unknown}"
CAPTURE_DIR="${2:-.}"
TIMESTAMP=$(date +%s%N)
OUTFILE="$CAPTURE_DIR/captures/${EVENT_NAME}-${TIMESTAMP}.json"
cat > "$OUTFILE"
echo "Captured: $OUTFILE" >&2
HOOK_SCRIPT
chmod +x "$CAPTURE_SCRIPT"

CAPTURE_SCRIPT_ABS="$(cd "$(dirname "$CAPTURE_SCRIPT")" && pwd)/$(basename "$CAPTURE_SCRIPT")"
CAPTURE_DIR_ABS="$(cd "$CAPTURE_DIR" && pwd)"

# Create probe agent config
AGENT_CONFIG_DIR="$KIRO_CONFIG_DIR/agents"
mkdir -p "$AGENT_CONFIG_DIR"
PROBE_CONFIG="$AGENT_CONFIG_DIR/entire-probe.json"

cat > "$PROBE_CONFIG" << EOF
{
  "name": "entire-probe",
  "description": "Entire CLI integration probe - captures hook payloads for analysis",
  "hooks": {
    "agentSpawn": [{"command": "$CAPTURE_SCRIPT_ABS agent-spawn $CAPTURE_DIR_ABS"}],
    "userPromptSubmit": [{"command": "$CAPTURE_SCRIPT_ABS user-prompt-submit $CAPTURE_DIR_ABS"}],
    "stop": [{"command": "$CAPTURE_SCRIPT_ABS stop $CAPTURE_DIR_ABS"}],
    "preToolUse": [{"command": "$CAPTURE_SCRIPT_ABS pre-tool-use $CAPTURE_DIR_ABS"}],
    "postToolUse": [{"command": "$CAPTURE_SCRIPT_ABS post-tool-use $CAPTURE_DIR_ABS"}]
  }
}
EOF

info "Created probe agent config: $PROBE_CONFIG"
echo ""
echo "=============================================="
echo "  Now run the following in another terminal:"
echo ""
echo "    kiro-cli chat --agent entire-probe"
echo ""
echo "  Then interact with Kiro (send a prompt,"
echo "  let it use tools, then stop the session)."
echo "  Press Enter here when done."
echo "=============================================="
echo ""

read -r -p "Press Enter to collect captures..."

echo ""
info "Phase 3: Capture collection"

# Collect and display captures
CAPTURE_COUNT=$(find "$CAPTURE_DIR/captures" -name "*.json" 2>/dev/null | wc -l | tr -d ' ')
if [[ "$CAPTURE_COUNT" -eq 0 ]]; then
    check_fail "No hook payloads captured"
    echo "Make sure you ran kiro-cli chat --agent entire-probe and interacted with it."
else
    check_pass "Captured $CAPTURE_COUNT hook payloads"
    echo ""

    for f in "$CAPTURE_DIR/captures"/*.json; do
        EVENT=$(basename "$f" | sed 's/-[0-9]*\.json$//')
        echo -e "${BLUE}--- $EVENT ---${NC}"
        if command -v jq &>/dev/null; then
            jq . "$f" 2>/dev/null || cat "$f"
        else
            cat "$f"
        fi
        echo ""

        # Check for key fields
        if command -v jq &>/dev/null; then
            HAS_SESSION_ID=$(jq -r 'has("session_id")' "$f" 2>/dev/null || echo "false")
            HAS_CONVERSATION_ID=$(jq -r 'has("conversation_id")' "$f" 2>/dev/null || echo "false")
            HAS_TRANSCRIPT_PATH=$(jq -r 'has("transcript_path")' "$f" 2>/dev/null || echo "false")
            HAS_HOOK_EVENT_NAME=$(jq -r 'has("hook_event_name")' "$f" 2>/dev/null || echo "false")
            HAS_CWD=$(jq -r 'has("cwd")' "$f" 2>/dev/null || echo "false")

            [[ "$HAS_SESSION_ID" == "true" ]] && info "  session_id: present"
            [[ "$HAS_CONVERSATION_ID" == "true" ]] && info "  conversation_id: present"
            [[ "$HAS_TRANSCRIPT_PATH" == "true" ]] && info "  transcript_path: present"
            [[ "$HAS_HOOK_EVENT_NAME" == "true" ]] && info "  hook_event_name: present"
            [[ "$HAS_CWD" == "true" ]] && info "  cwd: present"
        fi
        echo ""
    done
fi

# Cleanup probe config
rm -f "$PROBE_CONFIG"
info "Removed probe agent config: $PROBE_CONFIG"

echo ""
echo "=== Compatibility Report ==="
echo ""
echo "| Kiro Hook          | Entire EventType | Status   |"
echo "|-------------------|-----------------|----------|"

for EVENT in agent-spawn user-prompt-submit stop pre-tool-use post-tool-use; do
    COUNT=$(find "$CAPTURE_DIR/captures" -name "${EVENT}-*.json" 2>/dev/null | wc -l | tr -d ' ')
    case "$EVENT" in
        agent-spawn)        ENTIRE_EVENT="SessionStart";;
        user-prompt-submit) ENTIRE_EVENT="TurnStart";;
        stop)               ENTIRE_EVENT="TurnEnd";;
        pre-tool-use)       ENTIRE_EVENT="SubagentStart";;
        post-tool-use)      ENTIRE_EVENT="SubagentEnd";;
    esac
    if [[ "$COUNT" -gt 0 ]]; then
        echo "| $EVENT | $ENTIRE_EVENT | CAPTURED |"
    else
        echo "| $EVENT | $ENTIRE_EVENT | MISSING  |"
    fi
done

echo ""
echo "=== Summary ==="
echo -e "  ${GREEN}PASS${NC}: $PASS_COUNT"
echo -e "  ${YELLOW}WARN${NC}: $WARN_COUNT"
echo -e "  ${RED}FAIL${NC}: $FAIL_COUNT"
echo ""
info "Captures saved to: $CAPTURE_DIR/captures/"
echo "Review the JSON payloads to determine exact field names for the Kiro agent types."
