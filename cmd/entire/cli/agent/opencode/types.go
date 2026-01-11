package opencode

// Constants for OpenCode-specific identifiers
const (
	// PluginName is the npm package name of the Entire integration plugin
	PluginName = "opencode-entire-integration"
)

// OpenCode hook names - these become subcommands under `entire hooks opencode`
const (
	HookNameSessionStart = "session-start"
	HookNameStop         = "stop"
	HookNameTaskStart    = "task-start"
	HookNameTaskComplete = "task-complete"
)
