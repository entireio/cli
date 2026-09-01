#!/usr/bin/env bash
set -euo pipefail

# Probe script for Grok Build (xAI) agent integration.
#
# Grok-specific notes driving this script's shape:
#   1. Project hooks under .grok/hooks/*.json are SILENTLY SKIPPED until the
#      project is trusted. Trust with --trust or the /hooks-trust command.
#   2. Grok also natively loads .claude/settings.json and .cursor/hooks.json
#      (global AND project scope). In a repo where Entire already installed
#      Claude Code hooks, Grok will fire `entire hooks claude-code ...`.
#      This script detects that collision explicitly -- it is the single most
#      important thing to know before implementing the agent.
#   3. `Stop` does NOT fire on an interrupted/failed turn; StopCancelled and
#      StopFailure fire instead. All three are captured here.

AGENT_NAME="Grok Build"
AGENT_SLUG="grok"
AGENT_BIN="${GROK_BIN:-grok}"
GROK_HOME="${GROK_HOME:-$HOME/.grok}"

PROBE_DIR=".entire/tmp/probe-${AGENT_SLUG}-$(date +%s)"
PROBE_ABS="$(pwd)/$PROBE_DIR"

HOOKS_DIR=".grok/hooks"
HOOKS_FILE="$HOOKS_DIR/entire-probe.json"

RUN_CMD=""
MANUAL_LIVE=0
KEEP_CONFIG=0

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}PASS${NC} $1"; }
warn() { echo -e "${YELLOW}WARN${NC} $1"; }
fail() { echo -e "${RED}FAIL${NC} $1"; }

usage() {
    cat <<USAGE
Usage: $0 [--run-cmd '<cmd>'] [--manual-live] [--keep-config]

  --run-cmd '<cmd>'  Automated: run <cmd>, then collect captures.
                     e.g. --run-cmd 'grok --trust -p "create hello.txt" --permission-mode bypassPermissions'
  --manual-live      Interactive: wire hooks, wait for you to drive grok, press Enter.
  --keep-config      Do not remove the probe hook config on exit.
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --run-cmd)     RUN_CMD="$2"; shift 2 ;;
        --manual-live) MANUAL_LIVE=1; shift ;;
        --keep-config) KEEP_CONFIG=1; shift ;;
        -h|--help)     usage; exit 0 ;;
        *) fail "Unknown argument: $1"; usage; exit 1 ;;
    esac
done

# --- Phase 1: Static Checks ---
echo "=== Static Checks ($AGENT_NAME) ==="

if command -v "$AGENT_BIN" &>/dev/null; then
    pass "Binary present: $(command -v "$AGENT_BIN")"
else
    fail "Binary not found: $AGENT_BIN"
    echo "     Install: curl -fsSL https://x.ai/cli/install.sh | bash"
    exit 1
fi

VERSION=$("$AGENT_BIN" --version 2>&1 || true)
[ -n "$VERSION" ] && pass "Version: $VERSION" || warn "Version info not available"

HELP=$("$AGENT_BIN" --help 2>&1 || true)
[ -n "$HELP" ] && pass "Help output available" || warn "No help output"

echo "$HELP" | grep -qiE "hook|lifecycle|callback|event|trigger|plugin|extension" \
    && pass "Hook keywords found in help" \
    || warn "No hook keywords in help (hooks live in .grok/hooks/*.json, not flags)"

echo "$HELP" | grep -qiE "session|resume|continue|history|transcript" \
    && pass "Session keywords found in help" \
    || warn "No session keywords in help"

[ -d "$GROK_HOME" ] && pass "Config directory: $GROK_HOME" \
    || warn "Config directory not found: $GROK_HOME (created on first run)"

[ -f "$GROK_HOME/config.toml" ] && pass "Config file: $GROK_HOME/config.toml" \
    || warn "No config.toml yet at $GROK_HOME/config.toml"

# --- Phase 1b: Claude/Cursor hook collision check (Grok-specific) ---
echo ""
echo "=== Foreign Hook Config Collision ==="
echo "Grok natively loads Claude Code and Cursor hook configs. Any Entire hooks"
echo "already installed there will fire under a Grok session and mislabel it."

COLLISION=0
for f in .claude/settings.json .claude/settings.local.json .cursor/hooks.json \
         "$HOME/.claude/settings.json" "$HOME/.claude/settings.local.json" "$HOME/.cursor/hooks.json"; do
    if [ -f "$f" ] && grep -q "entire hooks" "$f" 2>/dev/null; then
        fail "COLLISION: $f contains Entire hooks that Grok will also execute"
        grep -o 'entire hooks [a-z-]* [a-z-]*' "$f" 2>/dev/null | sort -u | sed 's/^/       /'
        COLLISION=1
    fi
done
[ "$COLLISION" -eq 0 ] && pass "No foreign Entire hook configs Grok would pick up"

# --- Phase 2: Hook Wiring ---
echo ""
echo "=== Hook Wiring ==="

mkdir -p "$PROBE_DIR/captures"
mkdir -p "$HOOKS_DIR"

if [ -f "$HOOKS_FILE" ]; then
    cp "$HOOKS_FILE" "$PROBE_DIR/entire-probe.json.bak"
    warn "Existing $HOOKS_FILE backed up"
fi

# Capture script: dump stdin payload, keyed by event name + timestamp.
# Must exit 0 fast -- Stop and SubagentStop are BLOCKING gates.
cat > "$PROBE_DIR/capture.sh" <<'CAPTURE_EOF'
#!/usr/bin/env bash
CAPTURE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/captures"
mkdir -p "$CAPTURE_DIR"
EVENT="${GROK_HOOK_EVENT:-unknown}"
TS="$(date +%s%N)"
cat > "$CAPTURE_DIR/${EVENT}-${TS}.json"
{
  echo "event=$EVENT"
  echo "GROK_HOOK_EVENT=${GROK_HOOK_EVENT:-}"
  echo "GROK_HOOK_NAME=${GROK_HOOK_NAME:-}"
  echo "GROK_SESSION_ID=${GROK_SESSION_ID:-}"
  echo "GROK_WORKSPACE_ROOT=${GROK_WORKSPACE_ROOT:-}"
  echo "CLAUDE_PROJECT_DIR=${CLAUDE_PROJECT_DIR:-}"
} > "$CAPTURE_DIR/${EVENT}-${TS}.env"
exit 0
CAPTURE_EOF
chmod +x "$PROBE_DIR/capture.sh"

# One matcher-less group per event. PostToolUse is additionally probed with the
# subagent matcher so we can see the launch-stub shape vs SubagentStop.
python3 - "$HOOKS_FILE" "$PROBE_ABS/capture.sh" <<'PY_EOF'
import json, sys
out_path, cmd = sys.argv[1], sys.argv[2]
events = [
    "SessionStart", "SessionEnd",
    "UserPromptSubmit", "Stop", "StopFailure", "StopCancelled",
    "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionDenied",
    "Notification", "SubagentStart", "SubagentStop",
    "PreCompact", "PostCompact",
]
hooks = {e: [{"matcher": "", "hooks": [{"type": "command", "command": cmd, "timeout": 10}]}]
         for e in events}
json.dump({"hooks": hooks}, open(out_path, "w"), indent=2)
PY_EOF

pass "Wrote probe hook config: $HOOKS_FILE ($(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["hooks"]))' "$HOOKS_FILE") events)"
warn "Project hooks require trust. Use --trust on launch, or run /hooks-trust in the TUI."

cleanup() {
    if [ "$KEEP_CONFIG" -eq 1 ]; then
        warn "Keeping $HOOKS_FILE (--keep-config)"
        return
    fi
    if [ -f "$PROBE_DIR/entire-probe.json.bak" ]; then
        mv "$PROBE_DIR/entire-probe.json.bak" "$HOOKS_FILE"
        pass "Restored original $HOOKS_FILE"
    else
        rm -f "$HOOKS_FILE"
        rmdir "$HOOKS_DIR" 2>/dev/null || true
        rmdir .grok 2>/dev/null || true
        pass "Removed probe hook config"
    fi
}
trap cleanup EXIT

# --- Phase 3: Run ---
echo ""
echo "=== Run ==="
SESSIONS_BEFORE=$(find "$GROK_HOME/sessions" -name 'updates.jsonl' 2>/dev/null | wc -l | tr -d ' ')

if [ -n "$RUN_CMD" ]; then
    echo "Running: $RUN_CMD"
    set +e; eval "$RUN_CMD"; RUN_RC=$?; set -e
    echo "Exit code: $RUN_RC"
elif [ "$MANUAL_LIVE" -eq 1 ]; then
    echo "Hooks are wired. In another terminal, from this directory, run:"
    echo "    $AGENT_BIN --trust"
    echo "Submit a prompt that edits a file, let it finish, then exit the session."
    read -r -p "Press Enter when done... " _
else
    warn "No run mode selected (--run-cmd or --manual-live); captures will be empty"
fi

# --- Phase 4: Collect ---
echo ""
echo "=== Captured Payloads ==="
COUNT=$(find "$PROBE_DIR/captures" -name '*.json' 2>/dev/null | wc -l | tr -d ' ')
if [ "$COUNT" -eq 0 ]; then
    fail "No hook payloads captured"
    echo "     Most likely cause: project not trusted (run with --trust)."
else
    pass "$COUNT payload(s) captured in $PROBE_DIR/captures/"
    for f in "$PROBE_DIR"/captures/*.json; do
        echo ""
        echo "--- $(basename "$f") ---"
        python3 -m json.tool "$f" 2>/dev/null || cat "$f"
    done
fi

echo ""
echo "=== Session Transcript ==="
SESSIONS_AFTER=$(find "$GROK_HOME/sessions" -name 'updates.jsonl' 2>/dev/null | wc -l | tr -d ' ')
echo "updates.jsonl count before=$SESSIONS_BEFORE after=$SESSIONS_AFTER"
NEWEST=$(find "$GROK_HOME/sessions" -name 'updates.jsonl' -print0 2>/dev/null \
    | xargs -0 ls -t 2>/dev/null | head -1)
if [ -n "$NEWEST" ]; then
    pass "Newest transcript: $NEWEST"
    echo "Session dir contents:"
    ls -la "$(dirname "$NEWEST")" | sed 's/^/    /'
    echo "Encoded-cwd segment (GetSessionDir must reproduce this):"
    echo "    $(basename "$(dirname "$(dirname "$NEWEST")")")"
    echo "Distinct sessionUpdate types:"
    python3 -c '
import json,sys,collections
c=collections.Counter()
for line in open(sys.argv[1]):
    line=line.strip()
    if not line: continue
    try: c[json.loads(line).get("sessionUpdate","<none>")]+=1
    except Exception: c["<unparseable>"]+=1
for k,v in c.most_common(): print(f"    {k}: {v}")' "$NEWEST"
    cp "$NEWEST" "$PROBE_DIR/updates.jsonl.sample"
    pass "Copied transcript sample to $PROBE_DIR/updates.jsonl.sample"
else
    warn "No updates.jsonl found under $GROK_HOME/sessions"
fi

# --- Phase 5: Verdict ---
echo ""
echo "=== Verdict ==="
seen() { ls "$PROBE_DIR/captures/" 2>/dev/null | grep -qi "^$1-"; }
check() {
    if seen "$1"; then pass "$2 <- $1"; else warn "$2 <- $1 (not observed)"; fi
}
check "session_start"      "SessionStart"
check "user_prompt_submit" "TurnStart"
check "stop"               "TurnEnd"
check "stop_cancelled"     "TurnEnd (interrupted)"
check "stop_failure"       "TurnEnd (errored)"
check "session_end"        "SessionEnd"
check "pre_compact"        "Compaction"
check "subagent_start"     "SubagentStart"
check "subagent_stop"      "SubagentEnd (Final=true)"
check "post_tool_use"      "ToolUse / SubagentEnd launch stub"

echo ""
if [ "$COUNT" -gt 0 ] && [ -n "$NEWEST" ]; then
    if [ "$COLLISION" -eq 1 ]; then
        warn "PARTIAL - hooks and transcript work, but foreign Entire hook configs collide"
    else
        pass "COMPATIBLE - hooks fire and a native JSONL transcript exists"
    fi
else
    fail "INCOMPLETE - rerun with --manual-live --trust and inspect $PROBE_DIR"
fi
echo "Artifacts: $PROBE_DIR"
