# Generic Agent Adapter

A configurable agent adapter for Entire CLI that works with any coding agent producing JSONL session transcripts. Primary targets include [OpenClaw](https://openclaw.ai) and [AMP](https://sourcegraph.com/amp) (Sourcegraph's coding agent).

## Configuration

Create `.entire/generic.json` in your repository root:

```json
{
  "transcript_dir": "~/.openclaw/agents/main/sessions",
  "transcript_pattern": "*.jsonl",
  "agent_type": "OpenClaw",
  "session_id_from": "field:id"
}
```

### Fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `transcript_dir` | Yes | — | Directory containing session transcript files. Supports `~` expansion. |
| `transcript_pattern` | No | `*.jsonl` | Glob pattern for matching transcript files. |
| `agent_type` | No | `Generic Agent` | Display name shown in Entire metadata and trailers. |
| `session_id_from` | No | `filename` | How to extract session IDs. `filename` strips the extension; `field:<name>` reads a JSON field from the first JSONL line. |

### Session ID Extraction

- **`filename`** (default): Session ID is the transcript filename without extension. E.g., `abc123.jsonl` → session ID `abc123`.
- **`field:<name>`**: Reads the named field from the first line of the JSONL file. E.g., `field:id` extracts the `id` field from `{"type":"session","id":"abc123",...}`.

## Detection

The generic agent is detected when `.entire/generic.json` exists in the repository root.

## File Watching

Since the generic adapter can't install hooks into arbitrary agents, it implements the `FileWatcher` interface. Entire watches the configured `transcript_dir` for new or modified files matching `transcript_pattern`.

## Modified File Extraction

The adapter attempts to extract modified file paths from JSONL transcripts by walking the JSON structure looking for tool call objects with common file path fields (`file_path`, `path`, `file`, `filename`). This works across multiple agent transcript formats. If no files can be extracted (unknown format), it gracefully returns nil and Entire falls back to git-status-based file detection.
