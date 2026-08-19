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
		return renderDataAPIAuthError(ctx, errW, err)
	}
	return fn(ctx, client)
}

// renderDataAPIAuthError decides whether err should print or stay silent.
// Silence is reserved for the caller's own context actually firing (checked
// via ctx.Err(), not errors.Is(err, ...)): a resolver called from inside err's
// chain (e.g. resolveRepoCellTarget) runs under its own internal timeout, and
// the resulting wrapped error still satisfies errors.Is(err,
// context.DeadlineExceeded) even though the caller's context is perfectly
// live. Silencing on that alone would print nothing for a slow-but-reachable
// control plane — a worse outcome than the error it would otherwise show.
//
// The not-onboarded case is replaced rather than wrapped: it is the most common
// way a repo-scoped command fails, its raw chain is a stack of internal
// resolution steps the user can do nothing with, and every caller reaching here
// is scoped to the current repo, so the line needs no repo name.
func renderDataAPIAuthError(ctx context.Context, errW io.Writer, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return NewSilentError(err)
	}
	if errors.Is(err, auth.ErrNotLoggedIn) {
		fmt.Fprintln(errW, "Not logged in. Run 'entire login' to authenticate.")
		return NewSilentError(err)
	}
	if errors.Is(err, errRepoNotOnboarded) {
		fmt.Fprintln(errW, "This repository is not onboarded to Entire. Run 'entire repo mirror create' to onboard it.")
		return NewSilentError(err)
	}
	return err
}
