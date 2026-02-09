# Session Context

## User Prompts

### Prompt 1

our git status has broken, why?

### Prompt 2

1. this has happened twice now just fyi

### Prompt 3

yeah check what's triggering gc

### Prompt 4

or is my worktree folder choice the problem?

### Prompt 5

is it problematic, data wise?

### Prompt 6

log a linear improvement - Project:Troy

### Prompt 7

and for now can we document it in known_limitations?

### Prompt 8

no don't set it, let's see if it keeps happening

### Prompt 9

is there a reason the gc is getting invoked more often now?

### Prompt 10

we did a migration from entire/sessions to entire/checkpoints/v1 - but that should have been a one-off operation

### Prompt 11

> git gc
fatal: bad tree object 082c2c4495629bd585a0a11807b4e0ede9a99753
fatal: failed to run repack

HALP

### Prompt 12

success...?

### Prompt 13

run it

### Prompt 14

and those phantom refs, I guess were from the old sessions branch?

### Prompt 15

...sufficiently advanced technology...

### Prompt 16

learning Q: shouldn't the blobs have been reused in the new branch?

### Prompt 17

AHHHHHHHHH yes that would be it. and it wouldn't have kept it as diffs?

### Prompt 18

uh oh - it just happened again

I did a git read-tree HEAD to resolve

### Prompt 19

I did a add/commit/push... then the stop hook barfed after the file copies to .entire/metadata

### Prompt 20

I mean, we _see_ it on the stop hook; it doesn't mean the stop hook caused it

### Prompt 21

are you sure?

### Prompt 22

I've only recently started seeing this happen...but I also moved to 'persistent' worktrees in the last week
(previously had been blowing away worktrees and opening fresh ones)

### Prompt 23

can we do this in the git hooks directly or do we need to do it in code?

### Prompt 24

ooh, we caught something

### Prompt 25

can we also add which index specifically this is to the debug log?

### Prompt 26

it also always seems to be this same hash, I think? 
> git status
fatal: unable to read e53647ba1f1e5203bedff558b482f178db045555

### Prompt 27

it's in worktree 2 I'm seeing this issue

### Prompt 28

sorry, I didn't specify earlier but this recent error is from worktree 2, and the postcommit is also from there (it's a new operation) - so let's not jump to cross-worktree explanations just yet

### Prompt 29

and this error happened in the process of rebasing worktree 2 from its parent (we are stuck mid rebase now)

### Prompt 30

hmm, we made a change recently to shadow tracking where we replaced go-git with git status, could this be related?

### Prompt 31

which commit?

### Prompt 32

that was a little while back :|

### Prompt 33

This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.

Analysis:
Let me chronologically trace through the conversation:

1. **Initial problem**: User reports git status is broken. I investigated and found `fatal: unable to read e53647ba1f1e5203bedff558b482f178db045555` - a corrupted worktree index in `.git/worktrees/1/index`.

2. **First fix**: Ran `git read-tree HEAD` to rebuild the worktree index....

### Prompt 34

yeah close it

### Prompt 35

what are the settings I need to enable auto-summarization?

