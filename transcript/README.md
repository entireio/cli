# transcript

Package `github.com/entireio/cli/transcript` converts native agent transcripts
into the Entire Transcript Format and reads that format into typed Go values.

The library is versioned with CLI releases. Its API and the format may change;
consumers should pin a version.

## Entire Transcript Format v1

The format has been called the "compact transcript" (CLI code, metadata key
`compact_transcript`) and the "unified transcript" (entire-api). All three names
refer to the same thing. Stored names are unchanged: the file is
`transcript.jsonl`, the metadata key is `compact_transcript`, and the line field
is `cli_version`.

One JSON object per line, each line terminated by `\n`.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `v` | int | yes | Always `1`. |
| `type` | string | yes | `user` or `assistant`. |
| `agent` | string | no | The CLI agent registry name (`claude-code`, `codex`, `copilot-cli`, `cursor`, `factoryai-droid`, `opencode`, `pi`, `antigravity`); otherwise the agent type string, e.g. for external agents. An open set. |
| `cli_version` | string | no | Version of the producer that wrote the line. The name is historical. |
| `ts` | string | no | Source timestamp where available, normally RFC 3339. May be omitted or empty when the native transcript has no timestamp. The generic JSONL converter copies source timestamps verbatim, so a non-string value can occur; readers ignore it. |
| `id` | string | no | Assistant message ID. Streaming fragments with the same ID are merged into one line. The Codex item path writes the item ID on user lines too. |
| `model` | string | no | Assistant lines only. The model the source reports for this call, verbatim; omitted when it reports none. Never inferred from a session-wide default, and never a placeholder: a value the agent writes in place of a model (Claude Code's `<synthetic>` on API-error entries) is omitted. |
| `input_tokens` | int | no | Assistant lines only. Input not counted in the line's `cache_read_tokens` or `cache_write_tokens`; see below. |
| `output_tokens` | int | no | Assistant lines only; includes reasoning tokens when the source reports them as part of output. |
| `cache_read_tokens`, `cache_write_tokens` | int | no | Assistant lines only; per-call cached input read and total cached input written. Cache counts are separate from fresh `input_tokens`. |
| `cache_write_5m_tokens`, `cache_write_1h_tokens` | int | no | Assistant lines only; an optional breakdown of `cache_write_tokens`, emitted only when the source reports the split (even when a reported value is zero). Absence means unknown, not zero or all 5m. Do not add the split to the total when summing usage. |
| `reasoning_tokens` | int | no | Assistant lines only; subset of `output_tokens` when reported by the source, not additional output. |
| `content` | array of blocks, string, or `null` | yes | Normally an array. A string can appear when an assistant message's source content was a string. `null` can appear because the writer does not omit empty content. |

Usage and model fields are per call, never copied from a session aggregate. A zero omitted by JSON encoding does not assert that the source reported zero.

On every line, `input_tokens` excludes the tokens in that line's `cache_read_tokens` and `cache_write_tokens`, so adding them never double counts. The sum is the call's total input only on lines from a converter that writes the cache fields. Lines written before that carry no cache fields, and their `input_tokens` is one of two things:

- Fresh input only (Claude Code, and Pi except as below): the cached input is missing from the line, so the sum understates the call's input.
- Fresh plus cached input (Codex, and Pi on Google models before pi-ai subtracted cached tokens in March 2026): the total is right, but its cache share is unknown, so pricing it at the fresh-input rate overstates the cost.

Other agents' lines copy the native input count as the agent defines it. Only `agent` and the `cli_version` that wrote a line tell these cases apart; no field marks a line as old or new, and `model` in particular says nothing about how `input_tokens` was counted.

Converters fill only what the native transcript reports per call. Claude Code, Codex and Pi record per-call usage and model, and their converters write them (below). Copilot CLI has only a shutdown aggregate, Cursor is hook-only, and external plugins and Antigravity report usage out of band, so their lines do not get these fields. Native timestamps are preserved where present; a missing one stays empty rather than fabricated.

### Per-call usage

An assistant line can carry usage with empty content (`"content":[]` or `"content":null`, for example an aborted or thinking-only call): readers that render transcripts should skip it, and readers that count usage must keep it.

Claude Code (`Options.Agent` `claude-code`, and `NewClaudeAdapter`; Cursor and Droid keep legacy counts): streaming fragments of one message ID become one line carrying the whole usage row with the strictly highest `output_tokens` (earlier wins ties) and the model of the first fragment that reports one, skipping Claude Code's `<synthetic>` placeholder; `ts` is the last fragment's. The 5m/1h cache-write split is emitted only when both durations are reported. A message with no usage object on any fragment has no model or usage fields. User entries holding only tool results between fragments of one ID do not split the call: the later fragments become separate lines without usage, and the best row stays on the call's first line (the first with visible content, since a thinking-only message has no line), so a call's usage is counted once. Read per-call usage from the full converted transcript: in a checkpoint slice (`ConvertWithBoundary`), a usage row that arrives after a tool result goes to the call's first line, which can sit before the slice boundary.

Codex, `response_item` path (rollouts from Codex versions that do not record items): one assistant line per model call, made of the reasoning, message and tool-call items up to the `token_count` that closes it (real order: call, output, `token_count`; outputs attach to their call by `call_id`). Usage is the difference between consecutive cumulative `info.total_token_usage` snapshots, and `ts` is the closing snapshot's when present, otherwise the last item's. Fresh input is `input - cached` clamped at zero; Codex lines carry no cache-write fields. Repeated snapshots and snapshots missing the input, cached-input or output counter are ignored, `reasoning_tokens` needs both adjacent snapshots to report it, and a decrease in any counter stops usage on later lines. A call with usage but no text or tool calls (for example reasoning-only) emits `content: []`, so consumers may see empty assistant lines; a call with neither usage nor content is omitted. Without usable `total_token_usage` snapshots (flat per-event totals are not read), no `token_count` closes a call, so consecutive assistant items merge into one line until the next user line or tool output; with usable snapshots, a tool output does not end its call. After a counter decrease, total snapshots still close calls but carry no usage, starting with the call the decrease closes. With `StartLine`, the last line before the boundary is part of the slice when its tool output or closing snapshot arrives after it (a one-line head overlap, which can repeat that call's usage); a later output for any earlier call lands only on that call's earlier line.

Codex, item path (current rollouts; see Blocks): one line per item, not per call. A call is the items up to the `token_count` that closes it, and its usage is computed exactly as on the `response_item` path: the same snapshot differences, fresh input, cached reads and reasoning, the model from `turn_context`, repeated snapshots ignored, and a counter decrease stopping usage. The usage goes on the call's last assistant line, so the call's earlier lines carry none. A call with usage but no assistant line (reasoning only) gets a usage-only line with `"content":null` and no `id`, whose `ts` is the closing snapshot's; other lines keep their item's `ts`. A user message ends the open call. With `StartLine`, the skipped lines seed the usage baseline, and the last line before the boundary is part of the slice when its call's closing snapshot arrives after it (a one-line head overlap).

Pi, one native message per call (no fragments to merge): `model` is `message.model`, and Pi's `input` is used as `input_tokens` unchanged, since the backends that report cached tokens already exclude them from it (a backend that folds cache reads into `input` cannot be told apart per call). `cache_write_1h_tokens` is Pi's `cacheWrite1h` and `cache_write_5m_tokens` the rest of `cacheWrite` (clamped at 0 if 1h exceeds it), only when Pi reports `cacheWrite1h`. `reasoning_tokens` is Pi's `reasoning` when reported. An assistant message with no visible content but non-zero usage (thinking-only, aborted) gets a line with `"content":null` (`"content":[]` before Pi went through the fold), so per-line usage sums to the session totals; one with zero usage gets none. Abandoned branches are excluded. Read per-call usage from the full converted transcript. Pi user `image` items pass through verbatim in Pi's native shape (`data`, `mimeType`) and in source order, so an image-only user message still has a line; `data` is the inline base64, or an `entire-asset:assets/…` placeholder when image externalization (`redaction.externalize_images`) moved the image to the checkpoint's assets.

### Blocks

- User text: `{"id"?: string, "text": string}`. There is no `type` field (a v1 quirk).
- Assistant text: `{"type": "text", "text": string}`.
- Tool use: `{"type": "tool_use", "id"?: string, "name": string, "input": any, "result"?: {"output": string, "status": "success"|"error", "file"?: {"filePath": string, "numLines"?: int}, "matchCount"?: int, "agent"?: {"id": string}}}`.
  Tool results are inlined into the `tool_use` block that requested them,
  matched by tool call ID where the agent records one, including the results
  of parallel calls that the agent writes separately.
  `agent.id` is the agent's own ID for the subagent session or thread the call
  launched (never the tool call ID, which is the block's `id`), when the parent
  transcript records one: Claude Code's `toolUseResult.agentId` (Agent, and
  forked Skill calls), Codex's `SubAgentActivity` thread for a `spawn_agent`
  call, OpenCode's `task` session (`state.metadata.sessionId`), and Copilot's
  `subagent.started` agent for a task call.
- Image: passed through from the source verbatim. The image source is either
  inline base64 or an `entire-asset:assets/…` placeholder left by image
  externalization.
- Any other block type is passed through verbatim. Thinking blocks are dropped
  by the converters.

Codex: rollouts from Codex versions that record items (the first turn has a
user message `item_completed` event) convert item by item: each
`item_completed` item becomes its own line or lines.
Commands are `Bash` tool uses whose input is the script inside `<shell> -lc`
or `<shell> -c` (other commands are shown whole), with the command's output;
file changes are `FileChange` with each file's diff; MCP calls are named
`server.tool`; web searches are `WebSearch` and image views
`ImageView`; a sub-agent spawn is `spawn_agent`, an interrupt
`interrupt_agent` and any other interaction (a message or a follow-up task,
which the item doesn't tell apart) `interact_agent`, each with the
sub-agent's `agent.id`. Code-mode
wrapper calls no longer appear. Lines carry their item's time (RFC 3339 UTC
with milliseconds), and usage is per model call (see Per-call usage).
Items it can't read (such as one without an ID) or doesn't map (reasoning,
plans such as `update_plan`, compaction and other items) are skipped and
add no line. Older rollouts keep the `response_item` path, and so do
rollouts whose first turn has its prompt only as a `response_item` (a
guardian sub-session, whose agent message is an item) and thread_spawn
sub-agent rollouts (no items in the first turn, the task prompt as a
`response_item`).
`CodexNotificationRecords` maps a Codex app-server `item/started` or
`item/completed` notification to records with the same mapping, so a live
stream of a Codex session shows a completed item exactly as it is stored.
Its `CodexLive` argument carries what the stream knows beyond the item: the
agent text and command output so far, whether the turn ended while the item
was running (shown failed), and a fallback time. Usage is not part of it:
a stream attaches each call's usage itself, as an `OpUsage` record.

### Evolution rules

1. Readers ignore unknown fields, keep unknown block types (available through
   `Block.Raw`), and return lines with an unknown `type` as they are, so
   consumers can skip them.
2. New fields, block types and line types are additive and optional. `v` stays `1`. Old v1 lines without the new usage and model fields remain valid.
3. `v` is bumped only for a breaking change.

## Reading

`Decode` returns every line it could read. If it skipped any, it also returns a
`*SkippedLinesError` listing each skipped line's 1-based physical line number
and the reason. Blank lines are ignored (but still counted in line numbers).

A line is skipped when it:

- is not a JSON object (including malformed JSON),
- has no `v`, or a `v` other than `1`,
- has no `type`, or
- has a known field of the wrong JSON type, for example `"agent": 42`
  or `"input_tokens": "x"` (reason `invalid field: <name>`). A `ts` that is
  not a string is ignored rather than skipped.

For optional scalar fields, JSON `null` is treated like absence, not a wrong
type: strings (including `model`) become empty, token totals (including the
historical `input_tokens` and `output_tokens`) become zero, and cache-write
splits remain unknown (`nil`). These values do not cause a line to be skipped.

`Decode` normalizes three v1 quirks:

- A block that has `text` but no `type` gets `Type` `"text"`, on any line type.
- A `content` value that is a JSON string becomes a single text block.
- A `content` value that is `null`, absent, or any other non-array, non-string
  JSON value becomes no blocks. The line is still returned.

Every block carries `Raw`, the block exactly as stored (for string content,
the synthesized text block). A block that is not a JSON object, or whose known
fields have the wrong JSON type, is returned with only `Raw` set; it never
causes its line to be skipped.

```go
lines, err := transcript.Decode(b)
// strict:   if err != nil { return err }
// tolerant: log err and use lines

var skipped *transcript.SkippedLinesError
if errors.As(err, &skipped) {
	for _, s := range skipped.Skipped {
		log.Printf("line %d: %s", s.Line, s.Reason)
	}
}
```

`Parse` accepts any supported format. Input whose first non-blank line is a JSON
object with an integer `v` and a non-empty string `type` is decoded as is and
the options are ignored. Anything else is converted first.

```go
lines, err := transcript.Parse(native, transcript.Options{Agent: "claude-code", CLIVersion: "0.5.1"})
```

To get one checkpoint's portion of a session, convert with the boundary and
slice the decoded lines:

```go
full, boundary, err := transcript.ConvertWithBoundary(native, transcript.Options{
	Agent: "claude-code", CLIVersion: "0.5.1", StartLine: checkpointStart,
})
if err != nil {
	return err
}
lines, err := transcript.Decode(full)
// ...
checkpointLines := lines[boundary:]
```

There is no general-purpose writer for the format; the converters and the fold
produce it.

## Records and fold

A `Record` is one append-only change to a transcript. Records are never revised
once emitted, so they can be streamed and stored as plain appends. `Fold`
applies records in order and returns the transcript lines.

In this repository the fold is used by `Convert`: for Claude Code, Cursor and
Droid, it runs a `JSONLAdapter` that turns the native transcript into records
and then folds them, and for Pi a `PiAdapter`. The other converters still write
lines directly. The fold
is meant to be shared with a backend that folds the same records to show a live
session, so that a live view and a checkpoint's `transcript.jsonl` match byte
for byte.

A live producer uses the same adapter (`NewClaudeAdapter`, `NewCursorAdapter`,
`NewDroidAdapter`, `NewPiAdapter`). `Feed` accepts the transcript in chunks split anywhere and
returns the records for the lines it completed; `Flush` consumes a trailing
partial line and is called only once the transcript is final. Fold keys are
stable only when the adapter is fed from the transcript's first line, so a
producer that restarts replays from line 1. `Lines` reports how many source
lines have been consumed. Records carry the transcript's bytes as fed: redact
before feeding.

The Pi adapter keys each message by its Pi entry ID and sends it whole as a
`message` record, with its tool results spliced in; it sends the message again
when a result it shows changes, and `drop`s it when it leaves the active
branch (a `message` record restores it in place if the branch comes back). A
line that only grows the active branch is read without walking the tree, so a
session that only grows reads in linear time. Like the JSONL adapters, it has
no line size limit: a long line is held until its newline arrives and read
whole. `Convert` still fails on a Pi line of 10 MiB or more (newline
excluded), as it always has, so the fold matches `Convert` wherever `Convert`
succeeds.

### Record wire format v1

One JSON object per record. An omitted field and a zero-length one mean the same: in Go, a nil or empty `json.RawMessage` is absent. This is about the field, not its JSON value: `content` of `[]` or `null` is present, and the visibility rules below treat it differently from absent content.

| Field | Type | Ops | Notes |
| --- | --- | --- | --- |
| `op` | string | all | `message`, `append`, `result`, `usage` or `drop`. |
| `id` | string | all | The fold key: unique and stable within a session. Required. |
| `sourceId` | string | `message` | The agent's own message ID, printed as the line's `id`. Differs from `id` when the agent reuses an ID for a later, separate message. |
| `role` | string | `message` | `user` or `assistant`; becomes the line's `type`. Required on `message`. |
| `ts` | any JSON | `message`, `append` | Copied to the line's `ts` verbatim. An `append` without `ts` clears the timestamp. |
| `content` | any JSON | `message`, `append` | Normally an array of blocks. `message` replaces the content; `append` adds the blocks of an array and ignores anything else. Appending to content that is not an array discards that content, since it has no blocks. |
| `toolUseId` | string | `result` | The `tool_use` block to target, searching from the end. Empty targets the last `tool_use` block. |
| `result` | object | `result` | The tool result object described under Blocks. Required on `result`. |
| `usage` | object | `message`, `append`, `usage` | Optional per-call snapshot (`model`, `inputTokens`, `outputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `cacheWrite5mTokens`, `cacheWrite1hTokens`, `reasoningTokens`). When present it replaces the message's whole snapshot, and the line's usage fields come from it instead of `inputTokens`/`outputTokens`. |
| `inputTokens`, `outputTokens` | int | `message`, `append`, `usage` | A non-zero value replaces the previous one; agents report cumulative usage. |

What each op does to the message `id` names:

- `message` creates it, or restates it: role, source ID, timestamp and content are replaced, token counts and any usage snapshot are updated, and a dropped message is restored. A message keeps the position where it was first created.
- `append` concatenates content blocks and replaces the timestamp. Appending to non-array content (a string, `null`, or none) replaces it with the appended blocks, possibly none: the result is always an array.
- `result` sets the result of one `tool_use` block. Nothing changes when the content is not an array, when one of its blocks is not an object, or when no block matches.
- `usage` updates token counts and, when it carries `usage`, the per-call snapshot.
- `drop` hides the message.

Every op except `message` must target a message created earlier, in this batch or a previous one.

Compatibility: new optional fields may be added, and readers ignore fields they do not know. A new op is a breaking change: `Apply` rejects a batch containing an op it does not know.

### Incremental folding

A `Folder` folds incrementally. `Apply` validates a whole batch before changing anything and returns the IDs of the messages it touched. `Line(id)` returns one message's current line, or not visible when the message is dropped or is an assistant message whose content is `[]` or `null`. An assistant message with no content at all is visible and prints `"content":null`, as `Convert` always has. A live view re-renders the touched messages and removes those that are no longer visible:

```go
f := transcript.NewFolder(transcript.Options{Agent: "claude-code", CLIVersion: "0.5.1"})
touched, err := f.Apply(records)
if err != nil {
	return err
}
for _, id := range touched {
	if line, ok := f.Line(id); ok {
		render(id, line)
	} else {
		remove(id)
	}
}
```

Decode each streamed record into a zero `Record`. `encoding/json` leaves fields the input omits unchanged, so a reused variable would carry an earlier `ts` into an `append` that has none.

### Resource use

A `Folder` keeps every message in memory and has no limits of its own. Applying a record costs time in the record's size, with one exception: the first `append` or `result` after a `message` record splits that record's content into blocks, once. Until an `append` or a matching `result` takes the split blocks over as the content, they are kept beside the original bytes, so such a message briefly costs twice its size. Producing a line costs time in that message's size, and `Lines` in the size of all messages. A caller folding records from an untrusted source must bound the number and size of records and messages itself.
