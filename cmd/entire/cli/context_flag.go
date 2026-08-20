package cli

import (
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

// contextFlagValue applies --context to the process-wide selection as pflag
// parses it.
//
// Binding through a pflag.Value rather than a PersistentPreRun is deliberate:
// Cobra runs only the CLOSEST PersistentPreRun in the chain, and subtrees here
// define their own (agent_group.go, the per-agent hooks commands), so a root hook
// would be silently skipped for exactly the commands that inherited the flag.
// Set() runs during flag parsing — before any PreRun, before RunE, and before
// anything resolves a token — so there is no ordering to get wrong.
type contextFlagValue struct{ name string }

func (v *contextFlagValue) String() string { return v.name }
func (v *contextFlagValue) Type() string   { return "string" }

func (v *contextFlagValue) Set(name string) error {
	v.name = name
	contexts.SetFlagOverride(name)
	return nil
}

// addContextFlag registers --context as a persistent flag on the root command,
// so every subcommand that authenticates inherits it without per-command
// plumbing.
//
// It is global rather than per-command because it selects an identity, and every
// path that resolves one honours it (git, control plane, data API, cell routing,
// status, logout). Registering it only on the commands we think authenticate
// today is how it would drift out of sync: a new authenticating command would
// silently ignore it. The cost is that it also parses on commands with no
// identity to select, like `version` — harmless, and the same tradeoff kubectl
// makes with its own global --context.
func addContextFlag(cmd *cobra.Command) {
	// The back-quoted word is pflag's value placeholder, so this renders as
	// `--context name`. Any other back-quoted span here (e.g. around a command to
	// run) would be silently hijacked as the placeholder instead.
	cmd.PersistentFlags().Var(&contextFlagValue{}, "context",
		"Act as this saved login `name` for this command only, instead of the active context (entire auth contexts lists them)")
	if err := cmd.RegisterFlagCompletionFunc("context", completeContextFlag); err != nil {
		panic("register --context completion: " + err.Error())
	}
}

// completeContextFlag completes saved context names for --context. It reuses the
// same listing `auth use` completes against, so both offer the same names with
// the same descriptions.
func completeContextFlag(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// completeContextNames takes the positional args of `auth use <context>`; pass
	// none so it treats this as completing the first (and only) value.
	return completeContextNames(cmd, nil, toComplete)
}
