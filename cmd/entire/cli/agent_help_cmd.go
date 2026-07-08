package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
)

// agentHelpAnnotation marks an otherwise-hidden command as worth advertising to
// coding agents through `entire agent-help`. Hidden commands (e.g. trail) opt in
// by setting Annotations[agentHelpAnnotation] = "true".
const agentHelpAnnotation = "entire_agent_help"

// agentHelpRequiresTrailsAnnotation marks a command whose surface should only be
// advertised to agents when trails are enabled for the repo. While the trails
// product may not be available to a user yet, agent-help must not point agents at
// commands they can't use — so trail-gated commands are hidden until the same
// "is trails enabled" signal the first-turn injection already gates on says yes.
const agentHelpRequiresTrailsAnnotation = "entire_agent_help_requires_trails"

// agentHelpAnnotationEnabled is the truthy value for the agent-help annotations.
const agentHelpAnnotationEnabled = "true"

// agentHelpOverview is the only hand-maintained prose in agent-help: a terse,
// high-level "what entire is for" plus the standing repo-inference rule. It names
// no flags or subcommands — those are rendered live from the installed command
// tree — so it changes only when a whole capability area lands, not when a flag
// is added.
const agentHelpOverview = `Entire's CLI is the source of truth for its own usage. Do not guess flags or
subcommands — read them from this command. You are already inside the repo:
entire auto-detects it from the git origin remote, so never ask the user for the
repo name. Pass --repo only to target a DIFFERENT repo.`

// newAgentHelpCmd builds the `entire agent-help` command. It is visible in
// `entire help` (so agents on transports without context injection can still
// find it) and renders agent-facing usage live from rootCmd's command tree.
func newAgentHelpCmd(rootCmd *cobra.Command) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "agent-help [command...]",
		Short: "Machine-readable usage for coding agents (always matches the installed CLI)",
		Long: `Prints agent-facing usage for the Entire CLI, generated live from the installed
command tree so it always matches this binary. With no arguments it prints a
high-level map of when to use entire and which subcommand; pass a command path
(e.g. "agent-help checkpoint") to see that command's exact, current flags.`,
		RunE: func(c *cobra.Command, args []string) error {
			// Resolve the origin remote once and derive both the repo line and the
			// trails-enablement check from it (avoids two git subprocesses per run).
			repoLine, trailsEnabled := agentHelpRepoContext(c.Context())
			out, err := runAgentHelp(rootCmd, args, repoLine, asJSON, trailsEnabled)
			if err != nil {
				return err
			}
			fmt.Fprint(c.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured JSON instead of text")
	return cmd
}

// agentHelpRepoContext resolves the origin remote ONCE and derives both the repo
// line (forge/owner/repo, or "" when it can't be determined — no origin /
// detached HEAD — so the renderer degrades gracefully) and whether trails are
// enabled for that scope. Previously the repo line and the trails check resolved
// origin independently, costing two git subprocesses per `entire agent-help` run.
func agentHelpRepoContext(ctx context.Context) (repoLine string, trailsEnabled bool) {
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil {
		return "", false
	}
	if scope.Forge != "" && scope.Owner != "" && scope.Repo != "" {
		repoLine = scope.RepoKey
	}
	return repoLine, cachedTrailsEnablementForScope(ctx, scope, time.Now()) == trailEnablementCacheEnabled
}

// runAgentHelp resolves args to a command node and renders it. It is pure (no
// git / IO): the caller passes the already-resolved repoLine and trailsEnabled.
func runAgentHelp(rootCmd *cobra.Command, args []string, repoLine string, asJSON, trailsEnabled bool) (string, error) {
	target := rootCmd
	for _, name := range args {
		child := agentHelpFindChild(target, name)
		if child == nil {
			return "", fmt.Errorf("unknown command %q; run `entire agent-help` for the list of commands", name)
		}
		// Keep the specific, actionable message for the trail-gated case.
		if !trailsEnabled && child.Annotations[agentHelpRequiresTrailsAnnotation] == agentHelpAnnotationEnabled {
			return "", fmt.Errorf("`%s` is unavailable: trails are not enabled for this repo", child.Name())
		}
		// The drillable surface must match the advertised surface: a name an agent
		// guesses for a command the listing intentionally hides (help, deprecated,
		// or plain-hidden infra like `hooks`) reads as nonexistent here too.
		if !isAgentHelpAdvertised(child, trailsEnabled) {
			return "", fmt.Errorf("unknown command %q; run `entire agent-help` for the list of commands", name)
		}
		target = child
	}
	if asJSON {
		return renderAgentHelpJSON(rootCmd, target, repoLine, trailsEnabled)
	}
	if target == rootCmd {
		return renderAgentHelpTop(rootCmd, repoLine, trailsEnabled), nil
	}
	return renderAgentHelpCommand(target, repoLine, trailsEnabled), nil
}

// agentHelpFindChild finds a direct child of parent by name or alias. It
// includes hidden commands so an annotated one like trail resolves; the caller
// (runAgentHelp) then enforces isAgentHelpAdvertised, so the drillable surface
// matches the advertised one.
func agentHelpFindChild(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

type agentHelpFlagJSON struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

type agentHelpSubcommandJSON struct {
	Name  string `json:"name"`
	Short string `json:"short"`
}

type agentHelpJSON struct {
	Command     string                    `json:"command"`
	Short       string                    `json:"short,omitempty"`
	Long        string                    `json:"long,omitempty"`
	Repo        string                    `json:"repo,omitempty"`
	Flags       []agentHelpFlagJSON       `json:"flags,omitempty"`
	Subcommands []agentHelpSubcommandJSON `json:"subcommands,omitempty"`
}

// renderAgentHelpJSON renders the structured form of a command node.
func renderAgentHelpJSON(rootCmd, target *cobra.Command, repoLine string, trailsEnabled bool) (string, error) {
	doc := agentHelpJSON{
		Command: target.CommandPath(),
		Short:   target.Short,
		Long:    strings.TrimSpace(target.Long),
		Repo:    repoLine,
	}
	if target != rootCmd {
		collect := func(fs *flag.FlagSet) {
			fs.VisitAll(func(f *flag.Flag) {
				if f.Hidden {
					return
				}
				doc.Flags = append(doc.Flags, agentHelpFlagJSON{
					Name:      f.Name,
					Shorthand: f.Shorthand,
					Type:      f.Value.Type(),
					Default:   f.DefValue,
					Usage:     f.Usage,
				})
			})
		}
		collect(target.LocalFlags())
		collect(target.InheritedFlags())
	}
	for _, sub := range agentHelpCommands(target, trailsEnabled) {
		doc.Subcommands = append(doc.Subcommands, agentHelpSubcommandJSON{Name: sub.Name(), Short: sub.Short})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal agent-help json: %w", err)
	}
	return string(b) + "\n", nil
}

// isAgentHelpAdvertised reports whether sub should be exposed to agents through
// agent-help. The listing AND the drill-down resolver share this predicate so
// the drillable surface always matches the advertised surface: visible commands
// plus hidden commands that opt in via agentHelpAnnotation, minus the help
// command, deprecated commands, and (when trails are disabled) trail-gated ones.
func isAgentHelpAdvertised(sub *cobra.Command, trailsEnabled bool) bool {
	if sub.Name() == "help" || sub.Name() == "agent-help" || sub.Deprecated != "" {
		return false
	}
	if sub.Hidden && sub.Annotations[agentHelpAnnotation] != agentHelpAnnotationEnabled {
		return false
	}
	if !trailsEnabled && sub.Annotations[agentHelpRequiresTrailsAnnotation] == agentHelpAnnotationEnabled {
		return false
	}
	return true
}

// agentHelpCommands returns the child commands to advertise to agents.
func agentHelpCommands(parent *cobra.Command, trailsEnabled bool) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range parent.Commands() {
		if isAgentHelpAdvertised(sub, trailsEnabled) {
			out = append(out, sub)
		}
	}
	return out
}

// agentHelpRepoBlock formats the auto-detected repo line, degrading gracefully
// when the repo can't be resolved (no origin / detached HEAD) rather than
// implying a repo that isn't there.
func agentHelpRepoBlock(repoLine string) string {
	// Defense-in-depth: this line is emitted as plain text into agent context and
	// the user's terminal. A crafted origin URL's control characters (newline,
	// ANSI escapes) are rejected upstream in gitremote, but never let one reach
	// this plain-text sink — degrade to the not-detectable message instead.
	if strings.TrimSpace(repoLine) == "" || strings.IndexFunc(repoLine, unicode.IsControl) >= 0 {
		return "Current repo: not auto-detectable here (no origin remote / detached HEAD); pass --repo explicitly.\n"
	}
	return "Current repo: " + repoLine + "  (auto-detected from origin; pass --repo only for a DIFFERENT repo)\n"
}

// renderAgentHelpCommand renders one resolved command node for an agent: its
// path + Short, its Long description, the auto-detected repo line, its live flag
// usages (hidden flags are skipped by cobra), and its advertised subcommands.
func renderAgentHelpCommand(cmd *cobra.Command, repoLine string, trailsEnabled bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", cmd.CommandPath(), cmd.Short)
	if long := strings.TrimSpace(cmd.Long); long != "" && long != strings.TrimSpace(cmd.Short) {
		b.WriteString(long)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(agentHelpRepoBlock(repoLine))

	// LocalFlags()/InheritedFlags() trigger cobra's persistent-flag merge (plain
	// Flags() does not without Execute) and skip hidden flags in FlagUsages.
	if usages := strings.TrimRight(cmd.LocalFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nFlags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}
	if usages := strings.TrimRight(cmd.InheritedFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nInherited flags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}

	if subs := agentHelpCommands(cmd, trailsEnabled); len(subs) > 0 {
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		fmt.Fprintf(&b, "\nSubcommands: %s\n", strings.Join(names, " · "))
		fmt.Fprintf(&b, "Next:  entire agent-help %s <subcommand>\n", strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "))
	}
	return b.String()
}

// renderAgentHelpTop renders the top-level agent-facing overview: the curated
// intro + rule, the auto-detected repo line, and a live map of the advertised
// commands (their Short help), ending with the drill-down pointer.
func renderAgentHelpTop(rootCmd *cobra.Command, repoLine string, trailsEnabled bool) string {
	var b strings.Builder
	b.WriteString(agentHelpOverview)
	b.WriteString("\n\n")
	b.WriteString(agentHelpRepoBlock(repoLine))
	b.WriteString("\nWhen to use entire:\n")
	for _, sub := range agentHelpCommands(rootCmd, trailsEnabled) {
		fmt.Fprintf(&b, "  %-12s %s\n", sub.Name(), sub.Short)
	}
	// Use an example command that is actually advertised here (trail is gated on
	// trails being enabled), so we never point at a command the agent can't use.
	example := "checkpoint"
	if trailsEnabled {
		example = "trail"
	}
	fmt.Fprintf(&b, "\nDrill in for exact, currently-installed flags:  entire agent-help <command>  (e.g. entire agent-help %s)\n", example)
	b.WriteString("Add --json for structured output.\n")
	return b.String()
}
