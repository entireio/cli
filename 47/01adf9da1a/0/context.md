# Session Context

## User Prompts

### Prompt 1

Let's make some more changes to explain.
1. add a `--raw-transcript` option to the `entire explain -c {checkpoint-id}` command which just returns the raw transcript file
2. change `--full` to _replace_ the current scoped prompt section from `entire explain -c` to a parsed full transcript (does this make sense?), and don't append the raw transcript any more

### Prompt 2

commit this

### Prompt 3

is this section duplicated with L:597?

### Prompt 4

force push

### Prompt 5

hmm, the amend has broken our entire checkpoints 😬

can we amend again and re-add the checkpoint trailer? Entire-Checkpoint: 4a4241ca5b3d

we will lose some transcript as well unfortunately, but I'm not sure how we can do this without merging the shadow into the previous checkpoint commit on entire/sessions...

### Prompt 6

we do have a commit hook where we currently ignore amends, yes?

### Prompt 7

hmm, log a linear issue in Project:Troy with this (amend) observation, we will come back to it later

### Prompt 8

can we create a new markdown file in docs/ called KNOWN_LIMITATIONS.md, and note the loss of checkpoint information if an amend occurs

