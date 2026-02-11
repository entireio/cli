package cli

import (
	"fmt"
	"strings"
)

// maxDiffSize is the maximum size of the diff included in the review prompt.
// Large diffs degrade review quality, so we truncate.
const maxDiffSize = 100 * 1024 // 100KB

// reviewPromptTemplate is the prompt sent to Claude for code review.
//
// Security note: User content (diff, prompts, context) is wrapped in XML tags
// to provide clear boundary markers, similar to the summarization prompt pattern.
const reviewPromptTemplate = `You are a senior code reviewer performing an intent-aware review. Your job is not just to find bugs in the code — it is to evaluate whether the changes correctly and completely fulfill what the developer was trying to accomplish.

## Session Context

This review is part of an automated development workflow. The developer works with an AI coding agent that makes changes on their behalf. Below is the checkpoint data captured during the session, which tells you WHY these changes were made.

### Developer's Prompts
The original instructions the developer gave to the agent:
<prompts>
%s
</prompts>

### Commit Message
%s

### Session Context
Checkpoint data including the session summary and key actions taken:
<session-context>
%s
</session-context>

### Checkpoint Files
You have read-only access to the repository. If you need deeper context about the session,
you can read these checkpoint files (they may or may not exist):
- ` + "`%s`" + ` — session transcript (JSONL format)
- ` + "`%s`" + ` — user prompts collected during session
- ` + "`%s`" + ` — generated session context/summary

## Code Changes

Files changed: %s

<diff>
%s
</diff>

## Review Instructions

Use the session context above to understand the developer's intent. Then review the code changes with that intent in mind.

**Intent alignment** (most important):
- Do the changes actually accomplish what the developer asked for?
- Are there any prompts or requirements that were missed or only partially implemented?
- Does the implementation match the stated approach in the session context?

**Correctness**:
- Bugs, logic errors, or off-by-one mistakes
- Race conditions or concurrency issues
- Missing error handling for failure paths that matter

**Security**:
- Injection vulnerabilities (SQL, command, XSS)
- Hardcoded secrets or credentials
- Unsafe file operations or path traversal

**Robustness**:
- Edge cases not handled (empty inputs, nil pointers, large data)
- Resource leaks (unclosed files, connections, goroutines)
- Missing timeouts on external calls

Do NOT flag:
- Style preferences or formatting (the linter handles that)
- Missing comments or documentation on clear code
- Theoretical issues that cannot happen given the actual call sites

For each issue found, provide:
1. Severity: CRITICAL, WARNING, or SUGGESTION
2. File path and approximate line reference (from the diff)
3. Description of the issue
4. Suggested fix (code snippet when helpful)

Format your response as Markdown:

# Code Review

## Summary
Brief assessment: does this change accomplish its stated goal? What's the overall quality?

## Issues

### [SEVERITY] Short description
**File:** ` + "`path/to/file.go:42`" + `
**Description:** What the issue is and why it matters.
**Suggestion:**
` + "```" + `
// suggested fix
` + "```" + `

If no issues are found, confirm the changes look correct and match the intent.
Do NOT include any preamble or explanation outside the Markdown structure above.`

// buildReviewPrompt constructs the review prompt from the payload, context, and diff.
func buildReviewPrompt(prompts []string, commitMessage, sessionContext, sessionID, fileList, diff string) string {
	promptText := strings.Join(prompts, "\n\n---\n\n")
	if promptText == "" {
		promptText = "(no prompts captured)"
	}

	commitMsgText := commitMessage
	if commitMsgText == "" {
		commitMsgText = "(no commit message)"
	}

	contextText := sessionContext
	if contextText == "" {
		contextText = "(no session context available)"
	}

	// Build checkpoint file paths
	metadataDir := ".entire/metadata/" + sessionID
	if sessionID == "" {
		metadataDir = ".entire/metadata/<unknown>"
	}
	transcriptPath := metadataDir + "/full.jsonl"
	promptPath := metadataDir + "/prompt.txt"
	contextPath := metadataDir + "/context.md"

	// Truncate large diffs
	if len(diff) > maxDiffSize {
		diff = diff[:maxDiffSize] + "\n\n... (diff truncated at 100KB)"
	}

	return fmt.Sprintf(reviewPromptTemplate, promptText, commitMsgText, contextText,
		transcriptPath, promptPath, contextPath, fileList, diff)
}
