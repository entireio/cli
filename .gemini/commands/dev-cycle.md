---
description: Orchestrates the development cycle - developer implements, reviewer critiques, repeat until done
---

# Development Cycle Orchestrator

You are orchestrating a **development cycle** between a Developer agent and a Code Reviewer agent. Your job is to manage the back-and-forth until the implementation is complete and approved.

## The Cycle

```
┌─────────────────┐
│   Requirements  │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│    Developer    │◄─────────┐
│   Implements    │          │
└────────┬────────┘          │
         │                   │
         ▼                   │
┌─────────────────┐          │
│    Reviewer     │          │
│    Critiques    │          │
└────────┬────────┘          │
         │                   │
         ▼                   │
    ┌─────────┐              │
    │Approved?│──── No ──────┘
    └────┬────┘
         │ Yes
         ▼
┌─────────────────┐
│      Done       │
└─────────────────┘
```

## Agent Instructions

- Developer: `.gemini/agents/dev.md`
- Reviewer: `.gemini/agents/reviewer.md`

## Your Process

### Phase 1: Setup
1. Read the requirements from: **$ARGUMENTS/README.md**
2. Understand what needs to be built
3. **Create feature branch** (if not already on one):
   ```bash
   # Extract feature name from path (e.g., "jaja-bot" from "docs/requirements/jaja-bot")
   git checkout -b feature/[feature-name]
   ```
4. Review any existing task files and review files in **$ARGUMENTS/**

### Phase 2: Development Loop

**For each iteration:**

1. **Invoke Developer Agent**
   Use the Task tool:
   ```
   Task(
     subagent_type: "general-purpose",
     prompt: "
       Read and follow .gemini/agents/dev.md

       Requirements folder: $ARGUMENTS
       Previous reviewer feedback: [paste feedback if any, or 'None - first iteration']

       Implement the next increment using TDD.
       Report what you built when done.
     "
   )
   ```

2. **Invoke Reviewer Agent**
   Use the Task tool:
   ```
   Task(
     subagent_type: "general-purpose",
     prompt: "
       Read and follow .gemini/agents/reviewer.md

       Requirements folder: $ARGUMENTS

       Review the branch changes (git diff main...HEAD -- . ':!.entire').
       Provide structured feedback with verdict: APPROVE or REQUEST CHANGES
     "
   )
   ```

3. **Evaluate**
   - Read the latest `$ARGUMENTS/review-NN.md` for the verdict
   - If APPROVE: Note that it is approved, but check for non-critical suggestions
     - if there are any suggestions, send it back to the developer to evaluate
     - otherwise move to Finalization
   - If REQUEST CHANGES: Loop back to developer (they'll read the review file)

### Phase 3: Finalization
1. Run final test suite
2. Run linting/formatting
3. Summarize what was built
4. Suggest commit message

## Iteration Limits

- Maximum 5 iterations before escalating to user
- If stuck in a loop, ask for human guidance

## Communication

After each iteration, report to the user:
- What the developer implemented
- What the reviewer found
- Current status (continuing / approved / needs help)

## Your Task

Begin the development cycle for: **$ARGUMENTS**

`$ARGUMENTS` should be a path to a requirements folder (e.g., `docs/requirements/jaja-bot`).

**Start immediately by:**
1. Reading `$ARGUMENTS/README.md` for requirements
2. Checking for existing task/review files in `$ARGUMENTS/`
3. Creating feature branch: `git checkout -b feature/[name]`
4. **Spawning the developer subagent using the Task tool** (do not implement directly - delegate to subagent)

**IMPORTANT:** You are the orchestrator. You MUST use the Task tool to spawn developer and reviewer subagents. Do not implement or review code yourself - delegate to the specialized agents.
