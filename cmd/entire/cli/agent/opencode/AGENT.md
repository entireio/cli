# OpenCode — Subagent Tracking One-Pager

Scope: native tracking of the `task` tool's child sessions. Session and turn
lifecycle, transcript export and plugin installation are the existing
integration (`entire_plugin.ts`, `lifecycle.go`, `cli_commands.go`). The
generic task-record model is in
[Sessions and Checkpoints](../../../../../docs/architecture/sessions-and-checkpoints.md).

## Verdict: COMPATIBLE

Verified against OpenCode 1.18.30 and 1.18.33, and 2.1.1 (see
[OpenCode 2](#opencode-2)). The plugin sees every signal Entire needs,
and the child session ID is on the launch signal, the completion signal and
the child's own export. Live verification was done during development with a
probe script that is not kept in the repo.

## Subagent Model

- The primary agent calls the **`task` tool** (`{description, prompt,
  subagent_type, task_id?, background?}`); each call runs a **child session**
  with its own ID, messages, tokens and `opencode export` payload.
- Join keys: `ToolUseID` = the task part's `callID`; `SubagentID` = the child
  session ID (`ses_…`, also the export key); the parent is the child's
  `info.parentID`. `callID` is model-generated and unique only within the
  calling session; some providers number calls per conversation, so a later
  call can reuse one (the plugin keys its per-call state by session + `callID`).
- Record key (`tasks/<key>/`): a `callID` outside `[A-Za-z0-9_-]` is mapped
  to `<callID, unsafe chars as _>-<12 hex of its SHA-256>`
  (`functions.task:0` → `functions_task_0-…`). A stop reusing a recorded
  `callID` for another call is keyed `<callID>-<child>`, or
  `<callID>-<child>-<started_at ms>` for a later call on the same child
  (12 hex of a SHA-256 suffix if that is not path-safe). To join a record to
  its task part without a call field, apply the same mapping to `callID`, or
  match `agent_id` to the part's `state.metadata.sessionId`, then
  `started_at` to the call start when one child served several calls.
- `task_id` resumes an existing child, so one child can back several calls.
- Child sessions are suppressed plugin-side (`childSessions`, learned from
  `parentID` and task metadata): no `session-start`, `turn-start` or
  `turn-end` is ever sent for a child, including one the user drives directly.

## Event Mapping

| OpenCode signal | Entire event | Payload |
|-----------------|--------------|---------|
| `tool.execute.before`, `tool == "task"` | (plugin only) | Records the call's start, sent as `started_at`; falls back to the part's `state.time.start` when the hook was missed. |
| `message.part.updated`, task part `running` with `metadata.sessionId` | `SubagentStart` (`subagent-start`) | `ToolUseID = callID`, `SessionID` = top-level session, `SubagentID` = child, type/description from `args`, `DeferredCompletion`. |
| `tool.execute.after`, `tool == "task"` | `SubagentEnd` (`subagent-stop`) | Same IDs, `Final`, `CompletionWithoutLaunch`, no declared transcript. |
| announced task part `error` (abort, cancel, throw) | `SubagentEnd` (`subagent-stop`) | Same payload; OpenCode skips `tool.execute.after` in these cases. |
| background child's own `session.status` idle | `SubagentEnd` (`subagent-stop`) | The held payload for that child (see below). |

## One Start, One Stop

Every announced call fires exactly one `subagent-stop` under the same
`ToolUseID`. The plugin keeps each start payload until its task stops and
every stop path takes it, so a second path finds nothing to send. A stop
that runs before the start was announced fires under the hook key and the
late announcement is skipped. A call that fails before its child is bound
was never announced and fires nothing.

- **Background** (`background: true`, or promoted mid-run): `tool.execute.after`
  returns at launch with `metadata.background`, so the stop is held per child
  and fired on the child's idle. A `task_id` call that joins a running child
  queues behind it, and each idle after a busy period fires the oldest held
  stop. When the job ends (the synthetic `<task … state="completed|error">`
  result, or a `MessageAbortedError` for the child or its launcher), every
  held stop for that child fires; for a busy child, at its next idle.
- **Nested** (`subagent_depth > 1`): a child's own task call is announced and
  stopped on the **top-level** session (`rootOf`), as its own record.
- **Command subtasks** (`subtask: true`): the hooks carry the part `id`
  (`prt_…`), not the `callID`; the plugin maps one to the other when it
  announces, so start and stop share a `ToolUseID`.
- **Abort**: any end of the parent's stream cancels the child before OpenCode
  marks the part `error`, so the stop's export already holds the child's work.

## Transcript Export and Slicing

The Final capture, after its skip checks, runs `opencode export <child>`
(`FetchSubagentTranscript`, without git's repo-override variables) and
declares the result; files and exact tokens come from that export. The
declared file is `.entire/tmp/<child>.<callID>.json`, cut to this call:
from the first non-synthetic, non-compaction user message at or after
`started_at`, up to the next such prompt (and, for a later re-export, no
later than the record's completion). Without a `started_at` the slice starts
at the last such prompt by the completion (or by now, at stop time); a call
that never prompted its child declares an empty slice. A failed export
completes the record transcript-unavailable, and condensation exports a
completed record again with the same bounds; exports in one condensation or
SessionEnd sweep share one 30s budget (`agent.SubagentFetchBudget`).
The background result that OpenCode injects into the parent is synthetic
text, so prompt extraction and the summary skip it.

A running child's edits reach the parent's files only at its stop. When an
IDLE parent with no tracked files of its own commits alongside another
session, post-commit's co-authorship check asks the agent what the running
call has modified so far (`LiveSubagentModifiedFiles`): it exports the child
to a private staging file, cut to the running call, and removes it, writing
nothing a capture reads. These checks share 10s of the commit's 30s fetch
budget. The commit stores the live record without a transcript; it is
stored in full once the call completes.

## Gaps & Limitations

- macOS unresolved-symlink paths (`/var/…` vs `/private/var/…`) drop from a task's `files`; the parent's turn end still covers them.
- `opencode run` aborts a running background child at exit, and its teardown instance can leave the parent ACTIVE until the exited-owner sweep; the TUI is unaffected.
- Two calls in one message that resume the same child: the queued call ends at the first run's idle, so its own work lands in no record.
- A call whose start the plugin never saw (restart mid-task) declares the child's last call, which is this call unless the child served a later one first.
- A reused-`callID` call has no live marker while it runs (its start finds the earlier call's record under the same key, and the Go start path does not re-key), so commit overlap checks and the SessionEnd sweep miss it; if the parent ends before its stop, the call is lost.
- A reused-`callID` stop with no `started_at` (the plugin restarted between the call's before and after hooks) for a child already recorded under that `callID` is indistinguishable from a duplicate, so it is skipped and that call's work is not recorded; deliberate, to avoid double-counting a real duplicate.
- entire.io joins task records to task calls on exact `tool_use_id`, so mapped or re-keyed records show as unmatched until its subagent view joins as above.

## OpenCode 2

OpenCode 2 (npm `@opencode/cli`) changed the plugin API, the export command
and the export format. One plugin file serves both versions through its only
export. OpenCode 1 before v1.3 calls every export as a function (an object
export crashes it), while OpenCode 2 requires the default export to be an
`{id, setup}` object, so the default export is chosen when the module loads:
OpenCode 1 sets `OPENCODE=1` in its own environment and gets the plugin
function; anything else gets `{id, server, setup}`. Verified live on 1.0.224,
1.1.65, 1.2.27, 1.3.0, 1.3.3, 1.4.17, 1.14.51, 1.17.20, 1.18.33, 1.19.3 and
2.0.0, 2.0.16, 2.0.32, 2.1.1, 2.1.2. OpenCode 1 also calls `setup` with a
reduced context, which the plugin ignores.

The plugin lives in `.opencode/plugin/entire.ts`, which every OpenCode loads.
Earlier CLIs wrote `.opencode/plugins/entire.ts`; OpenCode 0.15.x and 1.0.0
refuse to start while `.opencode/plugins` exists, and 1.0.224+ and 2.x load
both directories. Installing removes an Entire plugin from the old directory
(and the directory, when that empties it), `entire status`/`doctor` report one
left there as outdated, and an install from an earlier CLI still counts as
installed. On 0.15.31 and 1.0.0 the plugin loads and its hooks fire
(verified live); those versions then crash on today's provider responses
(`DecimalError`) before finishing a turn, with or without Entire.

- **Shared service.** OpenCode 2 runs plugins in a background service shared
  by every repository (`opencode run --standalone` runs a private one). Prompt,
  context and tool hooks are per location; the event stream is process-wide and
  `session.execution.*` carries no location. The plugin acts only on sessions
  its own prompt hook saw, classified as top-level or child with
  `ctx.session.get` (a child's prompt reaches the hook before any event names
  its parent). Hooks never block: those that must finish first are awaited
  asynchronous spawns with a timeout.
- **Turns.** A turn is one execution. `turn-start` fires from the prompt hook
  when the session has no active turn; a prompt sent mid-turn joins it and is
  read from the transcript. An execution with no prompt (a background result
  delivered to the parent) starts a turn with no prompt. `turn-end` fires on
  `session.execution.{succeeded,failed,interrupted}`, compaction on
  `session.compaction.ended`, `session-end` on `location.shutdown`.
- **Exit and order.** `opencode run --standalone` kills plugin cleanup about
  3s after the turn. `turn-end`, `session-end`, compaction and held subagent
  stops therefore run detached, each after the session's detached hooks still
  running (polling their PIDs, for at most 120s), and the next `turn-start`
  waits for them, so a session's hooks land in OpenCode 1 order. Every
  subagent stop also waits for that call's `subagent-start`. Cleanup waits up
  to 2.5s for the final hooks; a slower `turn-end` can still be running when
  the next `opencode run` starts. Events are queued per top-level session, so
  one session's slow hook never delays another's. Hooks run without the
  service's `GIT_DIR`/`GIT_WORK_TREE`/`GIT_INDEX_FILE`/`GIT_COMMON_DIR`.
- **Subagents.** The tool is `subagent` (`{agent, description, prompt,
  background?, sessionID?}`). The child ID is first on `session.tool.progress`
  (`metadata.sessionID`, status `running`) → `subagent-start`;
  `tool.execute.after` (`result.output.sessionID`) → `subagent-stop`. A
  background call returns at launch (`result.output.status: "running"`), often
  before its first progress update, so it is announced there and its stop is
  held per child until the child's own execution ends (interrupt of the
  launcher drains every held stop, as on 1.x).
- **Export.** `opencode session export --standalone <id>` (without
  `--standalone` it would start or attach to the shared service). The CLI
  picks the command from `opencode --version`; only a readable 0.x/1.x version
  selects 1.x commands. A 2.x export (flat, typed messages) is converted to
  the 1.x shape at capture (`normalizeOpenCodeExport`), so every reader and
  stored checkpoint sees one format: tool names map back (`subagent`→`task`,
  `patch`→`apply_patch`, `shell`→`bash`), `path` inputs and patch `files[].file`
  become absolute `filePath`s, synthetic and migration-notice messages become
  synthetic user text, compaction becomes a 1.x marker plus a summary message,
  idle markers are dropped. Sessions migrated from a 1.x database keep 1.x
  tool names and input keys, so both are accepted.
- **Resume.** Each converted message keeps the OpenCode 2 messages it came
  from under `info.opencode2`, verbatim (an idle marker rides with the
  message before it). Resuming where OpenCode does not have the session
  imports exactly those (`openCodeV2ImportPayload`, `opencode session import
  --standalone`, run from the worktree, which becomes the session's location);
  attachments that redaction or image externalization changed are dropped,
  since OpenCode validates them and rejects the whole import otherwise. A
  session OpenCode still has is never deleted or replaced. A transcript
  captured from OpenCode 1 has no OpenCode 2 messages and cannot be imported
  into OpenCode 2.

OpenCode 2 gaps:
- Background subagents still running when `opencode run --standalone` exits
  are killed with it (as with 1.x `run`).
- OpenCode Desktop and Windows on 2.x were not verified live.
