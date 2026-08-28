# Antigravity CLI (`agy`) — Integration One-Pager

## Verdict: COMPATIBLE (Preview)

The `agy` binary (Antigravity 2.0, Google's Gemini CLI successor) supports
workspace-scoped hooks via `.agents/hooks.json` and writes JSONL transcripts to
a predictable per-conversation location. The integration is marked
**Preview** (`IsPreview() == true`) — the label shows in the agent selector,
`entire agent list`, and the hook-install message. Wire format captured on
agy **1.0.14/1.0.15** (real captured stdin, not docs) and re-verified
unchanged against agy **1.1.1** (2026-07-13); agy is fast-moving.

**Key differences from other agents:** hooks fire per *model invocation*, not
per user prompt; the transcript is written **after** the Stop hook; token usage
appears **only** in the title/statusline JSON feed (never in transcripts or
hook payloads); tool args can arrive double-encoded.

## Binary

- Install: `curl`-based installer / GitHub releases (`google-antigravity/antigravity-cli`).
- Headless: `agy -p "<prompt>" --add-dir <workspace>` (without `--add-dir`, agy
  runs in `~/.gemini/antigravity-cli/scratch/`, not the cwd).
- Resume: `agy --conversation <id>` (used by `FormatResumeCommand`).
- Auth: consumer OAuth, ADC + `--project`, or (agy ≥ 1.1.13) **Gemini API-key
  mode**: `{"modelProvider":"gemini"}` in `~/.gemini/antigravity-cli/settings.json`
  plus `GEMINI_API_KEY` in the environment — no account session, keyring or
  browser flow; startup fails fast if the key is unset. Optional
  `GOOGLE_GEMINI_BASE_URL`. agy prefers `GOOGLE_API_KEY` when both keys are set.
  The default (`cloudcode-pa.googleapis.com`) backend remains entitlement-gated;
  see `e2e/README.md`.
- Version notes (1.1.1 → 1.1.22): hook surface unchanged (5 hook types);
  1.1.10 fixed hook ordering so `Stop`/`PostInvocation` fire reliably;
  1.1.12 answers `-p "/hooks"` (and `/help`, `/changelog`) locally without a
  model turn — `entire doctor` uses `agy -p /hooks --add-dir <root>
  --output-format json` to verify the workspace hooks actually load;
  1.1.8/1.1.20 added `--output-format json|stream-json` and made headless exit
  codes reflect only run-level failures; 1.1.9 expands `/skill` in `-p`.

## Hook Mechanism

Hooks live in the workspace at `.agents/hooks.json` under a named key (ours is
`"entire"`). Tool events (`PreToolUse`/`PostToolUse`) use `matcher` + `hooks`
lists; `PreInvocation`/`PostInvocation`/`Stop` use flat handler lists. Install
replaces the whole `"entire"` entry (idempotent by compact-JSON comparison), so
stale installs self-heal on the next `entire enable`.

### Hook Names and Event Mapping

Only the three hooks with lifecycle meaning are installed. agy's
`PostToolUse`/`PostInvocation` are deliberately **not** installed — no
lifecycle mapping, and each would spawn a no-op `entire` subprocess per tool
call / model invocation. Unknown verbs parse to a nil event, so a stale
hooks.json can never fail an agy turn.

| agy hook | Fires | Entire event |
|----------|-------|--------------|
| `PreInvocation` | before **every model invocation** (`invocationNum` is **0-indexed**) | `TurnStart` when `invocationNum == 0`; for `> 0`, a `TurnStart` with `Event.SuppressIfSessionActive` — the dispatcher drops it only when a turn is genuinely mid-flight (`Phase == ACTIVE` and not stuck), so resumes (`agy --conversation`) are tracked while follow-up invocations don't clobber the pre-prompt baseline |
| `PreToolUse` (matcher `*`) | before each tool call | `ToolUse` for mutating tools (`write_to_file`, `replace_file_content`, `multi_replace_file_content`) → `FilesTouched` |
| `Stop` | at agent stop; payload carries `fullyIdle` | `TurnEnd` when `fullyIdle == true`; nil otherwise (background tasks still running) |

There is **no SessionStart surface** — no way to print a "tracked by entire"
banner inside agy (silent tracking, same as Cursor/OpenCode/Copilot/Pi).

### Hook Input Payloads (Captured)

Common fields consumed: `conversationId`, `transcriptPath`. agy also sends
`workspacePaths`, `artifactDirectoryPath`, `stepIdx`, `initialNumSteps`,
`executionNum`, `terminationReason`, `error` — tolerated but not decoded (add
fields only when something reads them). Captured fixtures in `testdata/`.

- `PreInvocation`: `invocationNum` (0-indexed; the docs now state this
  explicitly). `initialNumSteps` is unusable as a "first?" signal — agy inserts
  the user prompt as step 0, so it is already 1 on the first invocation.
- `PreToolUse`: `toolCall.name`, `toolCall.args`. **Args can be
  double-encoded** (`"TargetFile":"\"foo.txt\""`, `"Overwrite":"true"`);
  `decodeAgyString`/`decodeAgyBool` accept both the documented and the wire
  shape. Paths are symlink-resolved (`resolveAgySymlinks`) so macOS
  `/tmp → /private/tmp` doesn't defeat repo-relative filtering.
- `Stop`: `fullyIdle` (required).

## Transcript

Written to
`~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/transcript_full.jsonl`
(the hook payload's `transcriptPath` points there; agy also writes a truncated
`transcript.jsonl` alongside). Schema: one step object per line —
`step_index`, `source` (`USER_EXPLICIT`/`MODEL`/`SYSTEM`), `type`
(`USER_INPUT`/`PLANNER_RESPONSE`/`CODE_ACTION`/...), `content`, `tool_calls`.

**agy writes the transcript AFTER the Stop hook** (sometimes seconds later).
Consequences, all handled:

- `PrepareTranscript` (TranscriptPreparer) briefly waits, then materialises an
  empty placeholder so the framework's fileExists check doesn't abort the turn.
- Prompts are re-extracted at condensation via the late-flush fallback
  (`resolvePromptsFromLateFlushedTranscript`) when `prompt.txt` is empty.
- A first-turn mid-turn commit condenses against the empty placeholder as a
  **files/prompt-only checkpoint** (degrade is agy-scoped; other agents keep
  the error/retry invariant).

### TranscriptAnalyzer

`ExtractPrompts` (USER_INPUT steps, `<USER_REQUEST>` unwrap),
`GetTranscriptPosition`, and `ExtractModifiedFilesFromOffset` share one
offset metric: non-blank JSONL lines, blank lines skipped **before** counting
(`forEachNonBlankLine` is the single owner — the position one method stores is
consumed by the others). Validated against 385 real captured transcripts
(0 errors, 0 offset mismatches).

## Token Usage (out-of-band)

agy never writes token data into transcripts or hook payloads. The **only**
surface is the state JSON piped to the global `statusLine`/`title` command
slots (`context_window`: `total_input_tokens`, `total_output_tokens`,
`current_usage.*`). The integration:

- claims the lower-stakes **`title` slot** in agy's global
  `~/.gemini/antigravity-cli/settings.json` with
  `entire hooks antigravity title-tee` (wrapping and preserving any
  pre-existing user title command via `--wrap`);
- the tee appends deduped snapshots to a per-conversation JSONL cache;
- `OutOfBandTokenSource`: `SnapshotTokenBaseline` at TurnStart (streaming
  last-line read), `CalculateTokenUsageSince` at TurnEnd (cumulative totals
  minus baseline; cache fields from snapshot lines strictly after the
  baseline timestamp);
- the delta is recorded even when a turn ends with everything already
  committed (agy's normal flow), so no tokens are lost;
- checkpoint metadata takes the checkpoint-scoped accumulator
  (`state.CheckpointTokenUsage`), never the session-cumulative total.

`entire doctor` warns when hooks are installed but the title slot doesn't
route through the tee; `entire agent remove antigravity` restores/removes the
slot (matching the bare tee **by shape**, so uninstalling from a different
worktree than the install still cleans up, including the legacy local-dev
`go run …` form older versions wrote).

## Config Preservation

- `.agents/hooks.json`: foreign keys preserved; only the `"entire"` entry is
  managed. `HookConfig` keeps the `PostToolUse`/`PostInvocation`/`enabled`
  schema fields for round-trip fidelity and stale-install detection.
- Global `settings.json`: only the `title` key is touched; user title commands
  are wrapped, restored on uninstall, and anything unrecognized is left alone.

## Gaps & Limitations (Preview)

- **Silent tracking** in the agy UI (no SessionStart surface); `entire status`
  is the visibility surface.
- **Headless subagent runs leave a ghost parent session**: agy runs subagents
  as separate conversations with their own hooks; in `-p` mode the parent
  conversation only receives `fullyIdle=false` Stops before the process exits,
  so its session stays ACTIVE (visible in `entire status` until the stale
  threshold). The subagent's work condenses normally, and ghost sessions with
  no tracked files no longer pin shadow branches.
- **Mid-turn commit token scoping is coarse**: such checkpoints record zero
  tokens; the turn's delta lands on the next condensation (session totals stay
  correct).
- **First-turn mid-turn commits** may produce checkpoints without transcript
  content (see Transcript above).
- **Token capture depends on the title slot** staying routed through the tee
  (doctor-checked, setup-repaired).
- **agy does not execute hooks in Gemini API-key mode** (verified 1.1.22: hooks
  load, tools run, no hook fires — on OAuth/ADC the same hooks fire; reported as
  google-antigravity/antigravity-cli#893). So the
  e2e harness's API-key mode authenticates fine but cannot produce checkpoints,
  and the CI leg stays dispatch-only (ADC or OAuth needed to pass). The default
  `cloudcode-pa` backend stays entitlement-gated (AUTH_PERMISSION_DENIED,
  subject 110002 without a Gemini Code Assist subscription); the harness fails
  fast on those walls and on `GEMINI_API_KEY … not set` / `API_KEY_INVALID`.
  Details: `e2e/README.md`.
- **Transcript-derived file lists can be incomplete**: agy sometimes persists a
  `PLANNER_RESPONSE` step with `truncated_fields`, dropping `TargetFile` from a
  mutating tool call (~0.8% of `replace_file_content` calls in one corpus).
  `ExtractModifiedFilesFromOffset` logs a WARN per dropped call instead of
  silently skipping; live PreToolUse stdin is never truncated, so normal turns
  are unaffected — only the late-flush / first-turn mid-turn fallbacks are.
- **Untrusted-workspace trap**: `agy -p` in a folder agy doesn't trust resolves
  cwd to `~/.gemini/antigravity-cli/scratch/` — no hooks fire, no session, exit
  0. Workspace hooks (`.agents/hooks.json`) only load for a **trusted**
  workspace (`trustedWorkspaces` array of absolute paths in agy's global
  `settings.json`; interactive prompt "Do you trust the contents of this
  project?"). Always pass `--add-dir <workspace>` and, in a fresh HOME,
  pre-seed `trustedWorkspaces` (the e2e harness does both); `entire doctor`
  reports when the workspace hooks are not loaded.
- **Review**: agy is eligible in the `entire review` skill picker (skill
  discovery across `~/.gemini/config/skills` (agy 1.1+ global),
  `~/.gemini/antigravity-cli/skills`, `~/.gemini/skills`,
  `<repo>/.agents/skills` (agy 1.1+ workspace), and legacy
  `<repo>/.agent/skills`; `/name` invocation form) but is not a launchable
  reviewer.

## Captured Payloads

`testdata/hook_stdin_pre_invocation.json`, `testdata/hook_stdin_pre_tool_use.json`,
`testdata/hook_stdin_stop.json` — real agy 1.0.x stdin captures (fixtures carry
the full documented payloads to pin unknown-field tolerance);
`testdata/transcript_sample.jsonl` — captured transcript fixture.
