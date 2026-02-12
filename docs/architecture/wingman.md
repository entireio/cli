# Wingman: Automated Code Review

Wingman is an automated code review system that reviews agent-produced code changes after each commit and delivers actionable suggestions back to the agent.

## Overview

When enabled, wingman runs a background review after each commit (or checkpoint), writes suggestions to `.entire/REVIEW.md`, and ensures the agent addresses them. The system prioritizes **visible delivery** — the user should see the agent reading and applying the review in their terminal.

## Architecture

### Components

| Component | File | Purpose |
|-----------|------|---------|
| Trigger & state | `wingman.go` | Payload, state management, dedup, lock files, stale review cleanup |
| Review process | `wingman_review.go` | Detached subprocess: diff, Claude review call, REVIEW.md creation, session detection |
| Review prompt | `wingman_prompt.go` | Builds the review prompt from diff, prompts, context |
| Instruction | `wingman_instruction.md` | Embedded instruction injected into agent context |
| Process spawning | `wingman_spawn_unix.go` | Detached subprocess spawning (Unix) |
| Process spawning | `wingman_spawn_other.go` | No-op stubs (non-Unix) |
| Hook integration | `hooks_claudecode_handlers.go` | Prompt-submit injection, stop hook notifications, session-end trigger |

### State Files

| File | Purpose |
|------|---------|
| `.entire/REVIEW.md` | The review itself, read by the agent |
| `.entire/wingman-state.json` | Dedup state, session ID, apply tracking |
| `.entire/wingman.lock` | Prevents concurrent review spawns |
| `.entire/wingman-payload.json` | Payload passed to detached review process |
| `.entire/logs/wingman.log` | Review process logs (stderr redirect) |

## Lifecycle

### Phase 1: Trigger

A wingman review is triggered after code changes are committed:

- **Manual-commit strategy**: Git `post-commit` hook calls `triggerWingmanFromCommit()`
- **Auto-commit strategy**: Stop hook calls `triggerWingmanReview()` after `SaveChanges()`

Before spawning, preconditions are checked:
1. `ENTIRE_WINGMAN_APPLY` env var not set (prevents recursion from auto-apply)
2. No pending REVIEW.md for the current session (`shouldSkipPendingReview()`)
3. Lock file acquired atomically (`acquireWingmanLock()`)
4. File hash dedup — skip if same files were already reviewed this session

### Phase 2: Review (Detached Process)

The review runs in a fully detached subprocess (`entire wingman __review <payload-path>`):

```
┌─ Detached Process ─────────────────────────────────────┐
│ 1. Read payload from file                              │
│ 2. Wait 10s for agent turn to settle                   │
│ 3. Compute diff (merge-base with main/master)          │
│ 4. Load session context from checkpoint metadata       │
│ 5. Build review prompt (diff + prompts + context)      │
│ 6. Call claude --print (sonnet model, read-only tools)  │
│ 7. Write REVIEW.md                                     │
│ 8. Save dedup state                                    │
│ 9. Determine delivery path (see Phase 3)               │
│ 10. Remove lock file                                   │
└────────────────────────────────────────────────────────┘
```

The review process uses `--setting-sources ""` to disable hooks (prevents recursion). The Claude CLI calls (`callClaudeForReview` and `triggerAutoApply`) strip `GIT_*` environment variables via `wingmanStripGitEnv()` to prevent index corruption — the detached process itself inherits the parent's environment.

### Phase 3: Delivery

There are two delivery mechanisms. The system chooses based on whether any session is still alive.

#### Primary: Prompt-Submit Injection (Visible)

When a live session exists (IDLE, ACTIVE, or ACTIVE_COMMITTED phase — excluding stale ACTIVE sessions older than 4 hours), the review is delivered through the agent's next prompt:

```
User sends prompt → UserPromptSubmit hook fires
                  → REVIEW.md exists on disk
                  → Inject as additionalContext (mandatory agent instruction)
                  → Agent reads REVIEW.md, applies suggestions, deletes file
                  → Agent then proceeds with user's actual request
```

The `additionalContext` hook response field adds the instruction directly to Claude's context, making it a mandatory pre-step. The embedded instruction (`wingman_instruction.md`) tells the agent to read the review, address suggestions, delete the file, and briefly tell the user what changed.

#### Fallback: Background Auto-Apply (Invisible)

When no live sessions exist (all ENDED or none), REVIEW.md is applied via a background process:

```
entire wingman __apply <repoRoot>
  → Verify REVIEW.md exists
  → Check ApplyAttemptedAt not set (retry prevention)
  → Re-check session phase is not ACTIVE/ACTIVE_COMMITTED (guard against race)
  → claude --continue --print --permission-mode acceptEdits
```

This path is **invisible** — it runs silently. It exists as a fallback for when no session will receive the injection (e.g., user closed all sessions during the review window). Both IDLE and ENDED phases are considered safe for auto-apply — only truly active sessions (ACTIVE/ACTIVE_COMMITTED) block it.

### Trigger Points

| Trigger | When | What Happens |
|---------|------|-------------|
| **Review process** (`runWingmanReview`) | Review finishes | If no live sessions → background auto-apply. Otherwise defer. |
| **Prompt-submit hook** (`captureInitialState`) | User sends prompt | If REVIEW.md exists → inject as `additionalContext`. |
| **Stop hook** (`triggerWingmanAutoApplyIfPending`) | Agent turn ends | If REVIEW.md exists + no live sessions → spawn `__apply`. |
| **Session-end hook** (`triggerWingmanAutoApplyOnSessionEnd`) | User closes session | If REVIEW.md exists + no remaining live sessions → spawn `__apply`. |

## User-Visible Messages

Wingman outputs `systemMessage` notifications at key points so the user can see what wingman is doing in their agent terminal. These are informational only — they are NOT injected into the agent's context.

| Message | Hook | Condition | Purpose |
|---------|------|-----------|---------|
| `[Wingman] A code review is pending and will be addressed before your request.` | Prompt-submit | REVIEW.md exists (with `additionalContext` injection) | Tells user the agent will apply a review first |
| `[Wingman] Review in progress...` | Prompt-submit | Lock file exists (no REVIEW.md) | Tells user a review is running in the background |
| `[Wingman] Reviewing your changes...` | Stop | Lock file exists | Tells user a review was triggered and is still running |
| `[Wingman] Review pending — will be addressed on your next prompt` | Stop | REVIEW.md exists (no lock file) | Tells user a completed review will be delivered next prompt |

The prompt-submit REVIEW.md injection message is paired with `additionalContext` — the agent sees and acts on the review. All other messages use `outputHookMessage()` which emits `systemMessage`-only JSON (visible in terminal, invisible to agent).

## Timing

Typical timeline for a review cycle:

```
T+0s    Commit happens → wingman review triggered
T+0s    Lock file created, payload written
T+10s   Initial settle delay completes
T+10s   Diff computed (~30-50ms)
T+11s   Claude review API call starts
T+30-50s Review received, REVIEW.md written
T+30-50s Delivery path determined
```

The 10-second initial delay lets the agent turn fully settle before computing the diff, ensuring all file writes are flushed.

## Review Prompt Construction

The review prompt leverages Entire's checkpoint data to give the reviewer **full context about what the developer was trying to accomplish**. This enables intent-aware review — catching not just bugs, but misalignment between what was asked and what was built. A reviewer that only sees the diff cannot evaluate whether the code matches the original request.

### Context Sources

| Source | Origin | What It Provides |
|--------|--------|-----------------|
| **Developer prompts** | `prompt.txt` from checkpoint metadata | The original instructions given to the agent — the ground truth of intent |
| **Commit message** | Git commit or auto-commit message | A summary of what was done (may differ from what was asked) |
| **Session context** | `context.md` from checkpoint metadata | Generated summary of key actions, decisions, and session flow |
| **Checkpoint files** | `.entire/metadata/<session-id>/` | Paths provided so the reviewer can read the full transcript, prompts, or context if needed |
| **File list** | Payload from trigger | Which files changed and how (modified/new/deleted) |
| **Branch diff** | `git diff` against merge-base | The actual code changes — computed against `main`/`master` for a holistic branch-level view |

### Diff Strategy

The diff is computed against the **merge-base** with `main`/`master`, not just the latest commit. This gives the reviewer a holistic view of all branch changes rather than a narrow single-commit diff. Fallback chain:

1. `git merge-base main HEAD` → diff against merge-base (branch-level view)
2. `git merge-base master HEAD` → try master if main doesn't exist
3. `git diff HEAD` → uncommitted changes only
4. `git diff HEAD~1 HEAD` → latest commit if no uncommitted changes

### Read-Only Tool Access

The reviewer Claude instance has access to `Read`, `Glob`, and `Grep` tools with `--permission-mode bypassPermissions`. This allows it to read source files beyond the diff, search for related patterns, and inspect checkpoint metadata. Tools are restricted to read-only operations.

### Output Format

The reviewer outputs structured Markdown with:
- **Summary**: Does the change accomplish its goal? Overall quality assessment.
- **Issues**: Each with severity (`CRITICAL`, `WARNING`, `SUGGESTION`), file path with line reference, description, and suggested fix.

Diffs larger than 100KB are truncated to maintain review quality. The output is written directly to `.entire/REVIEW.md`.

## Stale Review Cleanup

Reviews can become stale in several scenarios. The `shouldSkipPendingReview()` function handles cleanup:

| Scenario | Detection | Action |
|----------|-----------|--------|
| REVIEW.md without state file | `state == nil` | Delete REVIEW.md (orphan) |
| REVIEW.md from different session | `state.SessionID != currentSessionID` | Delete REVIEW.md (stale) |
| REVIEW.md older than 1 hour | `time.Since(state.ReviewedAt) > 1h` | Delete REVIEW.md (TTL expired) |
| REVIEW.md from current session | Session matches + fresh | Keep (skip new review) |

## Retry Prevention

The `ApplyAttemptedAt` field in `WingmanState` prevents infinite auto-apply attempts:

- Set to current time before triggering auto-apply
- Reset to `nil` when a new review is written
- Checked before every auto-apply attempt — if set, skip

## Concurrency Safety

- **Lock file**: Atomic `O_CREATE|O_EXCL` prevents concurrent review spawns. Stale locks (>30 min) are auto-cleaned.
- **Dedup hash**: SHA256 of sorted file paths prevents re-reviewing identical change sets.
- **Detached processes**: Review and apply run in their own process groups (`Setpgid: true`), surviving parent exit.
- **GIT_* stripping**: Claude CLI calls strip `GIT_*` env vars via `wingmanStripGitEnv()` to prevent index corruption. Applied in `callClaudeForReview()` and `triggerAutoApply()`, not at process spawn time.
- **ENTIRE_WINGMAN_APPLY=1**: Set during auto-apply to prevent the post-commit hook from triggering another review (recursion prevention).
- **Stale session detection**: ACTIVE/ACTIVE_COMMITTED sessions not updated in 4+ hours are considered orphaned (crashed agent) and ignored by `hasAnyLiveSession`. IDLE sessions are always considered live regardless of age.

## Configuration

```bash
entire wingman enable   # Enable wingman auto-review
entire wingman disable  # Disable and clean up pending reviews
entire wingman status   # Show current status
```

Wingman state is stored in `.entire/settings.json`:

```json
{
  "strategy_options": {
    "wingman": {
      "enabled": true
    }
  }
}
```

## Key Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `wingmanInitialDelay` | 10s | Settle time before computing diff |
| `wingmanReviewModel` | `sonnet` | Model used for reviews |
| `wingmanGitTimeout` | 60s | Timeout for git diff operations |
| `wingmanReviewTimeout` | 5m | Timeout for Claude review API call |
| `wingmanApplyTimeout` | 15m | Timeout for auto-apply process |
| `wingmanStaleReviewTTL` | 1h | Max age before review is cleaned up |
| `staleLockThreshold` | 30m | Max age before lock is considered stale |
| `staleActiveSessionThreshold` | 4h | Max age before ACTIVE session is considered stale/orphaned |
