# Session Context

## User Prompts

### Prompt 1

question: for manual commit the branch is captured when the user makes the commit or when the checkpoint in the shadow branch is created?

### Prompt 2

This branch name extraction logic is duplicated in three places across the codebase (manual_commit_condensation.go lines 122-127, auto_commit.go lines 226-231 and 737-742). Consider extracting this into a helper function in common.go, similar to the existing GetGitAuthorFromRepo function. This would improve maintainability and ensure consistent behavior across all strategies.

### Prompt 3

does it make sense to then directly test the method instead of the overall functionality?

### Prompt 4

yes

