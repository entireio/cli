# Session Context

## User Prompts

### Prompt 1

in common.go:296 we are initializing the shadow branch with "entire@local" could we change this to use the current git user in the same way we do this on shadow branches and also following commits to the entire/checkpoints/v1 branch?

### Prompt 2

what kind of fallbacks does GetGitAuthorFromRepo have?

### Prompt 3

does the method has tests so far?

### Prompt 4

can you validate first what the default behavior from the git cli is if only one (name or email) is defined local? is the other then taken from global?

### Prompt 5

ok, then add some tests for this behavior

