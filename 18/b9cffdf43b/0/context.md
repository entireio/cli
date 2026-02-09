# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# ENT-242: Replace go-git worktree.Status() with git CLI shim

## Context

go-git v5's `worktree.Status()` reads **and rewrites** the worktree index file. When called from git hook paths (post-commit, prepare-commit-msg), this rewrites the index with stale cache-tree entries that reference objects pruned by GC, causing index corruption (`fatal: unable to read <sha>`). Confirmed via instrumentation showing post-commit hook changes the index checksum.

The fix: route...

### Prompt 2

[Request interrupted by user]

### Prompt 3

um, sorry, I should have mentioned this earlier - we did do a get status replacement for a different reason - shadow file tracking - is this reusable?

### Prompt 4

yeah that was the one `checkpoint/temporary.go:collectChangedFiles`. continue then

### Prompt 5

Base directory for this skill: /Users/alex/.claude/plugins/cache/claude-plugins-official/superpowers/4.2.0/skills/requesting-code-review

# Requesting Code Review

Dispatch superpowers:code-reviewer subagent to catch issues before they cascade.

**Core principle:** Review early, review often.

## When to Request Review

**Mandatory:**
- After each task in subagent-driven development
- After completing major feature
- Before merge to main

**Optional but valuable:**
- When stuck (fresh perspectiv...

