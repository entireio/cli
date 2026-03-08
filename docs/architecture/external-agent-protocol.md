# External Agent Plugin Protocol

## Overview

The Entire CLI supports external agent plugins — standalone binaries that implement the Agent interface via a subcommand-based protocol over stdin/stdout. This allows third-party agents to integrate with the CLI without modifying the main repository.

## Discovery

The CLI discovers external agents by scanning `$PATH` for executables matching the pattern `entire-agent-<name>`. For example, `entire-agent-cursor` would register as the "cursor" agent.

- Binaries whose `<name>` conflicts with an already-registered built-in agent are skipped.
- Discovery runs once during CLI initialization (before building the hooks command tree).
- The binary must be executable and respond to the `info` subcommand.

## Environment

Every subcommand invocation sets:

| Variable | Description |
|---|---|
| `ENTIRE_REPO_ROOT` | Absolute path to the git repository root |
| `ENTIRE_PROTOCOL_VERSION` | Protocol version (`1`) |

The working directory is set to the repository root.

## Communication Model

- **Subcommand-based:** Each Agent interface method maps to a CLI subcommand.
- **JSON over stdin/stdout:** Structured data uses JSON. Transcripts use raw bytes.
- **Stateless:** Each invocation is independent — no persistent connection.
- **Exit codes:** `0` = success, non-zero = error. Error messages go to stderr.

## Subcommands

### Always Required

#### `info`

Returns agent metadata and declared capabilities.

**Arguments:** None

**Output (stdout):** JSON

```json
{
  "protocol_version": 1,
  "name": "cursor",
  "type": "Cursor",
  "description": "Cursor - AI-powered code editor",
  "is_preview": true,
  "protected_dirs": [".cursor"],
  "hook_names": ["session-start", "session-end", "stop"],
  "capabilities": {
    "hooks": true,
    "transcript_analyzer": true,
    "transcript_preparer": false,
    "token_calculator": false,
    "text_generator": false,
    "hook_response_writer": false,
    "subagent_aware_extractor": false
  }
}
```

The `capabilities` object determines which optional subcommands the CLI will call. If a capability is `false` or missing, the CLI will never invoke the corresponding subcommands.

#### `detect`

Checks whether the agent is present/usable in the current environment.

**Arguments:** None

**Output (stdout):** JSON

```json
{"present": true}
```

#### `get-session-id`

Extracts the session ID from a hook input event.

**Input (stdin):** JSON — the [HookInput object](#hookinput-object)

**Output (stdout):** JSON

```json
{"session_id": "abc123"}
```

#### `get-session-dir --repo-path <path>`

Returns the directory where agent sessions are stored.

**Arguments:**
- `--repo-path` — Absolute path to the repository

**Output (stdout):** JSON

```json
{"session_dir": "/path/to/sessions"}
```

#### `resolve-session-file --session-dir <dir> --session-id <id>`

Resolves the session file path from a session directory and ID.

**Arguments:**
- `--session-dir` — Session directory path
- `--session-id` — Session identifier

**Output (stdout):** JSON

```json
{"session_file": "/path/to/session/file.jsonl"}
```

#### `read-session`

Reads session data from a hook input event.

**Input (stdin):** JSON — the [HookInput object](#hookinput-object)

**Output (stdout):** JSON — [AgentSession object](#agentsession-object)

```json
{
  "session_id": "abc123",
  "agent_name": "cursor",
  "repo_path": "/path/to/repo",
  "session_ref": "/path/to/file.jsonl",
  "start_time": "2026-01-13T12:00:00Z",
  "native_data": null,
  "modified_files": ["src/main.go"],
  "new_files": [],
  "deleted_files": []
}
```

#### `write-session`

Writes/updates session data.

**Input (stdin):** JSON — the [AgentSession object](#agentsession-object)

**Output:** Exit 0 on success.

#### `read-transcript --session-ref <path>`

Reads a transcript file and returns its raw bytes.

**Arguments:**
- `--session-ref` — Path to the transcript file

**Output (stdout):** Raw transcript bytes.

#### `chunk-transcript --max-size <n>`

Splits a transcript into chunks for storage.

**Input (stdin):** Raw transcript bytes.

**Arguments:**
- `--max-size` — Maximum chunk size in bytes

**Output (stdout):** JSON

```json
{"chunks": ["<base64-encoded-chunk>", "..."]}
```

#### `reassemble-transcript`

Reassembles transcript chunks back into a full transcript.

**Input (stdin):** JSON

```json
{"chunks": ["<base64-encoded-chunk>", "..."]}
```

**Output (stdout):** Raw transcript bytes.

#### `format-resume-command --session-id <id>`

Returns the command a user can run to resume a session.

**Arguments:**
- `--session-id` — Session identifier

**Output (stdout):** JSON

```json
{"command": "cursor --resume abc123"}
```

### Capability: `hooks`

These subcommands are required when `capabilities.hooks` is `true`.

#### `parse-hook --hook <name>`

Parses a raw agent hook payload into a structured event.

**Arguments:**
- `--hook` — Hook name (e.g., "stop", "session-start")

**Input (stdin):** Raw agent hook payload bytes.

**Output (stdout):** JSON — the parsed [Event object](#event-object), or `null` if the payload is not relevant.

#### `install-hooks [--local-dev] [--force]`

Installs agent hooks for Entire integration.

**Arguments:**
- `--local-dev` — Use local development binary path (optional)
- `--force` — Overwrite existing hooks (optional)

**Output (stdout):** JSON

```json
{"hooks_installed": 3}
```

#### `uninstall-hooks`

Removes installed agent hooks.

**Arguments:** None

**Output:** Exit 0 on success.

#### `are-hooks-installed`

Checks whether hooks are currently installed.

**Arguments:** None

**Output (stdout):** JSON

```json
{"installed": true}
```

### Capability: `transcript_analyzer`

Required when `capabilities.transcript_analyzer` is `true`.

#### `get-transcript-position --path <path>`

Returns the current byte position/size of a transcript file.

**Arguments:**
- `--path` — Path to the transcript file

**Output (stdout):** JSON

```json
{"position": 12345}
```

#### `extract-modified-files --path <path> --offset <n>`

Extracts the list of files modified by the agent from a transcript.

**Arguments:**
- `--path` — Path to the transcript file
- `--offset` — Byte offset to start reading from

**Output (stdout):** JSON

```json
{"files": ["path/to/file1.go", "path/to/file2.go"], "current_position": 12345}
```

The `current_position` field returns the transcript position after extraction, allowing the caller to resume from that point on subsequent calls.

#### `extract-prompts --session-ref <path> --offset <n>`

Extracts user prompts from a transcript.

**Arguments:**
- `--session-ref` — Path to the transcript file
- `--offset` — Byte offset to start reading from

**Output (stdout):** JSON

```json
{"prompts": ["first prompt text", "second prompt text"]}
```

#### `extract-summary --session-ref <path>`

Extracts an AI-generated summary from a transcript.

**Arguments:**
- `--session-ref` — Path to the transcript file

**Output (stdout):** JSON

```json
{"summary": "Summary text here", "has_summary": true}
```

### Capability: `transcript_preparer`

Required when `capabilities.transcript_preparer` is `true`.

#### `prepare-transcript --session-ref <path>`

Prepares/processes a transcript file (e.g., converting from raw format).

**Arguments:**
- `--session-ref` — Path to the transcript file

**Output:** Exit 0 on success.

### Capability: `token_calculator`

Required when `capabilities.token_calculator` is `true`.

#### `calculate-tokens --offset <n>`

Calculates token usage from a transcript.

**Input (stdin):** Raw transcript bytes.

**Arguments:**
- `--offset` — Byte offset to start calculating from

**Output (stdout):** JSON

```json
{
  "input_tokens": 1500,
  "cache_creation_tokens": 0,
  "cache_read_tokens": 200,
  "output_tokens": 500,
  "api_call_count": 3,
  "subagent_tokens": null
}
```

Only `input_tokens` and `output_tokens` are required. The optional fields (`cache_creation_tokens`, `cache_read_tokens`, `api_call_count`) default to `0` if omitted. `subagent_tokens` is an optional nested object with the same structure, for agents that spawn subagents.

### Capability: `text_generator`

Required when `capabilities.text_generator` is `true`.

#### `generate-text --model <model>`

Generates text using the agent's underlying LLM.

**Input (stdin):** Prompt text.

**Arguments:**
- `--model` — Model to use for generation

**Output (stdout):** JSON

```json
{"text": "Generated response text"}
```

### Capability: `hook_response_writer`

Required when `capabilities.hook_response_writer` is `true`.

#### `write-hook-response --message <message>`

Writes a message in the agent's native hook response format.

**Arguments:**
- `--message` — Message to write

**Output (stdout):** Agent-native format bytes (e.g., JSONL for Claude Code).

### Capability: `subagent_aware_extractor`

Required when `capabilities.subagent_aware_extractor` is `true`.

#### `extract-all-modified-files --offset <n> --subagents-dir <dir>`

Extracts modified files from both the main transcript and any subagent transcripts.

**Input (stdin):** Raw main transcript bytes.

**Arguments:**
- `--offset` — Byte offset for the main transcript
- `--subagents-dir` — Directory containing subagent transcripts

**Output (stdout):** JSON

```json
{"files": ["file1.go", "file2.go"]}
```

#### `calculate-total-tokens --offset <n> --subagents-dir <dir>`

Calculates total token usage across main transcript and subagent transcripts.

**Input (stdin):** Raw main transcript bytes.

**Arguments:**
- `--offset` — Byte offset for the main transcript
- `--subagents-dir` — Directory containing subagent transcripts

**Output (stdout):** JSON

```json
{
  "input_tokens": 5000,
  "cache_creation_tokens": 0,
  "cache_read_tokens": 1000,
  "output_tokens": 2000,
  "api_call_count": 8,
  "subagent_tokens": {
    "input_tokens": 2000,
    "output_tokens": 800
  }
}
```

Same token usage format as `calculate-tokens`. The `subagent_tokens` field aggregates usage from all subagents.

## Shared Object Definitions

### HookInput Object

The HookInput object is passed via stdin to `get-session-id` and `read-session`.

```json
{
  "hook_type": "stop",
  "session_id": "abc123",
  "session_ref": "/path/to/transcript.jsonl",
  "timestamp": "2026-01-13T12:00:00Z",
  "user_prompt": "Fix the login bug",
  "tool_name": "Write",
  "tool_use_id": "toolu_abc123",
  "tool_input": {"path": "/src/main.go"},
  "raw_data": {"custom_field": "value"}
}
```

| Field | Type | Description |
|---|---|---|
| `hook_type` | string | Hook type: `session_start`, `session_end`, `user_prompt_submit`, `stop`, `pre_tool_use`, `post_tool_use` |
| `session_id` | string | Agent session identifier |
| `session_ref` | string | Agent-specific session reference (typically a file path) |
| `timestamp` | string | RFC 3339 timestamp |
| `user_prompt` | string | User's prompt text (from `user_prompt_submit` hooks). Optional. |
| `tool_name` | string | Tool name (from `pre_tool_use`/`post_tool_use` hooks). Optional. |
| `tool_use_id` | string | Tool invocation ID. Optional. |
| `tool_input` | object | Raw tool input JSON. Optional. |
| `raw_data` | object | Agent-specific data for extension. Optional. |

### AgentSession Object

Used as input to `write-session` and output from `read-session`.

```json
{
  "session_id": "abc123",
  "agent_name": "cursor",
  "repo_path": "/path/to/repo",
  "session_ref": "/path/to/transcript.jsonl",
  "start_time": "2026-01-13T12:00:00Z",
  "native_data": null,
  "modified_files": ["src/main.go"],
  "new_files": ["src/new_file.go"],
  "deleted_files": []
}
```

| Field | Type | Description |
|---|---|---|
| `session_id` | string | Agent session identifier |
| `agent_name` | string | Agent registry name |
| `repo_path` | string | Absolute path to the repository |
| `session_ref` | string | Path/reference to session in agent's storage |
| `start_time` | string | RFC 3339 timestamp of session start |
| `native_data` | bytes/null | Session content in agent's native format (opaque to CLI) |
| `modified_files` | string[] | Files modified during the session |
| `new_files` | string[] | Files created during the session |
| `deleted_files` | string[] | Files deleted during the session |

### Event Object

Returned by `parse-hook`. Represents a normalized lifecycle event.

```json
{
  "type": 3,
  "session_id": "abc123",
  "session_ref": "/path/to/transcript.jsonl",
  "prompt": "Fix the login bug",
  "model": "claude-sonnet-4-20250514",
  "timestamp": "2026-01-13T12:00:00Z"
}
```

| Field | Type | Description |
|---|---|---|
| `type` | int | **Required.** Event type (see table below) |
| `session_id` | string | **Required.** Agent session identifier |
| `previous_session_id` | string | Non-empty when this event represents a session continuation/handoff. Optional. |
| `session_ref` | string | Agent-specific transcript reference. Optional. |
| `prompt` | string | User's prompt text (on `TurnStart` events). Optional. |
| `model` | string | LLM model identifier. Optional. |
| `timestamp` | string | RFC 3339 timestamp. Optional. |
| `tool_use_id` | string | Tool invocation ID (for `SubagentStart`/`SubagentEnd`). Optional. |
| `subagent_id` | string | Subagent instance ID (for `SubagentEnd`). Optional. |
| `tool_input` | object | Raw tool input JSON (for subagent type/description extraction). Optional. |
| `subagent_type` | string | Kind of subagent (for `SubagentStart`/`SubagentEnd`). Optional. |
| `task_description` | string | Subagent task description. Optional. |
| `response_message` | string | Message to display to the user via the agent. Optional. |
| `metadata` | object | Agent-specific state preserved across events. Optional. |

**Event types:**

| Value | Name | Description |
|---|---|---|
| 1 | SessionStart | Agent session has begun |
| 2 | TurnStart | User submitted a prompt, agent is about to work |
| 3 | TurnEnd | Agent finished responding to a prompt |
| 4 | Compaction | Agent is compressing its context window (triggers save + offset reset) |
| 5 | SessionEnd | Session has been terminated |
| 6 | SubagentStart | A subagent (task) has been spawned |
| 7 | SubagentEnd | A subagent (task) has completed |

## Error Handling

- Exit code `0` indicates success.
- Any non-zero exit code indicates an error.
- Error messages should be written to stderr.
- The CLI captures stderr and wraps it in a Go error.
- If the binary is not found in PATH, the agent is simply not registered.
- If `info` fails or returns invalid JSON, the binary is skipped during discovery.

## Versioning

The protocol version is declared in the `info` response (`protocol_version` field) and set via `ENTIRE_PROTOCOL_VERSION` environment variable. The CLI checks that the binary's protocol version matches its expected version before registering the agent.

Future protocol versions may add new subcommands or capabilities. Existing subcommands will maintain backwards compatibility within a major version.
