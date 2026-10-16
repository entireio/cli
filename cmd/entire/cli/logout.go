package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/spf13/cobra"
)

// logoutLoginTimeout bounds one login's server calls.
//
// Refresh and revoke run against a login server the user may no longer be
// able to reach. api.Client sets no response deadline, so a core that
// accepts the connection and never answers would otherwise hang the sweep
// and leave every later login in place. Fifteen seconds covers a slow
// refresh plus a revoke; a login that exceeds it is removed locally with a
// warning, exactly like one whose core is down.
const logoutLoginTimeout = 15 * time.Second

func newLogoutCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var everywhere bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out of Entire",
		Long: "Log out of every saved login.\n\n" +
			"For each saved login, this ends every CLI session on that login server,\n" +
			"other machines included, and removes the login from this machine.\n" +
			"Nothing narrows it: an explicit --context is refused rather than\n" +
			"ignored, and $ENTIRE_CONTEXT is ignored. Browser and web sessions\n" +
			"stay signed in.\n\n" +
			"Pass --everywhere to also end browser and web sessions on each login\n" +
			"server. A browser is signed out on its next request; the web app once\n" +
			"its access token expires.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rejectContextFlag(cmd); err != nil {
				return err
			}
			deps := logoutDeps{
				listContexts:     auth.StoredContexts,
				tokenForContext:  loginBearer,
				revoke:           revokeCLIAuthSessions,
				removeContext:    auth.RemoveContext,
				insecureHTTPAuth: applyInsecureHTTPAuth(insecureHTTPAuth),
			}
			if everywhere {
				deps.revoke = revokeAllAuthSessions
			}
			// Only names the file in a hint; nothing is created.
			if dir, err := userdirs.ConfigDirChecked(); err == nil {
				if path, err := contexts.FilePath(dir); err == nil {
					deps.contextsFile = path
				}
			}
			err := runLogout(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), deps)
			// An env token is per-process, not a saved login,
			// so it still authenticates after the sweep.
			if os.Getenv(auth.EnvTokenVar) != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Context provided by %s.\n", auth.EnvTokenVar)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&everywhere, "everywhere", false, "Also end browser and web sessions")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

// errContextFlagOnLogout rejects `entire logout --context <name>`.
//
// --context is persistent on the root and shell-completes saved login names,
// so the command reads as "log out of that one" — the opposite of what the
// sweep does. Silently ignoring it would end the sessions the user did not
// name, so a narrowing request is an error rather than a surprise.
var errContextFlagOnLogout = errors.New(
	"--context cannot narrow logout: it signs out of every saved login. " +
		"Run `entire logout` on its own, or `entire auth contexts` to see what is saved")

// rejectContextFlag fails when --context was passed explicitly.
//
// $ENTIRE_CONTEXT is deliberately not rejected: it is ambient, commonly
// exported for a whole shell, and failing on it would leave those users
// unable to log out at all. The flag is a per-invocation request; the
// variable is inherited state.
func rejectContextFlag(cmd *cobra.Command) error {
	if cmd.Flags().Changed(contextFlagName) {
		return errContextFlagOnLogout
	}
	return nil
}

// revokeCLIAuthSessions ends every CLI session on coreURL.
//
// One collection DELETE when the login server supports it. An older
// server answers 404 or 405 and cannot tell CLI sessions from browser
// ones, so only the bearer's own session is ended there.
func revokeCLIAuthSessions(ctx context.Context, coreURL, token string) error {
	client := newAuthSessionsClient(coreURL, token)
	err := client.RevokeCLIAuthSessions(ctx)
	if err == nil || !endpointMissing(err) {
		return err //nolint:wrapcheck // RevokeCLIAuthSessions already wraps with "revoke cli sessions"
	}
	return client.RevokeCurrentAuthSession(ctx) //nolint:wrapcheck // RevokeCurrentAuthSession already wraps with action context
}

// revokeAllAuthSessions ends every session on coreURL, browser and web
// included.
//
// One collection DELETE with scope=all when the login server supports
// it. An older server answers 404 or 405 to that; then each session is
// ended by id, which ends the same set in more round trips.
func revokeAllAuthSessions(ctx context.Context, coreURL, token string) error {
	client := newAuthSessionsClient(coreURL, token)
	err := client.RevokeAllAuthSessions(ctx)
	if err == nil || !endpointMissing(err) {
		return err //nolint:wrapcheck // RevokeAllAuthSessions already wraps with "revoke all sessions"
	}
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

// endpointMissing reports a route the server lacks.
func endpointMissing(err error) bool {
	return api.IsHTTPErrorStatus(err, http.StatusNotFound) ||
		api.IsHTTPErrorStatus(err, http.StatusMethodNotAllowed)
}

// bearer is one login's revoke credential.
//
// stale is set when the refresh failed and token is the stored copy. A 401
// on that token then proves nothing about the session, so runLogout warns
// instead of treating it as already ended.
type bearer struct {
	token string
	stale error
}

// loginBearer returns c's bearer, refreshed when possible.
func loginBearer(ctx context.Context, c *contexts.Context) (bearer, error) {
	tok, refreshErr := auth.RefreshedLoginToken(ctx, c)
	if refreshErr == nil && tok != "" {
		return bearer{token: tok}, nil
	}
	stored, err := auth.LoginTokenForContext(c)
	if err != nil {
		return bearer{}, err //nolint:wrapcheck // names the context already
	}
	return bearer{token: stored, stale: refreshErr}, nil
}

// revokeTargetFunc revokes sessions on one core.
type revokeTargetFunc func(ctx context.Context, coreURL, token string) error

// logoutDeps is what runLogout needs, injected for tests.
type logoutDeps struct {
	listContexts    contextsProvider
	tokenForContext func(context.Context, *contexts.Context) (bearer, error)
	revoke          revokeTargetFunc
	removeContext   func(name string) error
	// insecureHTTPAuth skips the TLS check on each core.
	insecureHTTPAuth bool
	// contextsFile names contexts.json in the list-failure hint.
	contextsFile string
	// loginTimeout overrides logoutLoginTimeout; zero keeps it.
	loginTimeout time.Duration
}

func (d logoutDeps) contextsFileOrDefault() string {
	if d.contextsFile != "" {
		return d.contextsFile
	}
	return "contexts.json"
}

// runLogout sweeps every stored login.
//
// Each login gets its own deadline and is removed locally whatever its
// server calls did, so one unreachable login server never strands the
// rest. A cancelled ctx stops the sweep instead: the logins it never
// reached keep their credentials. A local removal failure or an interrupt
// makes the command fail.
func runLogout(ctx context.Context, outW, errW io.Writer, deps logoutDeps) error {
	all, _, err := deps.listContexts()
	if err != nil {
		if deps.contextsFile != "" {
			fmt.Fprintf(errW, "Check the saved logins file: %s\n", deps.contextsFile)
		}
		return fmt.Errorf("list saved logins: %w", err)
	}
	if len(all) == 0 {
		fmt.Fprintln(outW, "Not logged in.")
		return nil
	}
	timeout := deps.loginTimeout
	if timeout <= 0 {
		timeout = logoutLoginTimeout
	}

	removed, failed := 0, 0
	// interrupted is the cancellation that stopped the sweep, if any.
	var interrupted error
	for _, c := range all {
		// Stop rather than delete the rest of the credentials without
		// revoking them: a Ctrl-C would otherwise finish logout's
		// destructive half and skip its protective half, faster than not
		// interrupting at all.
		if err := ctx.Err(); err != nil {
			interrupted = err
			break
		}
		if c == nil || c.Name == "" {
			// Nothing to revoke or remove by name; the file needs a hand edit.
			fmt.Fprintf(errW, "Warning: skipped a malformed saved login; check %s\n", deps.contextsFileOrDefault())
			continue
		}
		ended := false
		func() {
			lctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			ended = revokeLogin(lctx, errW, deps, c)
		}()
		// Checked again after the revoke: a login whose revocation was cut
		// short keeps its local credentials, so the surviving session stays
		// visible to `auth status` and a re-run can finish the job. When the
		// revoke is confirmed ended there is nothing left to protect, and
		// stopping here would strand a context for a session that no longer
		// exists — so that removal is finished and the next iteration's
		// check ends the sweep. A per-login deadline does not cancel ctx, so
		// a hung login server still gets removed locally.
		if err := ctx.Err(); err != nil && !ended {
			interrupted = err
			break
		}
		if rerr := deps.removeContext(c.Name); rerr != nil {
			fmt.Fprintf(errW, "Warning: failed to remove saved login %q: %v\n", c.Name, rerr)
			failed++
			continue
		}
		removed++
	}

	if removed > 0 {
		fmt.Fprintf(outW, "Logged out of %d saved login(s).\n", removed)
	}

	var errs []error
	if failed > 0 {
		errs = append(errs, fmt.Errorf("failed to remove %d saved login(s)", failed))
	}
	if interrupted != nil {
		// Everything the sweep did not remove, not just the entries it never
		// reached: a login whose removal failed and a malformed entry it
		// skipped are both still on the machine and still a retry's problem.
		fmt.Fprintf(errW, "Interrupted: %d saved login(s) still on this machine; run `entire logout` again.\n", len(all)-removed)
		errs = append(errs, fmt.Errorf("logout interrupted: %w", interrupted))
	}
	// Joined rather than ranked: main.go re-raises the signal only while
	// errors.Is(err, context.Canceled) holds, so a removal failure earlier in
	// the sweep must not demote an interrupt to an ordinary exit 1 — and the
	// failure still has to reach the user.
	return errors.Join(errs...)
}

// revokeLogin ends c's session(s) server-side, warning on failure.
//
// It reports whether that session is known to be over: a clean revoke, a
// family the server no longer has, or one it had already declared dead.
// Every warning path reports false — the session may still be live, and
// then c's local credentials are the only thing that can still revoke it.
func revokeLogin(ctx context.Context, errW io.Writer, deps logoutDeps, c *contexts.Context) (ended bool) {
	if c.CoreURL == "" {
		// No login server recorded, so there is no session to strand.
		return true
	}
	b, err := deps.tokenForContext(ctx, c)
	if err != nil {
		fmt.Fprintf(errW, "Warning: couldn't read token for %q; removing locally only: %v\n", c.Name, err)
		return false
	}
	if b.token == "" {
		// Nothing to authenticate a revoke with; a later run may refresh one.
		return false
	}
	if !deps.insecureHTTPAuth {
		if err := api.RequireSecureURL(c.CoreURL); err != nil {
			fmt.Fprintf(errW, "Warning: skipping server-side revocation for %q: %v\n", c.Name, err)
			return false
		}
	}
	err = deps.revoke(ctx, c.CoreURL, b.token)
	switch {
	case err == nil:
		return true
	case api.IsHTTPErrorStatus(err, http.StatusNotFound):
		// The family is already gone: the desired state.
		return true
	case errors.Is(b.stale, auth.ErrReauthRequired) && api.IsHTTPErrorStatus(err, http.StatusUnauthorized):
		// The login server already declared this session dead.
		return true
	case b.stale != nil && api.IsHTTPErrorStatus(err, http.StatusUnauthorized):
		fmt.Fprintf(errW, "Warning: couldn't refresh the login for %q; its session on %s may still be active: %v\n", c.Name, c.CoreURL, b.stale)
		return false
	default:
		fmt.Fprintf(errW, "Warning: server-side session revocation failed for %q: %v\n", c.Name, err)
		return false
	}
}
