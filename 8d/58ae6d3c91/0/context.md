# Session Context

## User Prompts

### Prompt 1

can you review PR 284 and I think we already have code that checks if git commits are setup on each session, or?

### Prompt 2

I mean not git config but git hooks, can you validate: we have EnsureSetup which calls IsGitHookInstalled and I think this happens during each session so basically we have what the PR does, right?

### Prompt 3

For auto commit we only need the push hook, or?

### Prompt 4

ok, let's do this fix then, ignore the existing PR

### Prompt 5

commit this

### Prompt 6

[Request interrupted by user]

### Prompt 7

Question: For manual strategy we assumed the user commits after a prompt is done, what now is a common pattern also is to say the agent "do this change, and then make a commit" so the commit happens before stop. This means if we do EnsureSetup in stop we might be to late. Is there a good reason not to move EnsureSetup forward to the first hook that is called in a session?

### Prompt 8

yes

### Prompt 9

what other things does EnsureSetup do?

### Prompt 10

do we have test coverage for this?

### Prompt 11

yes please do

