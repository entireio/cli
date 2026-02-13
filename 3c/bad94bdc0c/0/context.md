# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Fix: Wingman auto-apply never triggers on session close + improve logging

## Context

When a user closes a Claude session with a pending `REVIEW.md`, the auto-apply never fires. The root cause is a phase check bug in the auto-apply subprocess: `runWingmanApply` calls `isSessionIdle()` which only returns `true` for `PhaseIdle`. But when triggered from the SessionEnd hook, the session has already been marked `PhaseEnded`, so `isSessionIdle` returns `false` and the...

### Prompt 2

commit this

### Prompt 3

Update the docs according to the recent changes if needed

### Prompt 4

I would like to have "notifications" into the agent when hooks fire. E.g. commit hook, I want the notification that the Reviewer is send to do its job. Then on stop hook I want a "notification" if a review is still "pending". Does that make sense?

### Prompt 5

I haven't seen a [wingman] notice in claude, only the prompt injection one. But that's what I meant, I want to see it in the agent for user awareness but no prompt, we have that already. It's more to indicate what's happening in the back.

### Prompt 6

[Request interrupted by user for tool use]

