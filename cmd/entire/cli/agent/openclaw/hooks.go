package openclaw

import "github.com/entireio/cli/cmd/entire/cli/agent"

// Ensure OpenClawAgent implements HookHandler and HookSupport
var _ agent.HookHandler = (*OpenClawAgent)(nil)
var _ agent.HookSupport = (*OpenClawAgent)(nil)

// InstallHooks is a no-op for OpenClaw — hooks are invoked externally by the OpenClaw runtime.
// The OpenClaw gateway calls `entire hooks openclaw <verb>` directly.
func (o *OpenClawAgent) InstallHooks(_ bool, _ bool) (int, error) {
	return 4, nil // 4 hooks: session-start, session-end, stop, user-prompt-submit
}

// UninstallHooks is a no-op for OpenClaw — no config files to clean up.
func (o *OpenClawAgent) UninstallHooks() error {
	return nil
}

// AreHooksInstalled returns true for OpenClaw — hooks are always available
// since they are invoked by the OpenClaw runtime, not configured in a file.
func (o *OpenClawAgent) AreHooksInstalled() bool {
	return true
}

// GetSupportedHooks returns the hook types OpenClaw supports.
func (o *OpenClawAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookSessionEnd,
		agent.HookStop,
		agent.HookUserPromptSubmit,
	}
}

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
