# OpenCode Native Subagent Tracking Design

## Goal

Give OpenCode durable, per-task subagent tracking using the shared Entire task
record and checkpoint format, and stop OpenCode child sessions from registering
as top-level Entire sessions. One task record per `task` tool call; each record
carries the child's exact token usage from its own export.

## Verified OpenCode contract

Captured live on 2026-09-11 against OpenCode 1.18.30 with a probe plugin
planted beside Entire's own (`scripts/test-opencode-subagent-integration.sh`,
scenarios `single`, `concurrent`, `readonly`). Full detail in
`cmd/entire/cli/agent/opencode/AGENT.md`.

The primary agent invokes a subagent through the `task` tool. OpenCode creates
a **child session** with `parentID` set to the parent and runs a full turn in
it. Signals the plugin receives, per task, in order:

1. `tool.execute.before` on the parent: `{tool: "task", sessionID: P, callID}`
   plus `output.args: {prompt, subagent_type, description}`. No child ID yet.
2. `session.created` for the child: `info.parentID: P`, `info.agent`,
   `info.title: "<description> (@<agent> subagent)"`.
3. `message.part.updated` on the parent: the task part moves to
   `status: running` with `state.metadata: {parentSessionId: P, sessionId: C,
   model}`. This is the first moment `callID` is bound to the child ID.
4. The child's own turn: `message.updated`, `session.status busy`, its tool
   calls (`tool.execute.before/after` with `sessionID: C`), `session.status
   idle`, `session.idle`.
5. `tool.execute.after` on the parent: `{tool: "task", sessionID: P, callID,
   args}` and `output: {title, output: "<task id=\"C\" state=\"completed\">…",
   metadata: {parentSessionId: P, sessionId: C, model, truncated}}`.
6. `message.part.updated` on the parent: task part `status: completed` with
   `state.time: {start, end}`.

With two tasks launched in one assistant message, both launches (1–3) fire
before either completion (5), and the second child's `session.created` arrives
after the first's. The `callID → sessionId` binding therefore must come from
the task part metadata or the `tool.execute.after` output, never from event
adjacency.

`opencode export <childID>` returns the same `ExportSession` shape the parent
uses, with `info.parentID` and exact per-message tokens. A read-only child
(`explore`) produces the same signals with no `write`/`edit` parts.

Background tasks (`OPENCODE_EXPERIMENTAL_BACKGROUND_SUBAGENTS=true`) return
from `tool.execute.after` immediately with `metadata.background: true`; they
are out of scope and ignored.

## Current behaviour being replaced

The plugin's `session.created` handler treats every new session as the user's:
it resets tracking to the child and fires `session-start`, then `turn-start`
for the child's user message and `turn-end` on the child's idle. Observed
result: the child becomes a separate top-level Entire session, the checkpoint
is minted on the child, the commit condenses only the child, and the parent
logs "no files modified". The reset also clears the parent's seen-message set,
so the parent's hooks re-fire mid-turn once the child finishes.

## Design

### Plugin (`entire_plugin.ts`)

- **Child suppression.** Keep a `childSessions` set. Any `session.created` or
  `session.updated` whose `info.parentID` is set adds `info.id`. In the `event`
  handler, resolve the event's session ID first and return early when it is a
  child; no lifecycle hook fires for a child session. `resetSessionTracking`
  is therefore never called for a child, so the parent keeps its tracking
  state. `experimental.chat.system.transform` skips injection when
  `input.sessionID` is a child, so the parent's one-time context injection is
  not consumed by a child's first model call.
- **subagent-start** fires from the parent's `message.part.updated` when the
  part is a `task` tool part in `status: running` carrying
  `state.metadata.sessionId`, once per `callID` (a `announcedTasks` set).
  Payload: `{session_id: P, tool_use_id: callID, subagent_id: C,
  subagent_type: input.subagent_type, task_description: input.description}`.
  Synchronous, so the in-flight marker exists before the child does anything.
- **subagent-stop** fires from `tool.execute.after` when `input.tool ==
  "task"` and `output.metadata.background !== true`. Payload: `{session_id: P,
  tool_use_id: callID, subagent_id: output.metadata.sessionId, subagent_type:
  args.subagent_type, task_description: args.description, model}`.
  Synchronous: `opencode run` exits on the parent's idle shortly after, and an
  async hook would be killed.

### Go adapter (`agent/opencode`)

- Two new hook verbs, `subagent-start` and `subagent-stop`, in `HookNames`.
- `subagent-start` parses to `agent.SubagentStart` with `SessionID: P`,
  `SessionRef: sessionTranscriptPath(P)`, `ToolUseID`, `SubagentID: C`,
  `SubagentType`, `TaskDescription`, and the new `DeferredCompletion: true`.
  The child ID is validated with `validation.ValidateSessionID` because it
  later becomes an `opencode export` argument and a path component.
- `subagent-stop` exports the child via `fetchAndCacheExport(ctx, C)` (the
  existing staging-then-rename path; under
  `ENTIRE_TEST_OPENCODE_MOCK_EXPORT` it returns a pre-written file), reads the
  export, and computes `TokenUsage` with `CalculateTokenUsage(data, 0)`. It
  parses to `agent.SubagentEnd` with `Final: true`,
  `CompletionWithoutLaunch: true`, `SubagentTranscriptPath: <export path>`,
  `SubagentID: C`, labels from the payload, and `TokenUsage`. If the export
  fails, the event still fires with `SubagentTranscriptUnavailable: true`, no
  path, and no tokens, and the failure is logged at Warn: a record that says
  the transcript is unavailable beats a record that never exists.
- Files are not put on the event. The shared final capture extracts them from
  the declared child transcript through `ExtractModifiedFilesFromOffset`,
  which OpenCode already implements over the export format.

### Shared lifecycle (`agent/event.go`, `lifecycle.go`)

- New `Event.DeferredCompletion bool`: a `SubagentStart` whose completion
  arrives as a separate `Final` `SubagentEnd`. `handleLifecycleSubagentStart`
  records an in-flight task record (`EnsureTaskRecord`, so a duplicate start
  never overwrites a completed record) and skips the worktree pre-task
  baseline, which the analyzer-only final capture never reads.
- `handleSubagentStopFinal` passes `eventFilesOnly:
  event.SubagentTranscriptUnavailable` instead of
  `event.CompletionWithoutLaunch`. Copilot sets both flags, so its behaviour is
  unchanged; OpenCode sets only `CompletionWithoutLaunch`, so the final
  capture resolves the declared child transcript and runs the analyzer over
  it with `analyzerFilesOnly` (no worktree scan, which would sweep a
  concurrent sibling's files) and `bypassNoChangesSkip` (a read-only child
  still gets a record).
- `CompletionWithoutLaunch` on the stop makes the capture robust to a missed
  start (plugin restarted mid-task): `CompleteTaskRecord` creates the launch
  stub itself, and the parent is necessarily active mid-turn.

### Condensation

No change. `materializeTaskRecords` reads `DeclaredTranscriptPath`
(`.entire/tmp/<C>.json`), runs sanitize → externalize → redact, and writes
`tasks/<callID>/{agent-<C>.jsonl, task.json}` under the parent's checkpoint.
The child's files were merged into the parent's `FilesTouched` at completion,
so the parent's checkpoint attributes `docs/red.md` and the trail shows the
user's prompt.

## Security and privacy

- Child and parent IDs are validated at parse time and again by
  `DispatchLifecycleEvent`; an unsafe `tool_use_id` never reaches
  `tasks/<id>/`.
- The child export is written and read through the shared `.entire` root
  (`fetchAndCacheExport`), never through a hook-supplied path.
- Logs carry IDs, counts, and errors only.

## Non-goals

- Background subagents (experimental flag). If added later they need the
  two-signal `Final` model keyed on the child's own idle.
- Parent-level `SubagentTokens` aggregate (`SubagentAwareExtractor`). Record
  tokens are exact; the aggregate is a follow-up.
- Nested subagents beyond `subagent_depth: 1`. The same mapping applies
  recursively and is untested.

## Acceptance criteria

- A single foreground child yields exactly one completed task record on the
  parent with `AgentID = C`, the child's file, exact tokens, and a declared
  transcript path; the parent's checkpoint carries `tasks/<callID>/task.json`
  and `agent-<C>.jsonl`, and `entire session list` shows one session.
- Two concurrent children yield two records with disjoint files.
- A read-only child yields a completed record with no files and a transcript.
- No lifecycle hook fires for a child session; the parent's hooks fire once.
- An export failure yields a record with `TranscriptUnavailable` and no files
  rather than no record.
- Existing OpenCode parent-session behaviour and every other agent are
  unchanged; `CheckHookConfig` reports old plugins as outdated so `entire
  enable` upgrades them.
- Focused package tests, the integration suite, the Vogon canary, and `mise run
  check` pass; the paid OpenCode `TestSubagentCommitFlow` passes with the
  task-record assertion once authorized.
