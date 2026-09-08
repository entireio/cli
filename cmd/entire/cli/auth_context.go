package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

// newAuthUseCmd switches the active login context.
//
// The active context is the preferred identity for both `git clone entire://…`
// (it authenticates any cluster fronted by its login server) and the
// control-plane commands (auth status, org/project/repo/grant), which dial the
// context's core. Switching takes effect on the next operation; resolution
// recomputes every time. Activity/search/dispatch take their host from
// ENTIRE_API_BASE_URL; trail commands route to the repository's owning cell.
// All use the active identity.
func newAuthUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <context>",
		Short: "Switch the active login context",
		Long: "Switch the active login context.\n\n" +
			"The active context is the identity for every authenticated operation:\n" +
			"`git clone entire://…`, the control-plane commands (auth status,\n" +
			"org/project/repo/grant), and the data-API commands (activity, search,\n" +
			"trail, dispatch). The switch takes effect on the next operation.\n\n" +
			"This is persistent and machine-wide — it changes the identity for every\n" +
			"shell, worktree, and background git hook until you switch back. To act as\n" +
			"another login for a single command instead, pass --context, or set\n" +
			"ENTIRE_CONTEXT for one shell or one git operation:\n\n" +
			"  entire --context staging activity\n" +
			"  ENTIRE_CONTEXT=staging git push\n\n" +
			"Activity/search/dispatch take their host from ENTIRE_API_BASE_URL; trail\n" +
			"commands route to the repository's owning cell. The selected context\n" +
			"supplies the identity.",
		Args:              authUseArgs,
		ValidArgsFunction: completeContextNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := auth.SetCurrentContext(args[0]); err != nil {
				return err //nolint:wrapcheck // already a user-facing message
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Now using context %q.\n", args[0])
			return nil
		},
	}
}

// authUseArgs validates `auth use <context>`, and treats the one mistake this
// command's shape invites as its own case: `entire auth use --context NAME`.
//
// The two spellings mean nearly opposite things. `auth use` takes the context
// as a positional and writes it to contexts.json for every later command,
// while the global `--context` selects a login for the current invocation only
// — and on `auth use` it selects nothing at all, since switching the active
// context authenticates against nothing. So the flag reads like the obvious
// way to name a context and is not: cobra binds NAME to the flag, no
// positional is left, and cobra.ExactArgs(1) answers "accepts 1 arg(s),
// received 0" about an argument the user plainly did type. Nothing is
// switched, which the generic message also fails to say.
//
// The check is scoped to the zero-positional case on purpose. With a
// positional present the flag is a harmless no-op, and rejecting it would
// break anyone whose shell alias passes `--context` to every entire command.
func authUseArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		if f := cmd.Flags().Lookup("context"); f != nil && f.Changed {
			if name := strings.TrimSpace(f.Value.String()); name != "" {
				return fmt.Errorf("--context %s acts as that login for one command; it does not switch the active context.\n"+
					"To switch, name the context as an argument:\n\n  entire auth use %s", name, name)
			}
		}
	}
	// Cobra's arg-count message, returned verbatim: main.isPositionalArgError
	// keys on its "arg(s)" to print this command's usage alongside it.
	return cobra.ExactArgs(1)(cmd, args)
}

// completeContextNames is the ValidArgsFunction for commands taking a single
// <context> positional. It offers the stored context names, each annotated
// (shell-completion descriptions, after a tab) with handle, core URL, and an
// "(active)" marker for the current context. Errors are swallowed because
// completion runs on every TAB press; a failed read just yields no suggestions.
func completeContextNames(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		// <context> is a single positional; nothing to complete past it.
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	all, current, err := auth.Contexts()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	out := make([]string, 0, len(all))
	for _, c := range all {
		desc := c.Handle
		if c.CoreURL != "" {
			desc += " " + c.CoreURL
		}
		if c.Name == current {
			desc += " (active)"
		}
		out = append(out, c.Name+"\t"+desc)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// newAuthContextsCmd lists the stored login contexts and marks the active
// one. Purely local — it reads contexts.json, no network.
func newAuthContextsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "contexts",
		Short: "List stored login contexts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthContexts(cmd.OutOrStdout())
		},
	}
}

func runAuthContexts(w io.Writer) error {
	all, current, err := auth.Contexts()
	if err != nil {
		return err //nolint:wrapcheck // already a user-facing message
	}
	if len(all) == 0 {
		fmt.Fprintln(w, "No login contexts. Run 'entire login' to authenticate.")
		return nil
	}
	renderContextsTable(w, all, current)
	return nil
}

// renderContextsTable prints the saved login contexts as a styled, aligned
// table with column headers. The active context is flagged with "*" in the
// leading column. Purely local data — no network, no timestamps — so it
// reuses the auth-table styles but only the header/name/value/accent slots.
func renderContextsTable(w io.Writer, all []*contexts.Context, current string) {
	sty := newAuthTableStyles(w)

	header := []string{
		"", // active marker
		sty.render(sty.header, "CONTEXT"),
		sty.render(sty.header, "HANDLE"),
		sty.render(sty.header, "LOGIN SERVER"),
	}

	rows := make([][]string, 0, len(all))
	for _, c := range all {
		marker := " "
		name := sty.render(sty.value, c.Name)
		if c.Name == current {
			marker = sty.render(sty.id, "*")
			name = sty.render(sty.name, c.Name)
		}
		rows = append(rows, []string{
			marker,
			name,
			sty.render(sty.value, fallback(c.Handle, placeholderDash)),
			sty.render(sty.value, fallback(c.CoreURL, placeholderDash)),
		})
	}

	renderAlignedTable(w, header, rows)
}
