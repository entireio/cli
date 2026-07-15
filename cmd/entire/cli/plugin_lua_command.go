package cli

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/plugins"
	"github.com/spf13/cobra"
)

// MaybeRunLuaCommand resolves `entire <name>` to a Lua-plugin-contributed
// command and runs it. It enforces the resolution order built-in > Lua command
// > `entire-<name>` binary: a built-in always wins (this returns not-handled so
// Cobra runs it), and it is invoked from main.go BEFORE the binary plugin
// dispatcher so a Lua command wins over a same-named binary plugin.
//
// Returns (true, exitCode) when a Lua command handled the invocation, else
// (false, 0). Plugins are only loaded when the first arg is a plugin-shaped,
// non-built-in name, so built-in commands never pay the discovery cost.
func MaybeRunLuaCommand(ctx context.Context, rootCmd *cobra.Command, args []string) (handled bool, exitCode int) {
	if len(args) == 0 {
		return false, 0
	}
	name := args[0]
	if !isPluginCandidate(name) {
		return false, 0
	}
	// Built-in commands always win. Prime help/completion so their names aren't
	// shadowed, mirroring the binary dispatcher's resolvePlugin.
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd(args...)
	if cmd, _, err := rootCmd.Find(args); err == nil && cmd != rootCmd {
		return false, 0
	}

	code, found := plugins.RunCommand(ctx, name, args[1:])
	if !found {
		return false, 0
	}
	return true, code
}
