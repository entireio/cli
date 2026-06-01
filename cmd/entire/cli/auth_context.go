package cli

import (
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/spf13/cobra"
)

// newAuthUseCmd switches the active login context.
//
// For `git clone entire://…` the active context is only a fallback: a
// cluster already bound to a context (cluster_contexts) keeps using that
// binding, and the active context is consulted only the first time a
// not-yet-bound cluster is resolved. Control-plane commands (auth status/
// list/revoke, org/project/repo/grant) currently target the configured auth
// host (ENTIRE_AUTH_BASE_URL / the default) regardless of the active
// context's core, so switching to a context on a *different* core does not
// yet retarget them — auth use warns when that's the case.
func newAuthUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <context>",
		Short: "Switch the active login context",
		Long: "Switch the active login context.\n\n" +
			"For `git clone entire://…` the active context is only a fallback: a cluster\n" +
			"already bound to a context keeps using that binding, so switching here affects\n" +
			"only clusters not yet bound (and same-core tie-breaking on first resolve).\n\n" +
			"Control-plane commands (auth status/list/revoke, org/project/repo/grant) still\n" +
			"target the configured auth host (ENTIRE_AUTH_BASE_URL / the default), so\n" +
			"switching to a context on a different core does not retarget them yet.",
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
			"Note: %q authenticates against %s, but control-plane commands still target %s — cross-core control-plane switching isn't supported yet.\n"+
				"For `git clone entire://…`, a cluster already bound to another context keeps using it; the active context applies only to clusters not yet bound.\n",
			name, c.CoreURL, authHost)
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
	for _, c := range all {
		marker := " "
		if c.Name == current {
			marker = "*"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%s\n", marker, c.Name, c.Handle, c.CoreURL)
	}
	return nil
}
