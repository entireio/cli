# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Fix: Persist transcript_path in checkpoint metadata for restore

## Context

During restore, `GetSessionDir()` + `ResolveSessionFile()` can't reconstruct the correct path for Gemini sessions (wrong hashing algorithm, wrong filename format). The `transcript_path` is already in `SessionState` for both Claude and Gemini. Persist it in checkpoint metadata so restore reads it back directly — same codepath for all agents.

## Changes

### 1. `checkpoint/checkpoint.go...

