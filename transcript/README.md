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
| `ts` | string | no | RFC 3339 timestamp. May be omitted or empty (codex, gemini and copilot write `""`). The generic JSONL converter copies the source timestamp verbatim, so a non-string value can occur; readers ignore it. |
| `id` | string | no | Assistant message ID. Streaming fragments with the same ID are merged into one line. |
| `input_tokens`, `output_tokens` | int | no | Assistant lines only. |
| `content` | array of blocks, string, or `null` | yes | Normally an array. A string can appear when an assistant message's source content was a string. `null` can appear because the writer does not omit empty content. |

### Blocks

- User text: `{"id"?: string, "text": string}`. There is no `type` field (a v1 quirk).
- Assistant text: `{"type": "text", "text": string}`.
- Tool use: `{"type": "tool_use", "id"?: string, "name": string, "input": any, "result"?: {"output": string, "status": "success"|"error", "file"?: {"filePath": string, "numLines"?: int}, "matchCount"?: int}}`.
  Tool results are inlined into the `tool_use` block that requested them.
- Image: passed through from the source verbatim. The image source is either
  inline base64 or an `entire-asset:assets/…` placeholder left by image
  externalization.
- Any other block type is passed through verbatim. Thinking blocks are dropped
  by the converters.

### Evolution rules

1. Readers ignore unknown fields, keep unknown block types (available through
   `Block.Raw`), and return lines with an unknown `type` as they are, so
   consumers can skip them.
2. New fields, block types and line types are additive and optional. `v` stays `1`.
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
Droid, it runs an adapter that turns the native transcript into records and then
folds them. The other converters still write lines directly. The fold is meant
to be shared with a backend that folds the same records to show a live session,
so that a live view and a checkpoint's `transcript.jsonl` match byte for byte;
that integration does not exist yet.

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
| `inputTokens`, `outputTokens` | int | `message`, `append`, `usage` | A non-zero value replaces the previous one; agents report cumulative usage. |

What each op does to the message `id` names:

- `message` creates it, or restates it: role, source ID, timestamp and content are replaced, token counts are updated, and a dropped message is restored. A message keeps the position where it was first created.
- `append` concatenates content blocks and replaces the timestamp. Appending to non-array content (a string, `null`, or none) replaces it with the appended blocks, possibly none: the result is always an array.
- `result` sets the result of one `tool_use` block. Nothing changes when the content is not an array, when one of its blocks is not an object, or when no block matches.
- `usage` updates token counts.
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
