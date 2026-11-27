#!/usr/bin/env bash
# Test harness for entire-agent-hummin.
#
# Exercises every external agent protocol subcommand against a synthetic
# hummin session created in a temp dir, invoking the plugin exactly the way
# the Entire CLI does (cwd = repo root, ENTIRE_REPO_ROOT and
# ENTIRE_PROTOCOL_VERSION set, JSON on stdin, one JSON line on stdout).
#
# Phases:
#   A. Per-command tests against the synthetic session (PASS/FAIL each).
#   B. Read-only checks against a real hummin session store, when one exists
#      on this machine (PASS/SKIP each).
#   C. CLI interop: builds the entire CLI and runs
#      `entire hooks hummin <event>` through full external-agent discovery in
#      a throwaway repo (PASS/SKIP each). Settings files are hand-written; the
#      enable command is never run.
#
# Usage: ./test.sh [--keep]   (--keep preserves the temp dir for debugging)

set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PLUGIN="$SCRIPT_DIR/entire-agent-hummin"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
FIXTURE="${ENTIRE_HUMMIN_TEST_FIXTURE:-$REPO_ROOT/../entire-lab/fixture}"
FIXTURE="$(cd "$FIXTURE" 2>/dev/null && pwd || echo "")"

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

TMPDIR="${TMPDIR:-/tmp}"
TMPDIR="${TMPDIR%/}"   # a trailing slash would leak into every computed path
TMP="$(mktemp -d "$TMPDIR/entire-agent-hummin-test.XXXXXX")"
if [ "$KEEP" = "1" ]; then
  trap 'echo "temp dir kept: $TMP"' EXIT
else
  trap 'rm -rf "$TMP"' EXIT
fi

PASS=0
FAIL=0
SKIP=0
FAILED_NAMES=""

ok()   { PASS=$((PASS + 1)); printf 'PASS: %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); FAILED_NAMES="$FAILED_NAMES\n  - $1"; printf 'FAIL: %s\n' "$1"; [ $# -gt 1 ] && printf '      %s\n' "$2"; }
skipp(){ SKIP=$((SKIP + 1)); printf 'SKIP: %s\n' "$1"; }

# assert_eq <name> <expected> <actual>
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "expected [$2], got [$3]"; fi
}

# assert_contains <name> <needle> <haystack>
assert_contains() {
  case "$3" in
    *"$2"*) ok "$1" ;;
    *) bad "$1" "expected to contain [$2], got [$3]" ;;
  esac
}

# assert_ok <name> <exit-code-from-$?>
assert_ok() {
  if [ "$2" -eq 0 ]; then ok "$1"; else bad "$1" "exit code $2"; fi
}

# assert_fails <name> <exit-code-from-$?>
assert_fails() {
  if [ "$2" -ne 0 ]; then ok "$1"; else bad "$1" "expected non-zero exit, got 0"; fi
}

# assert_cond <name> <test args...> — e.g. assert_cond "desc" -f "$path"
assert_cond() {
  local name="$1"
  shift
  if test "$@"; then ok "$name"; else bad "$name" "condition failed: test $*"; fi
}

# jget <dot.path> — parse JSON on stdin, print the value at the dotted path
# (numbers as array indexes). Missing path exits 2.
jget() {
  node -e '
    const d = JSON.parse(require("fs").readFileSync(0, "utf8"));
    const v = process.argv[1].split(".").reduce((a, k) => {
      if (a == null) return a;
      if (Array.isArray(a) && k !== "length") return a[Number(k)];
      return a[k];
    }, d);
    if (v === undefined) process.exit(2);
    if (v !== null && typeof v === "object") process.stdout.write(JSON.stringify(v));
    else process.stdout.write(String(v));
  ' "$1"
}
NODE_BIN="$(command -v node)"

# inv <plugin-args...> on stdin; runs the plugin the way the CLI does:
# cwd = repo root, ENTIRE_REPO_ROOT + ENTIRE_PROTOCOL_VERSION set.
REPO="$TMP/repo"
inv() {
  (cd "$REPO" && PATH="$SHIM_DIR:$PATH" ENTIRE_REPO_ROOT="$REPO" ENTIRE_PROTOCOL_VERSION=1 \
    PI_CODING_AGENT_DIR="$PI_AGENT" \
    "$PLUGIN" "$@")
}

# ==========================================================================
# Setup: fake repo, fake hummin home, synthetic hummin session
# ==========================================================================

mkdir -p "$REPO"
git -C "$REPO" init -q 2>/dev/null
# --no-verify: a machine-level commit hook (Entire's) rejects test identities,
# and this commit is only there to make $REPO a real git repository.
git -C "$REPO" commit -q --allow-empty --no-verify -m init 2>/dev/null

PI_AGENT="$TMP/hummin-home/agent"
UUID="11111111-2222-4333-8444-555555555555"
SESS_TS="2026-11-17T10-00-00-000Z"
ENC="--$(printf %s "$REPO" | sed 's#^/##; s#[/\\:]#-#g')--"
SDIR="$PI_AGENT/sessions/$ENC"
mkdir -p "$SDIR"
FILE="$SDIR/${SESS_TS}_${UUID}.jsonl"

# Fake hummin binary for detect (present), the plugin itself under its
# discovered name (so the CLI can find it via PATH), and an empty dir for
# detect (absent).
SHIM_DIR="$TMP/shim-path"
mkdir -p "$SHIM_DIR"
printf '#!/bin/sh\nexit 0\n' > "$SHIM_DIR/hummin"
chmod +x "$SHIM_DIR/hummin"
ln -sf "$PLUGIN" "$SHIM_DIR/entire-agent-hummin"
EMPTY_PATH="$TMP/empty-path"
mkdir -p "$EMPTY_PATH"

cat > "$FILE" <<'EOF'
{"type":"session","version":3,"id":"11111111-2222-4333-8444-555555555555","timestamp":"2026-11-17T10:00:00.000Z","cwd":"__REPO__"}
{"type":"message","id":"sys00001","parentId":null,"timestamp":"2026-11-17T10:00:01.000Z","message":{"role":"system","content":"","timestamp":1765965601000}}
{"type":"model_change","id":"mc000001","parentId":"sys00001","timestamp":"2026-11-17T10:00:02.000Z","provider":"test","modelId":"test-model-1"}
{"type":"message","id":"m0000001","parentId":"mc000001","timestamp":"2026-11-17T10:00:03.000Z","message":{"role":"user","content":"first prompt","timestamp":1765965603000}}
{"type":"message","id":"m0000002","parentId":"m0000001","timestamp":"2026-11-17T10:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"doing work"},{"type":"toolCall","id":"tc1","name":"write","arguments":{"path":"src/new.ts"}}],"usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":5},"api":"test-api","provider":"test","model":"test-model-1","stopReason":"stop","timestamp":1765965604000}}
{"type":"message","id":"m0000003","parentId":"m0000002","timestamp":"2026-11-17T10:00:05.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"write","content":[{"type":"text","text":"ok"}],"isError":false,"timestamp":1765965605000}}
{"type":"message","id":"m0000004","parentId":"m0000003","timestamp":"2026-11-17T10:00:06.000Z","message":{"role":"user","content":"second prompt","timestamp":1765965606000}}
{"type":"message","id":"m0000005","parentId":"m0000004","timestamp":"2026-11-17T10:00:07.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc2","name":"edit","arguments":{"path":"src/existing.ts"}}],"usage":{"input":120,"output":60,"cacheRead":20,"cacheWrite":10},"api":"test-api","provider":"test","model":"test-model-1","stopReason":"stop","timestamp":1765965607000}}
{"type":"message","id":"m1000001","parentId":"m0000002","timestamp":"2026-11-17T10:00:08.000Z","message":{"role":"user","content":"abandoned prompt","timestamp":1765965608000}}
{"type":"message","id":"m1000002","parentId":"m1000001","timestamp":"2026-11-17T10:00:09.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc9","name":"write","arguments":{"path":"src/abandoned.ts"}}],"usage":{"input":999,"output":999,"cacheRead":999,"cacheWrite":999},"api":"test-api","provider":"test","model":"test-model-9","stopReason":"stop","timestamp":1765965609000}}
{"type":"message","id":"m0000006","parentId":"m0000005","timestamp":"2026-11-17T10:00:10.000Z","message":{"role":"toolResult","toolCallId":"tc2","toolName":"edit","content":[{"type":"text","text":"ok"}],"isError":false,"timestamp":1765965610000}}
{"type":"usage","id":"e0000001","parentId":"m0000006","timestamp":"2026-11-17T10:00:11.000Z","kind":"cache_warm","provider":"test","model":"test-model-1","usage":{"input":0,"output":0,"cacheRead":50000,"cacheWrite":0,"totalTokens":50000}}
{"type":"custom","id":"e0000002","parentId":"e0000001","timestamp":"2026-11-17T10:00:12.000Z","customType":"test-ext","data":{"count":42}}
{"type":"message","id":"m0000007","parentId":"e0000002","timestamp":"2026-11-17T10:00:13.000Z","message":{"role":"assistant","content":[{"type":"text","text":"all done"}],"usage":{"input":200,"output":80,"cacheRead":0,"cacheWrite":0},"api":"test-api","provider":"test","model":"test-model-2","stopReason":"stop","timestamp":1765965613000}}
EOF
sed -i '' "s#__REPO__#$REPO#" "$FILE" 2>/dev/null || sed -i "s#__REPO__#$REPO#" "$FILE"

TOTAL_LINES="$(grep -c '' "$FILE")"
A2_LINE="$(grep -n '"id":"m0000005"' "$FILE" | cut -d: -f1)"   # line of the second assistant message

# An older session file with the SAME id: resolve-session-file must prefer
# the lexicographically latest (newer timestamp).
OLD_FILE="$SDIR/2026-01-01T00-00-00-000Z_${UUID}.jsonl"
printf '{"type":"session","version":3,"id":"%s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"%s"}\n' "$UUID" "$REPO" > "$OLD_FILE"

# A separate small transcript for chunking tests.
CHUNK_FILE="$TMP/chunk.jsonl"
printf 'aaaaaaaa\nbbbbbbbb\ncccccccc' > "$CHUNK_FILE"   # 26 bytes, 3 lines, no trailing newline

echo "== entire-agent-hummin test harness =="
echo "plugin:      $PLUGIN"
echo "temp dir:    $TMP"
echo "session:     $FILE"
echo

# ==========================================================================
# Phase A: per-command tests
# ==========================================================================

echo "--- A. info ---"
OUT="$(inv info)"; RC=$?
assert_ok "info: exits 0" "$RC"
assert_eq "info: protocol_version" "1" "$(printf %s "$OUT" | jget protocol_version)"
assert_eq "info: name" "hummin" "$(printf %s "$OUT" | jget name)"
assert_eq "info: type" "Hummin" "$(printf %s "$OUT" | jget type)"
assert_eq "info: protected_dirs" '[".hummin"]' "$(printf %s "$OUT" | jget protected_dirs)"
assert_eq "info: hook_names count" "4" "$(printf %s "$OUT" | jget hook_names.length)"
assert_eq "info: capabilities.hooks" "true" "$(printf %s "$OUT" | jget capabilities.hooks)"
assert_eq "info: capabilities.transcript_analyzer" "true" "$(printf %s "$OUT" | jget capabilities.transcript_analyzer)"
assert_eq "info: capabilities.token_calculator" "true" "$(printf %s "$OUT" | jget capabilities.token_calculator)"
assert_eq "info: capabilities.compact_transcript" "false" "$(printf %s "$OUT" | jget capabilities.compact_transcript)"
assert_eq "info: caller_env_vars" '["PI_SESSION_ID"]' "$(printf %s "$OUT" | jget caller_env_vars)"
echo

echo "--- A. detect ---"
OUT="$(inv detect)"; RC=$?
assert_ok "detect (hummin on PATH): exits 0" "$RC"
assert_eq "detect (hummin on PATH): present" "true" "$(printf %s "$OUT" | jget present)"
# PATH carries no hummin here; the plugin is launched directly through node
# because the shebang itself would need PATH to find node.
OUT="$(cd "$REPO" && PATH="$EMPTY_PATH" ENTIRE_REPO_ROOT="$REPO" ENTIRE_PROTOCOL_VERSION=1 "$NODE_BIN" "$PLUGIN" detect)"
assert_eq "detect (hummin absent): present" "false" "$(printf %s "$OUT" | jget present)"
echo

echo "--- A. get-session-id ---"
OUT="$(printf '{"hook_type":"stop","session_id":"%s","session_ref":"%s"}' "$UUID" "$FILE" | inv get-session-id)"
assert_eq "get-session-id: from payload session_id" "$UUID" "$(printf %s "$OUT" | jget session_id)"
OUT="$(printf '{"hook_type":"session_start","session_ref":"%s"}' "$FILE" | inv get-session-id)"
assert_eq "get-session-id: from <timestamp>_<uuid>.jsonl filename" "$UUID" "$(printf %s "$OUT" | jget session_id)"
OUT="$(printf '{"hook_type":"session_start","raw_data":{"session_file":"%s"}}' "$FILE" | inv get-session-id)"
assert_eq "get-session-id: from raw_data.session_file" "$UUID" "$(printf %s "$OUT" | jget session_id)"
OUT="$(printf '{}' | inv get-session-id)"
assert_eq "get-session-id: empty input yields empty id" "" "$(printf %s "$OUT" | jget session_id)"
echo

echo "--- A. get-session-dir ---"
OUT="$(inv get-session-dir --repo-path "$REPO")"
assert_eq "get-session-dir: encoded per-repo dir" "$SDIR" "$(printf %s "$OUT" | jget session_dir)"
OUT="$(cd "$REPO" && env -u PI_CODING_AGENT_DIR HOME="$TMP/hummin-home" ENTIRE_REPO_ROOT="$REPO" ENTIRE_PROTOCOL_VERSION=1 \
  "$PLUGIN" get-session-dir --repo-path "$REPO")"
assert_eq "get-session-dir: default home (~/.hummin/agent)" \
  "$TMP/hummin-home/.hummin/agent/sessions/$ENC" "$(printf %s "$OUT" | jget session_dir)"
OUT="$(cd "$REPO" && PI_CODING_AGENT_DIR="$PI_AGENT" PI_CODING_AGENT_SESSION_DIR="$TMP/flat-store" \
  ENTIRE_REPO_ROOT="$REPO" ENTIRE_PROTOCOL_VERSION=1 "$PLUGIN" get-session-dir --repo-path "$REPO")"
assert_eq "get-session-dir: PI_CODING_AGENT_SESSION_DIR replaces store (flat)" \
  "$TMP/flat-store" "$(printf %s "$OUT" | jget session_dir)"
OUT="$(inv get-session-dir 2>/dev/null)"; RC=$?
assert_fails "get-session-dir: missing --repo-path fails" "$RC"
echo

echo "--- A. resolve-session-file ---"
OUT="$(inv resolve-session-file --session-dir "$SDIR" --session-id "$UUID")"
assert_eq "resolve-session-file: latest <timestamp>_<id>.jsonl wins" "$FILE" "$(printf %s "$OUT" | jget session_file)"
OUT="$(inv resolve-session-file --session-dir "$SDIR" --session-id "deadbeef-0000-0000-0000-000000000000")"
assert_eq "resolve-session-file: unknown id falls back to <id>.jsonl" \
  "$SDIR/deadbeef-0000-0000-0000-000000000000.jsonl" "$(printf %s "$OUT" | jget session_file)"
OUT="$(inv resolve-session-file --session-dir "$SDIR" --session-id "$FILE")"
assert_eq "resolve-session-file: absolute id passes through" "$FILE" "$(printf %s "$OUT" | jget session_file)"
echo

echo "--- A. read-session ---"
OUT="$(printf '{"hook_type":"agent_end","session_id":"%s","session_ref":"%s"}' "$UUID" "$FILE" | inv read-session)"
assert_ok "read-session: exits 0" $?
assert_eq "read-session: session_id from header" "$UUID" "$(printf %s "$OUT" | jget session_id)"
assert_eq "read-session: agent_name" "hummin" "$(printf %s "$OUT" | jget agent_name)"
assert_eq "read-session: repo_path from header cwd" "$REPO" "$(printf %s "$OUT" | jget repo_path)"
assert_eq "read-session: start_time from header" "2026-11-17T10:00:00.000Z" "$(printf %s "$OUT" | jget start_time)"
assert_eq "read-session: session_ref" "$FILE" "$(printf %s "$OUT" | jget session_ref)"
assert_eq "read-session: native_data is base64 of the file" "$(base64 < "$FILE" | tr -d '\n')" "$(printf %s "$OUT" | jget native_data)"
assert_eq "read-session: file lists empty (git status fallback)" "[]" "$(printf %s "$OUT" | jget modified_files)"

# Derived resolution: only a session id, ref resolved through get-session-dir.
OUT="$(printf '{"hook_type":"agent_end","session_id":"%s"}' "$UUID" | inv read-session)"
assert_eq "read-session: ref derived from session id + repo root" "$FILE" "$(printf %s "$OUT" | jget session_ref)"

printf '{"hook_type":"agent_end"}' | inv read-session >/dev/null 2>&1
assert_fails "read-session: no id and no ref fails" $?
echo

echo "--- A. write-session ---"
printf '%s' "$OUT" | inv write-session >/dev/null 2>&1   # round-trip of the derived read (ref == $FILE)
assert_ok "write-session: round-trip of read-session exits 0" $?
cp "$FILE" "$FILE.orig"
printf '%s' "$OUT" | inv write-session >/dev/null 2>&1
assert_ok "write-session: rewrites session file" $?
cmp -s "$FILE" "$FILE.orig"; assert_ok "write-session: bytes preserved (cmp)" $?

# Restore into a new path inside the store (the checkpoint-restore shape).
RESTORE="$SDIR/2026-11-17T11-00-00-000Z_${UUID}.jsonl"
printf '%s' "$OUT" | sed "s#$FILE#$RESTORE#" | inv write-session >/dev/null 2>&1
assert_ok "write-session: writes restored session file" $?
cmp -s "$FILE" "$RESTORE"; assert_ok "write-session: restored bytes identical" $?

printf '{"session_ref":"%s","native_data":"e30=","repo_path":"%s"}' "$TMP/outside.jsonl" "$REPO" | inv write-session >/dev/null 2>&1
assert_fails "write-session: ref outside session store fails" $?
printf '{"session_ref":"%s","native_data":""}' "$FILE" | inv write-session >/dev/null 2>&1
assert_fails "write-session: empty native_data fails" $?
printf '{"session_ref":"relative.jsonl","native_data":"e30="}' | inv write-session >/dev/null 2>&1
assert_fails "write-session: relative ref fails" $?
echo

echo "--- A. read-transcript ---"
inv read-transcript --session-ref "$FILE" > "$TMP/transcript.out"; RC=$?
assert_ok "read-transcript: exits 0" "$RC"
cmp -s "$TMP/transcript.out" "$FILE"; assert_ok "read-transcript: bytes identical (cmp)" $?
inv read-transcript --session-ref "$TMP/does-not-exist.jsonl" >/dev/null 2>&1
assert_fails "read-transcript: missing file fails" $?
echo

echo "--- A. chunk-transcript / reassemble-transcript ---"
OUT="$(inv chunk-transcript --max-size 17 < "$CHUNK_FILE")"; RC=$?
assert_ok "chunk-transcript: exits 0" "$RC"
assert_eq "chunk-transcript: 3 chunks at max-size 17" "3" "$(printf %s "$OUT" | jget chunks.length)"
printf '%s' "$OUT" | inv reassemble-transcript > "$TMP/chunk.out"; RC=$?
assert_ok "reassemble-transcript: exits 0" "$RC"
cmp -s "$TMP/chunk.out" "$CHUNK_FILE"; assert_ok "chunk+reassemble: round-trip byte-identical (cmp)" $?

# Trailing-newline file stays one chunk and survives the round trip.
printf 'x\ny\n' > "$TMP/chunk2.jsonl"
OUT="$(inv chunk-transcript --max-size 1048576 < "$TMP/chunk2.jsonl")"
assert_eq "chunk-transcript: small file is one chunk" "1" "$(printf %s "$OUT" | jget chunks.length)"
printf '%s' "$OUT" | inv reassemble-transcript | cmp -s - "$TMP/chunk2.jsonl"
assert_ok "chunk+reassemble: trailing newline preserved" $?

# Empty transcript chunks to an empty list.
OUT="$(printf '' | inv chunk-transcript --max-size 1000)"
assert_eq "chunk-transcript: empty input yields no chunks" "0" "$(printf %s "$OUT" | jget chunks.length)"

# A single line larger than max-size cannot be split.
printf '{"one":"%s"}' "$(head -c 200 /dev/zero | tr '\0' 'a')" | inv chunk-transcript --max-size 100 >/dev/null 2>&1
assert_fails "chunk-transcript: oversized single line fails" $?
inv chunk-transcript --max-size notanumber < "$CHUNK_FILE" >/dev/null 2>&1
assert_fails "chunk-transcript: invalid --max-size fails" $?
echo

echo "--- A. format-resume-command ---"
OUT="$(inv format-resume-command --session-id "$UUID")"
assert_eq "format-resume-command: hummin --session <id>" "hummin --session $UUID" "$(printf %s "$OUT" | jget command)"
OUT="$(inv format-resume-command --session-id "")"
assert_eq "format-resume-command: empty id falls back to --continue" "hummin --continue" "$(printf %s "$OUT" | jget command)"
echo

echo "--- A. get-transcript-position (transcript_analyzer) ---"
OUT="$(inv get-transcript-position --path "$FILE")"
assert_eq "get-transcript-position: line count" "$TOTAL_LINES" "$(printf %s "$OUT" | jget position)"
OUT="$(inv get-transcript-position --path "$TMP/missing.jsonl")"
assert_eq "get-transcript-position: missing file reports 0" "0" "$(printf %s "$OUT" | jget position)"
echo

echo "--- A. extract-modified-files (transcript_analyzer) ---"
OUT="$(inv extract-modified-files --path "$FILE" --offset 0)"
assert_eq "extract-modified-files: active-branch files" '["src/new.ts","src/existing.ts"]' "$(printf %s "$OUT" | jget files)"
assert_eq "extract-modified-files: current_position is line count" "$TOTAL_LINES" "$(printf %s "$OUT" | jget current_position)"
OUT="$(inv extract-modified-files --path "$FILE" --offset $((A2_LINE - 1)))"
assert_eq "extract-modified-files: offset scopes extraction" '["src/existing.ts"]' "$(printf %s "$OUT" | jget files)"
echo

echo "--- A. extract-prompts (transcript_analyzer) ---"
OUT="$(inv extract-prompts --session-ref "$FILE" --offset 0)"
assert_eq "extract-prompts: active-branch prompts" '["first prompt","second prompt"]' "$(printf %s "$OUT" | jget prompts)"
# Both prompts precede the second assistant message, so nothing remains after
# that offset.
OUT="$(inv extract-prompts --session-ref "$FILE" --offset "$((A2_LINE - 1))")"
assert_eq "extract-prompts: offset scopes extraction" '[]' "$(printf %s "$OUT" | jget prompts)"
echo

echo "--- A. calculate-tokens (token_calculator) ---"
OUT="$(inv calculate-tokens --offset 0 < "$FILE")"
assert_eq "calculate-tokens: input_tokens (active branch only)" "420" "$(printf %s "$OUT" | jget input_tokens)"
assert_eq "calculate-tokens: output_tokens" "190" "$(printf %s "$OUT" | jget output_tokens)"
assert_eq "calculate-tokens: cache_read_tokens" "30" "$(printf %s "$OUT" | jget cache_read_tokens)"
assert_eq "calculate-tokens: cache_creation_tokens" "15" "$(printf %s "$OUT" | jget cache_creation_tokens)"
assert_eq "calculate-tokens: api_call_count" "3" "$(printf %s "$OUT" | jget api_call_count)"
assert_eq "calculate-tokens: subagent_tokens null" "null" "$(printf %s "$OUT" | jget subagent_tokens)"
OUT="$(inv calculate-tokens --offset "$((A2_LINE - 1))" < "$FILE")"
assert_eq "calculate-tokens: offset scopes totals" "320" "$(printf %s "$OUT" | jget input_tokens)"
echo

echo "--- A. parse-hook (hooks) ---"
OUT="$(printf '{"type":"session_start","cwd":"%s","session_file":"%s"}' "$REPO" "$FILE" | inv parse-hook --hook session_start)"
assert_eq "parse-hook session_start: type SessionStart" "1" "$(printf %s "$OUT" | jget type)"
assert_eq "parse-hook session_start: session id from filename" "$UUID" "$(printf %s "$OUT" | jget session_id)"

OUT="$(printf '{"type":"before_agent_start","cwd":"%s","session_file":"%s","prompt":"Fix the login bug"}' "$REPO" "$FILE" | inv parse-hook --hook before_agent_start)"
assert_eq "parse-hook before_agent_start: type TurnStart" "2" "$(printf %s "$OUT" | jget type)"
assert_eq "parse-hook before_agent_start: prompt" "Fix the login bug" "$(printf %s "$OUT" | jget prompt)"
assert_eq "parse-hook before_agent_start: session_ref" "$FILE" "$(printf %s "$OUT" | jget session_ref)"

OUT="$(printf '{"type":"agent_end","cwd":"%s","session_file":"%s"}' "$REPO" "$FILE" | inv parse-hook --hook agent_end)"
assert_eq "parse-hook agent_end: type TurnEnd" "3" "$(printf %s "$OUT" | jget type)"
assert_eq "parse-hook agent_end: model backfilled from transcript" "test-model-2" "$(printf %s "$OUT" | jget model)"

OUT="$(printf '{"type":"session_shutdown"}' | inv parse-hook --hook session_shutdown)"
assert_eq "parse-hook session_shutdown: null (not a lifecycle event)" "null" "$OUT"

OUT="$(printf '{"type":"before_agent_start"}' | inv parse-hook --hook before_agent_start)"
assert_eq "parse-hook: sessionless payload yields null" "null" "$OUT"

OUT="$(printf '{"type":"session_start","session_id":"%s"}' "$UUID" | inv parse-hook --hook future_hook)"
assert_eq "parse-hook: unknown hook yields null" "null" "$OUT"
echo

echo "--- A. install-hooks / uninstall-hooks / are-hooks-installed (hooks) ---"
EXT_FILE="$REPO/.hummin/extensions/entire/index.ts"
OUT="$(inv install-hooks)"; RC=$?
assert_ok "install-hooks: exits 0" "$RC"
assert_eq "install-hooks: hooks_installed 1" "1" "$(printf %s "$OUT" | jget hooks_installed)"
assert_cond "install-hooks: extension file written" -f "$EXT_FILE"
assert_contains "install-hooks: extension has Entire marker" \
  "Auto-generated by \`entire enable --agent hummin\`" "$(cat "$EXT_FILE")"
assert_contains "install-hooks: extension forwards to entire hooks hummin" \
  "hooks hummin" "$(cat "$EXT_FILE")"
assert_contains "install-hooks: extension guards nested hummin processes" \
  "ENTIRE_PI_NESTED" "$(cat "$EXT_FILE")"

OUT="$(inv install-hooks)"
assert_eq "install-hooks: idempotent (hooks_installed 0)" "0" "$(printf %s "$OUT" | jget hooks_installed)"

OUT="$(inv are-hooks-installed)"
assert_eq "are-hooks-installed: true after install" "true" "$(printf %s "$OUT" | jget installed)"

printf '// my own extension\n' > "$EXT_FILE"
inv install-hooks >/dev/null 2>"$TMP/foreign.err"; RC=$?
assert_fails "install-hooks: refuses foreign file" "$RC"
assert_contains "install-hooks: refusal names --force" "--force" "$(cat "$TMP/foreign.err")"
OUT="$(inv install-hooks --force)"
assert_eq "install-hooks --force: overwrites foreign file" "1" "$(printf %s "$OUT" | jget hooks_installed)"

inv uninstall-hooks >/dev/null 2>&1
assert_ok "uninstall-hooks: exits 0" $?
assert_cond "uninstall-hooks: extension dir removed" ! -e "$REPO/.hummin/extensions/entire"
OUT="$(inv are-hooks-installed)"
assert_eq "are-hooks-installed: false after uninstall" "false" "$(printf %s "$OUT" | jget installed)"

# A symlinked .hummin/extensions must not be written through.
mkdir -p "$REPO/.hummin/extensions"
ln -s "$TMP" "$REPO/.hummin/extensions/entire"
inv install-hooks >/dev/null 2>&1; RC=$?
assert_fails "install-hooks: refuses symlinked extension dir" "$RC"
rm -rf "$REPO/.hummin"

# Without ENTIRE_REPO_ROOT the plugin falls back to the working directory.
OUT="$(cd "$REPO" && env -u ENTIRE_REPO_ROOT ENTIRE_PROTOCOL_VERSION=1 PI_CODING_AGENT_DIR="$PI_AGENT" "$PLUGIN" install-hooks)"
assert_eq "install-hooks: falls back to cwd for repo root" "1" "$(printf %s "$OUT" | jget hooks_installed)"
inv uninstall-hooks >/dev/null 2>&1
echo

# ==========================================================================
# Phase B: read-only checks against a real hummin session store
# ==========================================================================

echo "--- B. real hummin session store (fixture) ---"
if [ -n "$FIXTURE" ] && [ -d "$HOME/.hummin/agent/sessions" ]; then
  FIX_ENC="--$(printf %s "$FIXTURE" | sed 's#^/##; s#[/\\:]#-#g')--"
  FIX_DIR="$HOME/.hummin/agent/sessions/$FIX_ENC"
  FIX_UUID="01a1e98b-1a2b-75c3-8268-0de82657bac8"
  FIX_FILE=""
  for f in "$FIX_DIR"/*_"$FIX_UUID".jsonl; do
    [ -f "$f" ] || continue
    if [ -z "$FIX_FILE" ] || [ "$f" \> "$FIX_FILE" ]; then FIX_FILE="$f"; fi
  done
  if [ -n "${FIX_FILE:-}" ] && [ -f "$FIX_FILE" ]; then
    OUT="$(cd "$FIXTURE" && env -u PI_CODING_AGENT_DIR -u PI_CODING_AGENT_SESSION_DIR \
      ENTIRE_REPO_ROOT="$FIXTURE" ENTIRE_PROTOCOL_VERSION=1 "$PLUGIN" get-session-dir --repo-path "$FIXTURE")"
    assert_eq "fixture: get-session-dir matches real store layout" "$FIX_DIR" "$(printf %s "$OUT" | jget session_dir)"
    assert_cond "fixture: encoded dir really exists" -d "$FIX_DIR"

    OUT="$(cd "$FIXTURE" && env -u PI_CODING_AGENT_DIR -u PI_CODING_AGENT_SESSION_DIR \
      ENTIRE_REPO_ROOT="$FIXTURE" ENTIRE_PROTOCOL_VERSION=1 "$PLUGIN" resolve-session-file \
      --session-dir "$FIX_DIR" --session-id "$FIX_UUID")"
    assert_eq "fixture: resolve-session-file finds the real file" "$FIX_FILE" "$(printf %s "$OUT" | jget session_file)"

    OUT="$(printf '{"hook_type":"agent_end","session_id":"%s","session_ref":"%s"}' "$FIX_UUID" "$FIX_FILE" \
      | (cd "$FIXTURE" && env -u PI_CODING_AGENT_DIR ENTIRE_REPO_ROOT="$FIXTURE" ENTIRE_PROTOCOL_VERSION=1 "$PLUGIN" read-session))"
    assert_eq "fixture: read-session id" "$FIX_UUID" "$(printf %s "$OUT" | jget session_id)"
    assert_eq "fixture: read-session repo_path" "$FIXTURE" "$(printf %s "$OUT" | jget repo_path)"

    FIX_LINES="$(grep -c '' "$FIX_FILE")"
    OUT="$(cd "$FIXTURE" && env -u PI_CODING_AGENT_DIR ENTIRE_REPO_ROOT="$FIXTURE" ENTIRE_PROTOCOL_VERSION=1 \
      "$PLUGIN" get-transcript-position --path "$FIX_FILE")"
    assert_eq "fixture: get-transcript-position line count" "$FIX_LINES" "$(printf %s "$OUT" | jget position)"

    OUT="$(cd "$FIXTURE" && env -u PI_CODING_AGENT_DIR ENTIRE_REPO_ROOT="$FIXTURE" ENTIRE_PROTOCOL_VERSION=1 \
      "$PLUGIN" extract-prompts --session-ref "$FIX_FILE" --offset 0)"
    FIX_PROMPTS="$(printf %s "$OUT" | jget prompts.length)"
    if [ "$FIX_PROMPTS" -ge 1 ] 2>/dev/null; then ok "fixture: extract-prompts finds user prompts ($FIX_PROMPTS)"; else bad "fixture: extract-prompts finds user prompts" "got $FIX_PROMPTS"; fi

    OUT="$(cd "$FIXTURE" && env -u PI_CODING_AGENT_DIR ENTIRE_REPO_ROOT="$FIXTURE" ENTIRE_PROTOCOL_VERSION=1 \
      "$PLUGIN" calculate-tokens --offset 0 < "$FIX_FILE")"
    FIX_IN="$(printf %s "$OUT" | jget input_tokens)"
    if [ "$FIX_IN" -ge 0 ] 2>/dev/null; then ok "fixture: calculate-tokens parses real usage (input_tokens=$FIX_IN)"; else bad "fixture: calculate-tokens parses real usage" "non-numeric"; fi

    OUT="$(cd "$FIXTURE" && env -u PI_CODING_AGENT_DIR ENTIRE_REPO_ROOT="$FIXTURE" ENTIRE_PROTOCOL_VERSION=1 \
      "$PLUGIN" extract-modified-files --path "$FIX_FILE" --offset 0)"
    assert_ok "fixture: extract-modified-files parses real transcript" $?
  else
    skipp "fixture: no session file for $FIX_UUID found in $FIX_DIR"
  fi
else
  skipp "fixture: real hummin session store or fixture repo not found"
fi
echo

# ==========================================================================
# Phase C: CLI interop (builds the entire CLI; hand-written settings, the
# enable command is never run)
# ==========================================================================

echo "--- C. interop with the real entire CLI ---"
ENTIRE_BIN="$TMP/entire-bin/entire"
if command -v go >/dev/null 2>&1; then
  echo "building entire CLI from $REPO_ROOT (may take a moment)..."
  if (cd "$REPO_ROOT" && go build -o "$ENTIRE_BIN" ./cmd/entire) 2>"$TMP/gobuild.err"; then
    IREPO="$TMP/interop-repo"
    mkdir -p "$IREPO/.entire"
    (cd "$IREPO" && git init -q && git commit -q --allow-empty --no-verify -m init)
    printf '{\n  "enabled": true\n}\n' > "$IREPO/.entire/settings.json"
    printf '{\n  "external_agents": true\n}\n' > "$IREPO/.entire/settings.local.json"
    IHOME="$TMP/interop-home"
    mkdir -p "$IHOME"

    run_hook() {
      (cd "$IREPO" && PATH="$SHIM_DIR:$PATH" HOME="$IHOME" ENTIRE_TELEMETRY_OPTOUT=1 \
        "$ENTIRE_BIN" hooks hummin "$1")
    }

    printf '{"type":"session_start","cwd":"%s","session_file":"%s"}' "$IREPO" "$FILE" | run_hook session_start >/dev/null 2>"$TMP/hook1.err"; RC=$?
    assert_ok "interop: entire hooks hummin session_start exits 0" "$RC"

    printf '{"type":"before_agent_start","cwd":"%s","session_file":"%s","prompt":"interop test turn"}' "$IREPO" "$FILE" \
      | run_hook before_agent_start >/dev/null 2>"$TMP/hook2.err"; RC=$?
    assert_ok "interop: entire hooks hummin before_agent_start exits 0" "$RC"

    printf '{"type":"agent_end","cwd":"%s","session_file":"%s"}' "$IREPO" "$FILE" \
      | run_hook agent_end >"$TMP/hook3.out" 2>"$TMP/hook3.err"; RC=$?
    assert_ok "interop: entire hooks hummin agent_end exits 0" "$RC"
    if [ $RC -ne 0 ]; then
      sed 's/^/      stderr: /' "$TMP/hook3.err" | head -5
    fi

    # The dispatched turn must have recorded session state under
    # .entire/metadata (settings files written by setup are excluded by the
    # path filter).
    STATE_HITS="$(find "$IREPO/.entire/metadata" -type f 2>/dev/null | wc -l | tr -d ' ')"
    if [ "${STATE_HITS:-0}" -ge 1 ]; then
      ok "interop: hook dispatch recorded session state under .entire/metadata ($STATE_HITS files)"
    else
      bad "interop: hook dispatch recorded session state under .entire/metadata" "no metadata written; stderr: $(head -c 300 "$TMP/hook3.err")"
    fi
  else
    sed 's/^/      /' "$TMP/gobuild.err" | head -5
    skipp "interop: go build of the CLI failed"
  fi
else
  skipp "interop: go toolchain not available"
fi
echo

# ==========================================================================
# Summary
# ==========================================================================

echo "== summary =="
echo "PASS: $PASS  FAIL: $FAIL  SKIP: $SKIP"
if [ "$FAIL" -gt 0 ]; then
  printf 'failed:%b\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
