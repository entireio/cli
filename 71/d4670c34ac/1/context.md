# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Entire Wingman - Implementation Plan

## Context

The wingman feature adds an automated code review loop to the CLI. After an agent turn that modifies files, a background reviewer analyzes the changes (using checkpoint data to understand intent), writes suggestions to `.entire/REVIEW.md`, and then — after a short delay — triggers the main agent to apply those suggestions via `claude --continue -p`. This creates a self-improving development loop where agent ou...

### Prompt 2

Are all tests passing?

### Prompt 3

Commit the changes

### Prompt 4

Look at the @.entire/REVIEW.md, check if it's valid feedback and resolve if necessary.

### Prompt 5

Commit and empty the REVIEW.md file.

