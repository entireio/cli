# Session Context

## User Prompts

### Prompt 1

You are an expert code reviewer. Follow these steps:

      1. If no PR number is provided in the args, use Bash("gh pr list") to show open PRs
      2. If a PR number is provided, use Bash("gh pr view <number>") to get PR details
      3. Use Bash("gh pr diff <number>") to get the diff
      4. Analyze the changes and provide a thorough code review that includes:
         - Overview of what the PR does
         - Analysis of code quality and style
         - Specific suggestions for improvement...

### Prompt 2

bring it up to date with main, github is flagging merge conflicts

### Prompt 3

oh, what's outstanding? just the 'merge' fixes?

### Prompt 4

umm, we have changes in our working copy I'm sorry to say 😛

