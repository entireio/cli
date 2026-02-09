# Session Context

## User Prompts

### Prompt 1

Base directory for this skill: /Users/alex/.claude/skills/github-pr-review

# GitHub PR Review

## Overview

Technical mechanics for GitHub PR review workflows via `gh` CLI. Covers fetching review comments, replying to threads, creating/updating PRs.

**Companion skill:** For *how to evaluate* feedback, see `superpowers:receiving-code-review`. This skill covers *how to interact* with GitHub.

**Security:** Use fine-grained PAT with minimal permissions.

## Setup (One-Time)

### Fine-Grained PAT
...

### Prompt 2

this bit seems quite cyclomatic-complex. do we have unit test coverage for this area?

### Prompt 3

yeah we need to add this test case, and also any missing scenarios - I'd like to see if we can simplify but we need a safety net

### Prompt 4

should we also logWarn in that block?

