# Session Context

## User Prompts

### Prompt 1

we are reviewing https://github.com/entireio/cli/pull/102 - invoke reviewer. remember also you have LSP tool access.

Also the lint looks broken so let's fix that along the way

### Prompt 2

**ACTION REQUIRED: Spawn a subagent using the Task tool.**

Do NOT review code directly. Instead, immediately call the Task tool with:

```
Task(
  subagent_type: "general-purpose",
  description: "Reviewer checking [feature]",
  prompt: "
    Read and follow the instructions in .claude/agents/reviewer.md

    Requirements folder: https://github.com/entireio/cli/pull/102

    Your task:
    1. Read .claude/agents/reviewer.md for your role and process
    2. Read https://github.com/entireio/cli/p...

