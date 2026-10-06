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
| `ts` | string | no | RFC 3339 timestamp. Omitted when the source has none. |
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
- has a known field of the wrong JSON type, for example `"agent": 42`,
  `"input_tokens": "x"` or `"ts": 5` (reason `invalid field: <name>`).

`Decode` normalizes three v1 quirks:

- A user block that has `text` but no `type` gets `Type` `"text"`.
- A `content` value that is a JSON string becomes a single text block.
- A `content` value that is `null`, absent, or any other non-array, non-string
  JSON value becomes no blocks. The line is still returned.

Every block carries `Raw`, the block exactly as stored. A block that is not a
JSON object, or whose known fields have the wrong JSON type, is returned with
only `Raw` set; it never causes its line to be skipped.

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
object with an integer `v` and a `type` of `user` or `assistant` is decoded as
is and the options are ignored. Anything else is converted first.

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

There is no writer for the format; only the converters produce it.
