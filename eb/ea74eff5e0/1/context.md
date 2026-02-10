# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Fix: Per-session agent resolution in multi-session checkpoints

## Context

A checkpoint can contain multiple sessions from different agents (e.g., session 0 from Claude, session 1 from Gemini). The per-session agent type **is stored correctly** in each session's `metadata.json` on `entire/checkpoints/v1` (`CommittedMetadata.Agent`), but the consumption layer collapses everything to a single agent — always session 0's. This means session 1's transcript gets wri...

