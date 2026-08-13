package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// Local auth reads, as package vars so the login heuristic's branching is
// testable without a real keyring or config dir. Production wiring is the real
// auth functions.
var (
	importCurrentLogin = auth.CurrentLogin
	importLoginToken   = auth.StoredLoginToken
)

// importLoggedIn reports whether there is an active login the imported history
// could sync under: an ENTIRE_TOKEN env token, or the current stored login with
// a token in the credential store. It is local-only and never makes a network
// call, so it is safe on the
// import path.
//
// This is a presence check, not a liveness check. StoredLoginToken returns a
// present-but-expired token without error, so a dead-but-not-removed login can
// still read as "logged in": confirming a token is actually usable needs a
// network refresh, which we deliberately avoid here (same reason the pre-push
// hook avoids ls-remote — no surprise auth prompts mid-command). That narrow
// residual false-negative (expired token → notice suppressed) is accepted to
// keep the check local and prompt-free; the common broken case this guards
// against — no context, or a context whose token was removed — is handled.
//
// Package var so tests can force the whole outcome (see #1773 review thread).
var importLoggedIn = func() bool {
	if os.Getenv(auth.EnvTokenVar) != "" {
		return true
	}
	login, err := importCurrentLogin()
	if err != nil || login == nil {
		return false
	}
	tok, tokenErr := importLoginToken(login)
	return tokenErr == nil && tok != ""
}

// warnIfImportNotSynced prints a one-time notice, when the user is not logged
// in, that imported agent history is stored locally only and will not appear in
// the Entire dashboard. It is a no-op when logged in or when nothing local was
// imported.
//
// Import writes read-only checkpoints to the local entire/checkpoints/v1 store
// and never syncs on its own; sync happens later via the git pre-push hook once
// logged in. Importing while logged out therefore succeeds locally but silently
// never reaches the dashboard — this notice surfaces that instead of leaving the
// user to discover an empty dashboard (see issue #1773).
func warnIfImportNotSynced(w io.Writer, importedLocalHistory bool) {
	if !importedLocalHistory || importLoggedIn() {
		return
	}
	fmt.Fprintln(w, "Note: you're not logged in, so this history was imported locally only and won't appear in your Entire dashboard.")
	fmt.Fprintln(w, "Log in with 'entire login' before importing to have your history synced.")
}
