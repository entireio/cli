# OpenHands — native protocol notes

OpenHands is All Hands AI's MIT-licensed coding agent
(<https://docs.openhands.dev/>). Binary: `openhands`.

Captured against the version installed by `uv tool install openhands --python
3.12`, by reading the shipped Python package and by running a real headless
session against a local OpenAI-compatible endpoint.

| Provenance | Meaning |
| --- | --- |
| `[verified]` | Read from the installed package, or produced by running `openhands` |
| `[documented]` | From vendor docs / the Agentic Tools Almanac only |

Three things below contradict what the docs and the Almanac say. Each is marked.

## Storage: one JSON file per event

`[verified]` `openhands/sdk/conversation/persistence_const.py`:

```python
BASE_STATE = "base_state.json"
EVENTS_DIR = "events"
EVENT_FILE_PATTERN = "event-{idx:05d}-{event_id}.json"
```

which produces:

```
<persistence>/conversations/<conversation_id>/
    base_state.json
    events/
        event-00000-d06db83d-73e0-48c8-ba38-8d921cff351b.json
        event-00001-fdc07889-858f-4a2c-8193-66f28b529e72.json
```

`<persistence>` is `$OPENHANDS_PERSISTENCE_DIR` else `~/.openhands`, and the
conversations root can be overridden on its own with
`$OPENHANDS_CONVERSATIONS_DIR` (`openhands_cli/locations.py`).

**This is the only agent Entire supports whose transcript is a directory rather
than a file, and there is no export command.** Entire's `SessionRef` is a single
path, so this package serializes the event directory to JSONL: one event object
per line, in index order.

That is a transformation, and `agent-integration-checklist.md` warns against
inventing intermediate formats. The justification is that this one is
**lossless and mechanically reversible**: the filename is fully determined by
`(index, event.id)` via `EVENT_FILE_PATTERN`, and the index is the line's
position. `WriteSession` reconstructs byte-identical filenames from the JSONL
alone, which `TestEventDirRoundTrip` pins. No field is added, renamed or
dropped; only the container changes. Compare the OpenCode and Goose
integrations, which face the same problem and solve it with the agent's own
export/import commands. OpenHands ships none, so the serialization lives here.

`base_state.json` is **not** part of the transcript and is left untouched.
It holds agent configuration and run state, not conversation content.

### Event shape `[verified]`

Events are discriminated by `kind`:

| `kind` | Contents |
| --- | --- |
| `SystemPromptEvent` | `system_prompt`, `tools` |
| `MessageEvent` | `llm_message: {role, content: [{type, text}]}` |
| `ActionEvent` | `thought`, `tool_name`, `tool_call_id`, `tool_call: {id, name, arguments}` |
| `ObservationEvent` | tool result |
| `AgentErrorEvent` | error detail |

Every event carries `id`, `timestamp` and `source` (`user` or `agent`).

`tool_call.arguments` is a **JSON-encoded string**, not an object, so extracting
a file path needs a second unmarshal.

`[verified]` The first event of a conversation is large: the `SystemPromptEvent`
in the committed fixture serializes to **86KB on one line**, because it embeds
the whole system prompt and every tool schema. Chunking cannot split below a
single line, so any chunk bound under that size fails for this agent where it
would succeed for others.

The real tool name for file edits is `file_editor` `[verified]` from a live run.
This is the same class of trap the Almanac documents for the shell tool, where
the real name is `terminal` and a matcher of `execute_bash` or `bash` "will
silently never match".

## Hooks

### Config location — the Almanac is wrong here

`[verified]` `openhands/sdk/hooks/config.py` searches **both**:

```python
base_dir / ".openhands" / "hooks.json",
Path.home() / ".openhands" / "hooks.json",
```

The Almanac states "There is no user/global hook scope", and its
`config-file-locations.md` lists only the project path. A global scope exists.
Entire writes the **project** file, matching every other agent it supports.

### Config schema `[verified]`

Canonical keys are snake_case; PascalCase and a `{"hooks": {...}}` wrapper are
also accepted "for interoperability with existing integrations (e.g. Claude Code
plugin hook files)". Entire writes the canonical snake_case form.

```json
{
  "user_prompt_submit": [
    {"matcher": "*", "hooks": [{"type": "command", "command": "entire hooks openhands turn-start", "timeout": 60}]}
  ]
}
```

**`HookConfig` sets `model_config = {"extra": "forbid"}`** `[verified]`, so any
unrecognised top-level key makes OpenHands reject the whole file. This package
therefore writes **no marker comment** — unlike the Goose integration, which
identifies its own file by an `_comment` field. Ownership here is detected from
the hook command via `agent.IsManagedHookCommand`.

`HookDefinition` is `{type, command, timeout=60, async=false}`;
`HookMatcher` is `{matcher="*", hooks=[...]}`.

### Events `[verified]`

`openhands/sdk/hooks/types.py` defines exactly six:

```
PreToolUse  PostToolUse  UserPromptSubmit  SessionStart  SessionEnd  Stop
```

Entire maps `SessionStart`, `UserPromptSubmit` (TurnStart), `Stop` (TurnEnd),
`SessionEnd`.

### Hook stdin `[verified]`

`HookEvent` in `types.py`:

```python
event_type, tool_name, tool_input, tool_response, message,
session_id, working_dir, metadata
```

`session_id` is present; there is **no** `transcript_path`, so the conversation
directory is reconstructed from `session_id`. OpenHands also exports
`OPENHANDS_SESSION_ID`, `OPENHANDS_EVENT_TYPE`, `OPENHANDS_TOOL_NAME` and
`OPENHANDS_PROJECT_DIR` into the hook environment `[documented]`.

## Resume — the Almanac says UNKNOWN, it exists

`[verified]` A real headless run ended with:

```
Conversation ID: 04e2eedbe2d64736a1a4436334d9e1e6
Hint: run openhands --resume 04e2eedb-e2d6-4736-a1a4-436334d9e1e6 to resume this
```

Note the two spellings of the same id: the **directory** uses undashed hex32,
while **`--resume`** takes the dashed UUID form. `conversationDirID` and
`resumeID` normalize between them; getting this wrong points the reader at a
directory that does not exist.

## Not verified

- Hooks firing end to end. The event names, the stdin model and both config
  search paths come from the installed package, but firing one needs a full
  interactive session. The Almanac reports a live test on v1.21.0 where a
  `PreToolUse` hook blocked a command via `exit 2`.
- The fixture session ran against a local mock endpoint, so the assistant text
  and the arguments the model chose are synthetic. Every structural field
  (`kind`, `llm_message`, `tool_call`, filenames, ordering) is genuine.
