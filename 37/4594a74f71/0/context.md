# Session Context

## User Prompts

### Prompt 1

I do want to e2e test my entire tooling with claude code, to do so, I want to use the agent sdk library ( typescript one ) to run some prompting and then check how my git branches looks like, we are creating checkpoints at shadow branches entire/commitsha and record checkpoints at entire/sessions. I want to stimulate claude thru the agent sdk and then, run assertions to check what i have just told you.

### Prompt 2

[Request interrupted by user for tool use]

### Prompt 3

Implement the following plan:

# E2E Testing with Claude Agent SDK

## Goal
Create true end-to-end tests that run Claude via the Agent SDK (TypeScript) and verify that the Entire CLI correctly creates checkpoints on:
- Shadow branches: `entire/<commit-hash[:7]>`
- Metadata branch: `entire/sessions`

## Directory Structure

```
e2e/
├── package.json
├── tsconfig.json
├── vitest.config.ts
├── src/
│   ├── setup/
│   │   └── global-setup.ts      # Builds CL...

### Prompt 4

do i need to add any claude settings to this work ?

