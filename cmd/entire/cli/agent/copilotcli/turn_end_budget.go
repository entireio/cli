package copilotcli

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// hostDefaultHookTimeoutSec is Copilot CLI's default hook timeout, which applies
// to agentStop because Entire installs it without a timeoutSec. Copilot kills a
// hook that outlasts it and continues: "timeoutSec ... Default: 30"
// (https://docs.github.com/en/copilot/reference/hooks-configuration).
const hostDefaultHookTimeoutSec = 30

var _ agent.TurnEndHookTimeouter = (*CopilotCLIAgent)(nil)

// TurnEndHookTimeout implements agent.TurnEndHookTimeouter. Entire sets no
// timeoutSec, so the host default applies.
func (*CopilotCLIAgent) TurnEndHookTimeout() time.Duration {
	return time.Duration(hostDefaultHookTimeoutSec) * time.Second
}
