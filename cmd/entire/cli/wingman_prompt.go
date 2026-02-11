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
// Security note: User content (diff, prompts) is wrapped in XML tags to provide
// clear boundary markers, similar to the summarization prompt pattern.
const reviewPromptTemplate = `You are a senior code reviewer. Review the following code changes and provide actionable feedback.

## Context

The developer's intent (from their prompts):
<prompts>
%s
</prompts>

## Code Changes

Files changed: %s

<diff>
%s
</diff>

## Instructions

Review the code changes above. Focus on:
- Bugs or logic errors
- Security vulnerabilities
- Performance issues
- Missing error handling
- Code style and readability issues

For each issue found, provide:
1. Severity: CRITICAL, WARNING, or SUGGESTION
2. File path and line number (from the diff)
3. Description of the issue
4. Suggested fix

Format your response as Markdown with this structure:

# Code Review

## Summary
Brief overview of the changes and overall assessment.

## Issues

### [SEVERITY] Short description
**File:** ` + "`path/to/file.go:42`" + `
**Description:** What the issue is and why it matters.
**Suggestion:**
` + "```" + `
// suggested fix
` + "```" + `

If no issues are found, say so and optionally provide suggestions for improvement.
Do NOT include any preamble or explanation outside the Markdown structure above.`

// buildReviewPrompt constructs the review prompt from the payload and diff.
func buildReviewPrompt(prompts []string, fileList string, diff string) string {
	promptText := strings.Join(prompts, "\n\n---\n\n")
	if promptText == "" {
		promptText = "(no prompts captured)"
	}

	// Truncate large diffs
	if len(diff) > maxDiffSize {
		diff = diff[:maxDiffSize] + "\n\n... (diff truncated at 100KB)"
	}

	return fmt.Sprintf(reviewPromptTemplate, promptText, fileList, diff)
}
