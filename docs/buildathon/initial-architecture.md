# Entire Echo: initial understanding and intended architecture

**Milestone:** Bengaluru Tech Week Buildathon, Track E1 — Checkpoint-Native
Developer Experience.  
**Scope of this document:** investigation and architecture only. No product
functionality is implemented by this milestone.

## Executive decision

Build Entire Echo as a small, external Entire plugin named `entire-echo`.
It will read one existing checkpoint through the public CLI output, ask the
locally available `entire graph` command focused questions about the changed
symbols, and construct a portable, evidence-linked review model. A minimal
local web view (or a linear terminal view when a browser is unavailable) will
make the review keyboard- and screen-reader-friendly and offer opt-in speech.

This is deliberately an *evidence presentation and continuation aid*, not a
second checkpoint store, an agent runner, or an autonomous code changer.

## Confirmed repository findings

### Repository shape

This is a Go CLI using Cobra and huh. The relevant implementation is in
`cmd/entire/cli`; persistent checkpoint code is in
`cmd/entire/cli/checkpoint`; the exported checkpoint data contracts are in
`api/checkpoint`. `docs/architecture` documents system-level choices. There is
an existing external-command/plugin mechanism, so an Echo command need not
alter the built-in command tree.

### Checkpoint command and reconstruction surface

- `newCheckpointGroupCmd` registers `list`, `resume`, `explain`, `tokens`,
  `policy`, and `search`. `checkpoint explain` accepts a checkpoint ID or
  commit, can resolve a commit trailer to a checkpoint, and supports
  `--json`, `--transcript`, `--raw-transcript`, `--full`, and `--session-index`.
  Source: `cmd/entire/cli/checkpoint_group.go`, `cmd/entire/cli/explain.go`.
- The explanation flow opens a persistent checkpoint store, lists/resolves the
  target, loads its summary and latest session content, and renders the result.
  Source: `newExplainCheckpointLookup`, `loadCheckpointForExplain`,
  `runExplainCheckpointWithLookup`, and `formatCheckpointOutput` in
  `cmd/entire/cli/explain.go`.
- A checkpoint's transcript is explicitly scoped to its start offset.
  `scopeTranscriptForCheckpoint` uses agent-specific slicing (Gemini and
  OpenCode) or line slicing for other supported agents. This is the correct
  boundary for Echo's “what was requested/implemented” reconstruction; Echo
  must not treat the full session transcript as checkpoint-specific evidence.
  Source: `cmd/entire/cli/explain.go`.

### Checkpoint data and storage

- `checkpoint.SessionContent` provides session metadata, transcript bytes, and
  prompts. `checkpoint.Metadata` includes files touched, agent/model, transcript
  start, token usage, existing generated summary, review context, and
  attribution. `checkpoint.Summary` already has intent, outcome, learnings,
  friction, and open items. Source: `api/checkpoint/metadata.go`.
- The persisted tree has root `metadata.json` (`CheckpointSummary`), then
  numbered session directories containing metadata, `full.jsonl`, optional
  compact transcript, `prompt.txt`, and content hash; assets can be
  externalized. Persistent checkpoint reading is split into checkpoint-level
  and session-level interfaces. Source: `api/checkpoint/metadata.go`,
  `api/checkpoint/interfaces.go`, `cmd/entire/cli/checkpoint/persistent.go`.
- Existing data can be locally backed by git and read from a remote API reader
  for cross-repository explain. Source: `cmd/entire/cli/checkpoint/persistent.go`,
  `cmd/entire/cli/checkpoint_api_reader.go`.

### Extension and Graph surface

- If a word is not a built-in command, the CLI resolves `entire-<word>` from
  `PATH`, passes through stdin/stdout/stderr and exit code, and supplies
  `ENTIRE_CLI_VERSION`, `ENTIRE_REPO_ROOT` when available, and a per-plugin
  data directory. Built-ins win. Source: `cmd/entire/cli/plugin.go`,
  `cmd/entire/main.go`, and `cmd/entire/cli/plugin_store.go`.
- Plugin environments are filtered; only an allowlist, `ENTIRE_*`, locale/XDG,
  and explicit user opt-ins pass through. Echo therefore should not depend on
  ambient credentials. Source: `cmd/entire/cli/plugin_env.go`.
- The available `entire graph` command is a local code-graph tool. Its reported
  capabilities include Go call/type/data-flow relations. The repository's
  external-plugin architecture document uses `graph` as a plugin example; no
  linked Go package named as an Entire Graph client was found in the inspected
  CLI sources. Treat Graph invocation as a CLI capability, not an in-process
  API. Source: `.entire/graph-agent.md`,
  `docs/architecture/external-commands.md`, and `cmd/entire/cli/plugin.go`.
- Entire already exposes an `ACCESSIBLE` mode and `IsAccessibleMode()` helper.
  Source: `cmd/entire/cli/utils.go`, `cmd/entire/cli/uiform/uiform.go`.

## Smallest hackathon-ready architecture

### Input

`entire echo <checkpoint-id-or-commit>` resolves exactly one checkpoint using
the installed parent CLI:

1. run `entire checkpoint explain <target> --json` for non-transcript
   metadata;
2. run `entire checkpoint explain <target> --transcript` for the selected
   session's stored transcript; and
3. obtain the associated commit diff through local git only when it can be
   resolved from the checkpoint/commit linkage.

Inputs remain local to the repository and the user's machine. Echo should
accept an explicit transcript/session selector later, but the first demo can
match `checkpoint explain`'s latest-session default.

### Processing stages

1. **Acquire and validate.** Parse the parent CLI's machine-readable metadata,
   retain the exact checkpoint ID, session ID, source command, and byte/line
   boundaries. Refuse malformed or missing data rather than inventing it.
2. **Reconstruct.** Use the checkpoint-scoped transcript plus stored prompts,
   summary, files touched, and associated diff to form factual cards: request,
   implementation, and continuation state.
3. **Ground Graph inquiries.** Extract changed files and candidate symbols from
   the diff; issue focused `entire graph search --repo . --profile full`
   queries such as “callers and affected behavior of `<symbol>`.” Only attach
   returned locations/relations as Graph evidence. Do not claim a Graph result
   proves runtime behavior.
4. **Review synthesis.** Create findings in four fixed categories: implemented,
   missing/uncertain, potentially affected, and continuation. A deterministic
   template produces the first version; an optional local/approved summarizer
   may simplify wording, but cannot add a finding without an evidence link.
5. **Render and speak.** Render a linear, accessible review. Speech reads one
   short card at a time only on explicit user action; playback controls include
   pause, repeat, next, previous, rate, and “read evidence.”

### Evidence model

Every displayed proposition has one or more typed evidence records:

| Claim class | Required evidence |
| --- | --- |
| Requested | checkpoint-scoped user prompt or stored prompt |
| Implemented | diff hunk and/or checkpoint transcript action, plus changed file |
| Existing summary | `Metadata.Summary` field, labelled “stored summary” |
| Possibly affected | Graph result with symbol, relation, file, and lines; labelled “potential” |
| Missing/uncertain | an explicit open item/friction, an absent expected evidence item, or a reviewer question; never stated as fact |
| Continue safely | exact checkpoint/session/commit identifiers, changed files, open items, and commands to rerun |

Each record includes source kind, immutable locator (checkpoint/session,
commit/hunk, or Graph symbol/file/lines), retrieval command, excerpt bounded to
the minimum useful context, and confidence label. “Confirmed” is reserved for
direct checkpoint/diff/Graph locations; “inference” and “question” are visibly
different states.

### Accessible UI and voice interaction

The primary view is deliberately linear: a one-sentence overview followed by
four numbered cards (requested, implemented, uncertainty, impact), then
evidence and continuation. It avoids dense side-by-side diffs, auto-advancing
text, color-only status, and unbounded transcript dumps.

- Full keyboard operation, visible focus, semantic headings/landmarks, plain
  language, configurable reading density, and a “show source” control per
  claim are required.
- Respect `ACCESSIBLE=1` by choosing the linear text renderer and never
  requiring a terminal TUI.
- A local browser renderer may use browser speech synthesis for the demo. If
  unavailable, Echo still works as text and exposes the review JSON; it never
  makes speech required for comprehension.
- Voice commands are a stretch adapter, not the control plane: support a
  push-to-talk or explicit listen button for “next”, “previous”, “repeat”, and
  “read evidence”; always provide equivalent keyboard controls and display the
  recognized command before acting.

### Outputs

- Human-readable accessible review (terminal fallback and local web view).
- `--json` evidence bundle for agents and tools, with schema version and no
  hidden prose-only claims.
- Optional local HTML/JSON export, written only to a user-selected path.
- “Continue safely” handoff containing checkpoint ID, session ID, commit,
  changed files, open questions, evidence links, and safe read-only commands.

### Failure handling

| Failure | Behavior |
| --- | --- |
| Checkpoint/commit not found or ambiguous | Show the parent CLI error and request an explicit checkpoint ID; produce no review. |
| Transcript unavailable or cannot be scoped | Render metadata/diff-only cards and label request/implementation reconstruction unavailable. |
| No stored summary | Do not invoke `--generate`; build deterministic cards and label the summary absent. |
| Graph unavailable, times out, or has no result | Keep checkpoint review; omit impact claims and show “Graph evidence unavailable,” not “no impact.” |
| Graph result is partial | Preserve the tool warning and downgrade related claims to potential. |
| Diff unavailable | Do not assert implementation from file names alone; retain transcript-backed evidence. |
| Speech/browser unavailable | Fall back to accessible text and JSON, without blocking. |
| Voice recognition ambiguous | Display and ask for confirmation; no destructive actions exist in this milestone. |

## Rejected options

1. **Modify `checkpoint explain` directly.** Rejected for the hackathon: it
   increases risk in a mature command and couples the experiment to core
   release behavior. The plugin boundary already supports `entire echo`.
2. **Read git checkpoint storage directly.** Rejected: the CLI already handles
   store routing, remote/API reads, blob prefetch, legacy paths, and transcript
   scoping. Echo should consume supported CLI output first.
3. **Send checkpoints/diffs to a cloud reviewer by default.** Rejected: it
   weakens privacy and makes evidence provenance ambiguous. Any future model
   provider must be explicit and retain the same evidence gate.
4. **Autonomous review verdicts or code edits.** Rejected: this milestone is a
   review and handoff tool. “May be missing” must remain an evidence-backed
   question, not an automated merge decision.
5. **Voice-only interaction.** Rejected: voice is helpful but is not reliable
   or universally accessible. Text, keyboard, and screen-reader paths remain
   first-class.

## Assumptions and open questions

### Assumptions (not yet verified)

- `entire-echo` can invoke the installed `entire` binary and `entire graph`
  command from its inherited `PATH` on supported development machines.
- A local browser can be launched or the user can open a local exported page;
  the initial demo can use browser speech synthesis without a server-side
  component.
- A checkpoint's latest session is sufficient for the first demo; multi-session
  comparison is deferred.
- Diff linkage can be recovered for ordinary committed checkpoints; imported or
  unusual checkpoint shapes may be metadata/transcript-only.

### Open questions

- Which delivery form is preferred for judging: terminal-only plugin, local web
  app launched by the plugin, or both?
- Is an approved local model/runtime available, or should the demo be fully
  deterministic template synthesis?
- What is the desired policy for storing optional exports and browser history?
- Should Graph queries be user-visible and replayable in the output bundle?
- Which accessibility testing resources (screen reader, keyboard-only tester,
  dyslexia/ADHD feedback) are available before the demo?

## Technical risks and architecture responses

- **Transcript formats differ by agent.** Reuse parent CLI scoped transcript
  output rather than parsing agent formats in Echo.
- **Graph results are evidence of static relations, not certainty.** Use
  “potentially affected,” retain exact relation/locator, and never elevate to
  confirmed behavior without direct evidence.
- **Plugin environment is intentionally restricted.** Depend only on CLI
  commands and local paths; require explicit configuration for anything else.
- **Speech APIs differ by platform/browser.** Keep voice an adapter around a
  canonical review JSON model.
- **Reading overload can be recreated by excessive evidence.** Start with
  short claims, disclose one evidence item at a time, and leave raw transcript
  as an explicit drill-down.

## Entire Graph commands used in this investigation

1. `entire graph capabilities --json`
2. `entire graph search --repo . --profile full --query "Locate checkpoint CLI commands, checkpoint data structures and storage, checkpoint explanation or reconstruction logic, plugin extension points, and existing Graph integration for an accessible checkpoint review tool."` — the combined search did not emit result records before the command window ended.
3. `entire graph search --repo . --profile full --query "checkpoint explanation reconstruct transcript command"`
4. `entire graph search --repo . --profile full --query "checkpoint persistent store metadata transcript assets data structure"`
5. `entire graph search --repo . --profile full --query "checkpoint explain command resolve checkpoint render transcript diff files"` — no result output before the command window ended.
6. `entire graph search --repo . --profile fast --query "external plugin command dispatch"`

Graph returned relevant source candidates including
`scopeTranscriptForCheckpoint`, `Metadata.GetTranscriptStart`,
`kindRoutingStore.ReadSessionMetadata`, `apiCheckpointReader.ReadSessionMetadataAndPrompts`,
`generateCheckpointSummary`, and `isPluginCandidate`. Full-profile searches
reported a working-tree snapshot warning and skipped one minified JSON runner;
these are limitations on Graph completeness, not application findings.

## Source files read to verify Graph findings

- `cmd/entire/cli/checkpoint_group.go`
- `cmd/entire/cli/explain.go`
- `api/checkpoint/metadata.go`
- `api/checkpoint/interfaces.go`
- `cmd/entire/cli/checkpoint/persistent.go`
- `cmd/entire/cli/checkpoint_api_reader.go`
- `cmd/entire/cli/plugin.go`
- `cmd/entire/cli/plugin_env.go`
- `cmd/entire/cli/plugin_group.go`
- `cmd/entire/cli/plugin_manifest.go`
- `cmd/entire/cli/plugin_store.go`
- `cmd/entire/main.go`
- `cmd/entire/cli/utils.go`
- `cmd/entire/cli/uiform/uiform.go`
- `docs/architecture/external-commands.md`
- `.entire/graph-agent.md`

The source verification is the basis for all statements labelled confirmed in
this document. The proposed Echo behavior, voice stack, and delivery form are
architecture proposals, not existing product behavior.
