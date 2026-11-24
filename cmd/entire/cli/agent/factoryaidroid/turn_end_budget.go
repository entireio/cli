package factoryaidroid

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// hostDefaultHookTimeoutSec is Factory AI Droid's default hook timeout, which
// applies to Stop because Entire installs it without a timeout: "Per-command
// timeout in seconds. Defaults to 60"
// (https://docs.factory.com/reference/hooks-reference).
const hostDefaultHookTimeoutSec = 60

var _ agent.TurnEndHookTimeouter = (*FactoryAIDroidAgent)(nil)

// TurnEndHookTimeout implements agent.TurnEndHookTimeouter. Entire sets no
// timeout, so the host default applies.
func (*FactoryAIDroidAgent) TurnEndHookTimeout() time.Duration {
	return time.Duration(hostDefaultHookTimeoutSec) * time.Second
}
