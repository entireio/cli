# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Plan: Fix Deferred Condensation (ACTIVE_COMMITTED → IDLE via TurnEnd)

## Context

PR #172 comment from khaong identified a critical bug: when a session in ACTIVE_COMMITTED phase receives `EventTurnEnd` (agent finishes its turn), the state machine emits `ActionCondense` but `transitionSessionTurnEnd()` in `hooks_claudecode_handlers.go:751` ignores the return value of `ApplyCommonActions()`. The condensation never fires. Session data from in-turn commits stays s...

