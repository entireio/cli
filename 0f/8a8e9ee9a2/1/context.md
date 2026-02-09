# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Versioned Stanza Management for Shell Completion

## Context

The current shell completion installer appends a comment + command line to `~/.zshrc` or `~/.bashrc`, and detects existing installs via exact substring match. This has two problems:
1. If the desired stanza changes, the old one isn't removed — a duplicate gets appended
2. There's no way to remove the stanza on uninstall

We're replacing this with a versioned, delimited block (vim fold-marker style) t...

### Prompt 2

[Request interrupted by user for tool use]

### Prompt 3

Commit

