package cli

// User-level agent hook reconciliation.
//
// Split out of global_warn.go: this is not a warning. It MUTATES the user's
// global agent configuration — installing and uninstalling user-level hooks —
// as a side effect of globalPostRun, which runs after every command. That is
// the highest-consequence code in the global tier and it was sitting in a file
// named for a one-time notice, where a reviewer would not look for it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/globalhooks"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// reconcileUserHooks keeps user-level agent hooks in step with the global
// tier: installed while global tracking is enabled (for agents that are
// present on this machine or already carry an Entire entry), removed when
// the tier is configured but disabled. This is how a hand edit of the user
// settings file takes effect without a dedicated command — the next
// foreground `entire` invocation wires or unwires the hooks and says so.
// Unconfigured tier: nothing (zero cost for users who never opted in).
// Hook processes never reach this: the root post-run skips hidden commands
// (`hooks`, `hooks git`); userHookMutationSuppressed is defense in depth.
func reconcileUserHooks(ctx context.Context, us *settings.UserSettings, errW io.Writer) {
	if us == nil || !us.GlobalConfigured() || userHookMutationSuppressed() {
		return
	}
	selectionUsable := true
	if us.GlobalEnabled() {
		if _, err := globalhooks.Load(); err != nil {
			fmt.Fprintf(errW, "Note: global hooks need a usable selected installation. Run `entire agent` interactively and choose 'Select this installation for global hooks': %v\n", err)
			selectionUsable = false
		}
	}
	supports, _ := agent.UserHookSupports()
	for _, candidate := range supports {
		installed, err := candidate.Support.AreUserHooksInstalled(ctx)
		if err != nil {
			continue // doctor reports unreadable agent configs
		}
		switch {
		case us.GlobalEnabled() && selectionUsable && !installed && userHookAgentPresent(candidate.Name):
			if _, err := candidate.Support.InstallUserHooks(ctx); err != nil {
				fmt.Fprintf(errW, "Note: could not install %s user-level hooks for global tracking: %v\n", candidate.Name, err)
				continue
			}
			fmt.Fprintf(errW, "entire: installed user-level %s hooks (global tracking is on)\n", candidate.Name)
			fmt.Fprintln(errW, globalHookRestartNotice)
		case (!us.GlobalEnabled() || !selectionUsable) && (installed || userHookConfigContainsEntire(candidate.Name)):
			if err := candidate.Support.UninstallUserHooks(ctx); err != nil {
				fmt.Fprintf(errW, "Note: could not remove %s user-level hooks: %v\n", candidate.Name, err)
				continue
			}
			reason := "global tracking is off"
			if !selectionUsable {
				reason = "no usable installation is selected"
			}
			fmt.Fprintf(errW, "entire: removed user-level %s hooks (%s)\n", candidate.Name, reason)
			fmt.Fprintln(errW, globalHookRestartNotice)
		}
	}
}

// userHookMutationSuppressed reports that this process is an agent hook: the
// hidden-command walk in root.go already keeps globalPostRun out of hook
// processes, so this is defense in depth for any future direct caller.
func userHookMutationSuppressed() bool {
	return currentHookAgentName != ""
}

// userHookLookPath is exec.LookPath, swappable so tests can pretend an agent
// binary is (or is not) installed.
var userHookLookPath = exec.LookPath

// userHookAgentPresent: the agent binary is on PATH, or its user config
// already contains an Entire entry (a previous install we should keep current).
func userHookAgentPresent(name types.AgentName) bool {
	binaries := map[types.AgentName]string{agent.AgentNameClaudeCode: "claude", agent.AgentNameGemini: "gemini"}
	if bin, ok := binaries[name]; ok {
		if _, err := userHookLookPath(bin); err == nil {
			return true
		}
	}
	return userHookConfigContainsEntire(name)
}

func userHookConfigContainsEntire(name types.AgentName) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	var path string
	switch name {
	case agent.AgentNameClaudeCode:
		path = filepath.Join(home, ".claude", "settings.json")
	case agent.AgentNameGemini:
		path = filepath.Join(home, ".gemini", "settings.json")
	default:
		return false
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed per-user agent settings location
	if err != nil {
		return false
	}
	var config any
	if json.Unmarshal(data, &config) != nil {
		return false
	}
	return containsManagedHookCommand(config)
}

func containsManagedHookCommand(value any) bool {
	switch v := value.(type) {
	case string:
		return agent.IsManagedHookCommand(v)
	case []any:
		for _, item := range v {
			if containsManagedHookCommand(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if containsManagedHookCommand(item) {
				return true
			}
		}
	}
	return false
}
