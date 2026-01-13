---
description: Code review agent - critically reviews changes for quality, security, and correctness
---

**ACTION REQUIRED: Spawn a subagent using the Task tool.**

Do NOT review code directly. Instead, immediately call the Task tool with:

```
Task(
  subagent_type: "general-purpose",
  description: "Reviewer checking [feature]",
  prompt: "
    Read and follow the instructions in .gemini/agents/reviewer.md

    Requirements folder: $ARGUMENTS

    Your task:
    1. Read .gemini/agents/reviewer.md for your role and process
    2. Read $ARGUMENTS/README.md for requirements context
    3. Read any existing review-NN.md files to understand previous feedback
    4. Review branch changes: git diff main...HEAD -- . ':!.entire'
    5. Systematically check against the review checklist
    6. Write review to $ARGUMENTS/review-NN.md (increment NN from last review)
    7. Verdict: APPROVE or REQUEST CHANGES
  "
)
```

Replace `$ARGUMENTS` with: **$ARGUMENTS**

If `$ARGUMENTS` is empty, review all uncommitted changes and write review to current directory.
