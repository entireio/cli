package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// runAuthenticatedDataAPI centralizes the auth gate for commands that must
// call the Entire data API as the current user. Keep intentionally anonymous
// flows (for example recap's server-rendered 401 path) out of this helper.
func runAuthenticatedDataAPI(ctx context.Context, errW io.Writer, insecureHTTP bool, fn func(context.Context, *api.Client) error) error {
	client, err := NewAuthenticatedAPIClient(ctx, insecureHTTP)
	if err != nil {
		// No repo to name: this gate builds the generic data-API client, which
		// resolves no repo and so can never produce errRepoNotOnboarded.
		return renderDataAPIAuthError(ctx, errW, "", err)
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
// ownerRepo names the repo the failed call was scoped to, or "" when the
// caller has none. It is not optional decoration: --repo means the failing
// repo is often NOT the current clone, and renderRepoNotOnboarded returns a
// SilentError, so this is the only place the repo can still be named.
func renderDataAPIAuthError(ctx context.Context, errW io.Writer, ownerRepo string, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return NewSilentError(err)
	}
	if errors.Is(err, auth.ErrNotLoggedIn) {
		fmt.Fprintln(errW, "Not logged in. Run 'entire login' to authenticate.")
		return NewSilentError(err)
	}
	if rendered := renderRepoNotOnboarded(errW, ownerRepo, err); rendered != nil {
		return rendered
	}
	return err
}

// renderRepoNotOnboarded replaces an errRepoNotOnboarded chain with one
// actionable line and returns a SilentError, so the internal resolution steps
// never reach the user. It returns nil for every other error, so callers that
// do their own wrapping can chain it as a guard.
//
// Shared rather than inlined in renderDataAPIAuthError because the repo-scoped
// commands do not all funnel through that gate: `entire experts` and `entire
// checkpoint explain --repo` build their cell client directly and wrap the
// failure themselves, and without this they print the raw chain.
//
// The wording deliberately stops short of asserting a missing mirror.
// errRepoNotOnboarded covers three shapes and only one is really "not
// onboarded": zero rows in the repos index also means the caller cannot SEE
// the repo (where 'entire repo mirror create' would fail too), and a row whose
// primaries name no processing placement can be a repo mid-onboarding.
func renderRepoNotOnboarded(errW io.Writer, ownerRepo string, err error) error {
	if !errors.Is(err, errRepoNotOnboarded) {
		return nil
	}
	subject := strings.TrimSpace(ownerRepo)
	if subject == "" {
		subject = "This repository"
	}
	fmt.Fprintf(errW, "%s is not onboarded to Entire, or is not visible to your login. If it should be onboarded, run 'entire repo mirror create'.\n", subject)
	return NewSilentError(err)
}
