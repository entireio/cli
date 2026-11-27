package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// userLevelHookSessionStartWarning is the SessionStart notice for an agent
// whose user-level hooks need the user's attention, such as Codex hooks
// awaiting approval in /hooks. Only with global enable on: otherwise there
// are no user-level hooks to report.
func userLevelHookSessionStartWarning(ctx context.Context, ag agent.Agent) string {
	installer, ok := agent.AsUserHookInstaller(ag)
	if !ok || !settings.UserTierEnablesEverywhere(ctx) {
		return ""
	}
	report := installer.InspectUserHooks(ctx)
	if report.State != agent.UserHooksCurrent && report.State != agent.UserHooksOutdated {
		return ""
	}
	return strings.Join(report.Notes, " ")
}

// checkUserLevelHooks is doctor's report of every agent's user-level hooks,
// which global enable installs: where they are, whether they match this CLI,
// and agent-specific findings (Codex approval in /hooks). Report-only: the
// next command with global enable on rewrites an outdated user-level copy.
func checkUserLevelHooks(cmd *cobra.Command) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	if err := settings.UserSettingsReadError(ctx); err != nil {
		fmt.Fprintf(w, "Entire user settings: UNREADABLE: %v\n", err)
		fmt.Fprintln(w, "  global_enable and repos entries there are ignored until the file is fixed.")
		return
	}
	if !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	for _, installer := range userHookInstallers() {
		writeUserHookReport(w, installer, installer.InspectUserHooks(ctx))
	}
}

func writeUserHookReport(w io.Writer, installer agent.UserHookInstaller, report agent.UserHookReport) {
	name := installer.Type()
	switch report.State {
	case agent.UserHooksNotInUse:
		return
	case agent.UserHooksCurrent:
		fmt.Fprintf(w, "✓ %s user-level hooks: INSTALLED (version %d)\n", name, report.Version)
	case agent.UserHooksOutdated:
		fmt.Fprintf(w, "%s user-level hooks: OUT OF DATE (version %d); the next entire command rewrites them\n", name, report.Version)
	case agent.UserHooksAbsent:
		fmt.Fprintf(w, "%s user-level hooks: MISSING; the next entire command adds them\n", name)
	case agent.UserHooksUnavailable:
		fmt.Fprintf(w, "%s user-level hooks: NOT INSTALLED: %v\n", name, report.Err)
	}
	fmt.Fprintf(w, "  %s\n", report.Path)
	for _, note := range report.Notes {
		fmt.Fprintf(w, "  %s\n", note)
	}
}

// statusAgents is what status lists: the agents with this repository's own
// Entire hooks, and, with global enable on, those with user-level hooks and
// those of them whose project hooks here take over (shadowed).
type statusAgents struct {
	project, userLevel, shadowed []string
}

// resolveStatusAgents finds status's agents. In a repository at the home
// directory an agent's user-level hook file can be the very file its project
// hooks would be in; that install is the user-level one, not the
// repository's.
func resolveStatusAgents(ctx context.Context) statusAgents {
	installed := GetAgentsWithHooksInstalled(ctx)
	var userLevel []types.AgentName
	if settings.UserTierEnablesEverywhere(ctx) {
		root, rootErr := paths.WorktreeRoot(ctx)
		for _, installer := range userHookInstallers() {
			report := installer.InspectUserHooks(ctx)
			if report.State != agent.UserHooksCurrent && report.State != agent.UserHooksOutdated {
				continue
			}
			userLevel = append(userLevel, installer.Name())
			if locator, ok := agent.AsHookConfigLocator(installer); ok && rootErr == nil &&
				sameFile(filepath.Join(root, locator.HookConfigRelPath()), report.Path) {
				installed = slices.DeleteFunc(slices.Clone(installed), func(n types.AgentName) bool { return n == installer.Name() })
			}
		}
	}
	var shadowed []types.AgentName
	for _, name := range userLevel {
		if slices.Contains(installed, name) {
			shadowed = append(shadowed, name)
		}
	}
	return statusAgents{
		project:   agentDisplayNames(installed),
		userLevel: agentDisplayNames(userLevel),
		shadowed:  agentDisplayNames(shadowed),
	}
}

// sameFile reports whether a and b name one existing file, however spelled
// (a symlinked temp or home directory included).
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}
