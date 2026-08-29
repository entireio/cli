# Token Reports

How `entire session tokens`, `entire checkpoint tokens` and `entire tokens
profile` turn recorded usage into a **breakdown-first** report: where the
tokens went (tools, skills, subagents, context replay, assistant text), what
each part cost relative to the rest, and at most two sentences of advice. This
page is the contract the web mirrors and the reference a new agent
implementer works from. The code is authoritative; every rule below names the
function that owns it, and the longer rule sets are summarised here and
specified in full in that function's doc comment.

Packages:

- `cmd/entire/cli/tokenreport/` — pure functions over `agent/types` values, no
  IO: `cost.go` (price ratios, cost shares), `attribution.go` (contributor
  table), `recommend.go` (causes, gates, sentences), `profile.go` (what each
  agent records), `legacy.go` (pre-v0.11 checkpoint scope and dedupe),
  `format.go` (`FormatTokenCount`, `FormatPercent`, `FormatDuration`).
- `cmd/entire/cli/agent/types/token_attribution.go` — the value types an
  agent's attributor returns; `agent/token_attribution.go` — the
  `TokenAttributor` interface; `agent/<name>/token_attribution.go` — one
  implementation per attributing agent.
- `cmd/entire/cli/transcript/tool_detail.go` — `ToolDetail`, the one place the
  drill-down rules live.
- `cmd/entire/cli/session_tokens.go`, `checkpoint_tokens*.go`,
  `tokens_profile.go`, `tokens_load.go`, `tokens_render.go` — the commands:
  load → `tokenreport` → render, sharing one loader core and one set of
  writers so both single-report commands print the same shapes from the same
  values.

## 1. The three commands

**`entire session tokens [id] [--current] [--json | --agent-brief]`** — one
live or ended session.

- Totals are recomputed from the live transcript (`strategy.ResolveTranscriptPath`,
  whole file, start line 0): Σ attributed calls + Σ subagent transcripts read
  from `paths.SubagentsDir(dir(transcript), sessionID)` (`agent-<id>.jsonl`).
  When the transcript cannot be attributed the totals fall back to the state's
  running `token_usage` and a note says why.
- `source` is `"transcript"` or `"session_state"`.

**`entire checkpoint tokens <id> [--compare <id>] [--json | --agent-brief]`** —
one committed checkpoint, all its sessions.

- Totals are recomputed per session from the stored raw `full.jsonl`
  (`store.ReadSessionContent(ctx, id, i).Transcript` — never the compact
  `transcript.jsonl`, which drops cache classes, thinking and the per-call
  model) sliced at that session's `token_transcript_start`; subagent rows come
  from `store.ReadTaskRecords(ctx, id)` (`tasks/<tool-use-id>/task.json`)
  joined to the spawning call by `ToolUseID`. Fallbacks in §9.
- `source` is `"transcript"` or `"committed_checkpoint"`.

**`entire tokens profile [--limit N | --all] [--json]`** — the latest 50
committed checkpoints by default, grouped by agent.

- Reads committed metadata only (root summary + per-session `metadata.json`),
  dedupes legacy running-total rows per session, reads no transcript — so it
  has no attribution and no recommendations, and says so.
- `source` is `"committed_checkpoints"`.

**`entire tokens`** (bare) runs `session tokens --current`; with no session in
the worktree it prints a one-line hint and exits 0.

The `tokens` group is experimental (visible in developer/nightly builds,
hidden in stable, always runnable); `entire labs` advertises `tokens`, `tokens
profile`, `session tokens` and `checkpoint tokens`. Both single-report
commands share one per-session analysis (`attributeCheckpointTokenSession` /
`attributeSessionTokens` → `finishSessionTokenAnalysis` →
`assembleTokenReportView`), so a session's live report and its later
checkpoint report compute every figure the same way; the live report feeds the
session state to it in checkpoint-metadata shape (`sessionTokensMetadata`).
Nothing in either fails the command: every problem becomes a Notes line.

## 2. Data model

### 2.1 `types.TokenUsage` — four classes, two subsets

```go
type TokenUsage struct {
    InputTokens           int    `json:"input_tokens"`            // fresh, uncached
    CacheCreationTokens   int    `json:"cache_creation_tokens"`   // cache writes
    CacheReadTokens       int    `json:"cache_read_tokens"`       // cache hits
    OutputTokens          int    `json:"output_tokens"`
    APICallCount          int    `json:"api_call_count"`
    ThinkingTokens        int    `json:"thinking_tokens,omitempty"`          // SUBSET of OutputTokens
    CacheCreation1hTokens int    `json:"cache_creation_1h_tokens,omitempty"` // SUBSET of CacheCreationTokens
    Model                 string `json:"model,omitempty"`                    // set on subagent entries; "" when mixed
    SubagentTokens        *TokenUsage `json:"subagent_tokens,omitempty"`
}
```

- The four classes are disjoint and sum to what the provider billed. **Volume**
  everywhere in the reports is those four summed (`tokenVolume` /
  `tokenreport.volume`); subset fields and the call count are not part of it.
- `thinking_tokens` and `cache_creation_1h_tokens` are read verbatim from the
  agent's own usage fields and are never added to a total. A 0 in a summed
  metadata row is ambiguous — "none" or "not recorded" — which is why pricing
  distinguishes metadata rows from per-call rows (§3.4) and why `AgentProfile`
  (§6) decides whether a report prints `0` or `not recorded`.
- `types.AddTokenUsage` is the single summing primitive (saturating, recursing
  into `SubagentTokens`, `Model` kept when equal and cleared to `""` when two
  models are summed). Every total in the reports goes through it or through
  `flattenTokenUsage`, which is built on it.

### 2.2 Checkpoint scope: `token_usage_version` and `token_transcript_start`

The root `metadata.json` of a checkpoint written by this CLI from v0.11 (the
release that ships this work) stamps `token_usage_version: 2`
(`checkpoint.TokenUsageVersionDelta`): every session's `token_usage` is a
per-checkpoint delta, and each session's `metadata.json` carries
`token_transcript_start`, the `full.jsonl` line (or message index — §4.2)
where that delta begins. `checkpoint tokens` slices the stored transcript **at
`token_transcript_start`, never at `checkpoint_transcript_start`**: a
carry-forward resets the transcript offset to 0 for a self-contained
transcript but not the token window, so slicing at the transcript offset
would re-create the cumulative-total bug the version stamp exists to mark. See
`docs/architecture/sessions-and-checkpoints.md`, "`token_usage` contract",
for the write side.

**Legacy rows** are any checkpoint whose root `token_usage_version` is below 2
(absent decodes as 0; 1 was never written). A legacy row at
`checkpoint_transcript_start == 0` may be a delta or the session's running
total (`tokenreport.ClassifyScope` → `ScopeLegacyFromStart`); there is no
per-row signal to tell which, so `tokenreport.DedupeLegacyCheckpoints`
resolves it by order within each session — the latest such row is the anchor,
everything before it is dropped, everything after it is kept. The exact rule,
its ordering and its `CreatedAt` precondition are specified on that function.
Only `tokens profile` dedupes; a single legacy checkpoint is shown as-is with
the header line *"Token scope: legacy — may be the session's running total
(written before v0.11)"* and `legacy.cumulative: true` in JSON.

### 2.3 Attribution value types (`agent/types/token_attribution.go`)

| type | meaning |
|---|---|
| `ToolUseRef{ID, Tool, Detail, SkillName, SubagentType, Model}` | one tool call an API call emitted. `Detail` is the drill-down string (§5) — never the raw command. `SkillName` for a skill load, `SubagentType` for a spawn; `Model` is the model the *spawning* agent recorded for the subagent (Claude Code's requested alias `input.model`; OpenCode's actual `state.metadata.model.modelID`), `""` otherwise. |
| `ToolResultRef{ToolUse, Bytes}` | a result an API call consumed as new input. `ToolUse` is resolved from the FULL transcript, so a result whose call precedes the slice is still labelled; `Bytes` is the result's size and drives proportional attribution of fresh input. |
| `CallUsage{Usage, UsageUnknown, Model, Effort, At, Line, ActiveSkill, Emitted, Consumed}` | one API call. `Usage` is this call only (no subagent tokens), with the subset fields where the agent records them. `UsageUnknown` marks a call the agent recorded no usage for — it still counts, its `Emitted` refs still label the next call's `Consumed`, and reports say "N calls with no usage recorded" rather than treating 0 as measured. `Line` is in the same unit as `AttributeTokens`' `startLine`. `ActiveSkill` is the harness-stamped skill active on the call (Claude Code `attributionSkill`). |
| `SubagentRecord{ToolUseID, SubagentType, Model, Usage, Start, End}` | a subagent's own usage from its transcript (live) or a committed task record. Model precedence, stated once: `record.Model` > `record.Usage.Model` > the emitting `ToolUseRef.Model` (the requested alias). `Usage` nil = unavailable; `End` zero on a committed record = still in flight when the checkpoint was written. |
| `Attribution{Calls, Subagents, Start, End, AgentReportedCost}` | one transcript slice: calls in transcript order; subagents discovered from the FULL transcript when `subagentsDir != ""`; earliest/latest timestamp in the slice; the agent's own dollar figure summed over the slice (Pi `usage.cost.total`, OpenCode `info.cost`; 0 = not recorded). |

## 3. Cost model (`cost.go`)

### 3.1 Weights, not prices

A `Weights` row holds a model's per-token price **ratios** relative to its own
base input price (`Input` is 1). Multiplying token counts by weights gives
dimensionless **cost units**, comparable only within one model or, across
models, as shares of a total. No currency is ever computed by Entire; every
report that prices anything carries the note *"Cost shares use <Provider>
list-price ratios (…), not your plan's rates."* The agent's own dollar figure,
where one exists, is shown separately as `Agent-reported cost $x.xx`
(`agent_reported_cost` in JSON).

`Provider` (`anthropic` | `openai` | `google`) is coarse, for JSON and notes;
the pricing row is chosen by `Family`. `FamilyFor(model)` matches the trimmed,
lower-cased model id by prefix in this order (`cli.formatModel`'s dispatch
order, refined to the rows):

```
claude-*                                  → anthropic
gpt-5.6-sol*                              → openai-5.6-sol
gpt-5.6-terra*, gpt-5.6-luna*             → openai-5.6-terra-luna
gpt-5.4*, gpt-5.5*                        → openai-6x        (incl. -mini/-nano)
gpt-5, gpt-5-*, gpt-5.1*, gpt-5.2*, gpt-5.3-codex* → openai-8x
gemini-2.5-flash*                         → gemini-2.5-flash
gemini-2.5-pro*                           → gemini-2.5-pro
gemini-3.5-flash*                         → gemini-3.5-flash
gemini-3.6-flash*, gemini-3.7-flash*      → gemini-3.6-flash
anything else (empty, gemini-3-flash-preview, an unlisted gpt-5.6 variant,
older GPT/Gemini lines)                   → ok=false: volume only
```

### 3.2 The ratio table

Verified **2026-08-28** against the providers' public pricing pages
(`familyWeights`; re-verify quarterly and bump the date in the code comment):

- Anthropic: <https://www.anthropic.com/pricing> and
  <https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching>
- OpenAI: <https://openai.com/api/pricing/>
- Google: <https://ai.google.dev/gemini-api/docs/pricing>

| family | models | input | cache write 5m | cache write 1h | cache read | output | long-context tier |
|---|---|---|---|---|---|---|---|
| `anthropic` | every current Claude model (Fable, Opus, Sonnet, Haiku), incl. the `[1m]` suffix | 1 | 1.25 | 2 | 0.1 | 5 | none — 1M context is standard pricing on 4.6+, so `[1m]` takes the same row |
| `openai-8x` | gpt-5, gpt-5-mini/nano, gpt-5.1, gpt-5.2, gpt-5.3-codex | 1 | 0 | 0 | 0.1 | 8 | none listed |
| `openai-6x` | gpt-5.4 (incl. -mini/-nano), gpt-5.5 | 1 | 0 | 0 | 0.1 | 6 | > 272,000 input: 2× input, 2× cache read, 1.5× output |
| `openai-5.6-sol` | gpt-5.6-sol | 1 | 1.25 | 1.25 | 0.1 | 5 | > 272,000: 2× input, 2× cache write, 2× cache read, 1.5× output |
| `openai-5.6-terra-luna` | gpt-5.6-terra, gpt-5.6-luna | 1 | 1.25 | 1.25 | 0.1 | 6 | same as sol |
| `gemini-2.5-flash` | gemini-2.5-flash | 1 | 0 | 0 | 0.1 | 8.33 | none |
| `gemini-2.5-pro` | gemini-2.5-pro | 1 | 0 | 0 | 0.1 | 8 | > 200,000: 2× input, 2× cache read, 1.5× output |
| `gemini-3.5-flash` | gemini-3.5-flash | 1 | 0 | 0 | 0.1 | 6 | none |
| `gemini-3.6-flash` | gemini-3.6-flash, gemini-3.7-flash | 1 | 0 | 0 | 0.1 | 5 | none |

A zero cache-write ratio means the provider does not charge for cache writes
(the usage table prints no ratio for that row; the pricing note says "no
cache-write charge"). Gemini 2.5 Flash's cache storage is billed per hour and
cannot be derived from token counts, so it is 0 like the other Gemini rows.
Anthropic fast mode and data-residency uplifts are not detectable from
transcripts and are not modelled.

### 3.3 Long-context tiers are per call

`WeightsForCall(model, inputTokens)` scales the base row by the family's tier
when `inputTokens` **strictly exceeds** the threshold. The caller passes the
input size as the provider counts it toward the threshold — OpenAI and Google
count cached input too, so the attributors pass `InputTokens + CacheReadTokens
+ CacheCreationTokens`. Tiers are decidable only per call, so:

- attributed calls (transcript paths) are priced with `WeightsForCall`;
- a subagent record (a session aggregate) is priced at the base tier
  (`WeightsFor`);
- `tokens profile` and any committed-metadata fallback price at the base tier.

### 3.4 Cost shares, and why there are two entry points

`CostShares{Provider, Family, Input, CacheWrite, CacheRead, Output, Thinking,
Units, CacheWriteUnpriced}`: each class's share of `Units` (Σ tokens × weight
over the four classes; the shares sum to 1 when `Units > 0`). `Thinking` is a
subset view of `Output` (thinking tokens are priced as output) and is not part
of the sum. `Units == 0` means volume-only.

Cache-write units are `CacheCreation1hTokens × CacheWrite1h +
(CacheCreationTokens − CacheCreation1hTokens) × CacheWrite5m`, with one
exception that depends on where the usage came from:

- **`ComputeCostShares(u, w)`** — for a usage whose TTL may be **unknown**:
  summed metadata rows (`CheckpointSummary`, session `metadata.json`), where
  `cache_creation_1h_tokens` was only recorded from PR #2155 on and, under
  `omitempty`, an absent field reads as 0. When `w` prices the two TTLs
  differently, cache writes were recorded and the 1h split is 0, the TTL is
  unknown: `CacheWriteUnpriced` is set and the cache-write row contributes 0
  units. It is never blended — real sessions are all-1h or all-5m.
- **`ComputeCostSharesKnownTTL(u, w)`** — for a usage read from an
  agent-written per-call block (every `TokenAttributor` call, and a subagent
  record built from the subagent's own transcript): a 0 `CacheCreation1hTokens`
  means every write was a 5-minute write, priced at `CacheWrite5m`;
  `CacheWriteUnpriced` is never set.

`SumCostShares(parts…)` re-derives shares from summed units, so a mixed-model
report is weighted by cost rather than by call count; `Provider`/`Family` are
kept when every priced part agrees and cleared to `""` when they disagree
(parts with an empty Provider — unknown models — do not vote);
`CacheWriteUnpriced` is true if any part was.

### 3.5 Unknown models

`WeightsFor` returns `ok=false` for a model with no verified row. Its calls
contribute volume only; the model name goes to `Attributed.Unpriced` (once it
has any usage to report) and the report prints one Notes line per name: *"no
verified price ratios for `<model>`; its tokens count toward volume only"*. An
unrecorded model (`""`) is likewise unpriced but has no name to list; a report
whose parent model is unrecorded and nothing priced says *"model not
recorded; cost shares not estimated"*. Shares are then `—` in the tables,
`cost` is omitted from JSON, and no share-based recommendation gate fires.

## 4. Attribution

### 4.1 The `agent.TokenAttributor` contract

```go
type TokenAttributor interface {
    // AttributeTokens returns one CallUsage per API call from startLine (line
    // or message index, in the same unit as the agent's CalculateTokenUsage
    // offset) onward, in transcript order, plus SubagentRecords when
    // subagentsDir != "" (live sessions). Committed checkpoints pass "" and
    // supply subagent records from task records instead. Error contract
    // matches CalculateTokenUsage: an error is returned only when the
    // transcript as a whole cannot be parsed (a JSON-document format);
    // individual malformed lines are skipped, never fatal.
    AttributeTokens(transcript []byte, startLine int, subagentsDir string) (*types.Attribution, error)
}
```

An agent whose transcript does not record usage per API call, or does not
structurally record which tool calls each call emitted and consumed, **must
not** implement it. `agent.AsTokenAttributor(ag)` is built-in-only with no
`DeclaredCaps` gate (an external agent's parse-hook cannot convey transcript
shape), and callers treat "no attributor" as **cannot attribute**, never as
"attributed zero": the report falls back to the session total and
`resolveTokenAttributor` supplies the Notes wording — *"no agent recorded"*
when the session has no agent, *"agent "X" is not known to this CLI"* when
the registry has none, *"per-call attribution is not available for X"* when
the agent exists but does not attribute, and no note at all when the agent's
profile is totals-only (the profile note already says so).

Two properties every implementation carries:

- **Window independence** — tested by
  `TestAttributeTokens_CallsIndependentOfStartLine` in every agent package.
  `Consumed` and labels are collected from the FULL transcript, so a call's
  `Consumed` (and `Emitted`) is identical whatever `startLine` admits it, and
  consecutive slices charge each result exactly once. The set of calls
  returned for `startLine = n` is exactly the offset-0 calls with `Line >= n`,
  field for field.
- **Additivity** — Σ `Calls[].Usage` reproduces the agent's
  `CalculateTokenUsage` for the same start. Tested as
  `TestAttributeTokens_SumsMatchCalculateTokenUsage` in the Gemini CLI,
  OpenCode and Pi packages; Claude Code and Codex state it in their doc
  comments but do not yet carry that test (follow-up).

### 4.2 Per-agent call units

| agent | one `CallUsage` per | `Line` / `startLine` unit | `Consumed` (a call pays for the results it read) | `Bytes` | model / effort / skill | subagents | agent-reported cost |
|---|---|---|---|---|---|---|---|
| Claude Code | assistant message (`message.id`); streamed rows sharing an id are one call, `Usage` from the row with the highest `output_tokens`, `Emitted` the union of `tool_use` blocks deduped by id. A message is in the slice when its FIRST row is. | physical line, every `\n` counts (blank/malformed included) | every `tool_result` block in user rows after the previous message's first row and before this call's first row | `len()` of the raw JSON of the block's `content` | `message.model`; top-level `effort`; `attributionSkill` | `subagentsDir/agent-<id>.jsonl` for every `agentId` in an `Agent` (legacy `Task`) result; `Model` is the child transcript's own model, never the requested alias | 0 |
| Codex | DISTINCT cumulative `event_msg/token_count` total (a duplicate total is ignored). `Usage` is the delta: fresh input = Δinput − Δcached (clamped), cache read Δcached, output Δoutput, thinking Δreasoning, cache write Δ`cache_write_input_tokens`, `APICallCount` 1 | index in the `splitJSONL` sequence (blank lines dropped); `Line` is the `token_count` row — the LAST row of the group | every `function_call_output` / `custom_tool_call_output` since the previous distinct total (`Emitted`: every `function_call` / `custom_tool_call` since it) | `len()` of the raw JSON of `output` | latest `turn_context` `model` / `effort` (fallback `collaboration_mode.settings`), tracked across the full transcript; bare id, never provider-prefixed; no skill stamp | none: subagent rollouts are separate sessions beside the parent, not in a per-session dir | 0 |
| OpenCode | assistant message (`info`) with index ≥ `startLine`; `BilledOutput()` decides whether reasoning sits inside output | MESSAGE index into `ExportSession.Messages` (user messages included) | the previous assistant message's tool parts (`state.output` lives on the emitting part; there is no result message) | `len(state.output)` | `info.modelID` (bare); no effort; no skill stamp. `task` tool: `input.subagent_type`, `Model` from `input.model` else `state.metadata.model.modelID`; `skill` tool: `input.name` | none: task sessions live only in OpenCode's store | Σ `info.cost` |
| Gemini CLI | message of type `gemini` with index ≥ `startLine`; fresh input = `input − cached + tool`, output = `output + thoughts`, no cache writes | MESSAGE index (user/info messages included) | the previous gemini message's `toolCalls` (`result` lives on the emitting call) | `len()` of the `result` JSON compacted | per-message `model`; no effort; `activate_skill` → `args.name`; `delegate_to_agent` → `args.agent_name` (the `objective` is never stored) | none: no child transcript is written | 0 |
| Pi | assistant message entry on the active branch (`pijsonl.ForEachActiveEntry`), off-branch entries contribute nothing | physical line as `pijsonl.SkipLines` counts | every active-branch `toolResult` after the previous assistant message and before this one | `len()` of the raw JSON of `message.content` | `message.model` (bare); effort = the `thinking_level_change` level in force, verbatim (`"high"`, …); no thinking-token count; `cacheWrite1h` recorded | none | Σ `usage.cost.total` |
| Cursor, Copilot CLI, Factory AI Droid | no attributor — totals only (§6) | | | | | | |

Labelling differs by transcript shape. Claude Code, Codex and Pi keep tool
results in rows separate from the calls that emitted them, so each builds a
tool-use-id → ref map from EVERY row: a result whose call precedes `startLine`
is still labelled, and a result whose call is in no row at all keeps a ref
with only `ID` set (its bytes did enter the context) and lands on the
*Earlier tool results* row. OpenCode and Gemini CLI store a result on the very
part or `toolCall` that emitted it, so there is no map and no ID-only ref can
arise. `Start`/`End` are the earliest/latest parsable timestamps of in-slice
rows of any type.

**Call counts can differ from the committed `api_call_count` on Claude Code.**
Condensation's `CalculateTokenUsage` (`claudecode/transcript.go`,
`len(usageByMessageID)`) counts every assistant message with a `message.id`,
including one with no `usage` object (a skill-load row, for example), whereas
the report counts only calls with recorded usage: a `UsageUnknown` call
contributes 0 to `tokens.api_calls` and is listed in Notes as *"N calls with
no usage recorded"*. The two therefore differ by the number of no-usage
messages; the four classes, thinking and 1h counts agree. Pi, OpenCode and
Gemini CLI agree on calls as well. `effort.calls` is a third count: the calls
that carried the dominant effort value, usage recorded or not.

### 4.3 The rules (`tokenreport.Attribute`)

`Attribute(a *types.Attribution, w *Weights) Attributed` turns one slice into
the contributor table; its doc comment is the specification. In short: a
call's **output**, thinking and call count go to the refs it **emitted**,
split equally (largest-remainder), or to *Assistant text* when it emitted
nothing; its **fresh input and cache writes** go to the results it
**consumed**, proportionally to `Bytes`, or to *Prompt & system context* when
it consumed nothing (the next call pays for a tool's result); its **cache
reads** go to *Context replay*. A ref lands on `skill/<name>`,
`subagent/<type>`, `tool/<Tool>` or *Earlier tool results* (unresolved ref);
tool and text rows carry the call's `ActiveSkill` as `Skill`, which is part of
the row's identity (`Bash` during `systematic-debugging` is not plain
`Bash`). Subagent records are absorbed into the spawning ref's row (orphans
become `source: "task_record"` rows), same `Kind+Label+Skill` rows merge via
`types.AddTokenUsage`, and each piece is priced with
`ComputeCostSharesKnownTTL` at the call's long-context tier (records at the
base tier). Every one of `TokenUsage`'s seven numeric fields is **conserved**:
Σ over the contributors equals Σ `a.Calls[].Usage` + Σ `a.Subagents[].Usage`
(tested).

**Skill labels have two independent axes.** Which row a skill *load* lands on
is decided by the ref's `SkillName`: the attributor sets it from the agent's
own skill tool call, and `applySkillEventAnchors` (checkpoint `skill_events`
/ session-state anchors) fills it only for refs whose `SkillName` is still
empty, so the attributor wins. Which skill a *tool call or text* happened
during is the call's harness-stamped `ActiveSkill` (Claude Code
`attributionSkill`), which annotates tool and text rows as `Skill` and never
moves tokens between rows.

**Ordering.** `Contributors` is sorted by `CostShare` desc, then volume desc,
then `Label`, `Kind`, `Skill` asc. Each row keeps its top 3 `Details` by
tokens (ties: more calls, then name), computed **before** any rendering cut.

**Merging sessions.** `MergeContributors(perSession)` sums same
`Kind+Label+Skill` rows across a checkpoint's attributed sessions, sums
`PricedUnits` and recomputes every share over the combined total, unions
`Unpriced` in order, and sums same-named details then re-tops them to 3. Only
the details each session kept survive, so a merged detail's `Calls` and
`Tokens` are **lower bounds** — the report says so in Notes.

### 4.4 `Contributor` and `Detail` (the `contributors` JSON array)

```go
type Contributor struct {
    Kind      ContributorKind   `json:"kind"`             // tool | skill | subagent | text | replay | prompt
    Label     string            `json:"label"`            // tool name, skill name, subagent type, or a Label* constant
    Skill     string            `json:"skill,omitempty"`  // ActiveSkill annotation; tool and text rows only
    Model     string            `json:"model,omitempty"`  // "" when the row merges models or none was recorded
    Usage     types.TokenUsage  `json:"usage"`            // Model always "", SubagentTokens always nil (folded in)
    CostShare float64           `json:"cost_share"`       // units ÷ Attributed.PricedUnits; 0 when unpriced
    Source    ContributorSource `json:"source"`           // transcript | task_record
    Details   []Detail          `json:"details,omitempty"`
}

type Detail struct {
    Detail    string  `json:"detail"`     // the ToolUseRef.Detail value; never empty
    Calls     int     `json:"calls"`      // distinct tool calls: every emitted ref + every consumed ref first sighted here
    Tokens    int     `json:"tokens"`     // four classes summed
    CostShare float64 `json:"cost_share"` // same denominator as the row, so sub-rows compare with rows
}
```

The synthetic labels are the only display strings the package defines:
`Assistant text`, `Context replay`, `Prompt & system context`, `Earlier tool
results`, `(unknown)`. Skill and subagent rows keep the bare name as `Label`;
the renderer prefixes by kind. `Usage.APICallCount` on a row is that row's
share of the emitting calls' counts (split across every ref a call emitted),
so it undercounts a skill load emitted alongside other tools — the
recommendations therefore read a skill's load count from its self-named
detail's `Calls`, not from `APICallCount`.

## 5. Drill-down `Detail` (`transcript.ToolDetail`)

`ToolDetail(tool, in ToolInput) string` is the one place the drill-down rules
live; every attributor calls it with the agent-native tool name, matched
case-insensitively. **The raw command is never returned** — commands are user
content and are not stored — and the detail is derived on read and only ever
printed, never persisted (no checkpoint, session-state or telemetry field
carries one; the committed `task.json` and compaction's `RawToolDetail` are
separate, pre-existing contracts).

- **Shell tools** (`Bash`, `shell`, `exec`, `exec_command`, `run_terminal_cmd`,
  `run_shell_command`, `terminal`, `execute`): the head of the LAST command in
  the pipeline. The command is sanitized with
  `stringutil.SanitizeShellCommand` (quoted spans become NUL and are dropped as
  words), split on `;`, `&&`, `||`, `|` and newline (a lone `&` is not a
  separator, so `2>&1` stays intact), `(`/`{`/`)`/`}` grouping punctuation
  trimmed; then, word by word, these are **dropped**: words that could carry
  a secret (contain `://` or `@`, or start with `$` — URLs, `user@host`, git
  remotes, e-mail addresses, variable expansions), `KEY=value` assignments
  (leading `VAR=x`, `export`/`env` operands, and dotted config operands such
  as git's `-c core.pager=cat`), `-flag` words, and redirections with their
  targets (`>out`, `2>&1`, `<<EOF`, and a bare `>` plus the word after it).
  What survives is the first two words plus a third ONLY when it is path-like
  (contains `/`, starts with `.`, or contains `*`) — the command, its
  subcommand, and the path or glob it targets.
  `go test ./cmd/entire/... -run TestX` → `go test ./cmd/entire/...`;
  `git log -p -3` → `git log`; `npm run build` → `npm run`;
  `curl https://user:pass@example.com/x` → `curl`; `echo $TOKEN` → `echo`.
  Codex's `exec_command` passes its `cmd`, or a `command` argv joined with
  spaces after unwrapping a `<shell> -c|-lc <script>` wrapper; its
  `apply_patch` passes the first `*** Add|Update|Delete File:` path as the
  file path.
- **File tools** (`Read`, `Edit`, `Write`, `MultiEdit`, `NotebookEdit`,
  `read_file`, `write_file`, `edit_file`, `apply_patch`): the file path under
  whichever key the agent used (`file_path`, `filePath`, `path`; Gemini's
  older `absolute_path` is mapped in by its attributor), falling back to
  `notebook_path`.
- **`WebFetch` / `web_fetch` / `fetch`**: the URL's host, without port or path.
  Gemini's `web_fetch` takes a `prompt`, not a `url`, so its detail is `""`.
- **`Skill` / `activate_skill`**: the skill name.
- **`Task` / `Agent` / `spawn_agent` / `delegate_to_agent`**: the subagent
  type, with the requested model appended as ` (model)` when the input names
  one (a spawn with no subagent type yields `""` even when a model is
  present). OpenCode's detail is computed from the input alone, so the
  child's actual model from `state.metadata` never appears in it.
- **`WebSearch`, `Grep`, `Glob`, `LS`, `search`, `list_dir` and any other
  tool**: `""` — the tool name is the whole label; a query or pattern is user
  content.

The helpers an attributor builds the input with: `transcript.ToolInputFromJSON`
(best-effort decode of a raw `tool_use` input; folds key case) and
`transcript.ToolInputFromMap` / `transcript.StringArg` (exact-key reads of an
already-decoded map — OpenCode's `state.input`, Gemini's `toolCalls[].args`).

A subagent row's self-named detail (`Detail == Label`) exists only when the
call requested no model; when it did, the detail is `<type> (<model>)` and the
row has no self detail, so the recommendations omit its run count rather than
sum prefix matches that no row prints.

## 6. Agent profiles (`profile.go`)

`ProfileFor(agent)` states what an agent's transcript records, so reports print
`not recorded` instead of `0` and only offer recommendations whose inputs
exist. The table is populated from the 2026-08-27 survey of every agent's
committed transcripts — facts about what each agent writes, not targets. An
agent not in the table gets `AgentProfile{TotalsOnly: true}` (totals, no
breakdown, `Verified` false).

| agent | thinking | cache TTL | effort (`EffortSource`) | model | tool calls | subagents | recorded $ | totals only | verified |
|---|---|---|---|---|---|---|---|---|---|
| Claude Code | yes (since ~2026-08-11) | yes | yes — per-call field (`effort`) | per call | yes | yes (Agent tool + task records) | no | no | yes |
| Codex | yes | no | yes — turn context | per call | yes | no (`spawn_agent` seen, not aggregated) | no | no | yes |
| OpenCode | yes | no | no | per call | yes | yes (task tool) | no — `info.cost` exists but is always 0 in observed transcripts, so not treated as recorded | no | yes |
| Gemini CLI | yes | no | no | per call (`model` on each gemini message) | yes | no | no | no | yes |
| Pi | no | yes (`cacheWrite1h`) | yes — thinking-level events | per call | yes | no | yes (`usage.cost`) | no | yes |
| Cursor | no | no | no (model name is a display-only hint: `EffortSource` = model name, `RecordsEffort` false) | no | no | no | no | **yes** | yes |
| Copilot CLI | no | no | no | per **model** (`session.shutdown` `modelMetrics`), not per call | no | no | no | **yes** | yes (totals only) |
| Factory AI Droid | no | no | no | no | no | no | no | **yes** | no — no checkpoints exist to verify against |

`EffortSettingVerified` is false and `Levers` nil for every agent in B1: a
setting name may be printed in a recommendation only when the former is true,
so the thinking sentence says "a lower effort setting" for now.

What the profile controls in a report: the `of which thinking` row prints
`not recorded` when `RecordsThinking` is false and the count is 0; the
cache-write label says `, 5-minute` only when `RecordsCacheTTL` is true and
the provider prices the TTLs differently; `Effort:` appears only when
`RecordsEffort` is true and a call carried one; `thinking` fires only when
`RecordsEffort` is true; a `TotalsOnly` agent gets the Notes line *"<Agent>
records session totals only; the per-call breakdown is not verified for this
agent."* (or *"no verified capability profile for <Agent>; totals shown,
breakdown not verified."* when unverified). `tokens profile` uses
`RecordsThinking` to decide whether a legacy row's 0 thinking is a figure.

## 7. Recommendations (`recommend.go`)

The breakdown is the product; a recommendation is the one or two plain
sentences a colleague would add after reading it. `Recommend(report)` returns
**at most two** session-kind recommendations and **nil when nothing fires** —
there is no "looks fine". Names (model, command, skill, effort value) are set
in backticks; numbers are digits, formatted with the same `Format*` functions
the tables use.

```go
type Recommendation struct {
    Kind   RecommendationKind `json:"kind"`             // "session" in B1
    Text   string             `json:"text"`
    Cause  Cause              `json:"cause"`            // stable keys — do not rename
    Cited  []Citation         `json:"cited,omitempty"`  // every row (and detail) whose figures Text quotes
    Memory string             `json:"memory,omitempty"` // pattern kind only (B2); always empty
    Seen   int                `json:"seen,omitempty"`   // pattern kind only; always 0
    Of     int                `json:"of,omitempty"`     // pattern kind only; always 0
}

type Citation struct {
    Kind   ContributorKind `json:"kind"`
    Label  string          `json:"label"`
    Skill  string          `json:"skill,omitempty"`
    Detail string          `json:"detail,omitempty"` // a Detail.Detail when a sub-row's figures are quoted
}
```

### 7.1 Every cited number is visible

A recommendation may only quote figures the renderer prints from the same
`Report`: a contributor or detail row, a usage class, a cost share, the
duration, a call count. Class-level figures need no citation; every contributor
row and detail a sentence quotes is listed in `Cited`, and **the renderer must
print every cited row and cited detail even when it falls below the cutoffs**
`MaxRenderedRows = 6` / `MaxRenderedDetails = 3` (constants in
`attribution.go`, next to the table types, so renderer and recommendations
agree on what is visible). The web must honour the same rule.

### 7.2 Inputs (`tokenreport.Report`)

`Agent` (selects the gates), `Profile`, `Model` (the session's parent model:
the modal `CallUsage.Model`, or the checkpoint's recorded model; a subagent row
whose `Model` equals it or is empty counts as "on the session's model"),
`Effort` (the dominant `CallUsage.Effort`, quoted verbatim), `Usage`
(flattened, subagents folded in), `Cost`, `Attributed`, `Duration`
(`Attribution.End − Start`, else `SessionMetrics.DurationMs`; 0 disables the
duration arm), `Calls` (the parent session's API calls with recorded usage —
subagent calls excluded; 0 falls back to `Usage.APICallCount`), `Sessions`
(attributed sessions merged; raises the `repeated_skill` gate).

### 7.3 Causes and gates

Gate values come from `GatesFor(agent)`; the comment on that table in
`recommend.go` records how they were calibrated (each cause fires on a clear
minority of that agent's real sessions) and the date. Only two thresholds vary
by agent: `LongSessionDuration` and `CacheMissShare`. Every other agent
(Codex, Pi, Cursor, Copilot CLI, Factory AI Droid, unknown) takes the
defaults.

| cause | fires when | quotes / cites | addressed share (for ranking) |
|---|---|---|---|
| `long_session` | duration ≥ **8h** (Claude Code) / **4h** (others); or, when priced, cache read ≥ **70%** of cost with ≥ **20** calls | class figures only — no citation. The duration arm mentions context replay only when cache read is the largest class or ≥ 50% of cost | `Cost.CacheRead` |
| `context_growth` | priced, cache write is **strictly** the largest cost class, and the largest row that can carry cache writes (tool, skill, subagent, prompt — not text or replay) has share ≥ **25%** | the row; a tool row names its `Details[0]` (cited); a skill or subagent row folds in its self-detail's load/run count (cited). Advice by kind: narrower commands / a slimmer skill / shorter subagent briefs and results / shorter prompts | that row's share |
| `subagent_model` | the largest `subagent` row on the session's model (row `Model` == `Report.Model`, or empty) has share ≥ **15%** | the row; the run count from its self detail when present (cited) | that row's share |
| `thinking` | `Profile.RecordsEffort` and thinking ≥ **50%** of output tokens | class figures only: share of output, the two counts, the cost share when priced, the effort value when known; names a setting only when `EffortSettingVerified` (never, in B1) | `Cost.Thinking` (0 when volume-only, so it ranks last) |
| `cache_miss` | priced on **OpenAI or Google** (`cacheMissEligible`; Anthropic-priced reports run ≈0% fresh input, so a high share there is an artefact, and a mixed/unknown provider is ineligible) and fresh input ≥ **45%** (Codex, default) / **40%** (OpenCode) / **70%** (Gemini CLI) of cost | the tool row with the most fresh-input tokens and its `Details[0]` when present (cited); prefix-caching advice | `Cost.Input` |
| `repeated_skill` | a `skill` row's self detail counts ≥ `max(Sessions,1) + 2 − 1` loads — two in one session, three across two merged sessions (one load per session is expected); most-loaded row wins, ties to the higher share | the row and its self detail (cited); "across N sessions" when merged | that row's share |

Ranking: each cause is evaluated once; a `repeated_skill` that cites the same
skill row a fired `context_growth` cites is dropped (`dropOverlapping`); the
rest are sorted by addressed share descending (ties keep the table order
above) and the top two are returned. No class-share gate fires when
`Cost.Units == 0`, and no row-share gate when `Attributed.PricedUnits == 0`.

**Sentences produced by the test fixtures (`recommend_test.go`)** — the exact
strings the tests assert, labelled with the fixture that produces each:

- `longSessionByReplay`: Most of this session's cost (70%) was re-reading its
  own context: 20 calls replayed 3.7M cache-read tokens. Shorter sessions
  replay less context on each call.
- `contextGrowthReport(true)`: 25% of the cost was Bash output read back into
  context, led by `go test ./cmd/entire/...` (9 calls, 140.2k tokens, 22%).
  Narrower commands or trimmed output would have avoided most of it.
- `contextGrowthSkillReport`: 25% of the cost was loading the
  `artifact-design` skill into context 3 times (41.3k tokens). A slimmer skill
  would have avoided most of it.
- `contextGrowthSubagentReport`: 25% of the cost was Explore subagents (5
  runs, 4.7M tokens) writing results into context. Shorter subagent briefs and
  results would have avoided most of it.
- `subagentModelReport(testModel, 0.15)`: Explore subagents ran 5 times on
  `claude-fable-5` (4.7M tokens, 15% of cost); delegated work like this often
  runs well on a smaller model.
- `thinkingReport(false)`: Thinking took 50% of output tokens (50k of 100k,
  10% of cost) at effort `high`; a lower effort setting is enough for most
  work.
- `cacheMissReport(agentCodex, ProviderOpenAI, 0.45)`: 45% of the cost was
  uncached input — context that arrived fresh on each call instead of from
  the cache — with Bash the largest tool source, led by
  `go test ./cmd/entire/...` (12 calls, 1.2M tokens, 31%). Tool output is
  always fresh the first time it is read, but keeping the same system prompt
  and tool set across calls lets the rest of each request come from the
  cache.
- `repeatedSkillReport(2)`: `artifact-design` was loaded 2 times (41.3k
  tokens, 4% of cost); once per session is enough.
- `multiSessionSkillReport`: `artifact-design` was loaded 3 times across 2
  sessions (41.3k tokens, 4% of cost); once per session is enough.

The duration arm of `long_session` on a real checkpoint reads: *This session
ran 2d 0h; re-reading its own context on every call took 176.4M cache-read
tokens, 85% of cost. Splitting work this long into several shorter sessions
would have cost less.* (§10.1).

## 8. Rendering rules (`tokens_render.go`)

Both single-report commands print: a header, **Where it went** (when
attributed), **Usage**, the agent-reported cost line (when recorded), the
`--compare` section (checkpoint only, between Usage and Recommendations),
**Recommendations** (when any fired), **Notes** (when any).

**Header.** `checkpoint tokens`: `Checkpoint: <id>      Agent: …      Model: …`
(`Agents:`/`Models:` when more than one), `Session: <id>` or `Sessions: N`,
`Duration:   <d> · N API calls · <volume> tokens      Effort: high (N calls)`,
`Branch:`, and the legacy scope line when `legacy.cumulative`. `session
tokens`: `Session:  <id>      Agent: …      Model: …`, `Status:`, `Duration:
<d> so far · …` (no "so far" once ended), `Context:  N% full (x of y)` when
the agent's hooks reported context pressure. Any unrecorded figure prints
`not recorded`; the calls figure is `Report.Calls` (parent only).

**Where it went** (`writeTokenWhereItWent` / `selectContributorRows`): the top
`MaxRenderedRows` (6) contributors in table order; the top
`MaxRenderedDetails` (3) of them print all their details, rows 4–6 print only
details a recommendation cites; every row a recommendation cites that fell
below rank 6 is appended after the top block with its cited details; then
`(N smaller items omitted)` counts the rest. Labels: `Skill: <name> (loaded)`,
`Subagent: <type>`, `Context replay (cache read)`, `Prompt & system context`,
`Assistant text`, the tool name — the last two suffixed ` · during <skill>`
when the row carries a `Skill`. A ref-driven row with nothing attributed
carries `(usage not recorded)`. Details are indented under the row,
truncated to 40 runes with `…`, with `N calls` (blank when the detail's calls
all happened before the window) and their own share. Shares print `—` when
`PricedUnits == 0`. The `--json` `contributors` array is **not** cut this way:
it carries every contributor with its top-3 details (§4.3).

**Usage**: `Input (fresh)`, the cache-write row, `Cache read`, `Output`,
`of which thinking`, `Total`, `of which subagents` (when non-zero). The
cache-write row reads `Cache write, 1-hour` when every write carried the
1-hour TTL, `Cache write, 5-minute` when the agent records TTLs and none did,
plain `Cache write` with an `of which 1-hour` sub-row when mixed; its share is
`—` when `Cost.CacheWriteUnpriced`. The cache-write, cache-read and output
rows show their ratio as a note (`(1.25×)`, `(0.1×)`, `(5×)`; none for a zero
ratio); the input row, being the 1× base, carries none. The thinking row shows
its cost share and `N% of output`, `not recorded` for an agent that does not
record it, or the stored transcript's figure with `(from stored transcript)`
on a legacy checkpoint whose committed usage lacks it — that figure is never
added to the total. Every value comes from `Report.Usage` / `Report.Cost`, the
values `Recommend` quoted.

**Recommendations**: each sentence wrapped at 78 columns, indented two spaces,
blank line between them.

**Notes** (`tokenReportNotes`; also the JSON `limitations`), in this order:
the caller's limitations first — they say why the totals are what they are
(*"transcript unavailable; totals from session state"*, *"session 2: no API
calls in the token window; totals from committed metadata"*, unreadable
metadata / the root-summary fallback, unreadable task records, subagent calls
with no record, *"N of M sessions recomputed from their transcripts; the rest
use committed token_usage."*, the multi-session lower-bound note, the legacy
whole-transcript note) — then the price-ratio note, one line per unpriced
model, *"cache-write TTL not recorded; not priced"*, the agent's totals-only
caveat, and *"N calls with no usage recorded"*.

**Formats** (`format.go`): token counts abbreviate at 1,000 (`k`) and
1,000,000 (`M`) with one decimal and a trimmed `.0` (`1k`, `3.7M`; the tier is
chosen after rounding, so 999,950 → `1M`); shares print as whole percents,
`<1%` for a positive share below 0.5%, `0%` for zero; durations print `42s`,
`6m`, `1h 05m`, `2d 3h`.

## 9. Data sources and fallbacks

**`checkpoint tokens`** (`buildCheckpointTokensReport`). Per session, totals
come from the first rung that applies; `source` is `"transcript"` only when
every session stopped at rung 1:

1. attributed version-2 session → totals recomputed from its calls plus its
   subagent records;
2. attributed legacy session → committed `token_usage` for the totals, calls
   and class shares (whose cache writes carry no TTL and stay unpriced), the
   whole stored transcript for the breakdown, effort, model, duration; the
   thinking and 1h counts the transcript recorded are shown beside the
   committed totals with `(from stored transcript)` and never added in;
3. not attributed (no attributor, unreadable transcript, parse error, no API
   calls in the window) → committed `token_usage`, priced at the base tier
   with `ComputeCostShares` (TTL may be unknown);
4. a session's metadata unreadable → the root summary's aggregate stands in
   for every session's totals (it was aggregated over all sessions at write
   time; the readable sessions' sum would undercount), breakdown from the
   readable sessions;
5. nothing recorded anywhere → `not recorded`.

Task records (`ReadTaskRecords`) are assigned in `StartedAt` order to the
session whose attributed window emitted or consumed the spawning `ToolUseID`;
records no session claims go to the last attributed session as orphan rows;
with no attributed session they are dropped — each session's committed
`token_usage` already includes its subagents. Subagent refs with no record are
counted and noted (*"N subagent calls have no committed task record; that
usage is not included"*, or the Codex/OpenCode wording that their subagents
are separate sessions). A checkpoint's `Duration` is Σ per-session spans
(transcript `End − Start`, else `session_metrics.duration_ms`); the agent for
gates and profile is the one agent of a single-agent checkpoint, else the
agent of the most sessions.

**`session tokens`** (`buildSessionTokensReport`): the whole live transcript
at start line 0 with `subagentsDir` set; falls back to
`state.TokenUsage` with a note (`no transcript recorded`, `transcript
unavailable`, `transcript could not be attributed`, `no API calls in the
transcript yet`, each suffixed `; totals from session state`). Duration is the
transcript span, else the hook-reported duration, else `StartedAt` →
`LastInteractionTime` (or `EndedAt`).

**`tokens profile`** (`buildTokensProfileReport`): reads the latest N root
summaries and every session's metadata; groups by agent (test agents `Mock
Lifecycle Agent` and `Vogon` excluded; the pre-agent-field values `""` and
`Agent` fold into Claude Code); dedupes legacy rows per session (§2.2); merges
each checkpoint's kept rows into one sample priced with `ComputeCostShares`
per row model at the base tier. Per agent: checkpoints / with tokens /
collapsed, nearest-rank median and p90 tokens per checkpoint, median and p90
duration (`session_metrics.duration_ms` summed over the checkpoint's kept
sessions) with tokens per hour over the with-tokens subset, how often each
class was the largest cost, summed cost by class with the 1-hour bookkeeping
(`1-hour on x of y recorded`), median thinking share over checkpoints that
recorded thinking (version-2 rows, or legacy rows with a non-zero count),
effort (always `not recorded` — metadata carries none), and the two
checkpoints most worth opening (priced ones by cost units, then volume-only by
volume) with the figure that makes each stand out. Recurring contributors are
not computed (stated in Notes). The grand total is printed once, last, as
*"sum after collapsing overlaps"*.

## 10. `--json` shapes

Shared objects, derived from one `tokenReportView` (`applyView`) so both
single-report commands emit the same shapes:

| key | shape | when present |
|---|---|---|
| `tokens` | `{total, input, cache_read, cache_write, output, api_calls, subagent_total?, thinking_tokens?, cache_creation_1h_tokens?}` — `api_calls` includes subagent calls. `subagent_total` is the four-class volume of the **subagent records** (`tasks/*/task.json` or `agent-<id>.jsonl`, `a.subagent` in `tokens_load.go`), not of the `kind: "subagent"` contributor rows: a subagent row also absorbs the parent's own spawn tool-call tokens, which appear as that row's `details[]` (e.g. `Explore (haiku)`) and belong to the parent. The identity that holds, when no row's details were truncated, is `subagent_total == Σ(subagent rows' four classes) − Σ(their details[].tokens)`; `Σ contributors == tokens.*` holds regardless. | any usage recorded |
| `cost` | `{provider?, family?, weights?: Weights, shares: CostShares}` — `weights` only when one family priced everything and it is the report model's | `shares.units > 0` |
| `effort` | `{value, calls}` — `calls` counts every call carrying the value, usage recorded or not (so it can exceed `tokens.api_calls`) | profile records effort and a call carried one |
| `context` | `{tokens, window_size, percent}` — hook-reported context pressure | both figures known (single-session checkpoints and live sessions) |
| `contributors` | `[]Contributor` (§4.4) — **every** contributor, each with its top-3 details, in table order; always present, `[]` when not attributed | always |
| `recommendations` | `[]{kind, text, cause, cited?, memory?, seen?, of?, id, message}` — `id` = `cause` and `message` = `text` keep the previous schema's keys | any fired |
| `agent_reported_cost` | number (dollars) | > 0 |
| `duration_seconds` | whole seconds | > 0 |
| `limitations` | `[]string`, the Notes in print order | any |
| `source` | see §1 | always |

### 10.1 `entire checkpoint tokens <id> --json`

```
checkpoint_id, session_count, session_id?, agent?, agents?[], model?, models?[],
branch?, source, duration_seconds?, effort?, tokens?, context?, cost?,
contributors, recommendations?, agent_reported_cost?,
legacy?: {cumulative, thinking_recorded, cache_ttl_recorded,
          thinking_from_transcript?, cache_write_1h_from_transcript?},
comparison?, limitations?
```

`agent`/`model`/`session_id` are set for a single-session checkpoint. `agents`
and `models` are `omitempty` lists of distinct values in order of first
appearance (a session without an agent is `(unknown)`; `models` is absent when
no session recorded a model), `models` also carrying the attributed modal
model. The shared keys look exactly as in the session example below; what is
checkpoint-specific is shown here from a real legacy checkpoint of this
repository:

```json
{
  "checkpoint_id": "01M15KQ8YVE4F2K8E6T94Z1GF3",
  "session_count": 1,
  "session_id": "45f893c5-8c24-424d-b4d9-11466f12f1d7",
  "agent": "Claude Code",
  "agents": ["Claude Code"],
  "model": "claude-fable-5",
  "models": ["claude-fable-5"],
  "branch": "feat/global-trust",
  "source": "committed_checkpoint",
  "duration_seconds": 173038,
  "tokens": {
    "total": 189953198, "input": 91155, "cache_read": 176383662,
    "cache_write": 12850967, "output": 627414, "api_calls": 403
  },
  "cost": {
    "provider": "anthropic", "family": "anthropic",
    "shares": {"provider": "anthropic", "family": "anthropic",
               "input": 0.004368466278286988, "cache_write": 0,
               "cache_read": 0.8452921721109866, "output": 0.15033936161072634,
               "thinking": 0, "units": 20866591.2, "cache_write_unpriced": true}
  },
  "recommendations": [
    {"kind": "session",
     "text": "This session ran 2d 0h; re-reading its own context on every call took 176.4M cache-read tokens, 85% of cost. Splitting work this long into several shorter sessions would have cost less.",
     "cause": "long_session", "id": "long_session",
     "message": "This session ran 2d 0h; re-reading its own context on every call took 176.4M cache-read tokens, 85% of cost. Splitting work this long into several shorter sessions would have cost less."}
  ],
  "legacy": {"cumulative": true, "thinking_recorded": true, "cache_ttl_recorded": true,
             "thinking_from_transcript": 209535, "cache_write_1h_from_transcript": 2486471},
  "limitations": [
    "13 subagent calls have no committed task record; that usage is not included (this backend may not store task records).",
    "Cost shares use Anthropic list-price ratios (input 1×, 5m write 1.25×, 1h write 2×, cache read 0.1×, output 5×), not your plan's rates.",
    "cache-write TTL not recorded; not priced"
  ]
}
```

(`effort`, `cost.weights` and the 31 `contributors` are elided; they have the
shapes shown in §10.2.) Note how the legacy rungs show: `tokens` is the
committed total — its `cache_write` has no TTL, so `shares.cache_write` is 0
and `cache_write_unpriced` is true — while the breakdown was attributed from
the whole stored transcript, and `legacy.*_from_transcript` carry the subset
figures the committed usage lacks.

**`--compare <baseline>`** adds `comparison`:

```
baseline_checkpoint_id, target_checkpoint_id,
status: unavailable | observed_reduction | observed_increase | observed_no_change,
total?, input?, cache_read?, cache_write?, output?, api_calls?:
    {baseline, current, change, change_percent?, direction: down | up | unchanged},
cost_share?: {input, cache_write, cache_read, output:
    {baseline, current, change, change_percent?, direction}},   // shares 0..1; direction on whole points
cache_read_caveat?, qualification, limitations?
```

`status` follows the total's direction; `qualification` is a fixed sentence
per status, plus *"Cost mix: cache write 41% → 30% (down 11 points); …"* for
every class that moved ≥ 5 points when both checkpoints priced. Comparing a
checkpoint to itself is an error.

### 10.2 `entire session tokens --json`

```
session_id, agent, model?, status, source, duration_seconds?, effort?, tokens?,
context?, cost?, contributors, recommendations?, agent_reported_cost?, limitations?
```

A live Claude Code session from this repository (contributors after the first
four, and the details under them, elided as `"…"`; the `<synthetic>` model is
Claude Code's own placeholder on harness-generated rows):

```json
{
  "session_id": "45f893c5-8c24-424d-b4d9-11466f12f1d7",
  "agent": "Claude Code",
  "model": "claude-fable-5",
  "status": "active",
  "source": "transcript",
  "duration_seconds": 181491,
  "effort": {"value": "high", "calls": 480},
  "tokens": {
    "total": 267941927, "input": 151961, "cache_read": 249024472, "cache_write": 17515123,
    "output": 1250371, "api_calls": 831, "subagent_total": 42140050,
    "thinking_tokens": 547273, "cache_creation_1h_tokens": 3044483
  },
  "cost": {
    "provider": "anthropic", "family": "anthropic",
    "weights": {"provider": "anthropic", "family": "anthropic", "input": 1,
                "cache_write_5m": 1.25, "cache_write_1h": 2, "cache_read": 0.1, "output": 5},
    "shares": {"provider": "anthropic", "family": "anthropic",
               "input": 0.0027550189183962546, "cache_write": 0.4362457741145785,
               "cache_read": 0.44902443516599005, "output": 0.11197477180103567,
               "thinking": 0.048499236029171645, "units": 55140456.19999998,
               "cache_write_unpriced": false}
  },
  "contributors": [
    {"kind": "replay", "label": "Context replay", "model": "claude-fable-5",
     "usage": {"input_tokens": 0, "cache_creation_tokens": 0, "cache_read_tokens": 210875717,
               "output_tokens": 0, "api_call_count": 0},
     "cost_share": 0.38243375469207697, "source": "transcript"},
    {"kind": "tool", "label": "Bash", "model": "claude-fable-5",
     "usage": {"input_tokens": 92245, "cache_creation_tokens": 6495578, "cache_read_tokens": 0,
               "output_tokens": 580210, "api_call_count": 358,
               "thinking_tokens": 188633, "cache_creation_1h_tokens": 549204},
     "cost_share": 0.20900571548046068, "source": "transcript", "details": ["…"]},
    {"kind": "prompt", "label": "Prompt & system context", "model": "claude-fable-5",
     "usage": {"input_tokens": 266, "cache_creation_tokens": 6535392, "cache_read_tokens": 0,
               "output_tokens": 0, "api_call_count": 0, "cache_creation_1h_tokens": 1478813},
     "cost_share": 0.16827237911027662, "source": "transcript"},
    {"kind": "subagent", "label": "reviewer", "model": "claude-opus-5",
     "usage": {"input_tokens": 448, "cache_creation_tokens": 1021958, "cache_read_tokens": 12635341,
               "output_tokens": 164838, "api_call_count": 165,
               "thinking_tokens": 119208, "cache_creation_1h_tokens": 39181},
     "cost_share": 0.055348333334971585, "source": "transcript", "details": ["…"]}
  ],
  "recommendations": [
    {"kind": "session",
     "text": "This session ran 2d 2h; re-reading its own context on every call took 249M cache-read tokens, 45% of cost. Splitting work this long into several shorter sessions would have cost less.",
     "cause": "long_session", "id": "long_session",
     "message": "This session ran 2d 2h; re-reading its own context on every call took 249M cache-read tokens, 45% of cost. Splitting work this long into several shorter sessions would have cost less."}
  ],
  "limitations": [
    "Cost shares use Anthropic list-price ratios (input 1×, 5m write 1.25×, 1h write 2×, cache read 0.1×, output 5×), not your plan's rates.",
    "no verified price ratios for `<synthetic>`; its tokens count toward volume only"
  ]
}
```

A skill-annotated row from the same report, showing `skill` as part of the
row's identity and the drill-down under it:

```json
{"kind": "tool", "label": "Bash", "skill": "entire:search", "model": "claude-fable-5",
 "usage": {"input_tokens": 1074, "cache_creation_tokens": 27610, "cache_read_tokens": 0,
           "output_tokens": 4257, "api_call_count": 9, "thinking_tokens": 1091,
           "cache_creation_1h_tokens": 27610},
 "cost_share": 0.001406934315498101, "source": "transcript",
 "details": [
   {"detail": "tail +81", "calls": 1, "tokens": 7666, "cost_share": 0.00029212670895530253},
   {"detail": "head", "calls": 4, "tokens": 6896, "cost_share": 0.0003118218670087827},
   {"detail": "done", "calls": 2, "tokens": 6740, "cost_share": 0.00030690714524773927}
 ]}
```

### 10.3 `entire tokens profile --json`

```
source: "committed_checkpoints", checkpoints_available, checkpoints_analyzed,
checkpoints_with_token_data, collapsed, excluded_test_agents, metadata_read_warnings,
agents: [{agent, checkpoints, with_tokens, collapsed,
          tokens_per_checkpoint?: {median, p90},
          duration_seconds: {median, p90, recorded_on},
          tokens_per_hour_median?,
          largest_cost_class: {"<class>": count},        // input | cache write | cache read | output
          cost_by_class?: {input, cache_write, cache_read, output, priced,
                           cache_write_unpriced, cache_write_recorded_on, cache_write_1h_recorded_on},
          thinking_share: {median, recorded_on},
          effort,                                        // "not recorded"
          worth_opening: [{checkpoint_id, tokens, standout}]}],
total_tokens, limitations?
```

`entire tokens profile --limit 8 --json` against this repository:

```json
{
  "source": "committed_checkpoints",
  "checkpoints_available": 5404,
  "checkpoints_analyzed": 8,
  "checkpoints_with_token_data": 2,
  "collapsed": 6,
  "excluded_test_agents": 0,
  "metadata_read_warnings": 0,
  "agents": [
    {
      "agent": "Claude Code",
      "checkpoints": 2,
      "with_tokens": 2,
      "collapsed": 6,
      "tokens_per_checkpoint": {"median": 141715034, "p90": 189953198},
      "duration_seconds": {"median": 0, "p90": 0, "recorded_on": 0},
      "largest_cost_class": {"cache read": 2},
      "cost_by_class": {
        "input": 0.0030348430981314333, "cache_write": 0,
        "cache_read": 0.8373428957657665, "output": 0.15962226113610212,
        "priced": 2, "cache_write_unpriced": true,
        "cache_write_recorded_on": 2, "cache_write_1h_recorded_on": 0
      },
      "thinking_share": {"median": 0, "recorded_on": 0},
      "effort": "not recorded",
      "worth_opening": [
        {"checkpoint_id": "01M15KQ8YVE4F2K8E6T94Z1GF3", "tokens": 189953198, "standout": "cache read 85%"},
        {"checkpoint_id": "01M15AF5HPSTZBMVH52X83CMWX", "tokens": 141715034, "standout": "cache read 83%"}
      ]
    }
  ],
  "total_tokens": 331668232,
  "limitations": [
    "Limited to latest 8 of 5,404 committed checkpoints; use --limit or --all to change scope.",
    "Legacy checkpoints (no token_usage_version) were deduped per session; 6 legacy running-total rows collapsed.",
    "Cache writes on 2 checkpoints have no recorded TTL and are not priced.",
    "Recurring contributors are not computed for profiles (no transcripts are read).",
    "Cost shares use list-price ratios per model family, not your plan's rates."
  ]
}
```

### 10.4 `--agent-brief`

The compact next-step view both single-report commands print for an agent
(`writeTokenAgentBrief`); the same values as the report, no new figures:

```
Checkpoint token brief                    | Session token brief
Checkpoint: <id>                          | Session: <id>

Token usage: <volume> total; N API calls; <duration or "duration not recorded">; cache read X% of cost.
                                          (or "X% of volume" when unpriced; "Token usage: unavailable." when none)

Next best action:
<the top recommendation's text>           (or "Continue normally; no token recommendation fired for this report."
                                           / "Token usage is not recorded here; continue with the task and recheck once
                                           a newer checkpoint captures usage.")

Signals:
- <cause>                                 one line per fired recommendation
                                          (or "- none: no token recommendation fired" / "- token usage not recorded")
```

## 11. What is B2, and explicitly absent here

Plan B2 adds the **pattern** recommendation kind: the same six causes sourced
from recurrence across the user's recent sessions (a per-user local ledger fed
at turn end), with `Memory` (the line an agent could store verbatim), `Seen`
and `Of`; the `remember:` line in reports and the `[y/N]` append into the
agents' instruction files; the session-start nudge; `entire status`'s pattern
count. None of that exists in B1: `Recommendation.Kind` is always `"session"`,
`Memory`/`Seen`/`Of` are always empty, `tokens profile` prints no
recommendations at all, and no report writes anything anywhere. The `cause`
strings are the keys B2's ledger will count recurrences by — do not rename
them.

Also out of scope, by design: dollar estimates by Entire; persisting the
attribution breakdown (it is derived on read; only the subset fields are
persisted); Codex `spawn_agent` and OpenCode task-session subagent
aggregation (their child sessions live outside the parent's session
directory); Copilot CLI and Factory AI Droid attribution (no per-call data,
and for Droid no checkpoints to verify against); visuals beyond alignment
(a treemap or timeline is the web's job — the `--json` carries everything it
needs, drill-down included).

## 12. Privacy rules

- Details are derived, printed and never persisted, and never contain the raw
  command or any word that could carry a secret — the rules and their
  rationale are in §5.
- **The token reports take a stricter stance than the logging baseline.**
  `docs/architecture/logging.md` lists `transcript_path` among the fields that
  are safe to log; these commands treat transcript and file paths as user
  content and never log them. `session tokens` logs only a classification
  (`not_found` / `unreadable`) when the transcript cannot be read, because the
  error text names the path and the log sinks to a file; `checkpoint tokens`
  logs checkpoint IDs, session indices and agent names only. Notes lines name
  no paths either.
- Model ids, tool names, skill names and subagent types are printed as the
  agent recorded them; a delegation's objective, a search query, a file's
  contents and a command's arguments are never read into the report.
- `--json` carries exactly what the text report prints — the same
  contributors, details and notes — so mirroring it exposes nothing the
  terminal does not.
