#!/usr/bin/env bash
# Probe OpenCode's native subagent (Task tool / child session) signals as a
# plugin observes them, and record what Entire does with them today.
#
# Everything runs in a throwaway git repository under $TMPDIR — never in the
# repository this script lives in. A probe plugin is planted next to Entire's
# own plugin and appends every plugin event / tool hook it sees to
# <work>/captures/events.jsonl. Entire itself is enabled in the same repo so
# the run also shows how the current integration reacts to a child session.
#
# Usage:
#   scripts/test-opencode-subagent-integration.sh --run-cmd            # automated: opencode run <prompt>
#   scripts/test-opencode-subagent-integration.sh --manual-live        # you drive opencode in the repo, press Enter when done
#   scripts/test-opencode-subagent-integration.sh --run-cmd --no-entire # raw OpenCode signals only
#   scripts/test-opencode-subagent-integration.sh --run-cmd --scenario concurrent   # two children in one turn
#   scripts/test-opencode-subagent-integration.sh --run-cmd --scenario readonly     # one read-only explore child
#
# Env:
#   OPENCODE_MODEL   model for `opencode run` (default anthropic/claude-haiku-4-5)
#   ENTIRE_BIN       entire binary to put first on PATH (default: whatever is on PATH)
#   PROBE_KEEP=1     keep the work directory after the run
set -euo pipefail

AGENT_NAME="OpenCode"
AGENT_SLUG="opencode"
AGENT_BIN="opencode"
MODEL="${OPENCODE_MODEL:-anthropic/claude-haiku-4-5}"
MODE=""
WITH_ENTIRE=1
SCENARIO="single"
NEXT_IS_SCENARIO=0
for arg in "$@"; do
  if [ "$NEXT_IS_SCENARIO" = 1 ]; then SCENARIO="$arg"; NEXT_IS_SCENARIO=0; continue; fi
  case "$arg" in
    --run-cmd) MODE="run" ;;
    --manual-live) MODE="manual" ;;
    --no-entire) WITH_ENTIRE=0 ;;
    --scenario) NEXT_IS_SCENARIO=1 ;;
    --scenario=*) SCENARIO="${arg#--scenario=}" ;;
    -h|--help) sed -n 2,20p "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done
[ -n "$MODE" ] || { echo "pass --run-cmd or --manual-live" >&2; exit 2; }

pass() { printf 'PASS  %-28s %s\n' "$1" "${2:-}"; }
warn() { printf 'WARN  %-28s %s\n' "$1" "${2:-}"; }
fail() { printf 'FAIL  %-28s %s\n' "$1" "${2:-}"; }

echo "== Phase 2: static checks ($AGENT_NAME)"
if command -v "$AGENT_BIN" >/dev/null; then pass "binary present" "$(command -v "$AGENT_BIN")"; else fail "binary present" "install opencode"; exit 1; fi
VERSION="$("$AGENT_BIN" --version 2>/dev/null | head -1 || true)"
[ -n "$VERSION" ] && pass "version" "$VERSION" || warn "version" "no --version output"
"$AGENT_BIN" run --help 2>&1 | grep -q -- '--agent' && pass "run --agent flag" || warn "run --agent flag" "absent"
"$AGENT_BIN" export --help 2>&1 | grep -q 'export session data' && pass "export subcommand" || fail "export subcommand" "absent"
"$AGENT_BIN" models 2>/dev/null | grep -qx "$MODEL" && pass "model available" "$MODEL" || warn "model available" "$MODEL not listed"
[ -f "$HOME/.local/share/opencode/opencode.db" ] && pass "sqlite store" "$HOME/.local/share/opencode/opencode.db" || warn "sqlite store" "not found (older storage layout?)"
if [ "$WITH_ENTIRE" = 1 ]; then
  if [ -n "${ENTIRE_BIN:-}" ]; then ENTIRE_BIN_DIR="$(dirname "$ENTIRE_BIN")"; export PATH="$ENTIRE_BIN_DIR:$PATH"; fi
  command -v entire >/dev/null && pass "entire binary" "$(command -v entire) ($(entire version 2>/dev/null | head -1))" || { fail "entire binary" "not on PATH; set ENTIRE_BIN"; exit 1; }
fi

echo
echo "== Phase 3: probe workspace"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/probe-${AGENT_SLUG}-subagent.XXXXXX")"
REPO="$WORK/repo"
CAPTURES="$WORK/captures"
mkdir -p "$REPO" "$CAPTURES"
echo "work dir: $WORK"

git -C "$REPO" init -q -b main
git -C "$REPO" config user.name probe
git -C "$REPO" config user.email probe@example.com
git -C "$REPO" config commit.gpgsign false
printf '# probe\n' > "$REPO/README.md"
git -C "$REPO" add README.md
git -C "$REPO" commit -q -m "init"

# Same shape the e2e harness writes: non-interactive runs auto-reject the
# external_directory ask otherwise.
cat > "$REPO/opencode.json" <<'JSON'
{"$schema": "https://opencode.ai/config.json", "permission": {"external_directory": "allow"}}
JSON

# Reuse the user's global @opencode-ai/plugin tree when it pins the running
# version; otherwise opencode installs one for this project (slow, silent).
mkdir -p "$REPO/.opencode/plugins"
GLOBAL_PKG="$HOME/.config/opencode/package.json"
if [ -f "$GLOBAL_PKG" ] && grep -q "\"$VERSION\"" "$GLOBAL_PKG" && [ -d "$HOME/.config/opencode/node_modules" ]; then
  cp "$GLOBAL_PKG" "$REPO/.opencode/package.json"
  [ -f "$HOME/.config/opencode/package-lock.json" ] && cp "$HOME/.config/opencode/package-lock.json" "$REPO/.opencode/"
  ln -s "$HOME/.config/opencode/node_modules" "$REPO/.opencode/node_modules"
  printf 'node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore\n' > "$REPO/.opencode/.gitignore"
  pass "plugin deps" "linked from ~/.config/opencode (pin $VERSION)"
else
  warn "plugin deps" "no matching global tree; opencode will install for itself"
fi

# The probe plugin: append every plugin-visible signal as one JSON line.
cat > "$REPO/.opencode/plugins/entire-probe.ts" <<'TS'
// Entire research probe — records every plugin signal OpenCode delivers.
import { appendFileSync } from "node:fs"
import type { Plugin } from "@opencode-ai/plugin"

export const EntireProbe: Plugin = async ({ directory }) => {
  const out = process.env.ENTIRE_PROBE_CAPTURES + "/events.jsonl"
  let seq = 0
  const record = (kind: string, payload: unknown) => {
    try {
      appendFileSync(out, JSON.stringify({ seq: seq++, t: Date.now(), kind, directory, payload }) + "\n")
    } catch {}
  }
  return {
    event: async ({ event }) => record("event", event),
    "chat.message": async (input, output) => record("chat.message", { input, message: output.message, parts: output.parts }),
    "tool.execute.before": async (input, output) => record("tool.execute.before", { input, output }),
    "tool.execute.after": async (input, output) => record("tool.execute.after", { input, output }),
  }
}
TS
export ENTIRE_PROBE_CAPTURES="$CAPTURES"
pass "probe plugin" "$REPO/.opencode/plugins/entire-probe.ts"

if [ "$WITH_ENTIRE" = 1 ]; then
  # Isolate Entire's per-user state from the developer's real config.
  export ENTIRE_CONFIG_DIR="$WORK/entire-config" XDG_CACHE_HOME="$WORK/entire-cache" ENTIRE_TOKEN_STORE=file ENTIRE_TOKEN_STORE_PATH="$WORK/entire-config/tokens.json"
  mkdir -p "$ENTIRE_CONFIG_DIR" "$XDG_CACHE_HOME"
  ( cd "$REPO" && entire enable --agent opencode --local >"$WORK/enable.log" 2>&1 ) && pass "entire enable" "--agent opencode --local" || { fail "entire enable" "see $WORK/enable.log"; cat "$WORK/enable.log"; exit 1; }
  git -C "$REPO" add -A && git -C "$REPO" commit -q -m "enable entire" || true
fi

echo
echo "== Phase 4: run"
case "$SCENARIO" in
  single)
    PROMPT="Use the general subagent (the task tool with subagent_type general) exactly once to create docs/red.md containing one paragraph about the colour red. Run it in the foreground and wait for it to finish; never run it in the background. Do not create or edit the file yourself, do not delegate again, do not commit, and do not ask for confirmation." ;;
  concurrent)
    PROMPT="Call the task tool twice in the same response, in parallel, both with subagent_type general: the first creates docs/red.md containing one paragraph about the colour red, the second creates docs/blue.md containing one paragraph about the colour blue. Wait for both to finish. Do not create or edit any file yourself, do not delegate again, do not commit, and do not ask for confirmation." ;;
  readonly)
    PROMPT="Use the explore subagent (the task tool with subagent_type explore) exactly once to report which files exist in this repository and what README.md says. Wait for it to finish and repeat its answer. Do not create or edit any file, do not commit, and do not ask for confirmation." ;;
  *) echo "unknown scenario: $SCENARIO (single|concurrent|readonly)" >&2; exit 2 ;;
esac
echo "scenario: $SCENARIO"
case "$MODE" in
  run)
    echo "opencode run --model $MODEL <prompt>  (in $REPO)"
    ( cd "$REPO" && env -u ENTIRE_TEST_TTY PWD="$REPO" "$AGENT_BIN" run --model "$MODEL" "$PROMPT" ) >"$WORK/run.stdout" 2>"$WORK/run.stderr" || warn "opencode run" "exit $? — see $WORK/run.stderr"
    ;;
  manual)
    echo "Open another terminal, then:  cd $REPO && opencode"
    echo "Drive a subagent (e.g. '@general create docs/red.md ...'), quit opencode, then press Enter here."
    read -r _
    ;;
esac

echo
echo "== Phase 5: captures"
if [ ! -s "$CAPTURES/events.jsonl" ]; then
  fail "captures" "no events recorded — was the plugin loaded? check $WORK/run.stderr"
else
  N=$(wc -l < "$CAPTURES/events.jsonl" | tr -d ' ')
  pass "captures" "$N signals in $CAPTURES/events.jsonl"
  echo "-- signal timeline (seq, kind, type, session, detail)"
  jq -r '
    def sid: (.payload.properties.sessionID // .payload.properties.info.sessionID // .payload.properties.info.id // .payload.input.sessionID // .payload.properties.part.sessionID // "-");
    def detail:
      if .kind == "event" then
        (if .payload.type == "session.created" or .payload.type == "session.updated" then "parentID=\(.payload.properties.info.parentID // "-") title=\(.payload.properties.info.title // "-" | .[0:50])"
         elif .payload.type == "message.updated" then "role=\(.payload.properties.info.role) agent=\(.payload.properties.info.agent // "-") msg=\(.payload.properties.info.id)"
         elif .payload.type == "message.part.updated" then "part=\(.payload.properties.part.type) tool=\(.payload.properties.part.tool // "-") status=\(.payload.properties.part.state.status // "-") callID=\(.payload.properties.part.callID // "-")"
         elif .payload.type == "session.status" then "status=\(.payload.properties.status.type // "-")"
         else "" end)
      elif .kind | startswith("tool.execute") then "tool=\(.payload.input.tool) callID=\(.payload.input.callID)"
      elif .kind == "chat.message" then "agent=\(.payload.input.agent // "-")"
      else "" end;
    "\(.seq)\t\(.kind)\t\(.payload.type // "-")\t\(sid)\t\(detail)"' "$CAPTURES/events.jsonl" | column -t -s $'\t' | cut -c1-200
  echo
  echo "-- child sessions seen (session.created with parentID)"
  jq -c 'select(.kind=="event" and .payload.type=="session.created" and .payload.properties.info.parentID != null) | .payload.properties.info | {id, parentID, title, agent, directory}' "$CAPTURES/events.jsonl"
  echo "-- task tool: execute.before / execute.after"
  jq -c 'select(.kind | startswith("tool.execute")) | select(.payload.input.tool=="task") | {seq, kind, input: .payload.input, output_title: .payload.output.title, output_metadata: .payload.output.metadata, output_text: (.payload.output.output // "" | .[0:200])}' "$CAPTURES/events.jsonl"
  echo "-- task part states on the parent (first and last update per callID)"
  jq -c 'select(.kind=="event" and .payload.type=="message.part.updated" and .payload.properties.part.tool=="task") | {seq, status: .payload.properties.part.state.status, callID: .payload.properties.part.callID, metadata: .payload.properties.part.state.metadata, time: .payload.properties.part.state.time}' "$CAPTURES/events.jsonl" | awk '!seen[$0]++' | head -20
fi

if [ "$WITH_ENTIRE" = 1 ]; then
  echo
  echo "== Entire's view of the run (current behaviour)"
  ( cd "$REPO" && entire session list 2>&1 | head -20 ) || true
  echo "-- .entire/tmp exports"
  for f in "$REPO"/.entire/tmp/*.json; do [ -e "$f" ] && ls -la "$f"; done 2>/dev/null || echo "(none)"
  echo "-- commit and inspect checkpoint"
  ( cd "$REPO" && git add -A && git commit -q -m "Add red.md via subagent" 2>&1 && sleep 2 && git log -1 --format='%B' | grep -i entire; entire checkpoint list 2>&1 | head -20 ) || true
  echo "-- entire log tail"
  tail -40 "$REPO"/.entire/logs/*.log 2>/dev/null | grep -i -E "subagent|child|session|opencode" | tail -25 || true
fi

echo
echo "== Verdict"
if [ -s "$CAPTURES/events.jsonl" ]; then
  CHILD=$(jq -c 'select(.kind=="event" and .payload.type=="session.created" and .payload.properties.info.parentID != null)' "$CAPTURES/events.jsonl" | wc -l | tr -d ' ')
  TASK_AFTER=$(jq -c 'select(.kind=="tool.execute.after" and .payload.input.tool=="task")' "$CAPTURES/events.jsonl" | wc -l | tr -d ' ')
  TASK_BEFORE=$(jq -c 'select(.kind=="tool.execute.before" and .payload.input.tool=="task")' "$CAPTURES/events.jsonl" | wc -l | tr -d ' ')
  [ "$CHILD" -gt 0 ] && pass "SubagentStart signal" "$CHILD child session.created carrying parentID" || fail "SubagentStart signal" "no child session.created seen"
  [ "$TASK_BEFORE" -gt 0 ] && pass "task launch hook" "$TASK_BEFORE tool.execute.before(task)" || warn "task launch hook" "none"
  [ "$TASK_AFTER" -gt 0 ] && pass "SubagentEnd signal" "$TASK_AFTER tool.execute.after(task) with child sessionId in metadata" || fail "SubagentEnd signal" "no tool.execute.after(task)"
  if [ "$CHILD" -gt 0 ] && [ "$TASK_AFTER" -gt 0 ]; then echo "COMPATIBLE"; else echo "PARTIAL"; fi
else
  echo "INCOMPATIBLE (no signals captured)"
fi

if [ "${PROBE_KEEP:-}" = 1 ]; then echo "kept: $WORK"; else echo "(set PROBE_KEEP=1 to keep $WORK)"; fi
