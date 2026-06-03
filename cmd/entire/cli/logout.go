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

// tokenStore abstracts keyring access so commands that read or delete the
// stored bearer token can be unit-tested without hitting the real OS keyring.
// Used by logout and the auth subcommands.
type tokenStore interface {
	GetToken(baseURL string) (string, error)
	DeleteToken(baseURL string) error
}

// revokeCurrentFunc revokes the CLI's current login session server-side.
// The caller resolves the active context's core URL + bearer up-front and
// binds them into the closure, so the revocation hits the same core that
// `auth status` lists.
type revokeCurrentFunc func(ctx context.Context) error

// clearContextFunc removes the active contexts.json context (and its
// keyring token) so logout actually logs out under the contexts model.
// Injected so logout stays unit-testable without touching the real
// config dir.
type clearContextFunc func() error

func newLogoutCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var everywhere bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out of Entire",
		Long: "Log out of Entire.\n\n" +
			"By default this ends the active session only (server-side) and removes the\n" +
			"active login from this machine. Other saved logins (contexts) remain and can\n" +
			"still authenticate `git clone entire://…` against clusters fronted by their\n" +
			"login server. Pass --all to additionally revoke every session on the active\n" +
			"core server-side.\n\n" +
			"After logging out, the next saved login (if any) becomes active, so running\n" +
			"`entire logout` repeatedly drains every saved login in turn.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			// Revoke against the active context's core (matching what
			// `auth status` lists), not a static AuthBaseURL.
			target := resolveStatusTarget(auth.NewContextStore(), auth.Contexts, api.AuthBaseURL())
			if !insecureHTTPAuth {
				if err := api.RequireSecureURL(target.coreURL); err != nil {
					return fmt.Errorf("context core URL check: %w", err)
				}
			}
			revokeCurrent := func(ctx context.Context) error {
				return revokeCurrentSession(ctx, target.coreURL, target.token)
			}
			revokeAll := func(ctx context.Context) error {
				return revokeAllSessions(ctx, target.coreURL, target.token)
			}
			outW, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := runLogout(cmd.Context(), outW, errW,
				auth.NewContextStore(), revokeCurrent, revokeAll,
				auth.RemoveCurrentContext, api.AuthBaseURL(), everywhere); err != nil {
				return err
			}
			promoteNextLogin(outW, errW)
			return nil
		},
	}
	cmd.Flags().BoolVar(&everywhere, "everywhere", false, "Revoke every session server-side, not just the current one")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

// promoteNextLogin makes the first remaining saved context active after a
// logout cleared the previous one. This is what lets `entire logout` drain
// every login when run repeatedly: each call ends the active login and
// promotes the next, until none remain. Best-effort and informational —
// logout already succeeded by the time we get here.
func promoteNextLogin(outW, errW io.Writer) {
	all, current, err := auth.Contexts()
	if err != nil || current != "" || len(all) == 0 {
		return
	}
	next := all[0].Name
	if err := auth.SetCurrentContext(next); err != nil {
		fmt.Fprintf(errW, "Note: %d saved login(s) remain; run `entire auth use <context>` to switch.\n", len(all))
		return
	}
	fmt.Fprintf(outW, "Now using %q (%d saved login(s) remain; run `entire logout` again to remove each).\n", next, len(all))
}

// revokeCurrentSession revokes the active session on coreURL (the family the
// bearer belongs to) — the default `entire logout`.
func revokeCurrentSession(ctx context.Context, coreURL, token string) error {
	return newSessionsClient(coreURL, token).RevokeCurrentSession(ctx) //nolint:wrapcheck // RevokeCurrentSession already wraps with action context
}

// revokeAllSessions revokes every active login session on coreURL (the
// `entire logout --everywhere` path): list the families, then delete each by id.
// Best-effort across sessions — it attempts them all and returns the first
// failure, so one stuck session doesn't strand the rest.
func revokeAllSessions(ctx context.Context, coreURL, token string) error {
	client := newSessionsClient(coreURL, token)
	// ListSessions and RevokeSession already wrap with their own action
	// context (incl. the session id), so return their errors verbatim.
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return err //nolint:wrapcheck // ListSessions already wraps with "list sessions"
	}
	var firstErr error
	for _, s := range sessions {
		if err := client.RevokeSession(ctx, s.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runLogout ends the user's login. revokeCurrent revokes just the active
// session; revokeAll (used when all is set) revokes every session on the
// active core. Either way the local keyring entry and active context are
// removed, so the CLI reports logged-out even if the server call fails.
func runLogout(ctx context.Context, outW, errW io.Writer, store tokenStore, revokeCurrent, revokeAll revokeCurrentFunc, clearContext clearContextFunc, baseURL string, all bool) error {
	token, err := store.GetToken(baseURL)
	if err != nil {
		// Fall through to the local delete: we still want the keyring entry
		// gone, even if we couldn't read it well enough to revoke server-side.
		fmt.Fprintf(errW, "Warning: failed to read token before revocation: %v\n", err)
	}
	if token != "" {
		revoke := revokeCurrent
		if all {
			revoke = revokeAll
		}
		if err := revoke(ctx); err != nil && !api.IsHTTPErrorStatus(err, http.StatusUnauthorized) {
			// Best-effort: a transient network error shouldn't block local
			// logout. A 401 means the token is already invalid server-side,
			// so the desired state is achieved — no warning needed.
			fmt.Fprintf(errW, "Warning: server-side session revocation failed: %v\n", err)
		}
	}

	if err := store.DeleteToken(baseURL); err != nil {
		return fmt.Errorf("remove auth token: %w", err)
	}

	// Remove the active context so the context-preferring readers no longer
	// report a login. Best-effort: the legacy entry is already gone above.
	if err := clearContext(); err != nil {
		fmt.Fprintf(errW, "Warning: failed to clear current context: %v\n", err)
	}

	fmt.Fprintln(outW, "Logged out.")
	return nil
}
