# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Move shell completion prompt from `enable` to new `curl-bash-post-install` command

## Context

The CLI's `enable` command currently includes an optional shell-completion installation prompt. Now that we have an `install.sh` script (curl|bash installer) that calls `entire curl-bash-post-install` after installing the binary, we need a dedicated hidden command for that post-install step. For now it should just do the interactive completion prompt. We'll remove the ...

