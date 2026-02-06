# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# ENT-64: Status Shows Session Information

## Overview

Enhance `entire status` to show active sessions grouped by worktree, including first prompt, timestamp, checkpoint count, and uncheckpointed work indicator.

## Target Output

```
Enabled (manual-commit)

Active Sessions:
  /Users/alex/workspace/cli (main)
    abc-123  "Fix auth bug in login..."   2m ago   3 checkpoints
    def-456  "Add dark mode support..."   15m ago  1 checkpoint (uncheckpointed changes)

...

### Prompt 2

please please fix up any code quality issues you notice and don't leave them by the wayside if they are small

