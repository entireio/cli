package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// releaseDeps is the test seam for runReleaseAlias, matching the shape of
// unattributedPromptDeps: every network/auth edge is a func field.
type releaseDeps struct {
	loggedIn   func(ctx context.Context) bool
	release    func(ctx context.Context, email string) error // email already normalized
	invalidate func(ctx context.Context)
}

// runReleaseAlias is `entire doctor --release <address>`: an explicit user
// action, so unlike the check (checkUnattributedAuthors) it prints its
// failure and exits non-zero (SilentError — the printed line is the message).
// Ordering mirrors the declare path in runUnattributedAuthorsCheck: logged in
// → reserved-host (checked against the raw input, since reservedHostEmail
// normalizes internally) → normalize once → release → classify.
func runReleaseAlias(ctx context.Context, out, errw io.Writer, address string, deps releaseDeps) error {
	fail := func(line string, err error) error {
		fmt.Fprintln(errw, line)
		return NewSilentError(err)
	}
	if !deps.loggedIn(ctx) {
		return fail("Run `entire login` first", errors.New("not logged in"))
	}
	if !reservedHostEmail(address) {
		return fail("Only reserved-host addresses (.local, .localdomain, …) can be linked or released", errors.New("address is not a reserved host"))
	}
	norm := normalizeDeclaredEmail(address)
	err := deps.release(ctx, norm)
	// `exhaustive` is enabled, so every aliasErr must appear in a case.
	switch aliasErrKind(err) {
	case aliasErrNone:
		deps.invalidate(ctx)
		fmt.Fprintf(out, "✓ Released %s. Commits already linked stay linked; new ones will not be. Another account may now claim this address.\n", norm)
		return nil
	case aliasErrNotFound:
		return fail(norm+" is not a linked author address on your account", err)
	case aliasErrNotAvailable:
		return fail("Linking isn't available on this Entire yet.", err)
	case aliasErrConflict, aliasErrOther:
		return fail("Could not release "+norm+": "+apiErrText(err), err)
	}
	return nil // unreachable; every aliasErr value is handled above
}

// defaultReleaseDeps wires runReleaseAlias to the real login state, core
// client, and detection cache. Mirrors defaultUnattributedPromptDeps.
func defaultReleaseDeps() releaseDeps {
	return releaseDeps{
		loggedIn: func(ctx context.Context) bool {
			// Same rule as unattributedAuthorsLoggedIn, with its own log
			// prefix so a context error reads as "release alias: ..." here
			// rather than "unattributed authors: ...".
			return loggedInVia(ctx, "release alias", os.LookupEnv, auth.ActiveContext)
		},
		release: func(ctx context.Context, email string) error {
			c, err := coreapi.New()
			if err != nil {
				return fmt.Errorf("core client: %w", err)
			}
			nctx, cancel := context.WithTimeout(ctx, unattributedNetworkTimeout)
			defer cancel()
			// Bare on purpose: aliasErrKind / coreapi.APIError need the raw ogen
			// error; internal/coreapi is in wrapcheck's ignore list, so no lint
			// directive is needed here (see the matching comment in
			// defaultUnattributedPromptDeps.declare).
			return c.ReleaseAlias(nctx, coreapi.ReleaseAliasParams{Email: email})
		},
		invalidate: invalidateUnattributedAuthorsCacheForRepo,
	}
}
