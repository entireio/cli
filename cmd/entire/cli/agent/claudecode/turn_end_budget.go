package claudecode

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.TurnEndHookTimeouter = (*ClaudeCodeAgent)(nil)

// TurnEndHookTimeout implements agent.TurnEndHookTimeouter. Entire installs the
// turn-end hooks with checkpointHookTimeoutSec.
func (*ClaudeCodeAgent) TurnEndHookTimeout() time.Duration {
	return time.Duration(checkpointHookTimeoutSec) * time.Second
}
