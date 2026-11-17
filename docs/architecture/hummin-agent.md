# The hummin External Agent

[hummin](https://github.com/rennf93/hummin) is a fork of the pi coding agent:
the binary is `hummin` instead of `pi`, the config directory is `.hummin`
instead of `.pi`, and sessions live under `~/.hummin/agent/sessions/`. Session
files keep pi's JSONL format, so this integration mirrors the in-tree pi agent
(see [agent-guide.md](agent-guide.md), "JSONL Format (Pi pattern)") while
plumbing everything through the [external agent protocol](external-agent-protocol.md)
instead of a Go package.

This repo ships a reference plugin at `agents/hummin/entire-agent-hummin`: a
single-file Node script (Node >= 20, zero npm dependencies, executable bit set)
that implements the full required protocol surface plus three optional
capabilities. hummin is developed outside this repository, so the plugin stays
out of the Go tree; that is exactly what the external agent protocol is for.

## Install

```sh
# from this repository
cp agents/hummin/entire-agent-hummin /usr/local/bin/    # any dir on $PATH
chmod +x /usr/local/bin/entire-agent-hummin

cd /path/to/your/repo
entire agent add hummin
```

`entire agent add <name>` resolves `entire-agent-<name>` on `$PATH` even
without the `external_agents` setting, installs the agent's hooks, and turns
`external_agents` on in the untracked `.entire/settings.local.json` so hook
invocations keep discovering the plugin afterwards. The managed plugin
directory (`$XDG_DATA_HOME/entire/plugins/bin`) works as a `$PATH` target too.

## Session layout

hummin stores one JSONL file per session:

```
~/.hummin/agent/sessions/--<encoded-cwd>--/<timestamp>_<session-id>.jsonl
```

The encoding removes the leading separator, turns `/`, `\` and `:` into `-`,
and wraps the result in `--` on both sides (`/Users/foo/repo` encodes as
`--Users-foo-repo--`). The plugin mirrors this exactly, including hummin's two
relocation variables:

- `PI_CODING_AGENT_SESSION_DIR` replaces the session store outright (flat, no
  per-repo nesting), as hummin does.
- `PI_CODING_AGENT_DIR` moves the agent home (default `~/.hummin/agent`).

hummin's `sessionDir` settings.json key is not read by the plugin; the
in-tree pi agent does not read pi's either.

## Commands

Required surface:

| Command | Behavior |
|---|---|
| `info` | name `hummin`, type `Hummin`, protected dir `.hummin`, capabilities below, `caller_env_vars: ["PI_SESSION_ID"]` |
| `detect` | `hummin` binary present and executable on `$PATH` |
| `get-session-id` | payload `session_id`, else the `<uuid>` in a `<timestamp>_<uuid>.jsonl` ref, else `raw_data.session_id` / `raw_data.session_file` |
| `get-session-dir` | the encoded store above, honoring the relocation variables |
| `resolve-session-file` | glob `*_<id>.jsonl`, lexicographically latest wins; absolute ids pass through; falls back to `<id>.jsonl` |
| `read-session` | reads the hummin JSONL into an AgentSession: `native_data` is the raw file, `session_id` / `start_time` / `repo_path` come from the `session` header line |
| `write-session` | writes `native_data` back to `session_ref` (0600), refusing refs outside the reported store |
| `read-transcript` | raw transcript bytes |
| `chunk-transcript` | JSONL chunking at newline boundaries, byte-compatible with `agent.ChunkJSONL` (50 MB default) |
| `reassemble-transcript` | inverse of chunking, byte-compatible with `agent.ReassembleJSONL` |
| `format-resume-command` | `hummin --session <id>` (verified against hummin's `packages/coding-agent/src/cli/args.ts`); empty id falls back to `hummin --continue` |

Capabilities `hooks: true`, `transcript_analyzer: true` and
`token_calculator: true` add:

| Command | Behavior |
|---|---|
| `install-hooks` / `uninstall-hooks` / `are-hooks-installed` | manage `.hummin/extensions/entire/index.ts`, an Entire-owned hummin extension (marker string protects foreign files; `--force` overwrites) |
| `parse-hook` | maps the extension's payloads to lifecycle events |
| `get-transcript-position` | JSONL line count (missing file reports 0) |
| `extract-modified-files` | `write` / `edit` tool-call paths from assistant messages, active-branch filtered |
| `extract-prompts` | user message text, active-branch filtered |
| `calculate-tokens` | sums assistant-message `usage`: `input` → `input_tokens`, `output` → `output_tokens`, `cacheRead` → `cache_read_tokens`, `cacheWrite` → `cache_creation_tokens`, one `api_call_count` per message that carries usage |

The transcript commands treat `--offset` as a line offset and return line
counts as positions, the same convention the in-tree pi agent uses for this
format. Active-branch resolution walks `parentId` links from the last message
and ignores abandoned `/fork` and `/tree` branches, so token counts, file
lists, and prompts are not double-counted; `agent_end` events also backfill
`model` from the most recent active-branch assistant message, matching the
in-tree pi agent's model backfill.

## Hook mapping

The installed extension forwards hummin lifecycle events to
`entire hooks hummin <event>`:

| hummin event | Entire event | Notes |
|---|---|---|
| `session_start` | SessionStart (1) | session id taken from the session filename |
| `before_agent_start` | TurnStart (2) | carries `prompt` and the live session file |
| `agent_end` | TurnEnd (3) | TurnEnd is the source of truth for checkpointing |
| `session_shutdown` | none | agent_end already checkpoints; see the in-tree pi agent for the rationale |

Payloads that carry no resolvable session id are dropped (a nested
`hummin --no-session` run must not borrow the parent's session identity), and
the extension sets `ENTIRE_PI_NESTED` so a nested hummin process spawned by a
subagent extension does not forward its own lifecycle as the user's session.
The extension also hardens agent-driven `bash` calls with
`GIT_TERMINAL_PROMPT=0`, as the pi extension does.

## Testing

`agents/hummin/test.sh` exercises every command against a synthetic hummin
session in a temp dir, adds read-only checks against a real hummin session
store when one exists on the machine, and, when a Go toolchain is available,
builds the CLI and drives `entire hooks hummin <event>` through full external
agent discovery in a throwaway repo:

```sh
agents/hummin/test.sh        # prints PASS/FAIL per command, exits non-zero on failure
```

## Limitations

- `skill_events` are not reported: the external protocol's Event object has no
  skill field, so skill invocations hummin tracks in its extension events are
  not conveyed.
- Sessions and checkpoints are attributed to the agent name `hummin`; the
  plugin does not distinguish hummin's fork-specific features from pi's.
- `transcript_preparer`, `compact_transcript`, `text_generator`,
  `streaming_text_generator`, `hook_response_writer` and
  `subagent_aware_extractor` are declared false and their subcommands are not
  implemented.
