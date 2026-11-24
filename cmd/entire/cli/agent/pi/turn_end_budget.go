package pi

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// extensionHookTimeout is the execFile timeout the embedded extension applies to
// every hook it fires, agent_end included. It is the only limit on the hook, and
// Entire owns it, so TestExtensionHookTimeoutMatchesTheExtension keeps the two
// in step.
const extensionHookTimeout = 10 * time.Second

var _ agent.TurnEndHookTimeouter = (*PiAgent)(nil)

// TurnEndHookTimeout implements agent.TurnEndHookTimeouter. The extension's own
// execFile timeout is the only limit on agent_end.
func (*PiAgent) TurnEndHookTimeout() time.Duration {
	return extensionHookTimeout
}
