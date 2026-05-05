package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// Dispatch resolves an unknown subcommand to an installed plugin and execs
// it. Returns handled=true if a plugin was found and executed (even if it
// returned a non-zero exit code, which is propagated via err). When handled
// is false, the caller should fall through to its normal unknown-command
// handling.
//
// args is the full argv slice excluding the program name (e.g.
// os.Args[1:]). Cobra resolves built-ins first, so any name registered on
// rootCmd (including aliases) takes precedence over an installed plugin.
func Dispatch(ctx context.Context, rootCmd *cobra.Command, args []string) (bool, error) {
	mgr, err := NewManager()
	if err != nil {
		return false, err
	}
	return DispatchWith(ctx, rootCmd, args, mgr)
}

// DispatchWith is Dispatch with an explicit Manager (useful for tests so
// they can avoid mutating ENTIRE_PLUGIN_DIR).
func DispatchWith(ctx context.Context, rootCmd *cobra.Command, args []string, mgr *Manager) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}

	// Skip flags. The first non-flag arg is the candidate subcommand name.
	// We deliberately mirror cobra's "first positional after the root" model:
	// `entire --foo bar baz` → the candidate is "bar".
	first := -1
	for i, a := range args {
		if a == "--" {
			if i+1 < len(args) {
				first = i + 1
			}
			break
		}
		if len(a) == 0 || a[0] == '-' {
			continue
		}
		first = i
		break
	}
	if first == -1 {
		return false, nil
	}

	name := args[first]
	if !ValidName(name) {
		return false, nil
	}

	// Built-in precedence: cobra resolves built-ins first. If rootCmd.Find()
	// returns a non-root command, the user wanted a built-in (potentially
	// misspelled) and we should not shadow it.
	if isBuiltin(rootCmd, name) {
		return false, nil
	}

	if mgr == nil {
		return false, errors.New("plugin: nil manager")
	}
	p, err := mgr.Find(name)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}

	rest := args[first+1:]
	if err := Exec(ctx, p, rest, mgr.Root); err != nil {
		return true, err
	}
	return true, nil
}

// Exec runs the plugin executable with the given args, inheriting stdio and
// injecting ENTIRE_* env vars. The caller's exit code should mirror the
// child's; on a non-zero exit Exec returns an *exec.ExitError so callers can
// inspect ProcessState.
func Exec(ctx context.Context, p *Plugin, args []string, pluginRoot string) error {
	if p == nil {
		return errors.New("plugin: nil plugin")
	}
	if p.ExecPath == "" {
		return fmt.Errorf("plugin %q: missing executable path", p.Name)
	}
	if _, err := os.Stat(p.ExecPath); err != nil {
		return fmt.Errorf("plugin %q: %w", p.Name, err)
	}

	cmd, err := buildExecCommand(ctx, p, args)
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = pluginEnv(os.Environ(), p, pluginRoot)
	if err := cmd.Run(); err != nil {
		// Always wrap, but preserve the *exec.ExitError chain via %w so
		// callers can errors.As to recover the child's exit code.
		return fmt.Errorf("plugin %q: %w", p.Name, err)
	}
	return nil
}

// pluginEnv returns the child environment with ENTIRE_* injections.
//   - ENTIRE_PLUGIN_DATA_DIR: per-plugin durable storage under the plugins root.
//   - ENTIRE_REPO_ROOT: passes through if the parent already set it.
//   - ENTIRE_SESSION_ID: passes through if the parent already set it.
//
// Callers (e.g. main.go) are responsible for setting ENTIRE_REPO_ROOT before
// invoking dispatch. We don't compute it here to avoid pulling the paths
// package and its git CLI dependency into the dispatcher.
func pluginEnv(parent []string, p *Plugin, pluginRoot string) []string {
	out := make([]string, 0, len(parent)+1)
	out = append(out, parent...)
	out = append(out, "ENTIRE_PLUGIN_DATA_DIR="+pluginDataDir(pluginRoot, p.Name))
	return out
}

// pluginDataDir returns a per-plugin data directory adjacent to the plugin
// install location. Plugins should write durable data here, not into the
// install dir (which gets replaced on upgrade).
func pluginDataDir(root, name string) string {
	return root + string(os.PathSeparator) + Prefix + name + string(os.PathSeparator) + "data"
}

// isBuiltin reports whether name resolves to a built-in command or alias on
// rootCmd. This mirrors gh's install-time conflict check.
func isBuiltin(rootCmd *cobra.Command, name string) bool {
	if rootCmd == nil {
		return false
	}
	cmd, _, err := rootCmd.Find([]string{name})
	if err != nil {
		return false
	}
	// Find returns the deepest command it could resolve; for an unknown
	// subcommand it returns rootCmd itself with no error. So a real built-in
	// is anything that resolved past the root.
	return cmd != nil && cmd != rootCmd
}

// PropagateExitCode inspects err for an *exec.ExitError and returns the
// child's exit status, or -1 if err is nil or unrelated to a child exit.
// Callers (typically main.go) can use this to mirror the plugin's exit code.
func PropagateExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
