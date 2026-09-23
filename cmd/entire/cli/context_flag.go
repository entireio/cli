package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

// contextFlagName is the root's persistent identity selector. Named so the
// commands that read it back (logout refuses it) cannot drift from the
// registration below.
const contextFlagName = "context"

// contextFlagValue applies --context to the process-wide selection as pflag
// parses it.
//
// Binding through a pflag.Value rather than a PersistentPreRun is deliberate:
// Set() runs during flag parsing — before any PreRun, before RunE, and before
// anything resolves a token — so there is no ordering to get wrong. That
// ordering is the only reason: root.go sets cobra.EnableTraverseRunHooks, so a
// root PersistentPreRun is not shadowed by the subtrees that define their own
// (agent_group.go, the per-agent hooks commands) and would work too, just
// resolving the identity later than flag parsing does, for no gain.
type contextFlagValue struct{ name string }

func (v *contextFlagValue) String() string { return v.name }
func (v *contextFlagValue) Type() string   { return "string" }

// Export before recording the override: a failed export must not leave this
// process acting as one identity while its children act as another.
func (v *contextFlagValue) Set(name string) error {
	if err := exportContextToChildren(name); err != nil {
		return err
	}
	v.name = name
	contexts.SetFlagOverride(name)
	return nil
}

// inheritedContextEnv is ENTIRE_CONTEXT as this process received it, captured
// on the first export so a later blank --context can put it back. Without the
// snapshot `--context us --context ""` (an alias baking in the first, a wrapper
// appending the second) clears the in-process override but leaves `us` in the
// environment, where requestedContext picks it up as $ENTIRE_CONTEXT — the
// parent and every child act as a login the final flag value did not name, and
// any error blames a variable the user never set.
//
// The capture goes through a sync.Once rather than a `captured` bool. Nothing
// calls this concurrently — cobra parses flags once, on one goroutine, and only
// when --context was actually passed — but a check-then-act on a package
// variable is a data race the moment something does, and `go test -race`
// reports it at all three field accesses. Once is also the honest primitive for
// a one-shot snapshot. It does NOT make concurrent parsing meaningful: the
// environment and flagOverride are last-write-wins by construction, and the
// premise above — one invocation, one identity — is what actually rules
// concurrency out.
type inheritedEnv struct {
	once    sync.Once
	value   string
	present bool
}

var inheritedContextEnv inheritedEnv

// exportContextToChildren publishes the --context selection as ENTIRE_CONTEXT in
// this process's own environment, so every process it spawns inherits it.
//
// The in-process override alone does not reach a child, and several built-in
// commands spawn one that selects a login by itself: `repo clone` execs `git
// clone entire://…`, and git runs the `entire` remote helper as a separate
// process that resolves credentials from the saved contexts; `resume`,
// `explain`, `trail create` and checkpoint-policy fetch or push through the same
// helper whenever origin is an entire:// URL. Without the export the helper
// sees only the active context and, with several logins eligible for the
// cluster, fails with the ambiguity error the flag exists to avoid (COR-1630).
// ENTIRE_CONTEXT is the channel the helper honours for `ENTIRE_CONTEXT=… git
// push`, and it is set here rather than on each exec for the same reason the
// flag is global rather than per-command: a new spawn site would otherwise
// silently drop it. External plugins are outside this: main.go dispatches them
// before cobra parses anything, so they never reach Set and take
// `ENTIRE_CONTEXT=… entire <plugin>` instead.
//
// Mutating the process environment is deliberate and in scope: flagOverride is
// already process-global on the grounds that one CLI invocation acts as one
// identity, and this is that same identity, made visible to the children of
// that same invocation — including agents that `review` or `investigate`
// launch, whose own hooks and pushes then act as the flag's login for as long
// as they run. The flag outranks an inherited ENTIRE_CONTEXT in process
// (contexts.requestedContext), so overwriting it here keeps parent and children
// agreeing. A blank name restores the snapshot in inheritedContextEnv.
func exportContextToChildren(name string) error {
	inheritedContextEnv.once.Do(func() {
		inheritedContextEnv.value, inheritedContextEnv.present = os.LookupEnv(contexts.EnvContextVar)
	})
	name = strings.TrimSpace(name)
	switch {
	case name != "":
		return wrapExportErr(os.Setenv(contexts.EnvContextVar, name))
	case inheritedContextEnv.present:
		return wrapExportErr(os.Setenv(contexts.EnvContextVar, inheritedContextEnv.value))
	default:
		return wrapExportErr(os.Unsetenv(contexts.EnvContextVar))
	}
}

// wrapExportErr names the operation on a Setenv/Unsetenv failure. The failure
// is close to unreachable (a constant key, an argv value), but a silent
// continue would leave the parent acting as one identity and its children as
// another — the exact split the export exists to prevent — so it is an error.
func wrapExportErr(err error) error {
	if err != nil {
		return fmt.Errorf("export --context to child processes: %w", err)
	}
	return nil
}

// validateContextFlag fails early, in the parent and blaming the flag, when
// --context names no saved login. Commands that resolve a login themselves
// already produce that error; the ones that do not — `repo clone` handed a
// verbatim entire:// URL passes straight to git — would otherwise let the
// remote helper find out first and report `$ENTIRE_CONTEXT selected login
// context "typo"`, sending the user to look for a shell variable they never
// set. Only the flag is checked: an exported ENTIRE_CONTEXT is the user's own
// and is validated wherever it is consumed, as before. The gate is the flag's
// VALUE, not whether it was passed: a blank `--context ""` clears the override
// (SetFlagOverride treats whitespace as "clear"), so checking on presence would
// resolve straight through to $ENTIRE_CONTEXT and refuse the user's own
// variable — on every command, `version` included.
//
// On failure the export is undone. The name reached the environment during
// flag parsing, before this check could run, so refusing it without also
// retracting it would leave the process advertising a login it has just
// rejected. Nothing observes that today: cobra returns straight out of its
// pre-run loop on our error, so PersistentPostRun — and the analytics spawn
// inside it — never runs, and main.go prints and exits without starting
// anything else. The retraction is therefore a guard for a future caller on
// that path, not a live leak, which is also why the in-process override is
// left as it is: nothing resolves an identity again before the process exits.
func validateContextFlag(cmd *cobra.Command) error {
	f := cmd.Flags().Lookup(contextFlagName)
	if f == nil || strings.TrimSpace(f.Value.String()) == "" {
		return nil
	}
	if _, _, err := auth.Contexts(); err != nil {
		if restoreErr := exportContextToChildren(""); restoreErr != nil {
			return fmt.Errorf("%w (and %w)", err, restoreErr)
		}
		return err //nolint:wrapcheck // UnknownContextError is already a complete operator message
	}
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
	cmd.PersistentFlags().Var(&contextFlagValue{}, contextFlagName,
		"Act as this saved login `name` for this command only — including the git and agent processes it spawns — instead of the active context (entire auth contexts lists them)")
	if err := cmd.RegisterFlagCompletionFunc(contextFlagName, completeContextFlag); err != nil {
		panic("register --context completion: " + err.Error())
	}
}

// completeContextFlag completes saved context names for --context. It reuses the
// same listing `auth switch` completes against, so both offer the same names with
// the same descriptions.
func completeContextFlag(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// completeContextNames takes the positional args of `auth switch <context>`; pass
	// none so it treats this as completing the first (and only) value.
	return completeContextNames(cmd, nil, toComplete)
}
