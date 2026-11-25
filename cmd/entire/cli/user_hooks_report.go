package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
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
// agent-specific findings (Codex approval in /hooks), and a repository whose
// own Entire hooks are older than the user-level ones. Report-only: the
// project install always wins, and the next command with global enable on
// rewrites an outdated user-level copy.
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
		report := installer.InspectUserHooks(ctx)
		writeUserHookReport(w, installer, report)
		if line := olderProjectHooksLine(ctx, installer, report); line != "" {
			fmt.Fprintf(w, "  %s\n", line)
		}
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
		fmt.Fprintf(w, "%s user-level hooks: UNREADABLE: %v\n", name, report.Err)
	}
	fmt.Fprintf(w, "  %s\n", report.Path)
	for _, note := range report.Notes {
		fmt.Fprintf(w, "  %s\n", note)
	}
}

// olderProjectHooksLine flags a repository whose own Entire hooks for the
// agent are older than its current user-level ones. The project install
// still handles the agent here; this only says how to update it.
func olderProjectHooksLine(ctx context.Context, installer agent.UserHookInstaller, report agent.UserHookReport) string {
	freshness, ok := agent.AsHookFreshness(installer)
	if !ok || report.State != agent.UserHooksCurrent {
		return ""
	}
	if freshness.CheckHookConfig(ctx) != agent.HooksOutdated {
		return ""
	}
	return fmt.Sprintf("This repository's Entire hooks for %s are older than your user-level ones; run `entire enable` here to update them.", installer.Type())
}

// writeOlderProjectHooks is status's one line per agent whose project hooks
// here are older than its user-level ones. Only with global enable on.
func writeOlderProjectHooks(ctx context.Context, w io.Writer) {
	if !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	for _, installer := range userHookInstallers() {
		if line := olderProjectHooksLine(ctx, installer, installer.InspectUserHooks(ctx)); line != "" {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}
