package antigravity

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// DiscoverReviewSkills is a stub until the Antigravity on-disk extension layout
// is verified. Returns (nil, nil) so the picker relies on the per-agent
// install hint for Phase 1.
func (a *AntigravityAgent) DiscoverReviewSkills(_ context.Context) ([]agent.DiscoveredSkill, error) {
	return nil, nil
}
