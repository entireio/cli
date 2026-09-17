package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// makeLogoutContexts returns a fixed context list.
func makeLogoutContexts(cs ...*contexts.Context) contextsProvider {
	return func() ([]*contexts.Context, string, error) { return cs, "", nil }
}

func TestRunLogout_RevokesAndRemovesEachContext(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	tokens := map[string]string{"eu": "tok-eu", "us": "tok-us"}
	tokenFor := func(_ context.Context, c *contexts.Context) (string, error) { return tokens[c.Name], nil }

	revoked := map[string]string{} // coreURL -> token
	revoke := func(_ context.Context, coreURL, token string) error {
		revoked[coreURL] = token
		return nil
	}
	removed := map[string]bool{}
	remove := func(name string) error { removed[name] = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, false); err != nil {
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
	tokenFor := func(context.Context, *contexts.Context) (string, error) { return testLogoutToken, nil }
	revoked := 0
	revoke := func(context.Context, string, string) error { revoked++; return nil }
	var removed []string
	remove := func(name string) error { removed = append(removed, name); return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revoked != 1 || len(removed) != 1 || removed[0] != "us" {
		t.Fatalf("revoked=%d removed=%v, want only the named login handled", revoked, removed)
	}
	if !strings.Contains(out.String(), "Logged out of 1 saved login(s).") {
		t.Fatalf("stdout = %q, want count of 1", out.String())
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
	if err := runLogout(context.Background(), &out, &errOut, makeLogoutContexts(), nil, revoke, remove, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "Not logged in.") {
		t.Fatalf("stdout = %q, want %q", out.String(), "Not logged in.")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

func TestRunLogout_ListFailureIsAnError(t *testing.T) {
	t.Parallel()

	provider := func() ([]*contexts.Context, string, error) { return nil, "", errors.New("corrupt contexts.json") }

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, provider, nil, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "corrupt contexts.json") {
		t.Fatalf("err = %v, want the list failure", err)
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
	tokenFor := func(context.Context, *contexts.Context) (string, error) { return testLogoutToken, nil }
	revoke := func(_ context.Context, coreURL, _ string) error {
		if coreURL == "https://eu.auth.entire.io" {
			return errors.New("connection refused")
		}
		return nil
	}
	removed := map[string]bool{}
	remove := func(name string) error { removed[name] = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, false); err != nil {
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

func TestRunLogout_UnauthorizedRevokeIsSilent(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(context.Context, *contexts.Context) (string, error) { return testLogoutToken, nil }
	revoke := func(context.Context, string, string) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}
	remove := func(string) error { return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty: an already-invalid token is the desired state", errOut.String())
	}
}

func TestRunLogout_UnreadableTokenRemovesLocallyOnly(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(context.Context, *contexts.Context) (string, error) { return "", errors.New("keyring locked") }
	revokeCalled := false
	revoke := func(context.Context, string, string) error { revokeCalled = true; return nil }
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, false); err != nil {
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
	tokenFor := func(context.Context, *contexts.Context) (string, error) { return testLogoutToken, nil }
	revokeCalled := false
	revoke := func(context.Context, string, string) error { revokeCalled = true; return nil }
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogout(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, false); err != nil {
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

func TestRunLogout_RemoveFailureWarnsAndFails(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	tokenFor := func(context.Context, *contexts.Context) (string, error) { return testLogoutToken, nil }
	revoke := func(context.Context, string, string) error { return nil }
	remove := func(name string) error {
		if name == "eu" {
			return errors.New("keyring locked")
		}
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, false)
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

// coreRecorder counts session-endpoint calls.
type coreRecorder struct {
	mu            sync.Mutex
	listCount     int
	deleteCurrent int
	deleteByID    []string
}

func (r *coreRecorder) snapshot() (list, current, byID int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCount, r.deleteCurrent, len(r.deleteByID)
}

// newCoreServer fakes entire-core's session endpoints.
func newCoreServer(t *testing.T) (*httptest.Server, *coreRecorder) {
	t.Helper()
	rec := &coreRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == coreAuthSessionsPath:
			rec.listCount++
			fmt.Fprint(w, `{"tokens":[{"id":"s1"},{"id":"s2"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == coreAuthSessionsPath+"/current":
			rec.deleteCurrent++
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, coreAuthSessionsPath+"/"):
			rec.deleteByID = append(rec.deleteByID, strings.TrimPrefix(r.URL.Path, coreAuthSessionsPath+"/"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// seedTwoContexts records two logins on two fake cores.
func seedTwoContexts(t *testing.T) (recA, recB *coreRecorder) {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Setenv(contexts.EnvContextVar, "")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	srvA, recA := newCoreServer(t)
	srvB, recB := newCoreServer(t)
	exp := time.Now().Add(time.Hour).Unix()
	if _, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, srvA.URL, exp)), "", true); err != nil {
		t.Fatalf("seed context A: %v", err)
	}
	if _, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"bob","exp":%d}`, srvB.URL, exp)), "", true); err != nil {
		t.Fatalf("seed context B: %v", err)
	}
	return recA, recB
}

// execLogout runs the cobra command against http cores.
func execLogout(t *testing.T, flags ...string) string {
	t.Helper()
	cmd := newLogoutCmd()
	cmd.SetArgs(append([]string{"--insecure-http-auth"}, flags...))
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("logout %v: %v (stderr=%q)", flags, err, errOut.String())
	}
	return out.String()
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
	t.Run("default: current session on every core", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		out := execLogout(t)
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			if l, c, b := rec.snapshot(); l != 0 || c != 1 || b != 0 {
				t.Errorf("context %s: want one current-session revoke, got list=%d current=%d byID=%d", name, l, c, b)
			}
		}
		if !strings.Contains(out, "Logged out of 2 saved login(s).") {
			t.Errorf("stdout = %q, want count of 2", out)
		}
		assertNoContextsLeft(t)
	})

	t.Run("--everywhere: every session on every core", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		execLogout(t, "--everywhere")
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			if l, c, b := rec.snapshot(); l != 1 || c != 0 || b != 2 {
				t.Errorf("context %s: want list + 2 by-id revokes, got list=%d current=%d byID=%d", name, l, c, b)
			}
		}
		assertNoContextsLeft(t)
	})

	t.Run("--context override does not narrow the sweep", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		all, _, err := auth.StoredContexts()
		if err != nil || len(all) != 2 {
			t.Fatalf("StoredContexts = %v, %v; want 2 seeded", all, err)
		}
		contexts.SetFlagOverrideForTest(t, all[0].Name)
		execLogout(t)
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			if l, c, b := rec.snapshot(); l != 0 || c != 1 || b != 0 {
				t.Errorf("context %s: want one current-session revoke, got list=%d current=%d byID=%d", name, l, c, b)
			}
		}
		assertNoContextsLeft(t)
	})

	t.Run("no contexts: not logged in", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		t.Setenv(contexts.EnvContextVar, "")
		t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
		if out := execLogout(t); !strings.Contains(out, "Not logged in.") {
			t.Errorf("stdout = %q, want %q", out, "Not logged in.")
		}
	})
}
