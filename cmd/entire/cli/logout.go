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

// tokenStore abstracts keyring access so commands that read or delete the
// stored bearer token can be unit-tested without hitting the real OS keyring.
// Used by logout and the auth subcommands.
type tokenStore interface {
	GetToken(baseURL string) (string, error)
	DeleteToken(baseURL string) error
}

// boundRevokeFunc revokes login session(s) server-side — either just the
// current session or every session on the core, depending on which the caller
// selected (--everywhere). The caller resolves the active context's core URL +
// bearer up-front and binds them into the closure, so the revocation hits the
// same core that `auth status` lists.
type boundRevokeFunc func(ctx context.Context) error

// clearContextFunc removes the active contexts.json context (and its
// keyring token) so logout actually logs out under the contexts model.
// Injected so logout stays unit-testable without touching the real
// config dir.
type clearContextFunc func() error

func newLogoutCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var everywhere bool
	var allContexts bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out of Entire",
		Long: "Log out of Entire.\n\n" +
			"By default this ends the active session only (server-side) and removes the\n" +
			"active login from this machine. Other saved logins (contexts) remain and can\n" +
			"still authenticate `git clone entire://…` against clusters fronted by their\n" +
			"login server.\n\n" +
			"Pass --everywhere to revoke every session on the active login server\n" +
			"(all your devices), not just the current one.\n\n" +
			"Pass --all-contexts to log out of every saved login (context) at once: each\n" +
			"context's session is revoked server-side and the login is removed from this\n" +
			"machine. Combine with --everywhere to revoke every session on every context's\n" +
			"login server.\n\n" +
			"Without --all-contexts, logging out promotes the next saved login (if any) to\n" +
			"active, so running `entire logout` repeatedly drains every saved login in turn.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			outW, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()

			// Pick the per-target revocation: just the current session, or
			// every session on that context's core when --everywhere is set.
			revokeForTarget := revokeCurrentAuthSession
			if everywhere {
				revokeForTarget = revokeAllAuthSessions
			}

			if allContexts {
				return runLogoutAll(cmd.Context(), outW, errW, auth.Contexts,
					auth.LoginTokenForContext, revokeForTarget, auth.RemoveContext,
					auth.NewContextStore(), api.AuthBaseURL(), insecureHTTPAuth)
			}

			// Revoke against the active context's core (matching what
			// `auth status` lists), not a static AuthBaseURL.
			target := resolveStatusTarget(auth.NewContextStore(), auth.Contexts, api.AuthBaseURL())
			if !insecureHTTPAuth {
				if err := api.RequireSecureURL(target.coreURL); err != nil {
					return fmt.Errorf("context login server URL check: %w", err)
				}
			}
			revoke := func(ctx context.Context) error {
				return revokeForTarget(ctx, target.coreURL, target.token)
			}
			if err := runLogout(cmd.Context(), outW, errW,
				auth.NewContextStore(), revoke,
				auth.RemoveCurrentContext, api.AuthBaseURL()); err != nil {
				return err
			}
			promoteNextLogin(outW, errW)
			return nil
		},
	}
	cmd.Flags().BoolVar(&everywhere, "everywhere", false, "Revoke every session server-side, not just the current one")
	cmd.Flags().BoolVar(&allContexts, "all-contexts", false, "Log out of every saved login (context), not just the active one")
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
	// context (incl. the session id), so return their errors verbatim.
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
// when --everywhere is set. Either way the local keyring entry and active
// context are removed, so the CLI reports logged-out even if the server call
// fails.
func runLogout(ctx context.Context, outW, errW io.Writer, store tokenStore, revoke boundRevokeFunc, clearContext clearContextFunc, baseURL string) error {
	token, err := store.GetToken(baseURL)
	if err != nil {
		// Fall through to the local delete: we still want the keyring entry
		// gone, even if we couldn't read it well enough to revoke server-side.
		fmt.Fprintf(errW, "Warning: failed to read token before revocation: %v\n", err)
	}
	if token != "" {
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

// revokeTargetFunc revokes sessions on a specific core. The two production
// implementations are revokeCurrentAuthSession (just the bearer's own session)
// and revokeAllAuthSessions (every session on that core); `logout --all-contexts` picks
// one based on --everywhere and applies it to each saved context's core.
type revokeTargetFunc func(ctx context.Context, coreURL, token string) error

// runLogoutAll drains every saved login. For each context it revokes the
// session(s) on that context's own core (using its own bearer) and removes
// the login locally, then clears the legacy keyring entry. Per-context
// failures warn but never abort the sweep — one stuck login can't strand the
// rest, and local removal always proceeds so the CLI ends fully logged out.
//
// Dependencies are injected so the sweep is unit-testable without the real
// keyring or config dir: listContexts (auth.Contexts), tokenForContext
// (auth.LoginTokenForContext), revoke (revokeCurrentAuthSession/revokeAllAuthSessions),
// and removeContext (auth.RemoveContext).
func runLogoutAll(ctx context.Context, outW, errW io.Writer,
	listContexts contextsProvider,
	tokenForContext func(*contexts.Context) (string, error),
	revoke revokeTargetFunc,
	removeContext func(name string) error,
	store tokenStore, baseURL string, insecureHTTPAuth bool,
) error {
	all, _, err := listContexts()
	if err != nil {
		return fmt.Errorf("list saved logins: %w", err)
	}

	removed := 0
	for _, c := range all {
		token, terr := tokenForContext(c)
		if terr != nil {
			// Can't read this context's bearer — skip the server revoke but
			// still drop it locally so it stops being reported as a login.
			fmt.Fprintf(errW, "Warning: couldn't read token for %q; removing locally only: %v\n", c.Name, terr)
			token = ""
		}
		if token != "" && c.CoreURL != "" && !insecureHTTPAuth {
			if serr := api.RequireSecureURL(c.CoreURL); serr != nil {
				// Never send a bearer over a non-TLS core; warn and skip the
				// server revoke, but still remove the login locally.
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
			continue
		}
		removed++
	}

	// Clear the legacy keyring entry too so a pre-contexts login doesn't
	// linger after a full logout. Best-effort: the contexts above are the
	// source of truth for the logged-out state.
	if derr := store.DeleteToken(baseURL); derr != nil {
		fmt.Fprintf(errW, "Warning: failed to remove legacy auth token: %v\n", derr)
	}

	if removed == 0 {
		fmt.Fprintln(outW, "No saved logins to remove.")
	} else {
		fmt.Fprintf(outW, "Logged out of %d saved login(s).\n", removed)
	}
	return nil
}
