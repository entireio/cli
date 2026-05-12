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

// ComposeReviewPrompt assembles the prompt sent to the agent. It joins
// the configured skill invocations, the always-prompt, the per-run
// prompt, and a scope clause that pins the agent to commits unique to
// the current branch vs cfg.ScopeBaseRef plus any uncommitted changes.
//
// Empty sections are skipped (no triple-newline gaps). The scope clause
// is only added when cfg.ScopeBaseRef is non-empty.
func ComposeReviewPrompt(cfg reviewtypes.RunConfig) string {
	if cfg.PromptOverride != "" {
		return cfg.PromptOverride
	}

	var sections []string

	// Skills: one per line, joined as a single section.
	if len(cfg.Skills) > 0 {
		sections = append(sections, strings.Join(cfg.Skills, "\n"))
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
