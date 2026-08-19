# Qwen Code — native protocol notes

Qwen Code is Alibaba's Apache-2.0 coding agent, forked from Gemini CLI.
Binary: `qwen`. Docs: <https://qwenlm.github.io/qwen-code-docs/>.

Captured against **qwen v0.21.14** (macOS arm64, `npm i -g
@qwen-code/qwen-code`). Provenance is marked per claim, because the parts read
from documentation are the ones most likely to drift.

| Provenance | Meaning |
| --- | --- |
| `[verified]` | Observed from the shipped bundle, or produced by running `qwen` |
| `[documented]` | From vendor docs / the Agentic Tools Almanac only |

## The fork shows in the payload, not the layout

Qwen is a Gemini CLI fork, and that matters for parsing, but the two halves come
from different lineages `[verified]`:

- The **envelope** is Claude-shaped: one JSON object per line, carrying `uuid`,
  `parentUuid`, `sessionId`, `timestamp`, `type`, `provenance`, `cwd`, `version`.
- The **payload** under `message` is Gemini-shaped: `role` is `"model"` (not
  `"assistant"`), content lives in `parts[]`, and tool traffic uses
  `functionCall` / `functionResponse` blocks.

So Gemini CLI's parser does not transfer wholesale (Gemini stores one JSON
document; Qwen stores JSONL), but the per-message shape does.

## Storage `[verified]`

```
~/.qwen/projects/<sanitized-cwd>/chats/<sessionId>.jsonl
```

The directory slug is the working directory with non-alphanumerics replaced by
`-`, the same convention Claude and Gemini use. Recording is gated on
`--chat-recording`; `qwen --help` states that with it off "chat history is not
saved and --continue/--resume will not work".

Entire does not need to compute this path: the hook hands it over as
`transcript_path` (see below).

## Line types `[verified]`

From a real session driven end to end (committed as
`testdata/real_session_v0_21_14.jsonl`):

| `type` | `message.role` | Contents |
| --- | --- | --- |
| `user` | `user` | `parts: [{text}]` |
| `assistant` | `model` | `parts: [{text}, {functionCall}]`, plus `usageMetadata` and `model` |
| `tool_result` | `user` | `parts: [{functionResponse}]`, plus `toolCallResult` |
| `system` | — | `systemPayload`, with `subtype` `attribution_snapshot` or `ui_telemetry` |

**A tool result is a user-role message.** Its envelope `type` is `tool_result`,
but `message.role` is `user`, so filtering prompts on the role alone captures
every tool result as if the user had typed it. Qwen also stamps
`provenance: "real_user"` on genuine prompts, which is what `ExtractPrompts`
keys on.

A tool call:

```json
{"functionCall": {"id": "call_1", "name": "write_file",
                  "args": {"file_path": "hello.txt", "content": "..."}}}
```

File-touching tool names `[verified]` from the bundle: `write_file`, `replace`,
`edit`. Read/search tools (`read_file`, `glob`, `run_shell_command`,
`read_many_files`) do not contribute file paths.

## Token usage `[verified]`

Assistant lines carry Gemini's `usageMetadata`:

```json
{"promptTokenCount": 1234, "candidatesTokenCount": 56, "thoughtsTokenCount": 0,
 "totalTokenCount": 1290, "cachedContentTokenCount": 400}
```

`cachedContentTokenCount` maps to Entire's `CacheReadTokens`. Qwen reports no
cache-*write* figure, so `CacheCreationTokens` stays zero rather than being
invented. `thoughtsTokenCount` is folded into output, matching how the Gemini
integration accounts for reasoning tokens.

Unlike Goose, these are per-message, so token usage scopes to an offset
correctly.

## Hooks `[verified]`

All eleven event names appear as literals in the shipped bundle:

```
SessionStart  SessionEnd  UserPromptSubmit  Stop  Notification
PreToolUse    PostToolUse SubagentStart     SubagentStop
PreCompact    PostCompact
```

The four Entire uses map one to one: `SessionStart`, `UserPromptSubmit`
(TurnStart), `Stop` (TurnEnd), `SessionEnd`.

### Hook stdin `[verified]`

`session_id`, `transcript_path` and `hook_event_name` are all present in the
bundle (135, 26 and 10 occurrences). This is the richest payload of any agent
Entire supports: because the transcript path arrives on stdin, there is no
export command to shell out to and no path convention to reverse-engineer.

Entire still switches on the hook verb rather than `hook_event_name`, since the
subcommand already identifies the event.

### Config `[documented]`

`hooks` key in `settings.json`, layered user (`~/.qwen/settings.json`) then
project (`.qwen/settings.json`). Entire writes the project file, matching the
Gemini integration. A global `disableAllHooks` kill switch exists `[verified]`.

```json
{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "entire hooks qwen-code turn-end", "timeout": 30}]}
    ]
  }
}
```

`timeout` is milliseconds in Qwen's documented examples, not seconds.

## Resume `[verified]`

```
qwen -r <sessionId>      # --resume; bare -r opens a picker
qwen -c                  # --continue, most recent session for this project
```

## Not verified

- Hooks firing with the payload above. The event names, the three stdin fields
  and the kill switch are all in the binary, but firing one needs a full
  interactive session; the Almanac reports a live test on v0.20.1.
- `timeout` units come from docs, not observation.
- The session used for the fixture ran against a local mock OpenAI endpoint, so
  the tool *name* the model chose is not one a real Qwen model would pick. Every
  structural field (envelope, parts, usageMetadata, functionCall/Response) is
  genuine Qwen output.
