# gmeta POC Plan

## Context

gmeta is a proposed standard for attaching arbitrary metadata to Git projects, designed by Scott Chacon (GitButler). It defines a tree layout spec for serializing `(target, key, value)` tuples into Git trees on `refs/meta/local/main`, exchangeable over standard Git protocol.

Entire's `Entire-Checkpoint` trailer is functionally equivalent to jj's `change-id` — a stable identifier embedded in the commit message that survives rebase/cherry-pick. This maps directly to gmeta's `change-id` target type, meaning checkpoint metadata keyed to `change-id:<checkpoint-id>` survives rewrites without any additional tooling.

Reference implementation: `/Users/soph/Work/entire/research/gmeta`
Spec: `gmeta/spec/README.md`

## Goal

Write Entire checkpoint metadata in gmeta exchange format alongside v1/v2, proving interop by reading it back with the Rust `gmeta` CLI.

## Coexistence Policy

For the current rollout, `v2` and `gmeta` are allowed to coexist, but only as
an explicit opt-in via separate settings. There is no implicit dependency
between them.

- `strategy_options.checkpoints_v2 = true` enables v2 write/read/push behavior
- `strategy_options.gmeta = true` enables gmeta write/push behavior
- If both are true, parallel behavior is intentional and temporary
- `gmeta` does not require `checkpoints_v2`, and `checkpoints_v2` does not
  require `gmeta`
- Read precedence remains unchanged unless a future explicit setting is added
  (for example, `prefer_gmeta_reads`)

Operationally:
- `v2` remains the primary Entire-native storage format
- `gmeta` is an exchange/interoperability format written and pushed separately
- gmeta fetch/materialize is independent of v2 fetch logic
- gmeta failures must remain non-fatal to the v1/v2 checkpoint flow
- Current fallback reads from `gmeta` are compatibility behavior, not backend
  selection policy. They exist to keep resume/log lookup working while the
  rollout is in progress and should not be treated as a stable precedence model.

## Architecture

No go-git changes needed. gmeta is just a tree layout convention — Entire already has all the plumbing:
- `BuildTreeFromEntries`, `CreateBlobFromContent`, `CreateCommit` in `checkpoint/`
- `V2GitStore` as prior art for a second store struct composing `GitStore`
- Dual-write pattern already exists in `manual_commit_condensation.go`

## Tree Layout Mapping

### Fanout Rule

Per gmeta spec (`spec/exchange-format/exchange.md`), `change-id` targets use: **first 2 hex chars of SHA-1(target-value)**.

So for checkpoint ID `a3b2c4d5e6f7`: fanout = `sha1("a3b2c4d5e6f7")[:2]`.

This is NOT the same as `checkpoint-id[:2]`. The fanout is derived from a hash of the value, not the value itself.

### Per-Session Encoding

A checkpoint can contain multiple sessions (concurrent agents, `entire attach`). gmeta has no native session concept, so we namespace per session under the checkpoint's change-id target:

```
change-id/<fanout>/<checkpoint-id>/
├── entire/
│   ├── strategy/__value                           # "manual-commit"
│   ├── cli-version/__value                        # CLI version
│   ├── branch/__value                             # branch name
│   ├── checkpoints-count/__value                  # total across all sessions
│   ├── combined-attribution/
│   │   ├── calculated-at/__value
│   │   ├── agent-lines/__value
│   │   ├── agent-removed/__value
│   │   ├── human-added/__value
│   │   ├── human-modified/__value
│   │   ├── human-removed/__value
│   │   ├── total-committed/__value
│   │   ├── total-lines-changed/__value
│   │   ├── agent-percentage/__value
│   │   └── metric-version/__value
│   └── files-touched/__set/                       # merged from all sessions
│       ├── <sha1(value)[:10]>                     # "src/foo.go"
│       └── <sha1(value)[:10]>                     # "src/bar.go"
├── session/
│   ├── ids/__list/                                # ordered session IDs
│   │   ├── <timestamp>-<hash5>                    # "2026-01-13-uuid1"
│   │   └── <timestamp>-<hash5>                    # "2026-01-13-uuid2"
│   ├── <session-id-1>/
│   │   ├── agent/
│   │   │   ├── name/__value                       # "Claude Code"
│   │   │   ├── model/__value                      # "claude-opus-4-6[1m]"
│   │   │   └── provider/__value                   # "anthropic"
│   │   ├── entire/
│   │   │   ├── token-usage/
│   │   │   │   ├── input/__value
│   │   │   │   ├── output/__value
│   │   │   │   ├── cache-read/__value
│   │   │   │   ├── cache-creation/__value
│   │   │   │   ├── api-calls/__value
│   │   │   │   └── total/__value
│   │   │   └── attribution/
│   │   │       ├── calculated-at/__value
│   │   │       ├── agent-lines/__value
│   │   │       ├── agent-removed/__value
│   │   │       ├── human-added/__value
│   │   │       ├── human-modified/__value
│   │   │       ├── human-removed/__value
│   │   │       ├── total-committed/__value
│   │   │       ├── total-lines-changed/__value
│   │   │       ├── agent-percentage/__value
│   │   │       └── metric-version/__value
│   │   ├── prompt/__value                         # user prompt(s)
│   │   └── transcript/__list/                     # chunked JSONL entries
│   │       ├── <timestamp>-<hash5>
│   │       └── <timestamp>-<hash5>
│   └── <session-id-2>/
│       ├── agent/
│       │   ├── name/__value
│       │   ├── model/__value
│       │   └── provider/__value
│       ├── prompt/__value
│       └── transcript/__list/
│           └── ...
```

This means:
- Checkpoint-level fields (`strategy`, `branch`, `files-touched`) live at the top level
- Each session gets its own subtree under `session/<session-id>/`
- `session/ids` is a list preserving session ordering
- Adding a session (via `entire attach` or concurrent agent) appends to `session/ids` and creates a new `session/<id>/` subtree — no overwrites
- Entire-specific structured metrics live under `session/<id>/entire/...` so they stay queryable without pretending to be standard gmeta keys

### Single-Session Shorthand

When there is exactly one session, the layout is identical — just one `session/<id>/` subtree. No special-casing needed.

## Security: Redaction

All data written to `refs/meta/local/main` MUST be redacted before creating git blobs, matching v1/v2 behavior:

- Transcripts: pass through `redact.JSONLBytes()` before chunking into list entries
- Prompts: pass through `redact.String()` before writing `__value` blob
- File paths in `files-touched`: no redaction needed (already public via git history)

This is a hard requirement, not an implementation detail. The gmeta ref is permanent and pushable.

## Write Paths

`GmetaStore` must be wired into **all** paths that write committed checkpoints, not just condensation:

| Write path | File | When | Task checkpoints |
|---|---|---|---|
| Condensation | `manual_commit_condensation.go` | PostCommit hook — session data condensed on user commit | Full support |
| Stop finalization | `manual_commit_hooks.go` | Stop hook — transcript/prompts replaced with final versions | Full support |
| Attach | `attach.go` | `entire attach` — adds external session data to existing checkpoint | Full support |
| Migrate | `migrate.go` | `entire migrate` — copies v1 checkpoints to v2 (and now gmeta) | **Session data only** |

**Migrate and task checkpoints**: Migration copies task metadata from v1 to v2 via raw tree splicing (`copyTaskMetadataToV2`), not through `WriteCommittedOptions` — the opts only carry `IsTask` and `ToolUseID`, not `AgentID`, `CheckpointUUID`, subagent transcript content, or incremental payloads. Building a v1-tree-walker that re-extracts all task fields into gmeta format is not worth the complexity for a one-time operation. The gmeta migrate path writes session-level data only; task metadata remains available in v1 and v2.

### Task Checkpoint Encoding

Task checkpoints (subagent work) are written under `session/<session-id>/task/<tool-use-id>/`:

```
session/<session-id>/task/<tool-use-id>/
├── agent-id/__value                              # subagent identifier (opts.AgentID)
├── checkpoint-uuid/__value                       # UUID for transcript truncation on rewind (opts.CheckpointUUID)
├── transcript/__list/                            # subagent transcript (chunked, from opts.SubagentTranscriptPath)
│   ├── <timestamp>-<hash5>
│   └── <timestamp>-<hash5>
└── incremental/__list/                           # incremental checkpoints (ordered)
    ├── <timestamp>-<hash5>                       # JSON: {type, tool_use_id, data}
    └── <timestamp>-<hash5>
```

These fields map 1:1 to what `WriteCommittedOptions` already carries: `AgentID`, `CheckpointUUID`, `SubagentTranscriptPath`, and `IncrementalData`/`IncrementalType`/`IncrementalSequence`. No new plumbing needed.

**Final task checkpoints** write `agent-id`, `checkpoint-uuid`, and `transcript` entries.

**Incremental task checkpoints** (`opts.IsIncremental`) append to `incremental/__list/`. Each entry is a JSON blob with `{type, tool_use_id, timestamp, data}` — same content as v1's `checkpoints/NNN-<tool-use-id>.json` but as a gmeta list entry instead of numbered files.

Redaction applies to both: subagent transcripts via `redact.JSONLBytes()` (falling back to `redact.Bytes()`), incremental data via `redact.JSONLBytes()`.

### Task Key Policy

Task checkpoint keys are **Entire-specific gmeta data**. They are intentionally
serialized in the gmeta exchange tree so they can be transported and materialized
by generic gmeta tooling, but they are not expected to have any built-in
meaning to the Rust `gmeta` CLI beyond normal key/value storage.

Decision for the POC:
- `session:<id>:task:<tool-use-id>:...` keys are treated as opaque vendor data
- The Rust `gmeta` CLI only needs to preserve and expose them via normal
  `get`/`inspect` behavior
- Entire remains responsible for interpreting `agent-id`, `checkpoint-uuid`,
  `transcript`, and `incremental` semantics

Examples:

```bash
gmeta get change-id:<checkpoint-id> session:<session-id>:task:<tool-use-id>:agent-id
gmeta get change-id:<checkpoint-id> session:<session-id>:task:<tool-use-id>:checkpoint-uuid
gmeta get change-id:<checkpoint-id> session:<session-id>:task:<tool-use-id>:transcript
gmeta get change-id:<checkpoint-id> session:<session-id>:task:<tool-use-id>:incremental
```

This keeps the exchange format interoperable without requiring the Rust CLI to
understand Entire's subagent/task model.

### Entire Metrics Schema

Token usage and attribution are Entire-specific, so they are namespaced under
`session/<session-id>/entire/` rather than mixed into the generic session keys.
This keeps them queryable via gmeta while making it explicit that they are
vendor-defined fields, not part of the base spec.

**Token usage** is stored as individual scalar values under
`session/<session-id>/entire/token-usage/`:

```
session/<session-id>/entire/token-usage/
├── input/__value
├── output/__value
├── cache-read/__value
├── cache-creation/__value
├── api-calls/__value
└── total/__value
```

These map directly to `agent.TokenUsage`. Storing them as separate values keeps
them easy to query with `gmeta get change-id:<id> session:<sid>:entire:token-usage:input`.

**Attribution** is stored as individual scalar values under
`session/<session-id>/entire/attribution/`:

```
session/<session-id>/entire/attribution/
├── calculated-at/__value
├── agent-lines/__value
├── agent-removed/__value
├── human-added/__value
├── human-modified/__value
├── human-removed/__value
├── total-committed/__value
├── total-lines-changed/__value
├── agent-percentage/__value
└── metric-version/__value
```

These map directly to `checkpoint.InitialAttribution`.

**Checkpoint-level combined attribution** is stored at `entire/combined-attribution/`
using the same scalar field names as session attribution. This maps directly to
`checkpoint.CheckpointSummary.CombinedAttribution` and is updated after the full
set of sessions for a checkpoint is known.

Decision for the POC:
- Token usage is written and read at the session level
- Attribution is written and read at the session level
- Combined attribution is written and read at the checkpoint level

### Write API

```go
type GmetaStore struct {
    repo *git.Repository
    gs   *GitStore
}

// WriteCommitted writes or appends a session to a checkpoint in gmeta format.
// If the checkpoint already exists, the new session is added alongside existing ones.
// Handles both session and task checkpoints (including incremental).
func (s *GmetaStore) WriteCommitted(ctx context.Context, opts WriteCommittedOptions) error

// UpdateCommitted replaces transcript and prompts for an existing session.
// Used at stop time to finalize with complete session data.
// Replaces the transcript list entries and prompt value for the given session.
func (s *GmetaStore) UpdateCommitted(ctx context.Context, opts UpdateCommittedOptions) error
```

Both methods required from day one. `UpdateCommitted` replaces the `session/<id>/transcript/__list/` entries and `session/<id>/prompt/__value` blob for the target session. This matches the replace semantics v1/v2 already use — gmeta lists are append-only in the *exchange* model, but within a single serialization we control the full tree, so replacing entries before push is fine.

### Wiring Pattern

Same as v2 — non-fatal wrapper so gmeta failures don't break existing flow:

```go
gmetaStore := cpkg.NewGmetaStore(repo)
if err := gmetaStore.WriteCommitted(ctx, opts); err != nil {
    logging.Warn(ctx, "gmeta write failed", slog.String("error", err.Error()))
}
```

Applied in all four write paths above.

## Steps

### Step 1: GmetaStore struct + WriteCommitted + UpdateCommitted

New file: `cmd/entire/cli/checkpoint/gmeta_store.go`

Implementation details:
- String values: blob at `<key-path>/__value`
- List values: blobs at `<key-path>/__list/<timestamp-ms>-<hash5>`
- Set values: blobs at `<key-path>/__set/<sha1-of-value[:10]>` containing the value
- Target base path: `change-id/<sha1(checkpoint-id)[:2]>/<checkpoint-id>/`
- Ref: `refs/meta/local/main`
- Redaction: `redact.JSONLBytes()` for transcripts, `redact.String()` for prompts
- Multi-session: read existing tree, find/create `session/<id>/` subtree

Reuses from `GitStore`: `BuildTreeFromEntries`, `CreateBlobFromContent`, `CreateCommit`

### Step 2: Wire into all write paths

Add `GmetaStore` calls to condensation, stop finalization, attach, and migrate. Non-fatal in all paths.

### Step 3: Validate interop

Install the Rust gmeta CLI, point at a repo with Entire checkpoints:

```bash
gmeta get change-id:<checkpoint-id> session:<session-id>:agent:model
gmeta get change-id:<checkpoint-id> session:<session-id>:prompt
gmeta get change-id:<checkpoint-id> session:<session-id>:transcript
gmeta get change-id:<checkpoint-id> entire:files-touched
```

Verify data round-trips correctly.

## Open Questions

- **Push/fetch**: gmeta uses `refs/meta/local/main` for push, `refs/meta/remotes/main` for fetch. Need to decide if this coexists with or replaces v2's push mechanism. Likely coexists for now.
- **Read-side**: Not needed for POC. Future work to have `entire session list/show` read from gmeta format.

## Estimated Scope

~300-400 lines for `GmetaStore` (write + update + multi-session handling + redaction), ~20 lines to wire into all four write paths. Tests + interop validation on top.
