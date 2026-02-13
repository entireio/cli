# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Plan: `entire wingman agent` Command

## Context

Wingman currently hardcodes `claude` CLI and `sonnet` model for reviews. This change adds multi-agent support so users can choose which CLI tool and model performs wingman reviews (e.g., codex, gemini-cli). It also fixes a bug where `wingman enable`/`disable` overwrites agent/model settings.

## Files to Modify/Create

| File | Action |
|------|--------|
| `cmd/entire/cli/settings/settings.go` | Add `GetWingmanAge...

### Prompt 2

Create a branch based out of the current one and commit.

