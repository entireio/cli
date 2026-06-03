package cli

import (
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

// newAuthUseCmd switches the active login context.
//
// For `git clone entire://…` the active context is the preferred identity:
// it authenticates any cluster fronted by its login server, and switching
// here takes effect on the next operation (resolution recomputes the account
// every time). Control-plane commands (auth status/list/revoke, org/project/
// repo/grant) currently target the configured auth host (ENTIRE_AUTH_BASE_URL
// / the default) regardless of the active context's login server, so
// switching to a context on a *different* login server does not yet retarget
// them — auth use warns when that's the case.
func newAuthUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <context>",
		Short: "Switch the active login context",
		Long: "Switch the active login context.\n\n" +
			"For `git clone entire://…` the active context is the preferred identity: it\n" +
			"authenticates any cluster fronted by its login server, and the switch takes\n" +
			"effect on the next operation.\n\n" +
			"Control-plane commands (auth status/list/revoke, org/project/repo/grant) still\n" +
			"target the configured auth host (ENTIRE_AUTH_BASE_URL / the default), so\n" +
			"switching to a context on a different login server does not retarget them yet.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := auth.SetCurrentContext(args[0]); err != nil {
				return err //nolint:wrapcheck // already a user-facing message
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Now using context %q.\n", args[0])
			warnIfCrossCoreContext(cmd.ErrOrStderr(), args[0])
			return nil
		},
	}
}

// warnIfCrossCoreContext warns when the now-active context authenticates
// against a different core than the control plane targets. Clone resolves
// per-cluster and is unaffected, but auth status/list/revoke and the
// org/project/repo/grant commands still hit the configured auth host —
// switching cores there isn't wired up yet, so flag it rather than
// silently authenticate against the wrong host.
func warnIfCrossCoreContext(errW io.Writer, name string) {
	all, _, err := auth.Contexts()
	if err != nil {
		return
	}
	authHost := api.AuthBaseURL()
	for _, c := range all {
		if c.Name != name {
			continue
		}
		if c.CoreURL == "" || api.OriginOnly(c.CoreURL) == api.OriginOnly(authHost) {
			return
		}
		fmt.Fprintf(errW,
			"Note: %q authenticates against %s, but control-plane commands still target %s — switching the active context doesn't retarget control-plane commands yet.\n"+
				"For `git clone entire://…`, this context now authenticates any cluster fronted by %s.\n",
			name, c.CoreURL, authHost, c.CoreURL)
		return
	}
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
		sty.render(sty.header, "CORE URL"),
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
