# Goose — native protocol notes

Goose is an open-source (Apache-2.0) coding agent governed by the Agentic AI
Foundation. Homepage: <https://goose-docs.ai/>. Binary: `goose`.

Everything below was captured against **goose v1.46.0** (arm64 macOS, Homebrew
`block-goose-cli`). Each claim carries its provenance, because the parts that
were only read from documentation are the parts most likely to drift.

| Provenance | Meaning |
| --- | --- |
| `[verified]` | Observed directly from the v1.46.0 binary or by executing it |
| `[documented]` | From vendor docs only — **not** executed here |

## Storage: SQLite, not files

`[verified]` `goose info` reports:

```
Sessions DB (sqlite):    ~/.local/share/goose/sessions/sessions.db
Config yaml:             ~/.config/goose/config.yaml
```

The path honours `XDG_DATA_HOME` / `XDG_CONFIG_HOME` `[verified]` — the round-trip
below was performed in a fully isolated `XDG_DATA_HOME`.

`[verified]` The schema (read from the binary's embedded SQL) is:

- `sessions(id, name, user_set_name, session_type, working_dir, extension_data,
  goose_mode, created_at, updated_at, archived_at, project_id,
  parent_session_id, accumulated_cost)`
- `messages(message_id, session_id, role, content_json, created_timestamp,
  metadata_json)`
- `usage_ledger(session_id, total_tokens, input_tokens, output_tokens,
  cache_read_tokens, cache_write_tokens, accumulated_*)`

Because the transcript lives in a database rather than a file, Goose follows the
**OpenCode integration pattern**: read via the agent's canonical export command,
write via its canonical import command. Entire never touches `sessions.db`
directly — doing so would couple us to a schema the vendor migrates in place
(the binary carries `ALTER TABLE` migrations for at least four columns).

## Canonical export / import

`[verified]`

```
goose session export --session-id <id> --format json   # json | yaml | markdown
goose session import <file>                            # JSON, or Claude/Codex/Pi .jsonl
```

`--format json` is the format Entire stores as `NativeData`. Markdown is the
default for `export` and is lossy, so the format flag is **not** optional.

`goose session import` also ingests Claude Code / Codex / Pi JSONL transcripts.
That is how the export schema below was captured without a model provider
configured: a Claude Code fixture from this repo
(`transcript/compact/testdata/claude_full.jsonl`) was imported into an isolated
`XDG_DATA_HOME` and exported back out as JSON.

### Export schema `[verified]`

Top level (24 keys):

```
id, working_dir, name, user_set_name, session_type, created_at, updated_at,
extension_data, usage, accumulated_usage, accumulated_cost, schedule_id,
recipe, user_recipe_values, conversation, message_count, last_message_at,
provider_name, model_config, goose_mode, archived_at, project_id,
parent_session_id, last_message_snippet
```

**The messages array is named `conversation`, not `messages`.** This is the one
field most likely to be guessed wrong — OpenCode's analogous export uses
`messages`, and Goose does not.

`usage` and `accumulated_usage` both carry:

```json
{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
 "cache_read_input_tokens": 0, "cache_write_input_tokens": 0}
```

Note `cache_read_input_tokens` / `cache_write_input_tokens` — not the
`cache_read_tokens` / `cache_write_tokens` spelling used by the SQLite column
names. The export and the database disagree; the export spelling is what we
parse.

A message:

```json
{
  "id": "msg_<session>_<uuid>",
  "role": "user",
  "created": 1773792194,
  "content": [{"type": "text", "text": "..."}],
  "metadata": {"userVisible": true, "agentVisible": true}
}
```

Content block types `[verified]`: `text`, `toolRequest`, `toolResponse`,
`toolConfirmationRequest`. A tool request:

```json
{
  "type": "toolRequest",
  "id": "toolu_...",
  "toolCall": {
    "status": "success",
    "value": {"name": "Bash", "arguments": {"command": "git log -1"}}
  }
}
```

## Tool naming — read this before writing a matcher

`[verified]` Goose namespaces tools as `<extension>__<tool>` at runtime
(`developer__shell` appears in the binary). The names are **built at runtime**,
so they cannot be enumerated statically from the binary.

`[documented]` The Agentic Tools Almanac records a live-verified discrepancy
here: the vendor's own hook documentation shows `"tool_name": "developer__shell"`,
but the value actually delivered to a hook is the bare `"shell"` — so a matcher
of `developer__shell` *"silently never matches and the hook never fires."*

Consequence for this integration: **never match a Goose tool name for equality.**
`transcript.go` matches on suffix after the `__` separator, so both the bare and
namespaced spellings resolve to the same tool. The fixture-derived names in the
export above (`Bash`) are Claude's, inherited through the import path, which is a
third spelling we must tolerate.

## Hooks

`[verified]` All eleven event names are present as literals in the v1.46.0
binary:

```
SessionStart  SessionEnd  UserPromptSubmit  Stop
PreToolUse    PostToolUse PostToolUseFailure
BeforeReadFile AfterFileEdit BeforeShellExecution AfterShellExecution
```

alongside the loader strings `hooks/hooks.json`, `Loaded plugin hooks`,
`Ignoring unknown hook event`, `Invalid hook matcher regex; skipping rule`, and
`Ignoring unsupported hook action type`.

The four Entire needs map cleanly:

| Entire event | Goose event |
| --- | --- |
| `SessionStart` | `SessionStart` |
| `TurnStart` | `UserPromptSubmit` |
| `TurnEnd` | `Stop` |
| `SessionEnd` | `SessionEnd` |

### Discovery `[verified]`

`.agents/plugins/` is present in the binary as a discovery root. A plugin is any
directory containing `hooks/hooks.json`; `${PLUGIN_ROOT}` expands to the plugin
directory. Entire installs to the **project** root:

```
<repo>/.agents/plugins/entire/hooks/hooks.json
```

Project scope matches how `entire enable` works for every other agent, and keeps
enablement per-repo rather than per-machine.

### Config shape `[documented]`

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "entire hooks goose turn-start", "timeout": 10}]}
    ]
  }
}
```

`matcher` is a regex over an event-specific value and may be omitted to match
everything. Only `"type": "command"` is supported.

### Hook stdin `[documented]`

```json
{"event": "PreToolUse", "session_id": "...", "tool_name": "shell",
 "tool_input": {...}, "working_dir": "/path"}
```

`session_id` is present — that is what Entire keys session state on. There is
**no** `transcript_path` field, which is why `PrepareTranscript` shells out to
`goose session export` rather than reading a path handed to us.

The payload's event-name field is `event`. Entire does not depend on it: the hook
verb is already encoded in the subcommand (`entire hooks goose turn-end`), so
`ParseHookEvent` switches on the verb and reads only `session_id` from stdin.
That makes the integration robust to the field being renamed.

## Resume `[verified]`

```
goose session --resume --session-id <id>
```

`--session-id` is documented by `goose session --help` as *"Specify a session ID
to resume. Requires --resume."* The bare `--resume` resumes the most recent
session, which is why the flag pair is always emitted together.

## Not verified here

Running a live Goose session requires a configured model provider
(`goose doctor` reports `No provider configured`), so the following are
**`[documented]` only** and should be confirmed by anyone with credentials:

- That hooks fire with the payload shape above (the Almanac reports a live test
  on v1.44.0 doing exactly this, on a fresh Ubuntu box).
- The tool names appearing in a natively-generated (not imported) transcript.
- Token-usage population on a live session — the fixture's `usage` block came
  through the Claude import path.
