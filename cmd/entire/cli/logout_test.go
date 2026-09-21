package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

const testLogoutToken = "tok123"

func TestLogoutCmd_IsRegistered(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	found := false
	for _, c := range root.Commands() {
		if c.Use == "logout" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("logout command not registered on root")
	}
}

func TestLogoutCmd_RejectsAllContextsFlag(t *testing.T) {
	t.Parallel()

	cmd := newLogoutCmd()
	cmd.SetArgs([]string{"--all-contexts"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --all-contexts") {
		t.Fatalf("Execute() = %v, want unknown-flag error", err)
	}
}

// --context selects one identity, so on a command that ends all of them it
// is refused rather than ignored. Driven through the real root command,
// which is where the persistent flag lives. Process-global env, keyring and
// context override, so no t.Parallel().
func TestLogoutCmd_RejectsContextFlag(t *testing.T) {
	recA, recB := seedTwoContexts(t)
	t.Chdir(t.TempDir()) // no .entire here: keep the root pre-run off the real repo
	all, _, err := auth.StoredContexts()
	if err != nil || len(all) != 2 {
		t.Fatalf("StoredContexts = %v, %v; want 2 seeded", all, err)
	}
	// Restores whatever the override was before parsing sets it.
	contexts.SetFlagOverrideForTest(t, "")

	root := NewRootCmd()
	root.SetArgs([]string{"logout", "--context", all[0].Name})
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)

	if err := root.Execute(); !errors.Is(err, errContextFlagOnLogout) {
		t.Fatalf("Execute() = %v, want the --context refusal", err)
	}
	if recA.deleteCLI != 0 || recB.deleteCLI != 0 {
		t.Errorf("no session should be revoked, got A=%d B=%d", recA.deleteCLI, recB.deleteCLI)
	}
	if left, _, err := auth.StoredContexts(); err != nil || len(left) != 2 {
		t.Fatalf("StoredContexts = %v, %v; want both logins untouched", left, err)
	}
}

// makeLogoutContexts returns a fixed context list.
func makeLogoutContexts(cs ...*contexts.Context) contextsProvider {
	return func() ([]*contexts.Context, string, error) { return cs, "", nil }
}

// freshBearer returns a refreshed bearer for every context.
func freshBearer() func(context.Context, *contexts.Context) (bearer, error) {
	return func(context.Context, *contexts.Context) (bearer, error) { return bearer{token: testLogoutToken}, nil }
}

// unitDeps wires runLogout with no TLS check.
func unitDeps(list contextsProvider, tokenFor func(context.Context, *contexts.Context) (bearer, error), revoke revokeTargetFunc, remove func(string) error) logoutDeps {
	return logoutDeps{listContexts: list, tokenForContext: tokenFor, revoke: revoke, removeContext: remove}
}

func TestRunLogout_RevokesAndRemovesEachContext(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	tokens := map[string]string{"eu": "tok-eu", "us": "tok-us"}
	tokenFor := func(_ context.Context, c *contexts.Context) (bearer, error) {
		return bearer{token: tokens[c.Name]}, nil
	}

	revoked := map[string]string{} // coreURL -> token
	revoke := func(_ context.Context, coreURL, token string) error {
		revoked[coreURL] = token
		return nil
	}
	removed := map[string]bool{}
	remove := func(name string) error { removed[name] = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, tokenFor, revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if revoked["https://eu.auth.entire.io"] != "tok-eu" || revoked["https://us.auth.entire.io"] != "tok-us" {
		t.Fatalf("each context's session should be revoked against its own core+token, got %v", revoked)
	}
	if !removed["eu"] || !removed["us"] {
		t.Fatalf("both contexts should be removed locally, got %v", removed)
	}
	if !strings.Contains(out.String(), "Logged out of 2 saved login(s).") {
		t.Fatalf("stdout = %q, want count of 2", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

// A hand-edited contexts.json can hold null or nameless entries.
func TestRunLogout_SkipsMalformedEntries(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		nil,
		&contexts.Context{CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	revoked := 0
	revoke := func(context.Context, string, string) error { revoked++; return nil }
	var removed []string
	remove := func(name string) error { removed = append(removed, name); return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revoked != 1 || len(removed) != 1 || removed[0] != "us" {
		t.Fatalf("revoked=%d removed=%v, want only the named login handled", revoked, removed)
	}
	if !strings.Contains(out.String(), "Logged out of 1 saved login(s).") {
		t.Fatalf("stdout = %q, want count of 1", out.String())
	}
	if n := strings.Count(errOut.String(), "skipped a malformed saved login"); n != 2 {
		t.Fatalf("stderr = %q, want one warning per malformed entry", errOut.String())
	}
}

func TestRunLogout_NoContextsPrintsNotLoggedIn(t *testing.T) {
	t.Parallel()

	revoke := func(context.Context, string, string) error {
		t.Fatal("revoke should not run with no contexts")
		return nil
	}
	remove := func(string) error { t.Fatal("remove should not run with no contexts"); return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(makeLogoutContexts(), nil, revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "Not logged in.") {
		t.Fatalf("stdout = %q, want %q", out.String(), "Not logged in.")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

func TestRunLogout_ListFailureNamesTheFile(t *testing.T) {
	t.Parallel()

	provider := func() ([]*contexts.Context, string, error) {
		return nil, "", errors.New("parse contexts file: bad json")
	}
	deps := unitDeps(provider, nil, nil, nil)
	deps.contextsFile = "/home/u/.config/entire/contexts.json"

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "parse contexts file") {
		t.Fatalf("err = %v, want the list failure", err)
	}
	if !strings.Contains(errOut.String(), "/home/u/.config/entire/contexts.json") {
		t.Fatalf("stderr = %q, want a hint naming contexts.json", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

func TestRunLogout_RevokeFailureWarnsButContinues(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	revoke := func(_ context.Context, coreURL, _ string) error {
		if coreURL == "https://eu.auth.entire.io" {
			return errors.New("connection refused")
		}
		return nil
	}
	removed := map[string]bool{}
	remove := func(name string) error { removed[name] = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removed["eu"] || !removed["us"] {
		t.Fatalf("a server revoke failure must not strand local removal, got %v", removed)
	}
	if !strings.Contains(errOut.String(), `revocation failed for "eu"`) || !strings.Contains(errOut.String(), "connection refused") {
		t.Fatalf("stderr = %q, want a warning naming the failed context", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out of 2 saved login(s).") {
		t.Fatalf("stdout = %q, want count of 2 despite the warning", out.String())
	}
}

// A refreshed bearer that the server still rejects is not "already
// logged out": the session may be alive, so the user hears about it.
func TestRunLogout_UnauthorizedFreshBearerWarns(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	revoke := func(context.Context, string, string) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removed {
		t.Fatal("context should still be removed locally")
	}
	if !strings.Contains(errOut.String(), `revocation failed for "eu"`) {
		t.Fatalf("stderr = %q, want a warning: a fresh bearer got 401", errOut.String())
	}
}

// A family the server no longer has is the desired end state.
func TestRunLogout_NotFoundRevokeIsSilent(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	revoke := func(context.Context, string, string) error {
		return &api.HTTPError{StatusCode: http.StatusNotFound, Message: "not found"}
	}
	remove := func(string) error { return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty: the family is already gone", errOut.String())
	}
}

// Refresh failed, the stored token got 401: nothing proves the session
// ended, so warn and name the refresh failure.
func TestRunLogout_StaleBearerUnauthorizedWarns(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(context.Context, *contexts.Context) (bearer, error) {
		return bearer{token: testLogoutToken, stale: errors.New("dial tcp: connection refused")}, nil
	}
	revoke := func(context.Context, string, string) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, tokenFor, revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removed {
		t.Fatal("context should still be removed locally")
	}
	got := errOut.String()
	if !strings.Contains(got, "may still be active") || !strings.Contains(got, "connection refused") {
		t.Fatalf("stderr = %q, want a may-still-be-active warning carrying the refresh error", got)
	}
}

// The login server already declared the family dead during refresh, so
// the 401 on the stored token confirms the desired state.
func TestRunLogout_ReauthRequiredIsSilent(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(context.Context, *contexts.Context) (bearer, error) {
		return bearer{token: testLogoutToken, stale: fmt.Errorf("refresh: %w", auth.ErrReauthRequired)}, nil
	}
	revoke := func(context.Context, string, string) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}
	remove := func(string) error { return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, tokenFor, revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty: the server already ended this session", errOut.String())
	}
}

// Reauth-required excuses a 401 only. Any other revoke failure still
// means the session may be live.
func TestRunLogout_ReauthRequiredStillWarnsOnServerError(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(context.Context, *contexts.Context) (bearer, error) {
		return bearer{token: testLogoutToken, stale: fmt.Errorf("refresh: %w", auth.ErrReauthRequired)}, nil
	}
	revoke := func(context.Context, string, string) error {
		return &api.HTTPError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	}
	remove := func(string) error { return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, tokenFor, revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := errOut.String(); !strings.Contains(got, "revocation failed") || !strings.Contains(got, "boom") {
		t.Fatalf("stderr = %q, want a revocation-failed warning carrying the server error", got)
	}
}

func TestRunLogout_UnreadableTokenRemovesLocallyOnly(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(context.Context, *contexts.Context) (bearer, error) {
		return bearer{}, errors.New("keyring locked")
	}
	revokeCalled := false
	revoke := func(context.Context, string, string) error { revokeCalled = true; return nil }
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, tokenFor, revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revokeCalled {
		t.Error("revoke should be skipped when the token can't be read")
	}
	if !removed {
		t.Error("context should still be removed locally")
	}
	if !strings.Contains(errOut.String(), "removing locally only") {
		t.Fatalf("stderr = %q, want the locally-only warning", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out of 1 saved login(s).") {
		t.Fatalf("stdout = %q, want count of 1", out.String())
	}
}

func TestRunLogout_InsecureCoreSkipsRevoke(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "local", CoreURL: "http://insecure.example.com"})
	revokeCalled := false
	revoke := func(context.Context, string, string) error { revokeCalled = true; return nil }
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revokeCalled {
		t.Error("revoke should be skipped for a non-TLS core without --insecure-http-auth")
	}
	if !removed {
		t.Error("context should still be removed locally")
	}
	if !strings.Contains(errOut.String(), "skipping server-side revocation") {
		t.Fatalf("stderr = %q, want the insecure-skip warning", errOut.String())
	}
}

// A login server that accepts the connection and never answers must not
// hang the sweep: the per-login deadline fires and removal proceeds.
func TestRunLogout_HangingCoreHitsDeadline(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "hung", CoreURL: "https://hung.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	revoke := func(ctx context.Context, coreURL, _ string) error {
		if coreURL == "https://hung.auth.entire.io" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	removed := map[string]bool{}
	remove := func(name string) error { removed[name] = true; return nil }
	deps := unitDeps(provider, freshBearer(), revoke, remove)
	deps.loginTimeout = 50 * time.Millisecond

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removed["hung"] || !removed["us"] {
		t.Fatalf("both logins should be removed after the deadline, got %v", removed)
	}
	if !strings.Contains(errOut.String(), `revocation failed for "hung"`) || !strings.Contains(errOut.String(), "deadline exceeded") {
		t.Fatalf("stderr = %q, want a deadline warning for the hung login", errOut.String())
	}
}

// Ctrl-C before the sweep reaches a login: nothing is revoked and — the
// point of the check — nothing is deleted either. Removing locally without
// revoking would leave live sessions with no local record of them.
func TestRunLogout_CancelledBeforeSweepRemovesNothing(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "a", CoreURL: "https://a.auth.entire.io"},
		&contexts.Context{Name: "b", CoreURL: "https://b.auth.entire.io"},
		&contexts.Context{Name: "c", CoreURL: "https://c.auth.entire.io"},
	)
	revoke := func(context.Context, string, string) error {
		t.Error("revoke should not run once the sweep is cancelled")
		return nil
	}
	remove := func(name string) error {
		t.Errorf("removed %q after cancellation", name)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out, errOut bytes.Buffer
	err := runLogout(ctx, &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled so main.go re-raises the signal", err)
	}
	if !strings.Contains(errOut.String(), "3 saved login(s) still on this machine") {
		t.Fatalf("stderr = %q, want the count of untouched logins", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want no logged-out claim", out.String())
	}
}

// Ctrl-C during a revoke: the login it interrupted keeps its credentials
// (its session may well be alive), and the sweep does not walk on to the
// rest.
func TestRunLogout_CancelMidSweepStopsAndKeepsTheInterruptedLogin(t *testing.T) {
	t.Parallel()

	const bURL = "https://b.auth.entire.io"
	provider := makeLogoutContexts(
		&contexts.Context{Name: "a", CoreURL: "https://a.auth.entire.io"},
		&contexts.Context{Name: "b", CoreURL: bURL},
		&contexts.Context{Name: "c", CoreURL: "https://c.auth.entire.io"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var revoked []string
	revoke := func(_ context.Context, coreURL, _ string) error {
		revoked = append(revoked, coreURL)
		if coreURL == bURL {
			cancel() // the user hits Ctrl-C while b's revoke is in flight
			return context.Canceled
		}
		return nil
	}
	var removed []string
	remove := func(name string) error { removed = append(removed, name); return nil }

	var out, errOut bytes.Buffer
	err := runLogout(ctx, &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if len(removed) != 1 || removed[0] != "a" {
		t.Fatalf("removed = %v, want only the login whose revoke completed", removed)
	}
	if len(revoked) != 2 {
		t.Fatalf("revoked = %v, want the sweep to stop after the interrupted login", revoked)
	}
	if !strings.Contains(out.String(), "Logged out of 1 saved login(s).") {
		t.Fatalf("stdout = %q, want only the completed login counted", out.String())
	}
	if !strings.Contains(errOut.String(), "2 saved login(s) still on this machine") {
		t.Fatalf("stderr = %q, want b and c reported as still present", errOut.String())
	}
}

// Ctrl-C landing between a revoke and the local delete: whether the login
// is still removed turns on what the revoke proved. A session known to be
// over leaves nothing to protect, so stranding its credentials would just be
// a stale context; a revoke that proved nothing keeps them, because they are
// the only thing that can still end that session.
func TestRunLogout_CancelAfterRevokeRemovesOnlyConfirmedEndings(t *testing.T) {
	t.Parallel()

	unauthorized := &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	for _, tc := range []struct {
		name        string
		stale       error
		revokeErr   error
		wantRemoved bool
	}{
		{"revoke succeeded", nil, nil, true},
		{"family already gone", nil, &api.HTTPError{StatusCode: http.StatusNotFound, Message: "not found"}, true},
		{"server had already ended it", fmt.Errorf("refresh: %w", auth.ErrReauthRequired), unauthorized, true},
		{"revoke cut short", nil, context.Canceled, false},
		{"unrefreshable bearer rejected", errors.New("dial tcp: connection refused"), unauthorized, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const aURL = "https://a.auth.entire.io"
			provider := makeLogoutContexts(
				&contexts.Context{Name: "a", CoreURL: aURL},
				&contexts.Context{Name: "b", CoreURL: "https://b.auth.entire.io"},
			)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			tokenFor := func(context.Context, *contexts.Context) (bearer, error) {
				return bearer{token: testLogoutToken, stale: tc.stale}, nil
			}
			revoke := func(_ context.Context, coreURL, _ string) error {
				if coreURL == aURL {
					cancel() // Ctrl-C as a's revoke returns
					return tc.revokeErr
				}
				t.Errorf("revoke reached %q; the sweep should have stopped", coreURL)
				return nil
			}
			var removed []string
			remove := func(name string) error { removed = append(removed, name); return nil }

			var out, errOut bytes.Buffer
			err := runLogout(ctx, &out, &errOut, unitDeps(provider, tokenFor, revoke, remove))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the interrupt reported either way", err)
			}
			if tc.wantRemoved && (len(removed) != 1 || removed[0] != "a") {
				t.Fatalf("removed = %v, want a removed: its session is known to be over", removed)
			}
			if !tc.wantRemoved && len(removed) != 0 {
				t.Fatalf("removed = %v, want a kept: the revoke proved nothing", removed)
			}
			want := "2 saved login(s) still on this machine"
			if tc.wantRemoved {
				want = "1 saved login(s) still on this machine"
			}
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("stderr = %q, want %q", errOut.String(), want)
			}
		})
	}
}

// A removal failure and an interrupt in the same sweep are both reported:
// the interrupt must stay visible through errors.Is so main.go re-raises the
// signal rather than exiting 1 on the removal failure, and the count left
// behind must include the login that failed to go.
func TestRunLogout_RemoveFailureAndInterruptAreBothReported(t *testing.T) {
	t.Parallel()

	const bURL = "https://b.auth.entire.io"
	provider := makeLogoutContexts(
		&contexts.Context{Name: "a", CoreURL: "https://a.auth.entire.io"},
		&contexts.Context{Name: "b", CoreURL: bURL},
		&contexts.Context{Name: "c", CoreURL: "https://c.auth.entire.io"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	revoke := func(_ context.Context, coreURL, _ string) error {
		if coreURL == bURL {
			cancel() // Ctrl-C, one login after the failed removal
			return context.Canceled
		}
		return nil
	}
	remove := func(name string) error {
		if name == "a" {
			return errors.New("keyring locked")
		}
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(ctx, &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the interrupt to survive the removal failure", err)
	}
	if !strings.Contains(err.Error(), "failed to remove 1 saved login(s)") {
		t.Fatalf("err = %v, want the removal failure reported too", err)
	}
	// a stayed (removal failed), b and c were never removed.
	if !strings.Contains(errOut.String(), "3 saved login(s) still on this machine") {
		t.Fatalf("stderr = %q, want the failed removal counted as still present", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want no logged-out claim", out.String())
	}
}

func TestRunLogout_RemoveFailureWarnsAndFails(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	revoke := func(context.Context, string, string) error { return nil }
	remove := func(name string) error {
		if name == "eu" {
			return errors.New("keyring locked")
		}
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, unitDeps(provider, freshBearer(), revoke, remove))
	if err == nil || !strings.Contains(err.Error(), "failed to remove 1 saved login(s)") {
		t.Fatalf("err = %v, want the removal failure count", err)
	}
	if !strings.Contains(errOut.String(), `failed to remove saved login "eu"`) || !strings.Contains(errOut.String(), "keyring locked") {
		t.Fatalf("stderr = %q, want a warning naming the failed context", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out of 1 saved login(s).") {
		t.Fatalf("stdout = %q, want the one successful removal counted", out.String())
	}
}

// coreRecorder counts session-endpoint calls on a fake core.
type coreRecorder struct {
	mu            sync.Mutex
	refreshes     int
	listCount     int
	deleteCLI     int // bare collection DELETE: CLI sessions
	deleteAll     int // collection DELETE with scope=all
	deleteCurrent int
	deleteByID    []string
	bearers       []string // Authorization bearers seen on revoke calls

	// failRefresh makes /oauth/token answer 500.
	failRefresh bool
	// noRevokeAll makes the collection DELETE answer 405 (older core).
	noRevokeAll bool
	// revokeStatus overrides the bare collection and /current answers;
	// zero means 200.
	revokeStatus int
}

func (r *coreRecorder) snapshot() (list, current, byID int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCount, r.deleteCurrent, len(r.deleteByID)
}

// newCoreServer fakes entire-core's refresh and session endpoints.
func newCoreServer(t *testing.T) (*httptest.Server, *coreRecorder) {
	t.Helper()
	rec := &coreRecorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, coreAuthSessionsPath) && r.Method == http.MethodDelete {
			rec.bearers = append(rec.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			rec.refreshes++
			if rec.failRefresh {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fresh := makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, srv.URL, time.Now().Add(time.Hour).Unix()))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"entr_new","token_type":"Bearer","expires_in":3600}`, fresh)
		case r.Method == http.MethodGet && r.URL.Path == coreAuthSessionsPath:
			rec.listCount++
			fmt.Fprint(w, `{"tokens":[{"id":"s1"},{"id":"s2"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == coreAuthSessionsPath:
			all := r.URL.Query().Get("scope") == "all"
			if all {
				rec.deleteAll++
			} else {
				rec.deleteCLI++
			}
			switch {
			case rec.noRevokeAll:
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			case !all && rec.revokeStatus != 0:
				w.WriteHeader(rec.revokeStatus)
				return
			}
			fmt.Fprint(w, `{"success":true}`)
		case r.Method == http.MethodDelete && r.URL.Path == coreAuthSessionsPath+"/current":
			rec.deleteCurrent++
			if rec.revokeStatus != 0 {
				w.WriteHeader(rec.revokeStatus)
				return
			}
			fmt.Fprint(w, `{"success":true}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, coreAuthSessionsPath+"/"):
			rec.deleteByID = append(rec.deleteByID, strings.TrimPrefix(r.URL.Path, coreAuthSessionsPath+"/"))
			fmt.Fprint(w, `{"success":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// isolateLogoutState points config and keyring at temp dirs. ENTIRE_TOKEN is
// not neutralised here: TestMain isolates it by absence, and setting it blank
// instead means "set but blank", which ParseEnvToken rejects for every command
// a seeded context is handed to.
func isolateLogoutState(t *testing.T) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(contexts.EnvContextVar, "")
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
}

// seedLogin records a valid login for handle on core.
func seedLogin(t *testing.T, coreURL, handle string) {
	t.Helper()
	exp := time.Now().Add(time.Hour).Unix()
	jwt := makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":%q,"exp":%d}`, coreURL, handle, exp))
	if _, err := auth.RecordLoginContext(jwt, "", true); err != nil {
		t.Fatalf("seed login %s: %v", handle, err)
	}
}

// seedExpiredLogin records a login whose access token has expired,
// paired with refresh token "entr_old".
func seedExpiredLogin(t *testing.T, coreURL, handle string) {
	t.Helper()
	seedLogin(t, coreURL, handle)
	if _, err := auth.RecordLoginContext(
		makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":%q,"exp":%d}`, coreURL, handle, time.Now().Add(time.Hour).Unix())),
		"entr_old", true); err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}
	past := time.Now().Add(-time.Hour).Unix()
	expired := makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":%q,"exp":%d}`, coreURL, handle, past))
	svc := tokenstore.CoreKeyringService(coreURL)
	if err := tokenstore.Set(svc, handle, expired+tokenstore.TokenExpirationSeparator+strconv.FormatInt(past, 10)); err != nil {
		t.Fatalf("expire access token: %v", err)
	}
}

// seedTwoContexts records two logins on two fake cores.
func seedTwoContexts(t *testing.T) (recA, recB *coreRecorder) {
	t.Helper()
	isolateLogoutState(t)
	srvA, recA := newCoreServer(t)
	srvB, recB := newCoreServer(t)
	seedLogin(t, srvA.URL, "alice")
	seedLogin(t, srvB.URL, "bob")
	return recA, recB
}

// execLogout runs the cobra command against http cores.
func execLogout(t *testing.T, flags ...string) (stdout, stderr string) {
	t.Helper()
	cmd := newLogoutCmd()
	cmd.SetArgs(append([]string{"--insecure-http-auth"}, flags...))
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("logout %v: %v (stderr=%q)", flags, err, errOut.String())
	}
	return out.String(), errOut.String()
}

// assertNoContextsLeft fails if any login survived.
func assertNoContextsLeft(t *testing.T) {
	t.Helper()
	all, _, err := auth.StoredContexts()
	if err != nil {
		t.Fatalf("StoredContexts: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("want every context removed, %d left", len(all))
	}
}

// TestLogoutCommand_SweepsEveryContext runs the real cobra command against two
// fake cores. Process-global env + keyring backend, so no t.Parallel().
func TestLogoutCommand_SweepsEveryContext(t *testing.T) {
	t.Run("default: every CLI session on every core", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		out, errOut := execLogout(t)
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			if l, c, b := rec.snapshot(); rec.deleteCLI != 1 || rec.deleteAll != 0 || l != 0 || c != 0 || b != 0 {
				t.Errorf("context %s: want one bare collection DELETE, got cli=%d all=%d list=%d current=%d byID=%d", name, rec.deleteCLI, rec.deleteAll, l, c, b)
			}
		}
		if !strings.Contains(out, "Logged out of 2 saved login(s).") {
			t.Errorf("stdout = %q, want count of 2", out)
		}
		if errOut != "" {
			t.Errorf("stderr = %q, want empty", errOut)
		}
		assertNoContextsLeft(t)
	})

	t.Run("default: older core falls back to the current session", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		recA.noRevokeAll = true
		_, errOut := execLogout(t)
		if l, c, b := recA.snapshot(); recA.deleteCLI != 1 || l != 0 || c != 1 || b != 0 {
			t.Errorf("old core: want 405 then one /current revoke, got cli=%d list=%d current=%d byID=%d", recA.deleteCLI, l, c, b)
		}
		if _, c, _ := recB.snapshot(); recB.deleteCLI != 1 || c != 0 {
			t.Errorf("new core: want one bare collection DELETE only, got cli=%d current=%d", recB.deleteCLI, c)
		}
		if errOut != "" {
			t.Errorf("stderr = %q, want empty: the fallback is not a failure", errOut)
		}
		assertNoContextsLeft(t)
	})

	t.Run("--everywhere: one scope=all DELETE per core", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		execLogout(t, "--everywhere")
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			l, c, b := rec.snapshot()
			if rec.deleteAll != 1 || rec.deleteCLI != 0 || l != 0 || c != 0 || b != 0 {
				t.Errorf("context %s: want one scope=all DELETE only, got all=%d cli=%d list=%d current=%d byID=%d", name, rec.deleteAll, rec.deleteCLI, l, c, b)
			}
		}
		assertNoContextsLeft(t)
	})

	t.Run("--everywhere: older core falls back to list + delete", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		recA.noRevokeAll = true
		_, errOut := execLogout(t, "--everywhere")
		if l, c, b := recA.snapshot(); recA.deleteAll != 1 || l != 1 || c != 0 || b != 2 {
			t.Errorf("old core: want 405 then list + 2 by-id revokes, got all=%d list=%d current=%d byID=%d", recA.deleteAll, l, c, b)
		}
		if l, _, b := recB.snapshot(); recB.deleteAll != 1 || l != 0 || b != 0 {
			t.Errorf("new core: want one scope=all DELETE only, got all=%d list=%d byID=%d", recB.deleteAll, l, b)
		}
		if errOut != "" {
			t.Errorf("stderr = %q, want empty: the fallback is not a failure", errOut)
		}
		assertNoContextsLeft(t)
	})

	// $ENTIRE_CONTEXT is ambient — often exported for a whole shell — so it
	// is ignored rather than refused the way an explicit --context is
	// (TestLogoutCmd_RejectsContextFlag).
	t.Run("$ENTIRE_CONTEXT does not narrow the sweep", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		all, _, err := auth.StoredContexts()
		if err != nil || len(all) != 2 {
			t.Fatalf("StoredContexts = %v, %v; want 2 seeded", all, err)
		}
		t.Setenv(contexts.EnvContextVar, all[0].Name)
		execLogout(t)
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			if l, c, b := rec.snapshot(); rec.deleteCLI != 1 || l != 0 || c != 0 || b != 0 {
				t.Errorf("context %s: want one bare collection DELETE, got cli=%d list=%d current=%d byID=%d", name, rec.deleteCLI, l, c, b)
			}
		}
		assertNoContextsLeft(t)
	})

	t.Run("no contexts: not logged in", func(t *testing.T) {
		isolateLogoutState(t)
		if out, _ := execLogout(t); !strings.Contains(out, "Not logged in.") {
			t.Errorf("stdout = %q, want %q", out, "Not logged in.")
		}
	})

	// An env token is not a saved login and survives the sweep, which
	// `auth status` will then report; say so rather than surprise.
	t.Run("ENTIRE_TOKEN set: note that it still authenticates", func(t *testing.T) {
		seedTwoContexts(t)
		t.Setenv(auth.EnvTokenVar, "env-bearer")
		_, errOut := execLogout(t)
		if !strings.Contains(errOut, "Context provided by ENTIRE_TOKEN.") {
			t.Errorf("stderr = %q, want the ENTIRE_TOKEN note", errOut)
		}
		assertNoContextsLeft(t)
	})
}

// TestLogoutCommand_RefreshesBeforeRevoking drives the real bearer resolver:
// an expired access token is re-minted from the refresh token and the
// revoke carries the new bearer. Process-global state, so no t.Parallel().
func TestLogoutCommand_RefreshesBeforeRevoking(t *testing.T) {
	t.Run("expired token is refreshed, then revoked", func(t *testing.T) {
		isolateLogoutState(t)
		srv, rec := newCoreServer(t)
		seedExpiredLogin(t, srv.URL, "alice")

		_, errOut := execLogout(t)
		if rec.refreshes != 1 || rec.deleteCLI != 1 {
			t.Fatalf("want one refresh then one revoke, got refreshes=%d cli=%d", rec.refreshes, rec.deleteCLI)
		}
		if len(rec.bearers) != 1 || !strings.Contains(rec.bearers[0], ".") || rec.bearers[0] == "" {
			t.Fatalf("revoke bearers = %v, want the re-minted JWT", rec.bearers)
		}
		claims := strings.Split(rec.bearers[0], ".")
		if len(claims) != 3 {
			t.Fatalf("bearer %q is not a JWT", rec.bearers[0])
		}
		if errOut != "" {
			t.Errorf("stderr = %q, want empty", errOut)
		}
		assertNoContextsLeft(t)
	})

	t.Run("refresh fails and stored token is rejected: warn", func(t *testing.T) {
		isolateLogoutState(t)
		srv, rec := newCoreServer(t)
		rec.failRefresh = true
		rec.revokeStatus = http.StatusUnauthorized
		seedExpiredLogin(t, srv.URL, "alice")

		_, errOut := execLogout(t)
		if rec.refreshes < 1 || rec.deleteCLI != 1 {
			t.Fatalf("want a refresh attempt then one revoke, got refreshes=%d cli=%d", rec.refreshes, rec.deleteCLI)
		}
		if !strings.Contains(errOut, "may still be active") {
			t.Fatalf("stderr = %q, want a may-still-be-active warning", errOut)
		}
		assertNoContextsLeft(t)
	})

	t.Run("core down: warn and remove locally", func(t *testing.T) {
		isolateLogoutState(t)
		srv, _ := newCoreServer(t)
		seedExpiredLogin(t, srv.URL, "alice")
		srv.Close()

		out, errOut := execLogout(t)
		if !strings.Contains(errOut, "Warning:") {
			t.Fatalf("stderr = %q, want a warning about the unreachable core", errOut)
		}
		if !strings.Contains(out, "Logged out of 1 saved login(s).") {
			t.Fatalf("stdout = %q, want the login counted as removed", out)
		}
		assertNoContextsLeft(t)
	})
}
