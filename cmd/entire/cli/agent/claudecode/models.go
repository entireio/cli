package claudecode

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.ModelLister = (*ClaudeCodeAgent)(nil)

// ListModels returns common Claude model aliases for `entire review --model`.
// Claude Code's CLI accepts these aliases (per `claude --help`) as well as full
// model identifiers; the list is advisory and intentionally non-exhaustive.
func (c *ClaudeCodeAgent) ListModels(_ context.Context) ([]agent.ModelInfo, error) {
	return []agent.ModelInfo{
		{ID: "opus", Note: "alias — latest Claude Opus"},
		{ID: "sonnet", Note: "alias — latest Claude Sonnet"},
		{ID: "haiku", Note: "alias — latest Claude Haiku (fast)"},
	}, nil
}
