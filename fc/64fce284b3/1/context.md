# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Plan: Fix hook response format and add wingman warning message

## Context

The wingman review injection via `additionalContext` isn't working because the hook response JSON format is wrong. The code outputs `{"additionalContext": "..."}` at the top level, but Claude Code expects it nested under `hookSpecificOutput`:

```json
{
  "hookSpecificOutput": {
    "hookEventName": "UserPromptSubmit",
    "additionalContext": "..."
  }
}
```

This also affects `SessionSt...

### Prompt 2

commit this

