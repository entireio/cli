package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// runAuthenticatedDataAPI centralizes the auth gate for commands that must
// call the Entire data API as the current user. Keep intentionally anonymous
// flows (for example recap's server-rendered 401 path) out of this helper.
func runAuthenticatedDataAPI(ctx context.Context, errW io.Writer, insecureHTTP bool, fn func(context.Context, *api.Client) error) error {
	client, err := NewAuthenticatedAPIClient(ctx, insecureHTTP)
	if err != nil {
		return renderDataAPIAuthError(errW, err)
	}
	return fn(ctx, client)
}

func renderDataAPIAuthError(errW io.Writer, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewSilentError(err)
	}
	if errors.Is(err, auth.ErrNotLoggedIn) {
		fmt.Fprintln(errW, "Not logged in. Run 'entire login' to authenticate.")
		return NewSilentError(err)
	}
	return err
}
