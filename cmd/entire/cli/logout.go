package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/spf13/cobra"
)

func newLogoutCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var everywhere bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out of Entire",
		Long: "Log out of every saved login.\n\n" +
			"For each saved login, this ends the CLI's own session on that login\n" +
			"server and removes the login from this machine. --context and\n" +
			"$ENTIRE_CONTEXT do not narrow it.\n\n" +
			"Pass --everywhere to also end every other session on each login server:\n" +
			"browser sessions and other machines' CLI sessions included. A browser is\n" +
			"signed out once its access token expires.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			revoke := revokeCurrentAuthSession
			if everywhere {
				revoke = revokeAllAuthSessions
			}
			return runLogout(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				auth.StoredContexts, auth.LoginTokenForContext, revoke, auth.RemoveContext,
				applyInsecureHTTPAuth(insecureHTTPAuth))
		},
	}
	cmd.Flags().BoolVar(&everywhere, "everywhere", false, "Also end browser and other machines' sessions")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

// revokeCurrentAuthSession ends the bearer's own session.
func revokeCurrentAuthSession(ctx context.Context, coreURL, token string) error {
	return newAuthSessionsClient(coreURL, token).RevokeCurrentAuthSession(ctx) //nolint:wrapcheck // RevokeCurrentAuthSession already wraps with action context
}

// revokeAllAuthSessions ends every session on coreURL.
func revokeAllAuthSessions(ctx context.Context, coreURL, token string) error {
	client := newAuthSessionsClient(coreURL, token)
	sessions, err := client.ListAuthSessions(ctx)
	if err != nil {
		return err //nolint:wrapcheck // ListAuthSessions already wraps with "list sessions"
	}
	var firstErr error
	for _, s := range sessions {
		if err := client.RevokeAuthSession(ctx, s.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// revokeTargetFunc revokes sessions on one core.
type revokeTargetFunc func(ctx context.Context, coreURL, token string) error

// runLogout sweeps every stored login.
func runLogout(ctx context.Context, outW, errW io.Writer,
	listContexts contextsProvider,
	tokenForContext func(*contexts.Context) (string, error),
	revoke revokeTargetFunc,
	removeContext func(name string) error,
	insecureHTTPAuth bool,
) error {
	all, _, err := listContexts()
	if err != nil {
		return fmt.Errorf("list saved logins: %w", err)
	}
	if len(all) == 0 {
		fmt.Fprintln(outW, "Not logged in.")
		return nil
	}

	removed, failed := 0, 0
	for _, c := range all {
		token, terr := tokenForContext(c)
		if terr != nil {
			fmt.Fprintf(errW, "Warning: couldn't read token for %q; removing locally only: %v\n", c.Name, terr)
			token = ""
		}
		if token != "" && c.CoreURL != "" && !insecureHTTPAuth {
			if serr := api.RequireSecureURL(c.CoreURL); serr != nil {
				fmt.Fprintf(errW, "Warning: skipping server-side revocation for %q: %v\n", c.Name, serr)
				token = ""
			}
		}
		if token != "" && c.CoreURL != "" {
			if rerr := revoke(ctx, c.CoreURL, token); rerr != nil && !api.IsHTTPErrorStatus(rerr, http.StatusUnauthorized) {
				fmt.Fprintf(errW, "Warning: server-side session revocation failed for %q: %v\n", c.Name, rerr)
			}
		}
		if rerr := removeContext(c.Name); rerr != nil {
			fmt.Fprintf(errW, "Warning: failed to remove saved login %q: %v\n", c.Name, rerr)
			failed++
			continue
		}
		removed++
	}

	if removed > 0 {
		fmt.Fprintf(outW, "Logged out of %d saved login(s).\n", removed)
	}
	if failed > 0 {
		return fmt.Errorf("failed to remove %d saved login(s)", failed)
	}
	return nil
}
