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

// revokeCurrentFunc revokes the CLI's current token server-side. The
// implementation resolves its own data-API bearer (same audience-
// matching rule as sessionLister); callers don't pass the keyring
// entry through.
type revokeCurrentFunc func(ctx context.Context) error

// clearContextFunc removes the active contexts.json context (and its
// keyring token) so logout actually logs out under the contexts model.
// Injected so logout stays unit-testable without touching the real
// config dir.
type clearContextFunc func() error

func newLogoutCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var all bool
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
			outW, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := runLogout(cmd.Context(), outW, errW,
				auth.NewContextStore(), defaultRevokeCurrentSession, defaultRevokeAllSessions,
				auth.RemoveCurrentContext, api.AuthBaseURL(), all); err != nil {
				return err
			}
			promoteNextLogin(outW, errW)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Also revoke every session on the active core server-side, not just the active one")
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

func defaultRevokeCurrentSession(ctx context.Context) error {
	token, err := resolveAuthHostToken(ctx)
	if err != nil {
		return err
	}
	return newSessionsClient(token).RevokeCurrentSession(ctx) //nolint:wrapcheck // RevokeCurrentSession already wraps with action context
}

// defaultRevokeAllSessions revokes every active login session on the active
// core (the `entire logout --all` path). It resolves a data-API bearer once,
// lists the user's sessions, and deletes each by id. Best-effort across
// sessions: it attempts them all and returns the first failure, so one stuck
// session doesn't strand the rest. Cross-core revoke is out of scope — these
// endpoints target api.AuthBaseURL()'s core only.
func defaultRevokeAllSessions(ctx context.Context) error {
	token, err := resolveAuthHostToken(ctx)
	if err != nil {
		return err
	}
	client := newSessionsClient(token)
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	var firstErr error
	for _, s := range sessions {
		if err := client.RevokeSession(ctx, s.ID); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("revoke session %s: %w", s.ID, err)
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
