# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Git Hook Chaining Implementation

## Context

`entire enable` overwrites existing custom git hooks (e.g., `prepare-commit-msg`) without warning. `entire disable --uninstall` needs to handle the case where another tool moved our hooks using the same `.pre-<tool>` backup pattern.

## Files to Modify

- `cmd/entire/cli/strategy/hooks.go` — core install/remove logic
- `cmd/entire/cli/strategy/hooks_test.go` — tests

No changes needed to `setup.go` or other caller...

