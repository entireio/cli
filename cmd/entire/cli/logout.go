package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

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
// matching rule as authTokenLister); callers don't pass the keyring
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
			"By default this removes the active login only. Other saved logins (contexts)\n" +
			"remain and can still authenticate `git clone entire://…` against clusters they\n" +
			"are bound to. Use --all to remove every saved login and its cluster bindings.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			outW, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()
			clearFn := auth.RemoveCurrentContext
			if all {
				clearFn = func() error { _, err := auth.RemoveAllContexts(); return err } //nolint:wrapcheck // RemoveAllContexts already returns a contextual error
			}
			if err := runLogout(cmd.Context(), outW, errW,
				auth.NewContextStore(), defaultRevokeCurrentToken, clearFn, api.AuthBaseURL()); err != nil {
				return err
			}
			if !all {
				noteRemainingLogins(errW)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Remove all saved logins (contexts) and their cluster bindings, not just the active one")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

// noteRemainingLogins warns when other saved contexts survive a default
// logout — they can still authenticate clone/push against clusters bound to
// them, so "Logged out." alone would overstate the result.
func noteRemainingLogins(errW io.Writer) {
	all, _, err := auth.Contexts()
	if err != nil || len(all) == 0 {
		return
	}
	names := make([]string, 0, len(all))
	for _, c := range all {
		names = append(names, c.Name)
	}
	fmt.Fprintf(errW,
		"Note: %d other saved login(s) remain and can still authenticate clones: %s\n"+
			"Run `entire logout --all` to remove them, or `entire auth use <context>` to switch.\n",
		len(all), strings.Join(names, ", "))
}

func defaultRevokeCurrentToken(ctx context.Context) error {
	token, err := resolveDataAPIToken(ctx)
	if err != nil {
		return err
	}
	return newAPITokensClient(token).RevokeCurrentToken(ctx) //nolint:wrapcheck // RevokeCurrentToken already wraps with action context
}

func runLogout(ctx context.Context, outW, errW io.Writer, store tokenStore, revoke revokeCurrentFunc, clearContext clearContextFunc, baseURL string) error {
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
			fmt.Fprintf(errW, "Warning: server-side token revocation failed: %v\n", err)
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
