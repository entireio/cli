# Copilot CLI — Integration One-Pager

## Verdict: COMPATIBLE

Copilot CLI has a complete hook system with 9 hook types, JSONL transcripts, and session management. All required lifecycle events map cleanly.

## Static Checks

| Check | Result | Notes |
|-------|--------|-------|
| Binary present | PASS | `/opt/homebrew/bin/copilot` |
| Help available | PASS | Full `--help` with subcommands |
| Version info | PASS | Native subagent contract verified with `1.0.82` |
| Hook keywords | PASS | 9 hook types in `.github/hooks/*.json` |
| Session keywords | PASS | `--resume`, `--continue`, `--share` |
| Config directory | PASS | `~/.copilot/` (user-level), `.github/hooks/` (repo-level) |
| Documentation | PASS | docs.github.com/en/copilot/reference/hooks-configuration |

## Binary

- Name: `copilot`
- Verified version: `1.0.82`
- Install: `brew install copilot-cli` or `npm install -g @github/copilot`

## Hook Mechanism

- Config file: `.github/hooks/*.json` (all JSON files in directory are auto-discovered)
- Our file: `.github/hooks/entire.json` (dedicated file, avoids conflicts)
- Config format: JSON
- Hook registration: Array of hook entries per event name, each with `type: "command"` and `bash` field

### Hook Config Format

```json
{
  "version": 1,
  "hooks": {
    "hookName": [
      {
        "type": "command",
        "bash": "entire hooks copilot-cli hook-name"
      }
    ]
  }
}
```

Note: Uses `bash` key (not `command` like Claude Code/Cursor). Also supports `powershell` for Windows. Each entry can have optional `cwd`, `timeoutSec` (default 30), `env`, and `comment` fields.

### Hook Names and Event Mapping

| Native Hook Name | When It Fires | Stdin Payload Fields | Entire EventType |
|-----------------|---------------|---------------------|-----------------|
| `userPromptSubmitted` | User submits a prompt | `timestamp`, `cwd`, `sessionId`, `prompt` | `TurnStart` |
| `sessionStart` | Agent session begins/resumes | `timestamp`, `cwd`, `sessionId`, `source`, `initialPrompt` | `SessionStart` |
| `agentStop` | Agent finishes a turn | `timestamp`, `cwd`, `sessionId`, `transcriptPath`, `stopReason` | `TurnEnd` |
| `sessionEnd` | Session completed/terminated | `timestamp`, `cwd`, `sessionId`, `reason` | `SessionEnd` |
| `subagentStart` | Subagent starts | parent `sessionId`/`transcriptPath`, agent name | pass-through; payload has no child identity |
| `subagentStop` | Subagent finishes | parent `sessionId`/`transcriptPath`, child `agentId`, `agentType`, `agentName` | correlated `SubagentEnd` |
| `preToolUse` | Before tool execution | *(TBD — needs capture)* | *(pass-through)* |
| `postToolUse` | After tool execution | *(TBD — needs capture)* | *(pass-through)* |
| `errorOccurred` | Error during execution | *(TBD — needs capture)* | *(pass-through)* |

**Event ordering quirk:** `userPromptSubmitted` fires BEFORE `sessionStart` on the first prompt. This matches Claude Code's behavior and the framework's session phase state machine handles it correctly (TurnStart can arrive before SessionStart).

**Valid Entire EventTypes:** `SessionStart`, `TurnStart`, `TurnEnd`, `Compaction`, `SessionEnd`, `SubagentStart`, `SubagentEnd`

### Hook Input Payloads (Captured)

**userPromptSubmitted:**
```json
{"timestamp":1771480081360,"cwd":"/path/to/repo","sessionId":"b0ff98c0-8e01-4b73-bf92-9649b139931b","prompt":"hi"}
```

**sessionStart:**
```json
{"timestamp":1771480081383,"cwd":"/path/to/repo","sessionId":"b0ff98c0-8e01-4b73-bf92-9649b139931b","source":"new","initialPrompt":"hi"}
```

**agentStop:**
```json
{"timestamp":1771480085412,"cwd":"/path/to/repo","sessionId":"b0ff98c0-8e01-4b73-bf92-9649b139931b","transcriptPath":"/home/user/.copilot/session-state/b0ff98c0-8e01-4b73-bf92-9649b139931b/events.jsonl","stopReason":"end_turn"}
```

**sessionEnd:**
```json
{"timestamp":1771480085425,"cwd":"/path/to/repo","sessionId":"b0ff98c0-8e01-4b73-bf92-9649b139931b","reason":"complete"}
```

## Transcript

- Location: `~/.copilot/session-state/<sessionId>/events.jsonl`
- Format: JSONL (one JSON object per line)
- Session ID extraction: `sessionId` field in hook payloads (UUID format)
- Session ID format: UUID (`b0ff98c0-8e01-4b73-bf92-9649b139931b`)

### Transcript Entry Schema

Each line has: `type`, `data`, `id` (UUID), `timestamp` (ISO 8601), `parentId`

**Event types observed:**
- `session.start` — Session metadata (sessionId, version, producer, copilotVersion, context)
- `user.message` — User messages (`content`, `transformedContent`, `attachments`, `interactionId`)
- `assistant.turn_start` — Start of assistant turn (`turnId`, `interactionId`)
- `assistant.message` — Assistant response (`content`, `toolRequests[]`, `reasoningText`)
- `session.usage_record` — Per-API-call token usage (`usage.inputTokens`, `outputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `apiCallId`); newer releases only, see Token usage
- `session.shutdown` — Per-run aggregate `modelMetrics`, written after the last hook returns
- `tool.execution_complete` — Tool result (`toolCallId`, `parentToolCallId`, top-level `agentId`, and JSON-encoded `filePaths` under `toolTelemetry.properties` or `restrictedProperties`)
- `subagent.started` — Stable join between top-level child `agentId` and the parent task `toolCallId`
- `subagent.completed` — Completion totals, written after the synchronous `subagentStop` hook returns
- `assistant.turn_end` — End of assistant turn (`turnId`)

**Example entries:**
```jsonl
{"type":"session.start","data":{"sessionId":"4de47255-...","version":1,"producer":"copilot-agent","copilotVersion":"0.0.420","startTime":"2026-03-02T01:11:45.379Z","context":{"cwd":"/path","gitRoot":"/path","branch":"main","repository":"org/repo"}},"id":"c1c1...","timestamp":"2026-03-02T01:11:45.421Z","parentId":null}
{"type":"user.message","data":{"content":"exit","transformedContent":"...","attachments":[],"interactionId":"6cd0..."},"id":"20fa...","timestamp":"...","parentId":"00ac..."}
{"type":"assistant.turn_start","data":{"turnId":"0","interactionId":"6cd0..."},"id":"3b61...","timestamp":"...","parentId":"20fa..."}
{"type":"assistant.message","data":{"messageId":"a078...","content":"To exit...","toolRequests":[],"interactionId":"6cd0...","reasoningText":"..."},"id":"c892...","timestamp":"...","parentId":"aaeb..."}
{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"0ae4...","timestamp":"...","parentId":"4989..."}
```

### Tool Usage in Transcripts

The `assistant.message` entries have a `toolRequests` array with tool call IDs. After each tool executes, a `tool.execution_complete` event is emitted with a JSON-encoded `filePaths` value. Current releases place sensitive telemetry under `restrictedProperties`; older fixtures use `properties`, so the parser supports both.

**Token usage:** `TokenCalculator` reads three sources, and counts every API call from exactly one of them:

- `session.usage_record` — one event per model API call (main agent and subagents), written just before that call's `assistant.message` and joined to it by `apiCallId`. Carries `inputTokens`, `outputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `reasoningTokens`. First seen in local sessions at Copilot CLI 1.0.94; absent through 1.0.93.
- `assistant.message.outputTokens` — the per-call fallback for older releases (seen through 1.0.63). Output only, no input or cache counts. Skipped for any call a usage record covers. Releases between those two (seen at 1.0.83–1.0.93) write neither, so a checkpoint slice there has no per-call tokens at all and only the session total is recoverable.
- `session.shutdown.modelMetrics` — per-model aggregates (`usage` with the same four counts, plus `requests.count`), written when a Copilot process exits. A resumed session (`copilot --resume`) appends to the same `events.jsonl` and gets one shutdown per run, and the meaning changed between releases: on 1.0.3 each shutdown covered only its own run; on 1.0.97 each covers the whole session so far (cumulative). `requests.count` equals the number of `assistant.message` events covered on every release observed (0.0.422 through 1.0.97), so the parser classifies each shutdown by that count instead of by version.

Copilot's `inputTokens` is the **total** prompt size, including cache reads and cache writes: its own billing breakdown (`tokenDetails.input`) equals `inputTokens - cacheReadTokens - cacheWriteTokens` on every release that writes one (1.0.49 through 1.0.97). Entire reports fresh input, so both the usage records and the shutdown aggregate are normalized that way; `cacheWriteTokens` maps to `CacheCreationTokens`. `reasoningTokens` is a subset of `outputTokens` (`tokenDetails.output` equals `outputTokens`) and is not added.

Sliced reads (a checkpoint's window, `fromOffset > 0`) count only per-call sources inside the slice and never read `session.shutdown`, which is not checkpoint-scoped. Events before the slice are still parsed, only to learn which `apiCallId`s were already counted (and whether the current run has written a record), so a call whose usage is written twice across a checkpoint boundary is counted once and independently computed slices add up to the full read. Per call, the first source in file order wins. Full reads (`fromOffset == 0`) walk the runs between shutdowns. A run whose every call has a usage record is counted from the records (exact). Otherwise the shutdown's share of the run is used: the shutdown itself if its request count matches the run's messages, or the shutdown minus what earlier shutdowns covered if it matches every message since the start. A shutdown matching neither is ignored and its run counted per call, as is the live run after the last shutdown. On 1.0.94 sessions the per-turn usage-record sums equal the shutdown totals exactly, so checkpoints sum to the session total. **Timing constraint:** Copilot CLI writes `session.shutdown` AFTER all hooks (including `sessionEnd`) return — hooks are synchronous/blocking — so hook-time reads only ever see per-call sources. Condensation re-reads the full transcript and backfills `state.TokenUsage` with the full-read total (`sessionStateBackfillTokenUsage`), which is how the shutdown aggregate reaches session state on releases without usage records. Subagent calls are included in the main totals; `SubagentTokens` stays unset (see Subagents).

### Transcript Position

Position = line count (JSONL format). Use `agent.ChunkJSONL()` / `agent.ReassembleJSONL()` for chunking.

### TranscriptAnalyzer

The `TranscriptAnalyzer` interface is implemented for Copilot CLI, providing:
- `GetTranscriptPosition` — counts JSONL lines (lightweight, no JSON parsing)
- `ExtractModifiedFilesFromOffset` — collects `filePaths` from `tool.execution_complete` events after a given line offset
- `ExtractPrompts` — collects `content` from `user.message` events

## Session State Directory

```
~/.copilot/session-state/<sessionId>/
├── events.jsonl          # Transcript (JSONL)
├── workspace.yaml        # Workspace metadata (cwd, git info, summary)
├── checkpoints/          # Agent's own checkpoints
├── files/                # File snapshots
├── research/             # Research data
└── rewind-snapshots/     # Agent's rewind snapshots
```

## Config Preservation

- Hook config is in `.github/hooks/*.json` — each file is auto-discovered
- We create a **dedicated** `.github/hooks/entire.json` file, leaving other hook files untouched
- No need for read-modify-write of existing files
- If `entire.json` already exists, read-modify-write to preserve any user additions

## CLI Flags

- Non-interactive prompt (documented): `copilot -p "prompt" --allow-all-tools`
- Interactive with initial prompt: `copilot -i "prompt"`
- Interactive mode: `copilot`
- Resume most recent: `copilot --continue`
- Resume specific: `copilot --resume <sessionId>`
- Autopilot (no confirmations): `copilot --autopilot`
- Config directory: `--config-dir <dir>` (default: `~/.copilot`)
- Disable built-in MCP servers: `--disable-builtin-mcps` (skips GitHub MCP loading, ~28% fewer input tokens per call)
- Relevant env vars: `COPILOT_ALLOW_ALL` (equivalent to `--allow-all-tools`)

### Summary Text Generation Invocation

For `explain --generate` and auto-summarize, Entire invokes Copilot via stdin
rather than the documented `-p "prompt"` form:

```
copilot --deny-tool shell --deny-tool write --deny-tool url \
  --no-ask-user --no-custom-instructions --disable-builtin-mcps \
  --available-tools=entire_text_generation_uses_no_tools -s --disallow-temp-dir   # prompt piped to stdin
```

The prompt carries untrusted transcript content, so the run has no tools at
all: the allowlist names a tool that does not exist (an empty
`--available-tools` leaves every tool available), and `-s` keeps Copilot's
"Disabled tools" notice off stdout. It runs from a fresh empty directory.
`generateTextArgs` in `generate.go` documents each flag;
`TestTextGeneration_LiveHasNoToolReach` (opt-in, `ENTIRE_TEST_REAL_AGENTS=1`)
checks the contract against the installed CLI.

This matches the pattern used by every other summary-capable agent in the
repo (Claude, Codex, Cursor), which all converge on one transport
through the shared `agent.RunIsolatedTextGeneratorCLI` helper. It also
sidesteps the OS `ARG_MAX` limit on long transcripts — and while Copilot's
`--help` does not explicitly document stdin input, Copilot's own error
output does (`"provide a prompt with -p or via standard in."`), and end-to-end
summary generation has been verified against the installed CLI.

If a future Copilot release changes this, the error surface is clear — the
generator helper returns either "CLI returned empty output" or a non-zero
exit with stderr. At that point reverting to `-p <prompt>` with a prompt-size
cap is the obvious fallback. See `cmd/entire/cli/agent/copilotcli/generate.go`
for the implementation.

## Presence Detection

- No repo-level `.copilot/` directory (unlike other agents)
- `DetectPresence` delegates to `AreHooksInstalled`, which reads `.github/hooks/entire.json` and checks if any hook entry has an Entire command prefix (`entire ` or `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go `)
- Simply having a `.github/hooks/` directory is NOT sufficient -- the directory must contain `entire.json` with Entire hook entries
- Alternative: check for `copilot` binary in PATH

## Protected Directories

- `.github/hooks` — the hook config directory; listed in `ProtectedDirs()`, so Entire filters it out of change tracking and checkpoints
- No agent-specific repo directory to protect

## Subagent Lifecycle

Copilot CLI 1.0.82 emits both native hooks for custom agents and the built-in
`general-purpose` agent. `subagentStart` names the parent session and agent type but
does not expose a child ID or task call ID; two concurrent same-type starts can be
identical, so Entire deliberately treats it as pass-through.

At true completion, `subagentStop` adds the child UUID as `agentId`. Before invoking
that hook, Copilot has already written `subagent.started` to the parent
`events.jsonl`; that event carries the same top-level child UUID and the stable parent
task `toolCallId`. Entire reads the parent transcript through `SessionStore` and joins
on the UUID. Child tool events carry the same UUID plus `parentToolCallId`, allowing
two concurrent children to receive disjoint file lists even when they share a type.
Copilot 1.0.63 omits the stop `agentId`; Entire falls back only when exactly one
not-yet-completed start matches the stop's agent name, so ambiguous concurrent
children still fail closed.

Current children also emit ordinary `userPromptSubmitted` and `agentStop` hooks
with the child UUID as `sessionId`, while `agentStop.transcriptPath` still names
the parent transcript. Entire uses that mismatch to retire any transient child
session state without scanning the parent transcript as the child. The older
`toolu_…` child-session shape remains ignored at hook parsing time.

Copilot does not create standalone child transcript files. Task metadata therefore
records transcript unavailability and never probes the generic Claude-style
`agent-<id>.jsonl` layout. Per-child token breakdown is not recorded: child calls are counted in the
session's main totals (their `session.usage_record` events, tagged
`interactionType: "conversation-subagent"`, are interleaved into the parent
transcript, and `session.shutdown.modelMetrics` includes them). Entire leaves
child token usage unset rather than misclassifying Copilot's total-only
`subagent.completed` value.

## Gaps & Limitations

- No `Compaction` event — Copilot CLI doesn't appear to have a context compaction hook
- No standalone child transcript; Copilot interleaves child events into the parent JSONL
- No per-child token breakdown: child calls count toward the session's main totals
- `subagentStart` cannot be correlated safely and is retained as an observed pass-through hook
- `transcriptPath` only available in `agentStop` hook — `userPromptSubmitted` and `sessionStart` don't include it, so we compute it from `~/.copilot/session-state/<sessionId>/events.jsonl`

## Captured Payloads

- Contract reverified on 2026-09-03 with Copilot CLI 1.0.82 using two
  concurrent same-type custom agents (one writing, one read-only) and one
  built-in `general-purpose` agent.
- Token format reverified on 2026-10-13 with Copilot CLI 1.0.94 and 1.0.97:
  `session.usage_record` per call, no `assistant.message.outputTokens`, and
  per-turn record sums equal to the `session.shutdown` totals. The scrubbed
  1.0.94 capture is `testdata/usage_record_session.jsonl`.
- Tests retain only synthetic structural fixtures; prompts, responses, and
  local capture paths are not committed.
