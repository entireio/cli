# Session Context

## User Prompts

### Prompt 1

I just got this during a rebase:

❯ git rebase main
Link this commit to Agent session context? [Y/n] n
Link this commit to Agent session context? [Y/n] n
Rebasing (23/25)

I think we should somehow prevent this from happening. Basically I think the rebase had commits without a trailer, but since the session was still active the hooks tried to link them. This shouldn't be a thing.

### Prompt 2

can we add tests for this?

### Prompt 3

The function should use the existing GetGitDir() helper instead of duplicating the git rev-parse --git-dir logic. GetGitDir() properly handles relative paths by making them absolute, which is more robust and consistent with the rest of the codebase. Replace lines 148-154 with a call to GetGitDir().

