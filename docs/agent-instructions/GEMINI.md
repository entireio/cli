# Entire CLI

Entire CLI is installed in this repo. Your session transcripts are automatically captured alongside git commits via Gemini CLI hooks.

## Commands

- **`entire status`** — Check if your session is being tracked.
- **`entire explain <commit-hash>`** — Understand the reasoning behind a commit. Run this before modifying code you didn't write.
- **`entire rewind`** — Browse checkpoints and restore a known-good state if a session went sideways.
- **`entire log`** — View the commit history with agent session metadata.

## Hooks

Entire installs Gemini CLI lifecycle hooks. These run automatically — no action needed. If hooks are missing, run `entire hooks install`.

## Workflow

1. Start your session normally. Entire tracks it via hooks.
2. Before editing unfamiliar code, run `entire explain <commit>` to read the original author's reasoning.
3. Commit as usual. Entire captures your transcript and attaches it to the commit.
4. If something breaks, `entire rewind` lets you roll back to any checkpoint.
