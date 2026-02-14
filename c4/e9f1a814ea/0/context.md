# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Fix: Wingman auto-apply never triggers on session close + improve logging

## Context

When a user closes a Claude session with a pending `REVIEW.md`, the auto-apply never fires. The root cause is a phase check bug in the auto-apply subprocess: `runWingmanApply` calls `isSessionIdle()` which only returns `true` for `PhaseIdle`. But when triggered from the SessionEnd hook, the session has already been marked `PhaseEnded`, so `isSessionIdle` returns `false` and the...

### Prompt 2

commit this

