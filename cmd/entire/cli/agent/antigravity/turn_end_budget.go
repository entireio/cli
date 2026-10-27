package antigravity

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.TurnEndHookTimeouter = (*AntigravityAgent)(nil)

// TurnEndHookTimeout implements agent.TurnEndHookTimeouter. Entire installs the
// Stop hook with stopHookTimeoutSeconds, and RepairHookTimeouts adds it to
// older installs.
func (*AntigravityAgent) TurnEndHookTimeout() time.Duration {
	return time.Duration(stopHookTimeoutSeconds) * time.Second
}
