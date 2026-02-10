# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Fix `entire explain` branch filtering with merge commits

## Context

`entire explain` branch filtering breaks when a feature branch has merge commits from main. The root cause: `repo.Log()` with `git.LogOrderCommitterTime` traverses ALL parents of merge commits (full DAG walk). After merging main into a feature branch, the walker enters main's full history. The `consecutiveMainLimit` (100) fires before older feature branch checkpoints are found, silently droppin...

### Prompt 2

commit this

