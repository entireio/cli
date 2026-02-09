# Session Context

## User Prompts

### Prompt 1

can you review the changes in this PR? I think there is an issue with the code currently, when we captured multiple sessions and someone rewinds ah checkpoint we only recover the latest session, but instead we should do both. Can you take a look and suggest something? I'm not even sure how to communicate this to the user

### Prompt 2

yes, please implement

### Prompt 3

can we add the prompt (what is written behind "Session 1:") as " # <prompt>" behind each claude --continue command?

### Prompt 4

continue

### Prompt 5

what happens currently when I have to claude sessions open and run prompt in one, then in the other, then in the other again each finishes before I run in the other session. each will trigger a commit in the shadow branch, and then when we condense during real commit the one that finished last is the one that is directly in the folder, right?

