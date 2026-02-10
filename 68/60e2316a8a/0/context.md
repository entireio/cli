# Session Context

## User Prompts

### Prompt 1

I got this feedback for the changes in cmd/entire/cli/strategy/manual_commit_hooks.go:281 "Truncating the string by byte index can break multi-byte UTF-8 characters. If the prompt contains non-ASCII characters (e.g., emoji, non-Latin scripts), slicing at byte 77 could split a multi-byte character, resulting in invalid UTF-8. Consider using rune-based slicing instead, converting to []rune, checking the length, and slicing accordingly."

Can you address this and also can we check if we need to do ...

### Prompt 2

Can you check this is not replicating go basic functionality?

