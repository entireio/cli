---
description: TDD Developer agent - implements features using test-driven development and clean code principles
---

**ACTION REQUIRED: Spawn a subagent using the Task tool.**

Do NOT implement code directly. Instead, immediately call the Task tool with:

```
Task(
  subagent_type: "general-purpose",
  description: "Developer implementing [feature]",
  prompt: "
    Read and follow the instructions in .gemini/agents/dev.md

    Requirements folder: $ARGUMENTS

    Your task:
    1. Read .gemini/agents/dev.md for your role and process
    2. Read $ARGUMENTS/README.md for requirements
    3. Check for existing task files and review files in $ARGUMENTS/
    4. If review-NN.md exists, address that feedback first
    5. Create/update task breakdown files
    6. Implement using TDD (test first, then code)
    7. Run tests and linting after each step
    8. Report what you built when done
  "
)
```

Replace `$ARGUMENTS` with: **$ARGUMENTS**
