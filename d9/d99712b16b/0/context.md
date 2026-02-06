# Session Context

## User Prompts

### Prompt 1

remove the 'choose strategy' step from entire enable

document the alternative (use --strategy flag) - in README / getting started docs

### Prompt 2

[Request interrupted by user for tool use]

### Prompt 3

Implement the following plan:

# Plan: Remove Strategy Selection from `entire enable`

## Summary

Remove the interactive strategy selection prompt from `entire enable` and use `manual-commit` as the default. Document the `--strategy` flag as the alternative for users who want `auto-commit`.

## Changes

### 1. Modify `cmd/entire/cli/setup.go`

**Update command description (line 56):**
```go
Long: `Enable Entire with session tracking for your AI agent workflows.

Uses the manual-commit strategy ...

### Prompt 4

add an integration test that checks default value is manual-mode

### Prompt 5

I would add a specific section inside README.md with the most important flags available at entire enable command.

