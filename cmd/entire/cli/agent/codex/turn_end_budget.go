package codex

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.TurnEndHookTimeouter = (*CodexAgent)(nil)

// TurnEndHookTimeout implements agent.TurnEndHookTimeouter. Entire installs the
// Stop hook with defaultHookTimeoutSec.
func (*CodexAgent) TurnEndHookTimeout() time.Duration {
	return time.Duration(defaultHookTimeoutSec) * time.Second
}
