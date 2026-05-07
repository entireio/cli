package codex

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// DiscoverReviewSkills is a stub until the Codex on-disk plugin layout is
// verified against codex-rs source (see codex-rs/tui/src/slash_command.rs).
// Returns (nil, nil) so the picker treats Codex as "built-ins + install
// hint only" for Phase 1.
func (c *CodexAgent) DiscoverReviewSkills(_ context.Context) ([]agent.DiscoveredSkill, error) {
	return nil, nil
}
