# Session Context

## User Prompts

### Prompt 1

can you take a look at this? I would have expected that the UserPromptSubmit would have surfaced this in claude (blocking the prompt) as usual and not dump an error including tool usage help: 

  If you need specific details from before exiting plan mode (like exact code snippets, error messages, or content you generated), read the full transcript at:
  /Users/alex/.claude/projects/-Users-alex-workspace-cli/c2b51eb0-d0e9-43cf-b431-42c05d49450b.jsonl
  ⎿  UserPromptSubmit hook error: Failed wi...

### Prompt 2

how does a concurent session differ from the session id conflict?

### Prompt 3

ah the different is that we got the hook from the first session being stopped?

### Prompt 4

[Request interrupted by user]

### Prompt 5

ah the different is that we got the hook from the first claude being closed?

### Prompt 6

can you do the fix?

### Prompt 7

can you explain me the difference between ShadowBranchConflictError and SessionIDConflictError

### Prompt 8

can we add a test for each to at least check it's returning the blocking (but not error) response?

