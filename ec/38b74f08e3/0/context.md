# Session Context

## User Prompts

### Prompt 1

print shadowBranch into message only if hasShadowBranch is trye

### Prompt 2

is there any way to not repeat the whole error?

### Prompt 3

update that user message to display when an agent sessions is opened but there is already another session.
This used to be the warning:
    var message string
        suppressHint := "\n\nTo suppress this warning in future sessions, run:\n  entire enable --disable-multisession-warning"
        if otherPrompt != "" {
            message = fmt.Sprintf("Another session is active: \"%s\"\n\nYou can continue here, but checkpoints from both sessions will be interleaved.\n\nTo resume the other session ...

