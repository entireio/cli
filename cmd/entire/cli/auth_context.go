package cli

import (
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

// newAuthUseCmd switches the active login context, by name or by picking one
// from the saved contexts.
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
		Use:   "use [context]",
		Short: "Switch the active login context",
		Long: "Switch the active login context.\n\n" +
			"With no argument this lists the saved contexts and asks which to switch\n" +
			"to; pass a name to switch without being asked. One context is active at\n" +
			"a time.\n\n" +
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
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeContextNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			if len(args) == 1 {
				name = args[0]
			} else {
				chosen, err := selectContextToUse(cmd)
				if err != nil {
					return err
				}
				if chosen == "" {
					// Nothing to switch to (none saved) or the user cancelled;
					// selectContextToUse has already said which.
					return nil
				}
				name = chosen
			}
			if err := auth.SetCurrentContext(name); err != nil {
				return err //nolint:wrapcheck // already a user-facing message
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Now using context %q.\n", name)
			return nil
		},
	}
}

// selectContextToUse asks which saved context to switch to. It returns the
// chosen name, or "" when there is nothing to switch to and the reason has
// already been written for the user.
//
// The candidates and the "(active)" marker come from StoredContexts, not
// Contexts: `use` writes current_context, so the stored pointer is what it
// replaces, and resolving the effective identity instead would fail outright on
// a dangling `--context`/$ENTIRE_CONTEXT — one of the situations someone runs
// this command to get out of.
func selectContextToUse(cmd *cobra.Command) (string, error) {
	all, current, err := auth.StoredContexts()
	if err != nil {
		return "", err //nolint:wrapcheck // already a user-facing message
	}

	// A nameless entry can only come from a hand-edited or corrupted
	// contexts.json; it is not selectable, since SetCurrentContext looks a
	// context up by name.
	named := make([]*contexts.Context, 0, len(all))
	for _, c := range all {
		if c != nil && c.Name != "" {
			named = append(named, c)
		}
	}

	switch len(named) {
	case 0:
		fmt.Fprintln(cmd.OutOrStdout(), "No login contexts. Run 'entire login' to authenticate.")
		return "", nil
	case 1:
		// One saved login is the only answer a picker could give.
		return named[0].Name, nil
	}

	if !interactive.CanPromptInteractively() {
		return "", fmt.Errorf("%d login contexts saved; name one, e.g. `entire auth use %s` (list them with `entire auth contexts`): %s",
			len(named), named[0].Name, strings.Join(contextNames(named), ", "))
	}

	header, options := contextPickerTable(named, current)

	// Start the cursor on the context being replaced.
	selected := current
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Switch the active login context").
				Description("One context is active at a time; it supplies the identity for every authenticated operation.\n" + header).
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(cmd.Context()); err != nil {
		// handleFormCancellation prints "Switch cancelled." and returns nil for a
		// Ctrl+C / cancelled-context abort; a real form error propagates.
		if cerr := handleFormCancellation(cmd.ErrOrStderr(), "Switch", err); cerr != nil {
			return "", cerr
		}
		return "", nil
	}
	if selected == "" {
		// The form succeeded but handed back nothing that was on offer. Nothing
		// has been printed here, so this must NOT be a SilentError — main.go
		// suppresses those and the command would exit non-zero with no message.
		return "", fmt.Errorf("no context selected from the %d offered", len(named))
	}
	return selected, nil
}

// contextPickerTable lays the saved contexts out as the picker's rows: the
// same table `entire auth contexts` prints, built by contextTableLines, with
// each row valued by context name (which is what SetCurrentContext takes).
//
// The header is returned separately because it is not selectable: it goes in
// the field description, above the options, indented by selectOptionIndent so
// the columns line up with the option text rather than with huh's cursor.
//
// Rows carry no styling. huh renders each through its selected/unselected
// option style, and a color embedded here would fight it — so the styles go in
// unset, which authTableStyles already treats as "render plain text".
func contextPickerTable(all []*contexts.Context, current string) (string, []huh.Option[string]) {
	lines := contextTableLines(authTableStyles{}, all, current)

	options := make([]huh.Option[string], 0, len(all))
	for i, c := range all {
		options = append(options, huh.NewOption(lines[i+1], c.Name))
	}
	return selectOptionIndent + lines[0], options
}

// contextNames lists the context names, for the no-terminal error that points
// at the positional form.
func contextNames(all []*contexts.Context) []string {
	out := make([]string, 0, len(all))
	for _, c := range all {
		out = append(out, c.Name)
	}
	return out
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

// activeContextMarker flags the context in use. It sits in a trailing column
// in both the listing and the picker, rather than as a leading "*": huh draws
// its cursor in the leading column and the cursor *moves*, so a marker there
// would make whichever row you are sitting on read as the current one. The
// listing follows the picker so the two render identically, and so the word
// matches the one shell completion already appends.
const activeContextMarker = "(active)"

// contextTableLines lays the saved contexts out in aligned CONTEXT / HANDLE /
// LOGIN SERVER columns, with activeContextMarker in a trailing column on the
// context in use, and returns the header line followed by one line per
// context.
//
// One function because `entire auth contexts` and the `entire auth use` picker
// print the same table; they differ only in styling, and sty carries that. An
// unset authTableStyles renders every cell plain, which is what the picker
// passes.
func contextTableLines(sty authTableStyles, all []*contexts.Context, current string) []string {
	header := []string{
		sty.render(sty.header, "CONTEXT"),
		sty.render(sty.header, "HANDLE"),
		sty.render(sty.header, "LOGIN SERVER"),
		"", // the marker column heads itself
	}

	rows := make([][]string, 0, len(all))
	for _, c := range all {
		marker := ""
		name := sty.render(sty.value, c.Name)
		if c.Name == current {
			marker = sty.render(sty.id, activeContextMarker)
			name = sty.render(sty.name, c.Name)
		}
		rows = append(rows, []string{
			name,
			sty.render(sty.value, orDash(c.Handle)),
			sty.render(sty.value, orDash(c.CoreURL)),
			marker,
		})
	}

	return alignTableLines(header, rows)
}

// renderContextsTable prints the saved login contexts as a styled, aligned
// table with column headers. Purely local data — no network, no timestamps —
// so it reuses the auth-table styles but only the header/name/value/accent
// slots.
func renderContextsTable(w io.Writer, all []*contexts.Context, current string) {
	for _, line := range contextTableLines(newAuthTableStyles(w), all, current) {
		fmt.Fprintln(w, line)
	}
}
