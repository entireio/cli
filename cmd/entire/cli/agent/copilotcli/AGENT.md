# Copilot CLI — Integration One-Pager

## Verdict: COMPATIBLE

Copilot CLI has a complete hook system with 8 hook types, JSONL transcripts, and session management. All required lifecycle events map cleanly.

## Static Checks

| Check | Result | Notes |
|-------|--------|-------|
| Binary present | PASS | `/opt/homebrew/bin/copilot` |
| Help available | PASS | Full `--help` with subcommands |
| Version info | PASS | `0.0.420` |
| Hook keywords | PASS | 8 hook types in `.github/hooks/*.json` |
| Session keywords | PASS | `--resume`, `--continue`, `--share` |
| Config directory | PASS | `~/.copilot/` (user-level), `.github/hooks/` (repo-level) |
| Documentation | PASS | docs.github.com/en/copilot/reference/hooks-configuration |

## Binary

- Name: `copilot`
- Version: `0.0.420` (GA as of 2026-02-25)
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

Note: Uses `bash` key (not `command` like Claude Code/Gemini). Also supports `powershell` for Windows. Each entry can have optional `cwd`, `timeoutSec` (default 30), `env`, and `comment` fields.

### Hook Names and Event Mapping

| Native Hook Name | When It Fires | Stdin Payload Fields | Entire EventType |
|-----------------|---------------|---------------------|-----------------|
| `userPromptSubmitted` | User submits a prompt | `timestamp`, `cwd`, `sessionId`, `prompt` | `TurnStart` |
| `sessionStart` | Agent session begins/resumes | `timestamp`, `cwd`, `sessionId`, `source`, `initialPrompt` | `SessionStart` |
| `agentStop` | Agent finishes a turn | `timestamp`, `cwd`, `sessionId`, `transcriptPath`, `stopReason` | `TurnEnd` |
| `sessionEnd` | Session completed/terminated | `timestamp`, `cwd`, `sessionId`, `reason` | `SessionEnd` |
| `subagentStop` | Subagent finishes | `timestamp`, `cwd`, `sessionId`, *(TBD — needs capture)* | `SubagentEnd` |
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
- `tool.execution_complete` — Tool result (`toolCallId`, `toolTelemetry.properties.filePaths` (JSON-encoded string array), `linesAdded`, `linesRemoved`)
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

The `assistant.message` entries have a `toolRequests` array with tool call IDs. After each tool executes, a `tool.execution_complete` event is emitted with `toolTelemetry.properties.filePaths`, where `filePaths` is a string containing a JSON array of file paths modified by that tool call. The `TranscriptAnalyzer` implementation parses this `filePaths` property to extract modified files for checkpoint metadata.

**Token usage:** Extracted via `TokenCalculator` from `session.shutdown` events, which contain aggregate `modelMetrics` with per-model `inputTokens`, `outputTokens`, `cacheReadTokens`, `cacheWriteTokens`, and `requests.count`. Mid-session checkpoints use per-message `outputTokens` fallback from `assistant.message` events. **Timing constraint:** Copilot CLI writes `session.shutdown` AFTER all hooks (including `sessionEnd`) return — hooks are synchronous/blocking. This means in-hook token extraction can only use the per-message fallback. The authoritative aggregate is captured at condensation time (commit/push), when the framework re-reads the full transcript and `session.shutdown` is present. Condensation also backfills `state.TokenUsage` so the session JSON reflects the authoritative totals after the first commit.

### Transcript Position

Position = line count (JSONL format). Use `agent.ChunkJSONL()` / `agent.ReassembleJSONL()` for chunking.

### TranscriptAnalyzer

The `TranscriptAnalyzer` interface is implemented for Copilot CLI, providing:
- `GetTranscriptPosition` — counts JSONL lines (lightweight, no JSON parsing)
- `ExtractModifiedFilesFromOffset` — collects `filePaths` from `tool.execution_complete` events after a given line offset
- `ExtractPrompts` — collects `content` from `user.message` events
- `ExtractSummary` — returns the `content` of the last `assistant.message` event

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
copilot --allow-all-tools --disable-builtin-mcps   # prompt piped to stdin
```

This matches the pattern used by every other summary-capable agent in the
repo (Claude, Codex, Gemini, Cursor), which all converge on one transport
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

- `.github` — contains hook configs (committed to repo, unlikely to be affected by rewind)
- No agent-specific repo directory to protect

## Subagent Lifecycle

Copilot CLI has **one-sided** subagent hooks: `subagentStop` fires but `subagentStart` does NOT
(tested by registering the hook — Copilot CLI ignores it). The transcript does contain
`subagent.started` / `subagent.completed` events with `toolCallId`, but these are not
surfaced as hooks.

The `preToolUse` / `postToolUse` hooks fire with `toolName: "task"` for subagent lifecycle,
which could be used as an alternative mechanism for `SubagentStart`/`SubagentEnd` in the future.

Because there is no `SubagentStart` hook, the framework cannot capture pre-task state (untracked
files snapshot). The `handleLifecycleSubagentEnd` dispatcher falls back to the session's
pre-prompt state to avoid spurious task checkpoints from pre-existing untracked files
(e.g., `.github/hooks/entire.json`).

## Gaps & Limitations

- No `Compaction` event — Copilot CLI doesn't appear to have a context compaction hook
- No `SubagentStart` hook — only `subagentStop` fires; framework falls back to pre-prompt state
- `subagentStop` payload fields beyond `timestamp`/`cwd`/`sessionId` not yet fully captured
- `preToolUse` / `postToolUse` payloads not yet leveraged (could provide SubagentStart equivalent)
- Tool request schema in transcripts needs verification with a real tool-using session
- `transcriptPath` only available in `agentStop` hook — `userPromptSubmitted` and `sessionStart` don't include it, so we compute it from `~/.copilot/session-state/<sessionId>/events.jsonl`

## Captured Payloads

- Hook capture logs: `tmp/test-copilot/logs/copilot-hooks.jsonl`
- Local transcript: `~/.copilot/session-state/4de47255-3d43-4938-b8fa-b6e49f1d0aca/events.jsonl`
