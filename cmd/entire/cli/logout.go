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

// revokeCurrentFunc revokes the supplied token server-side. Mirrors the
// openURL injection pattern in login.go so tests can replace the real HTTP call.
type revokeCurrentFunc func(ctx context.Context, token string) error

func newLogoutCmd() *cobra.Command {
	var insecureHTTPAuth bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out of Entire",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireSecureBaseURL(insecureHTTPAuth); err != nil {
				return err
			}
			return runLogout(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				auth.NewStore(), defaultRevokeCurrentToken, api.BaseURL())
		},
	}
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	return cmd
}

func defaultRevokeCurrentToken(ctx context.Context, token string) error {
	return api.NewClient(token).RevokeCurrentToken(ctx) //nolint:wrapcheck // RevokeCurrentToken already wraps with action context
}

func runLogout(ctx context.Context, outW, errW io.Writer, store tokenStore, revoke revokeCurrentFunc, baseURL string) error {
	token, err := store.GetToken(baseURL)
	if err != nil {
		// Fall through to the local delete: we still want the keyring entry
		// gone, even if we couldn't read it well enough to revoke server-side.
		fmt.Fprintf(errW, "Warning: failed to read token before revocation: %v\n", err)
	}
	if token != "" {
		if err := revoke(ctx, token); err != nil && !api.IsHTTPErrorStatus(err, http.StatusUnauthorized) {
			// Best-effort: a transient network error shouldn't block local
			// logout. A 401 means the token is already invalid server-side,
			// so the desired state is achieved — no warning needed.
			fmt.Fprintf(errW, "Warning: server-side token revocation failed: %v\n", err)
		}
	}

	if err := store.DeleteToken(baseURL); err != nil {
		return fmt.Errorf("remove auth token: %w", err)
	}

	fmt.Fprintln(outW, "Logged out.")
	return nil
}
