package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
)

// unattributedSentence is the one sentence both the report line and the
// prompt title use, so they cannot drift.
func unattributedSentence(a unattributedAuthor) string {
	verb := "are"
	if a.Count == 1 {
		verb = "is"
	}
	return fmt.Sprintf("%d %s in this repo %s authored by %s, which isn't linked to any account.", a.Count, pluralize("commit", a.Count), verb, a.Email)
}

// unattributedPromptDeps is the test seam for runUnattributedAuthorsCheck:
// every network/prompt edge is a func field, matching the pattern used by
// unattributedAuthorsDeps and identityProfileDependencies.
type unattributedPromptDeps struct {
	canPrompt    func() bool
	confirm      func(ctx context.Context, w io.Writer, title string) (bool, error)
	profileLabel func(ctx context.Context) string // account email, else "@handle", else "your account"
	declare      func(ctx context.Context, email, repoID string) error
	invalidate   func(ctx context.Context)
}

// unattributedNetworkTimeout bounds every network round trip this check
// makes: detection's own cell fetch (passed into
// defaultUnattributedAuthorsDeps) and, once the user has said yes, the
// prompt-time /me lookup and the DeclareAlias call in
// defaultUnattributedPromptDeps. Without it a hung core stalls doctor
// forever after confirmation, since neither the bare command context nor
// coreapi's http.Client carries a deadline of its own.
const unattributedNetworkTimeout = 5 * time.Second

// checkUnattributedAuthors is doctor's COR-1289 check: commits in this repo
// that Entire indexed under a reserved-host address the current OS user
// synthesized (you@Your-Laptop.local), offered for linking to the logged-in
// account. First networked check in doctor. It has no error return on
// purpose: every failure degrades to one line (spec: "must never cause doctor
// to fail"). --force does not auto-declare — see the Long text.
func checkUnattributedAuthors(cmd *cobra.Command) {
	ctx := cmd.Context()
	out := detectUnattributedAuthors(ctx, defaultUnattributedAuthorsDeps(unattributedNetworkTimeout))
	runUnattributedAuthorsCheck(ctx, cmd.OutOrStdout(), out, defaultUnattributedPromptDeps())
}

// runUnattributedAuthorsCheck renders in doctor's own vocabulary: a
// "Label: STATUS" header (✓ only for the healthy OK case, matching
// checkDisconnectedMetadata and friends — DISCONNECTED/UNLINKED/etc. carry no
// symbol), two-space detail lines, "  Fix: …" for an unapplied remedy, and
// "  ✓ Fixed: …" for one just applied.
func runUnattributedAuthorsCheck(ctx context.Context, w io.Writer, out detectionOutcome, p unattributedPromptDeps) {
	if len(out.Candidates) == 0 {
		return
	}
	if !out.LoggedIn {
		n := len(out.Candidates)
		// Not pluralize(): it is a naive s+"s" and would print "addresss".
		// pluralize("commit", n) below is fine.
		noun, looks, them := "addresses", "look", "them"
		if n == 1 {
			noun, looks, them = "address", "looks", "it"
		}
		fmt.Fprintln(w, "Unattributed authors: NOT CHECKED (not logged in)")
		fmt.Fprintf(w, "  %d author %s here %s like yours (%s).\n", n, noun, looks, strings.Join(out.Candidates, ", "))
		fmt.Fprintf(w, "  Fix: run `entire login`, then `entire doctor` again to check %s against Entire.\n", them)
		return
	}
	if out.Skipped != "" {
		fmt.Fprintf(w, "Unattributed authors: SKIPPED (%s)\n", out.Skipped)
		return
	}
	if len(out.Authors) == 0 {
		fmt.Fprintln(w, "✓ Unattributed authors: OK")
		return
	}
	fmt.Fprintln(w, "Unattributed authors: UNLINKED")
	for _, a := range out.Authors {
		fmt.Fprintln(w, "  "+unattributedSentence(a))
	}
	if !p.canPrompt() {
		fmt.Fprintln(w, "  Fix: run `entire doctor` in a terminal to link them.")
		return
	}
	label := p.profileLabel(ctx)
	for _, a := range out.Authors {
		ok, err := p.confirm(ctx, w, unattributedSentence(a)+" Link them to "+label+"?")
		if err != nil {
			logging.Debug(ctx, "doctor: unattributed authors: prompt failed; skipping", "error", err)
			return
		}
		if !ok {
			continue
		}
		err = p.declare(ctx, a.Email, out.RepoID)
		// `exhaustive` is enabled, so every aliasErr must appear in a case.
		switch aliasErrKind(err) {
		case aliasErrNone:
			p.invalidate(ctx)
			fmt.Fprintf(w, "  ✓ Fixed: linked %d %s by %s to %s\n", a.Count, pluralize("commit", a.Count), a.Email, label)
		case aliasErrNotAvailable:
			fmt.Fprintln(w, "  Linking isn't available on this Entire yet.")
			return // every further declare would fail the same way
		case aliasErrConflict:
			fmt.Fprintf(w, "  %s is already linked to another account.\n", a.Email)
		case aliasErrNotFound, aliasErrOther: // typed 404 cannot happen on declare; rendered the same if it did
			fmt.Fprintf(w, "  Could not link %s: %s\n", a.Email, apiErrText(err))
		}
	}
}

// aliasErr classifies a DeclareAlias / ReleaseAlias error so both surfaces
// tell "not declared" from "core has the flag off".
type aliasErr int

const (
	aliasErrNone         aliasErr = iota
	aliasErrNotAvailable          // unregistered route: text/plain 404 → ogen decode error carrying "(code 404)"
	aliasErrNotFound              // typed 404: not a declared alias (release only)
	aliasErrConflict              // typed 409: owned by another account / account deleting
	aliasErrOther
)

// aliasErrKind: a typed *coreapi.ErrorModelStatusCode carries the real status;
// an untyped error containing "code 404" is the decode failure core's mux
// produces for an UNREGISTERED route (static catch-all, text/plain body),
// which is what the flag-off state looks like from here. Same trap
// isKeychainTokenRejected handles for 401. Once the flag is on, declare never
// 404s and release's 404 is always typed, so the split is unambiguous.
func aliasErrKind(err error) aliasErr {
	if err == nil {
		return aliasErrNone
	}
	var typed *coreapi.ErrorModelStatusCode
	if errors.As(err, &typed) {
		switch typed.StatusCode {
		case http.StatusNotFound:
			return aliasErrNotFound
		case http.StatusConflict:
			return aliasErrConflict
		default:
			return aliasErrOther
		}
	}
	if strings.Contains(err.Error(), "code 404") {
		return aliasErrNotAvailable
	}
	return aliasErrOther
}

// apiErrText prefers the control plane's problem detail (coreapi.APIError)
// and falls back to err.Error().
func apiErrText(err error) string {
	if msg := coreapi.APIError(err); msg != "" {
		return msg
	}
	return err.Error()
}

func defaultUnattributedPromptDeps() unattributedPromptDeps {
	return unattributedPromptDeps{
		canPrompt: interactive.CanPromptInteractively,
		confirm:   confirmDoctorFix,
		profileLabel: func(ctx context.Context) string {
			// One /me round trip at prompt time, bounded so a hung core can't
			// stall doctor after the user already said yes. Passing
			// allowInsecure: false means resolveEntireIdentityProfile enforces
			// api.RequireSecureURL, so an http:// dev core falls back to "your
			// account" — expected, not a bug.
			nctx, cancel := context.WithTimeout(ctx, unattributedNetworkTimeout)
			defer cancel()
			res, err := resolveEntireIdentityProfile(nctx, defaultIdentityProfileDependencies(false))
			if err != nil || res.profile == nil {
				return "your account"
			}
			if e := strings.TrimSpace(res.profile.Email); e != "" {
				return e
			}
			if h := strings.TrimSpace(res.profile.Handle); h != "" {
				return "@" + h
			}
			return "your account"
		},
		declare: func(ctx context.Context, email, repoID string) error {
			nctx, cancel := context.WithTimeout(ctx, unattributedNetworkTimeout)
			defer cancel()
			c, err := coreapi.New()
			if err != nil {
				return fmt.Errorf("core client: %w", err)
			}
			// Returned bare on purpose: aliasErrKind and coreapi.APIError need the
			// raw ogen error. No nolint: wrapcheck does not flag it because
			// internal/coreapi is in wrapcheck's ignore-package-globs
			// (.golangci.yaml), so a //nolint:wrapcheck here would be unused and
			// fail nolintlint. Same shape as removeMirror (repo_mirror.go).
			_, err = c.DeclareAlias(nctx, &coreapi.DeclareAliasInputBody{Email: email, RepoId: repoID})
			return err
		},
		invalidate: invalidateUnattributedAuthorsCacheForRepo,
	}
}
