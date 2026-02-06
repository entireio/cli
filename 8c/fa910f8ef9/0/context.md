# Session Context

## User Prompts

### Prompt 1

when doing `entire resume` we might flag to the user he should also pull `entire/sessions` can we just do this automatically instead?

### Prompt 2

can you check, I think this might be duplicated now in the code base: Added FetchMetadataBranch() function

### Prompt 3

I just had an error when doing resume and it did output the command information and then actual error, I don't remember which error it was but can you check where it would make sense to raise a SilentError?

### Prompt 4

Ah I remember now, the issue was that I had un-commited files locally so it couldn't switch the branch. Which I guess is also this what we just fixed, right?

### Prompt 5

I am running entire resume <branch> and it is throwing the following error:


entire resume soph/fetch-sessions-if-needed-during-resume
Fetching session metadata from origin...
Failed to fetch metadata: failed to fetch entire/sessions from origin: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain
You can try manually: git fetch origin entire/sessions:entire/sessions
Usage:
  entire resume <branch> [flags]

Flags:
  -f, --force   R...

