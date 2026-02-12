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
| Hook integration | `hooks_claudecode_handlers.go` | Prompt-submit injection, stop hook trigger, session-end trigger |

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

The review process uses `--setting-sources ""` to disable hooks (prevents recursion) and strips `GIT_*` environment variables for isolation.

### Phase 3: Delivery

There are two delivery mechanisms. The system chooses based on whether any session is still alive.

#### Primary: Prompt-Submit Injection (Visible)

When a live session exists (IDLE, ACTIVE, or ACTIVE_COMMITTED phase), the review is delivered through the agent's next prompt:

```
Review finishes → REVIEW.md written → live session detected → defer to injection

User sends prompt → UserPromptSubmit hook fires
                  → REVIEW.md exists on disk
                  → Inject as additionalContext (mandatory agent instruction)
                  → Agent reads REVIEW.md, applies suggestions, deletes file
                  → Agent then proceeds with user's actual request
```

The `additionalContext` hook response field adds the instruction directly to Claude's context, making it a mandatory pre-step rather than an ignorable warning. The embedded instruction (`wingman_instruction.md`) tells the agent to:

1. Read `.entire/REVIEW.md`
2. Address each suggestion (skip any it disagrees with)
3. Delete `.entire/REVIEW.md` when done
4. Briefly tell the user what changed

This path is **visible** — the user sees the agent working through the review in their terminal.

#### Fallback: Background Auto-Apply (Invisible)

When no live sessions exist (all ENDED or none), REVIEW.md is applied via a background process:

```
Review finishes → REVIEW.md written → no live sessions → background auto-apply

entire wingman __apply <repoRoot>
  → Verify REVIEW.md exists
  → Check ApplyAttemptedAt not set (retry prevention)
  → Re-check session idle (guard against race)
  → claude --continue --print --permission-mode acceptEdits
```

This path is **invisible** — it runs silently. It exists as a fallback for when no session will receive the injection (e.g., user closed all sessions during the review window).

### Decision Flow

```
                    REVIEW.md written
                          │
                          ▼
                ┌─────────────────┐
                │ Any live session │
                │   exists?        │
                └────────┬────────┘
                    │          │
                   Yes         No
                    │          │
                    ▼          ▼
            ┌──────────┐ ┌──────────────┐
            │  Defer   │ │  Background  │
            │  to next │ │  auto-apply  │
            │  prompt  │ │  immediately │
            └──────────┘ └──────────────┘
                    │
                    ▼
            User sends prompt
                    │
                    ▼
            additionalContext
            injection fires
                    │
                    ▼
            Agent applies review
            (visible in terminal)
```

### Trigger Points Summary

| Trigger | When | What Happens |
|---------|------|-------------|
| **Review process** (`runWingmanReview`) | Review finishes | If no live sessions → background auto-apply. Otherwise defer. |
| **Prompt-submit hook** (`captureInitialState`) | User sends prompt | If REVIEW.md exists → inject as `additionalContext`. |
| **Stop hook** (`triggerWingmanAutoApplyIfPending`) | Agent turn ends | If REVIEW.md exists + no live sessions → spawn `__apply`. |
| **Session-end hook** (`triggerWingmanAutoApplyOnSessionEnd`) | User closes session | If REVIEW.md exists + no remaining live sessions → spawn `__apply`. |

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

The review prompt is the core of wingman's value. Unlike a naive diff-only review, it leverages Entire's checkpoint data to give the reviewer **full context about what the developer was trying to accomplish**. This enables intent-aware review — catching not just bugs, but misalignment between what was asked and what was built.

### Context Sources

The prompt is assembled from six sources, each contributing a different layer of understanding:

| Source | Origin | What It Provides |
|--------|--------|-----------------|
| **Developer prompts** | `prompt.txt` from checkpoint metadata | The original instructions given to the agent — the ground truth of intent |
| **Commit message** | Git commit or auto-commit message | A summary of what was done (may differ from what was asked) |
| **Session context** | `context.md` from checkpoint metadata | Generated summary of key actions, decisions, and session flow |
| **Checkpoint files** | `.entire/metadata/<session-id>/` | Paths provided so the reviewer can read the full transcript, prompts, or context if needed |
| **File list** | Payload from trigger | Which files changed and how (modified/new/deleted) |
| **Branch diff** | `git diff` against merge-base | The actual code changes — computed against `main`/`master` for a holistic branch-level view |

### Prompt Structure

```
┌─ System Role ──────────────────────────────────────────┐
│ "You are a senior code reviewer performing an           │
│  intent-aware review."                                  │
├─ Session Context ──────────────────────────────────────┤
│ Developer's Prompts     <prompts>...</prompts>          │
│ Commit Message          (plain text)                    │
│ Session Context         <session-context>...</session-context> │
│ Checkpoint File Paths   (for deeper investigation)      │
├─ Code Changes ─────────────────────────────────────────┤
│ Files changed:          file.go (modified), ...         │
│ Diff                    <diff>...</diff>                │
├─ Review Instructions ──────────────────────────────────┤
│ Intent alignment        (most important)                │
│ Correctness             bugs, logic errors, races       │
│ Security                injection, secrets, path traversal │
│ Robustness              edge cases, leaks, timeouts     │
│ Do NOT flag             style, docs on clear code       │
│ Output format           Markdown with severity levels   │
└────────────────────────────────────────────────────────┘
```

### Intent-Aware Review

The review instructions prioritize **intent alignment** as the most important category:

1. Do the changes actually accomplish what the developer asked for?
2. Are there any prompts or requirements that were missed or only partially implemented?
3. Does the implementation match the stated approach in the session context?

This is only possible because Entire captures the developer's prompts and session context as checkpoint metadata. A reviewer that only sees the diff cannot evaluate whether the code matches the original request.

### Diff Strategy

The diff is computed against the **merge-base** with `main`/`master`, not just the latest commit. This gives the reviewer a holistic view of all branch changes rather than a narrow single-commit diff. Fallback chain:

1. `git merge-base main HEAD` → diff against merge-base (branch-level view)
2. `git merge-base master HEAD` → try master if main doesn't exist
3. `git diff HEAD` → uncommitted changes only
4. `git diff HEAD~1 HEAD` → latest commit if no uncommitted changes

### Read-Only Tool Access

The reviewer Claude instance has access to `Read`, `Glob`, and `Grep` tools with `--permission-mode bypassPermissions`. This allows it to:

- Read source files beyond the diff for additional context
- Search for related code patterns or usages
- Inspect the checkpoint metadata files referenced in the prompt

Tools are restricted to read-only operations — the reviewer cannot modify files.

### Truncation

Diffs larger than 100KB are truncated to maintain review quality. Very large diffs tend to produce unfocused reviews.

### Output Format

The reviewer outputs structured Markdown with:
- **Summary**: Does the change accomplish its goal? Overall quality assessment.
- **Issues**: Each with severity (`CRITICAL`, `WARNING`, `SUGGESTION`), file path with line reference, description, and suggested fix.

This output is written directly to `.entire/REVIEW.md` and later read by the agent during delivery.

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
- **GIT_* stripping**: Subprocesses strip all `GIT_*` env vars to prevent index corruption.
- **ENTIRE_WINGMAN_APPLY=1**: Set during auto-apply to prevent the post-commit hook from triggering another review (recursion prevention).

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
