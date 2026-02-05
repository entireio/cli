# Session Context

## User Prompts

### Prompt 1

why is lint failing on the build?

### Prompt 2

uh...you got blatted again it looks like...

### Prompt 3

is the fix not to make the local run match CI?

### Prompt 4

if we just commit and push it will it persist and not get stripped afterwards?

### Prompt 5

arrggghh that is no good.

can we restructure the code in any way (call an explicit exit at each block?) to workaround?

### Prompt 6

You are an expert code reviewer. Follow these steps:

      1. If no PR number is provided in the args, use Bash("gh pr list") to show open PRs
      2. If a PR number is provided, use Bash("gh pr view <number>") to get PR details
      3. Use Bash("gh pr diff <number>") to get the diff
      4. Analyze the changes and provide a thorough code review that includes:
         - Overview of what the PR does
         - Analysis of code quality and style
         - Specific suggestions for improvement...

### Prompt 7

the migration creates a new branch, yes?

### Prompt 8

does this version write to the v1 branch?

### Prompt 9

that was my question

1. run migration script
2. push new v1 branch
3. point clients at v1?

Do we have the sessions branch configuration anywhere?

### Prompt 10

how do I invoke the migration script? is it documented?

### Prompt 11

I'd like to do two things:
1. add the ability to migrate a specific checkpoint (given as an argument)
2. add usage information at the top of the sh file

### Prompt 12

ok, let's go

### Prompt 13

Q: entire explain - where is it getting the Author from? e.g. `entire explain -c 7674489fb0a9`

### Prompt 14

how can I check that file manually?

### Prompt 15

okay, run that and find the earliest commit - the author doesn't match up 🤔

