# Session Context

## User Prompts

### Prompt 1

need a little patch for `entire explain` that prints the help text if a parameter is passed without any qualifier flags

### Prompt 2

**ACTION REQUIRED: Spawn a subagent using the Task tool.**

Do NOT review code directly. Instead, immediately call the Task tool with:

```
Task(
  subagent_type: "general-purpose",
  description: "Reviewer checking [feature]",
  prompt: "
    Read and follow the instructions in .claude/agents/reviewer.md

    Requirements folder: 

    Your task:
    1. Read .claude/agents/reviewer.md for your role and process
    2. Read /README.md for requirements context
    3. Read any existing review-NN.md...

### Prompt 3

let's do the hint if it's small

