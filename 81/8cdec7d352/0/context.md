# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Move shell completion prompt from `enable` to new `curl-bash-post-install` command

## Context

The CLI's `enable` command currently includes an optional shell-completion installation prompt. Now that we have an `install.sh` script (curl|bash installer) that calls `entire curl-bash-post-install` after installing the binary, we need a dedicated hidden command for that post-install step. For now it should just do the interactive completion prompt. We'll remove the ...

### Prompt 2

commit.

### Prompt 3

One more thing, when installing Zsh completions, let's also add `autoload -Uz compinit && compinit` before our `source ...`

### Prompt 4

That case statement looks duplicated.  Do we need all the logic in promptShellCompletion and setupShellCompletionNonInteractive to be duplicated?  Let's try and simplify.

