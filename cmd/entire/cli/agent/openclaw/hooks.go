package openclaw

import "github.com/entireio/cli/cmd/entire/cli/agent"

// Ensure OpenClawAgent implements HookHandler
var _ agent.HookHandler = (*OpenClawAgent)(nil)

// OpenClaw hook names - these become subcommands under `entire hooks openclaw`
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
)

// GetHookNames returns the hook verbs OpenClaw supports.
// These become subcommands: entire hooks openclaw <verb>
func (o *OpenClawAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
	}
}
