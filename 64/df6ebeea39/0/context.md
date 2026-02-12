# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Plan: Wingman status notifications in agent terminal

## Context

Wingman status messages (review started, review pending) currently go to stderr, which is invisible in Claude Code's UI. The only visible wingman output is the `additionalContext` prompt injection when REVIEW.md is applied. The user wants informational `systemMessage` notifications so they can see what wingman is doing — not injected into the agent's context, just displayed in the terminal.

## C...

### Prompt 2

commit this

### Prompt 3

push it

### Prompt 4

Update the documentation make sure there's no redundant parts in there, add a table for messaging and when they happen. Look at the PR review comments and see if there's anything valid in there. Adress them. Commit and push.

