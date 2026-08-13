package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/spf13/cobra"
)

// boundRevokeFunc revokes login session(s) server-side — either just the
// current session or every session on the core, depending on which the caller
// selected (--everywhere). The caller resolves the current login's server URL +
// bearer up-front and binds them into the closure, so the revocation hits the
// same core that `auth status` lists.
type boundRevokeFunc func(ctx context.Context) error

// clearLoginFunc removes the current login and its credentials.
// Injected so logout stays unit-testable without touching the real
// config dir.
type clearLoginFunc func() error

func newLogoutCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var everywhere bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out of Entire",
		Long: "Log out of Entire.\n\n" +
			"By default this revokes this machine's session and removes the current login.\n\n" +
			"Pass --everywhere to revoke every session on the login server (all your\n" +
			"devices), then remove the current login from this machine.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			outW, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()

			// Pick the per-target revocation: just the current session, or
			// every session on the login server when --everywhere is set.
			revokeForTarget := revokeCurrentAuthSession
			if everywhere {
				revokeForTarget = revokeAllAuthSessions
			}

			// Revoke against the current login's server, matching what
			// `auth status` lists. The refreshing resolver means an
			// expired-but-refreshable session still yields a bearer that can
			// authenticate the revoke call.
			target, err := resolveStatusTarget(cmd.Context(), auth.CurrentLogin, auth.RefreshedLoginToken)
			if err != nil {
				return err
			}
			if target.coreURL == "" {
				fmt.Fprintln(outW, "Not logged in.")
				return nil
			}
			if !applyInsecureHTTPAuth(insecureHTTPAuth) {
				if err := api.RequireSecureURL(target.coreURL); err != nil {
					return fmt.Errorf("login server URL check: %w", err)
				}
			}
			revoke := func(ctx context.Context) error {
				return revokeForTarget(ctx, target.coreURL, target.token)
			}
			if err := runLogout(cmd.Context(), outW, errW,
				target.token, revoke, auth.RemoveCurrentLogin); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&everywhere, "everywhere", false, "Revoke all login sessions on all devices")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

// revokeCurrentAuthSession revokes the active session on coreURL (the family the
// bearer belongs to) — the default `entire logout`.
func revokeCurrentAuthSession(ctx context.Context, coreURL, token string) error {
	return newAuthSessionsClient(coreURL, token).RevokeCurrentAuthSession(ctx) //nolint:wrapcheck // RevokeCurrentAuthSession already wraps with action context
}

// revokeAllAuthSessions revokes every active login session on coreURL (the
// `entire logout --everywhere` path): list the families, then delete each by id.
// Best-effort across sessions — it attempts them all and returns the first
// failure, so one stuck session doesn't strand the rest.
func revokeAllAuthSessions(ctx context.Context, coreURL, token string) error {
	client := newAuthSessionsClient(coreURL, token)
	// ListAuthSessions and RevokeAuthSession already wrap with their own action
	// operation (including the session id), so return errors verbatim.
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

// runLogout ends the user's login. revoke is the caller-selected server-side
// revocation — just the active session, or every session on the active core
// when --everywhere is set. token is the resolved bearer for the revoke call
// (empty skips it). The current login and its credentials are removed
// either way, so the CLI reports logged-out even if the server call fails.
func runLogout(ctx context.Context, outW, errW io.Writer, token string, revoke boundRevokeFunc, clearLogin clearLoginFunc) error {
	if token != "" {
		if err := revoke(ctx); err != nil && !api.IsHTTPErrorStatus(err, http.StatusUnauthorized) {
			// Best-effort: a transient network error shouldn't block local
			// logout. A 401 means the token is already invalid server-side,
			// so the desired state is achieved — no warning needed.
			fmt.Fprintf(errW, "Warning: server-side session revocation failed: %v\n", err)
		}
	}

	if err := clearLogin(); err != nil {
		return fmt.Errorf("remove login: %w", err)
	}

	fmt.Fprintln(outW, "Logged out.")
	return nil
}
