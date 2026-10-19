# OpenCode — Subagent Tracking One-Pager

Scope: native tracking of the `task` tool's child sessions. Session and turn
lifecycle, transcript export and plugin installation are the existing
integration (`entire_plugin.ts`, `lifecycle.go`, `cli_commands.go`). The
generic task-record model is in
[Sessions and Checkpoints](../../../../../docs/architecture/sessions-and-checkpoints.md).

## Verdict: COMPATIBLE

Verified against OpenCode 1.18.30 and 1.18.33. The plugin sees every signal Entire needs,
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

## Gaps & Limitations

- macOS unresolved-symlink paths (`/var/…` vs `/private/var/…`) drop from a task's `files`; the parent's turn end still covers them.
- `opencode run` aborts a running background child at exit, and its teardown instance can leave the parent ACTIVE until the exited-owner sweep; the TUI is unaffected.
- Two calls in one message that resume the same child: the queued call ends at the first run's idle, so its own work lands in no record.
- A call whose start the plugin never saw (restart mid-task) declares the child's last call, which is this call unless the child served a later one first.
- A reused-`callID` call has no live marker while it runs (its start finds the earlier call's record under the same key, and the Go start path does not re-key), so commit overlap checks and the SessionEnd sweep miss it; if the parent ends before its stop, the call is lost.
- A reused-`callID` stop with no `started_at` (the plugin restarted between the call's before and after hooks) for a child already recorded under that `callID` is indistinguishable from a duplicate, so it is skipped and that call's work is not recorded; deliberate, to avoid double-counting a real duplicate.
- entire.io joins task records to task calls on exact `tool_use_id`, so mapped or re-keyed records show as unmatched until its subagent view joins as above.
- A running background child's edits reach the parent's files only at its stop. Until then, an IDLE parent with no tracked files of its own is left out of a commit that another session also claims; otherwise it condenses and stores the child's export so far.
