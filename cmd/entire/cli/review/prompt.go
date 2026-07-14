// Package review — see env.go for package-level rationale.
//
// prompt.go implements the shared prompt composer used by all per-agent
// reviewers. The scope clause pins agents to "commits unique to this branch
// vs the mainline base ref, plus uncommitted working-tree changes" —
// preventing the divergent-default problem where codex defaulted to
// origin/main...HEAD and claude defaulted to working-tree-only on the same
// invocation (regression class from #1018 commit b9ed9c074; enforced
// structurally here).
package review

import (
	"strings"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// ComposeReviewPrompt assembles the prompt sent to a worker agent. It joins
// the configured skill invocations, the profile's canonical task, per-agent
// instructions, the per-run prompt, and a scope clause that pins the agent to
// commits unique to the current branch vs cfg.ScopeBaseRef plus any
// uncommitted changes.
//
// Empty sections are skipped (no triple-newline gaps). The scope clause is
// only added when cfg.ScopeBaseRef is non-empty.
func ComposeReviewPrompt(cfg reviewtypes.RunConfig) string {
	if cfg.PromptOverride != "" {
		return cfg.PromptOverride
	}

	var sections []string

	// Skills: one per line, joined as a single section. These are agent-specific
	// mechanics; the canonical task below keeps multi-agent fan-out coherent.
	if len(cfg.Skills) > 0 {
		sections = append(sections, strings.Join(cfg.Skills, "\n"))
	}

	if cfg.ProfileName != "" {
		sections = append(sections, "Review profile: "+cfg.ProfileName)
	}
	if trimmed := strings.TrimRight(cfg.Task, "\n\r "); trimmed != "" {
		sections = append(sections, "Task: "+trimmed)
		sections = append(sections, reviewerOutputFormatInstructions)
	}

	// AlwaysPrompt and PerRunPrompt: each is its own section if non-empty after trim.
	if trimmed := strings.TrimRight(cfg.AlwaysPrompt, "\n\r "); trimmed != "" {
		sections = append(sections, trimmed)
	}
	if trimmed := strings.TrimRight(cfg.PerRunPrompt, "\n\r "); trimmed != "" {
		sections = append(sections, trimmed)
	}

	// Scope clause: only when a base ref was detected. Includes uncommitted
	// working-tree changes alongside the committed branch diff so iterative
	// edits-in-progress are reviewed too — without this, agents correctly
	// follow "commits-only" wording and silently skip uncommitted work,
	// which is the most common case when a developer is mid-feature.
	if cfg.ScopeBaseRef != "" {
		sections = append(sections,
			"Scope: review the commits unique to this branch vs "+cfg.ScopeBaseRef+
				", plus any uncommitted changes in the working tree. Ignore code outside this scope.")
	}
	if trimmed := strings.TrimRight(cfg.CheckpointContext, "\n\r "); trimmed != "" {
		sections = append(sections, trimmed)
	}

	return strings.Join(sections, "\n\n")
}

const reviewerOutputFormatInstructions = `Output format:
- Start with one verdict line: approve / approve with nits / request changes, plus a short reason.
- Then list actionable findings only. Each finding MUST be a separate top-level Markdown bullet starting with [high], [medium], or [low].
- Include an exact file:line pointer in each finding when possible, plus the bug, impact, and fix in one concise paragraph.
- Do not combine multiple defects in one bullet or paragraph. Do not emit severity-heading paragraphs like "**[HIGH] ...**" without a leading bullet.
- If there are no actionable findings, output only the verdict line.
- Keep the report compact: quote only the minimal relevant snippet (a few lines) per finding. Never paste whole files, full diffs, or large logs, and skip decorative formatting like tables or ASCII art — it wastes effort and is not rendered.`
