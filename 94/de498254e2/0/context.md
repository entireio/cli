# Session Context

## User Prompts

### Prompt 1

can you help me understand what `entire clean` is doing?

### Prompt 2

can you recheck the code base, we added some auto cleanup for shadow branches. so I wonder if this is an edge case now

### Prompt 3

I feel the command is to present on the top level so I wonder if it makes sense to move it under the "entire sessions" what do you think?

### Prompt 4

Yes

### Prompt 5

What do you think about renaming it to "cleanup" ?

### Prompt 6

yes

### Prompt 7

is there any other cleanup necessary in relation to the dual strategy?

### Prompt 8

for the "entire session cleanup" command, can you see anything that would needs to be cleaned up if someone uses auto-commit?

### Prompt 9

yes

### Prompt 10

I also understand that auto-commit is not creating shadow branches, but maybe there are other things that could be left in a stale state and should be cleaned up?

### Prompt 11

yes

### Prompt 12

3 and I would even consider cleaning up independent of what strategy is currently active, the otherone might have been active before?

### Prompt 13

yes

### Prompt 14

one last modification: there is now strategy specific logic outside of the strategy, can we maybe refactor that too?

### Prompt 15

can we rename the "clean" file names to "cleanup" too?

