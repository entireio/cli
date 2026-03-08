# Cursor CLI (`agent`) — Integration One-Pager

## Verdict: COMPATIBLE

The `agent` binary supports hooks via `.cursor/hooks.json` and stores JSONL transcripts in a predictable location. The existing Cursor agent implementation in this package already handles both IDE and CLI modes. The CLI fires `sessionStart`, `sessionEnd`, `preToolUse`, and `postToolUse` hooks in headless (`-p`) mode. In interactive mode, `beforeSubmitPrompt` and `stop` also fire.

**Key difference from IDE:** In `-p` (headless/print) mode, `beforeSubmitPrompt` and `stop` hooks do **not** fire. Only `sessionStart`, `sessionEnd`, and tool-use hooks fire. This means E2E tests using `RunPrompt` (headless) will not get `TurnStart`/`TurnEnd` events — only `SessionStart`/`SessionEnd`. Interactive tmux-based tests get the full lifecycle.

## Static Checks

| Check | Result | Notes |
|-------|--------|-------|
| Binary present | PASS | `/Users/robin/.local/bin/agent` |
| Help available | PASS | Full CLI help with subcommands |
| Version info | PASS | `2026.02.13-41ac335` |
| Hook keywords | PASS | `session`, `resume`, `continue` in help |
| Session keywords | PASS | `--resume`, `--continue`, `ls` (list sessions) |
| Config directory | PASS | `~/.cursor/`, `.cursor/` (project-local) |
| Documentation | PASS | https://cursor.com/docs/agent/hooks, https://cursor.com/docs/cli/using |

## Binary

- Name: `agent`
- Version: `2026.02.13-41ac335`
- Install: `curl -fsSL https://cursor.com/install-agent | bash` (or via Cursor IDE: install shell integration)
- Also accessible as: `cursor agent` (when Cursor IDE is installed)

## Hook Mechanism

- Config file: `.cursor/hooks.json` (project-local) or `~/.cursor/hooks.json` (user-global)
- Config format: JSON
- Hook registration: Array of `{"command": "...", "matcher": "..."}` entries per hook type (matcher is optional, used for tool-use hooks)

### Hook Names and When They Fire

| Native Hook Name | When It Fires | Entire EventType | Fires in `-p` mode? |
|-----------------|---------------|-----------------|---------------------|
| `sessionStart` | New conversation created | `SessionStart` | Yes |
| `beforeSubmitPrompt` | After user presses send, before backend request | `TurnStart` | **No** |
| `stop` | Agent loop ends (one turn completes) | `TurnEnd` | **No** |
| `sessionEnd` | Conversation ends | `SessionEnd` | Yes |
| `preCompact` | Before context compaction | `Compaction` | Needs long context |
| `subagentStart` | Before spawning a subagent (Task tool) | `SubagentStart` | Yes (when subagent used) |
| `subagentStop` | Subagent completes | `SubagentEnd` | Yes (when subagent used) |
| `preToolUse` | Before any tool execution | *(not mapped — informational)* | Yes |
| `postToolUse` | After tool execution | *(not mapped — informational)* | Yes |

### Hook Input (stdin JSON)

All hooks share these common fields:

```json
{
  "conversation_id": "uuid",
  "generation_id": "uuid",
  "model": "gpt-5.2-codex-xhigh-fast",
  "hook_event_name": "sessionStart",
  "cursor_version": "2026.02.13-41ac335",
  "workspace_roots": ["/path/to/repo"],
  "user_email": "user@example.com",
  "transcript_path": null
}
```

**Important:** `transcript_path` is **always `null`** in CLI mode. The existing cursor agent handles this via `resolveTranscriptRef()` which computes the path dynamically from the repo root.

#### sessionStart additional fields

```json
{
  "session_id": "uuid",
  "is_background_agent": false
}
```

Note: IDE also sends `composer_mode: "agent"` — CLI omits this field.

#### sessionEnd additional fields

```json
{
  "session_id": "uuid",
  "reason": "completed",
  "duration_ms": 5505,
  "is_background_agent": false,
  "final_status": "completed"
}
```

#### beforeSubmitPrompt additional fields (interactive mode only)

```json
{
  "prompt": "user prompt text"
}
```

#### stop additional fields (interactive mode only)

```json
{
  "status": "completed",
  "loop_count": 0
}
```

#### subagentStart additional fields

```json
{
  "subagent_id": "uuid",
  "subagent_type": "generalPurpose",
  "subagent_model": "model-name",
  "task": "task description",
  "parent_conversation_id": "uuid",
  "tool_call_id": "id",
  "is_parallel_worker": false
}
```

#### subagentStop additional fields

```json
{
  "subagent_id": "uuid",
  "subagent_type": "generalPurpose",
  "status": "completed",
  "duration_ms": 5000,
  "summary": "result text",
  "parent_conversation_id": "uuid",
  "message_count": 10,
  "tool_call_count": 3,
  "modified_files": ["file.txt"],
  "loop_count": 1,
  "task": "task description",
  "description": "...",
  "agent_transcript_path": "/path/to/transcript"
}
```

#### preToolUse additional fields

```json
{
  "tool_name": "Write",
  "tool_input": {"file_path": "/path", "content": "..."},
  "tool_use_id": "call_xxx\nctc_xxx"
}
```

#### postToolUse additional fields

```json
{
  "tool_name": "Write",
  "tool_input": {"file_path": "/path", "content": "..."},
  "tool_output": "{\"success\":true}",
  "duration": 36.841,
  "tool_use_id": "call_xxx\nctc_xxx"
}
```

## Transcript

- Location: `~/.cursor/projects/<sanitized-repo-path>/agent-transcripts/<conversation-id>.jsonl`
  - CLI uses flat layout: `<dir>/<id>.jsonl`
  - IDE uses nested layout: `<dir>/<id>/<id>.jsonl`
  - The existing `ResolveSessionFile()` handles both
- Path sanitization: leading `/` stripped, all non-alphanumeric chars replaced with `-`
- Format: JSONL (one JSON object per line)
- Session ID extraction: `conversation_id` field from hook payload (same value as `session_id`)
- Example entries:

```jsonl
{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncreate a file\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Created the file."}]}}
```

- Note: Transcript does NOT contain tool_use blocks — file detection relies on git status
- Override for testing: set `ENTIRE_TEST_CURSOR_PROJECT_DIR` env var to override the transcript directory

## Config Preservation

- `.cursor/hooks.json`: Read-modify-write using `map[string]json.RawMessage` to preserve unknown fields
- `~/.cursor/cli-config.json`: User-level config — do not modify (contains auth, permissions, model settings)
- Keys to preserve: `version`, any unknown hook types, user's custom hooks

## CLI Flags

- Non-interactive prompt: `agent -p "prompt text" --force --trust --workspace <dir>`
  - `-p` / `--print`: Headless mode, prints response to stdout
  - `--force` / `--yolo`: Auto-approve all tool use
  - `--trust`: Trust workspace without prompting (headless only)
  - `--workspace <path>`: Set working directory
  - `--model <model>`: Model override (e.g., `sonnet-4`, `gpt-5`)
  - `--output-format <fmt>`: `text` (default), `json`, `stream-json`
- Interactive mode: `agent --force` (launches TUI)
  - Prompt pattern for TUI ready: TBD (needs interactive probe)
  - `--resume [chatId]`: Resume specific session
  - `--continue`: Resume most recent session
- Relevant env vars:
  - `CURSOR_API_KEY`: API key for authentication
  - `ENTIRE_TEST_CURSOR_PROJECT_DIR`: Override transcript directory (for testing)
  - `ENTIRE_TEST_TTY=0`: Disable TTY detection in Entire hooks

## Gaps & Limitations

1. **`beforeSubmitPrompt` and `stop` don't fire in `-p` mode**: This is the main limitation. In headless mode, Entire won't get TurnStart/TurnEnd events. Checkpoints can only be created via sessionStart/sessionEnd flow. E2E tests using `RunPrompt` won't trigger the normal TurnStart→TurnEnd checkpoint flow.
2. **`transcript_path` is always `null` in CLI mode**: Handled by existing `resolveTranscriptRef()` which computes the path dynamically.
3. **No `composer_mode` field in CLI**: IDE sends `"agent"`, CLI omits it. Not impactful.
4. **Transcript lacks tool_use blocks**: Modified file detection relies on git status (already handled).
5. **`tool_use_id` format**: Contains newline (`call_xxx\nctc_xxx`) — may need sanitization if used as identifiers.

## Captured Payloads

Probe run on 2026-03-02 using `agent -p` in a temp git repo.

Hooks captured in headless (`-p`) mode:
- `sessionStart` (1 capture)
- `sessionEnd` (1 capture)
- `preToolUse` (2 captures: Read, Write)
- `postToolUse` (1 capture: Write)

Hooks NOT captured in headless mode:
- `beforeSubmitPrompt` — does not fire in `-p` mode
- `stop` — does not fire in `-p` mode
- `preCompact` — requires long context (not triggered by short prompt)
- `subagentStart/Stop` — requires subagent usage

See `.entire/tmp/probe-cursor-cli-*/captures/` for raw JSON captures.