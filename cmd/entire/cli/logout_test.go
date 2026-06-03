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

type mockTokenStore struct {
	tokens     map[string]string
	deleted    map[string]bool
	getErr     error
	deleteErr  error
	getCalls   int
	deleteCall int
}

func newMockTokenStore() *mockTokenStore {
	return &mockTokenStore{
		tokens:  make(map[string]string),
		deleted: make(map[string]bool),
	}
}

func (m *mockTokenStore) GetToken(baseURL string) (string, error) {
	m.getCalls++
	if m.getErr != nil {
		return "", m.getErr
	}
	return m.tokens[baseURL], nil
}

func (m *mockTokenStore) DeleteToken(baseURL string) error {
	m.deleteCall++
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleted[baseURL] = true
	return nil
}

func TestRunLogout_RevokesServerSideThenDeletesLocally(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken

	revokeCalled := false
	revoke := func(context.Context) error {
		revokeCalled = true
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, func(context.Context) error { return nil }, func() error { return nil }, "https://entire.io", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !revokeCalled {
		t.Error("revoke should be called when a local token exists")
	}
	if !store.deleted["https://entire.io"] {
		t.Fatal("expected token to be deleted for https://entire.io")
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

func TestRunLogout_NoTokenSkipsRevoke(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore() // no token stored

	revokeCalled := false
	revoke := func(context.Context) error {
		revokeCalled = true
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, func(context.Context) error { return nil }, func() error { return nil }, "https://entire.io", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if revokeCalled {
		t.Fatal("revoke should not be called when no local token exists")
	}
	if !store.deleted["https://entire.io"] {
		t.Fatal("expected DeleteToken to be called even when no token was stored")
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_RevokeFailureWarnsButSucceeds(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken

	revoke := func(context.Context) error {
		return errors.New("connection refused")
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, func(context.Context) error { return nil }, func() error { return nil }, "https://entire.io", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !store.deleted["https://entire.io"] {
		t.Fatal("local token should still be deleted when server revoke fails")
	}
	if !strings.Contains(errOut.String(), "server-side session revocation failed") {
		t.Fatalf("stderr = %q, want warning about revoke failure", errOut.String())
	}
	if !strings.Contains(errOut.String(), "connection refused") {
		t.Fatalf("stderr = %q, want underlying error message", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_RevokeUnauthorizedIsSilent(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken

	revoke := func(context.Context) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, func(context.Context) error { return nil }, func() error { return nil }, "https://entire.io", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !store.deleted["https://entire.io"] {
		t.Fatal("local token should still be deleted after silent 401")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty for already-invalid token", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_GetTokenErrorWarnsAndFallsThrough(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.getErr = errors.New("keyring locked for read")

	revokeCalled := false
	revoke := func(context.Context) error {
		revokeCalled = true
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, func(context.Context) error { return nil }, func() error { return nil }, "https://entire.io", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if revokeCalled {
		t.Fatal("revoke should not be called when token read fails")
	}
	if !store.deleted["https://entire.io"] {
		t.Fatal("DeleteToken should still be attempted after GetToken failure")
	}
	if !strings.Contains(errOut.String(), "failed to read token before revocation") {
		t.Fatalf("stderr = %q, want warning about read failure", errOut.String())
	}
}

func TestRunLogout_ReturnsErrorOnDeleteFailure(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken
	store.deleteErr = errors.New("keyring locked")

	revoke := func(context.Context) error { return nil }

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, func(context.Context) error { return nil }, func() error { return nil }, "https://entire.io", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "keyring locked") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "keyring locked")
	}
	if strings.Contains(out.String(), "Logged out.") {
		t.Fatal("should not print success message when local delete fails")
	}
}

func TestRunLogout_AllRevokesAllSessions(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken

	currentCalled, allCalled := false, false
	revokeCurrent := func(context.Context) error { currentCalled = true; return nil }
	revokeAll := func(context.Context) error { allCalled = true; return nil }

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store,
		revokeCurrent, revokeAll, func() error { return nil }, "https://entire.io", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if currentCalled {
		t.Error("--everywhere should not call the current-session revoke")
	}
	if !allCalled {
		t.Error("--everywhere should call the revoke-all path")
	}
	if !store.deleted["https://entire.io"] {
		t.Fatal("local token should still be deleted under --everywhere")
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

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

// makeLogoutContexts builds a contextsProvider returning the given contexts
// with no active marker — `logout --all-contexts` ignores which one is current.
func makeLogoutContexts(cs ...*contexts.Context) contextsProvider {
	return func() ([]*contexts.Context, string, error) { return cs, "", nil }
}

func TestRunLogoutAll_RevokesAndRemovesEachContext(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	tokens := map[string]string{"eu": "tok-eu", "us": "tok-us"}
	tokenFor := func(c *contexts.Context) (string, error) { return tokens[c.Name], nil }

	revoked := map[string]string{} // coreURL -> token
	revoke := func(_ context.Context, coreURL, token string) error {
		revoked[coreURL] = token
		return nil
	}
	removed := map[string]bool{}
	remove := func(name string) error { removed[name] = true; return nil }

	store := newMockTokenStore()
	var out, errOut bytes.Buffer
	if err := runLogoutAll(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, store, "https://entire.io", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if revoked["https://eu.auth.entire.io"] != "tok-eu" || revoked["https://us.auth.entire.io"] != "tok-us" {
		t.Fatalf("each context's session should be revoked against its own core+token, got %v", revoked)
	}
	if !removed["eu"] || !removed["us"] {
		t.Fatalf("both contexts should be removed locally, got %v", removed)
	}
	if !store.deleted["https://entire.io"] {
		t.Error("legacy keyring entry should be cleared on logout --all-contexts")
	}
	if !strings.Contains(out.String(), "Logged out of 2 saved login(s).") {
		t.Fatalf("stdout = %q, want count of 2", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

func TestRunLogoutAll_NoContexts(t *testing.T) {
	t.Parallel()

	revoke := func(context.Context, string, string) error {
		t.Fatal("revoke should not run with no contexts")
		return nil
	}
	remove := func(string) error { t.Fatal("remove should not run with no contexts"); return nil }
	store := newMockTokenStore()

	var out, errOut bytes.Buffer
	if err := runLogoutAll(context.Background(), &out, &errOut, makeLogoutContexts(), nil, revoke, remove, store, "https://entire.io", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "No saved logins to remove.") {
		t.Fatalf("stdout = %q, want the empty-state message", out.String())
	}
	if !store.deleted["https://entire.io"] {
		t.Error("legacy keyring entry should still be cleared even with no contexts")
	}
}

func TestRunLogoutAll_RevokeFailureWarnsButContinues(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(
		&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"},
		&contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"},
	)
	tokenFor := func(*contexts.Context) (string, error) { return testLogoutToken, nil }
	revoke := func(_ context.Context, coreURL, _ string) error {
		if coreURL == "https://eu.auth.entire.io" {
			return errors.New("connection refused")
		}
		return nil
	}
	removed := map[string]bool{}
	remove := func(name string) error { removed[name] = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogoutAll(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, newMockTokenStore(), "https://entire.io", false); err != nil {
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

func TestRunLogoutAll_UnauthorizedRevokeIsSilent(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(*contexts.Context) (string, error) { return testLogoutToken, nil }
	revoke := func(context.Context, string, string) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}
	remove := func(string) error { return nil }

	var out, errOut bytes.Buffer
	if err := runLogoutAll(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, newMockTokenStore(), "https://entire.io", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty: an already-invalid token is the desired state", errOut.String())
	}
}

func TestRunLogoutAll_UnreadableTokenRemovesLocallyOnly(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "eu", CoreURL: "https://eu.auth.entire.io"})
	tokenFor := func(*contexts.Context) (string, error) { return "", errors.New("keyring locked") }
	revokeCalled := false
	revoke := func(context.Context, string, string) error { revokeCalled = true; return nil }
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	if err := runLogoutAll(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, newMockTokenStore(), "https://entire.io", false); err != nil {
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
}

func TestRunLogoutAll_InsecureCoreSkipsRevoke(t *testing.T) {
	t.Parallel()

	provider := makeLogoutContexts(&contexts.Context{Name: "local", CoreURL: "http://insecure.example.com"})
	tokenFor := func(*contexts.Context) (string, error) { return testLogoutToken, nil }
	revokeCalled := false
	revoke := func(context.Context, string, string) error { revokeCalled = true; return nil }
	removed := false
	remove := func(string) error { removed = true; return nil }

	var out, errOut bytes.Buffer
	// insecureHTTPAuth=false: a plain-http core must not receive the bearer.
	if err := runLogoutAll(context.Background(), &out, &errOut, provider, tokenFor, revoke, remove, newMockTokenStore(), "https://entire.io", false); err != nil {
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

// coreRecorder counts the session-endpoint calls a fake entire-core sees, so
// the flag-matrix test can assert exactly which revoke shape each context's
// core received.
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

// newCoreServer stands up a fake entire-core that answers the three session
// endpoints logout uses: GET (list), DELETE /current, DELETE /<id>. The list
// returns two sessions so --everywhere has something to delete per core.
func newCoreServer(t *testing.T) (*httptest.Server, *coreRecorder) {
	t.Helper()
	rec := &coreRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == coreSessionsPath:
			rec.listCount++
			fmt.Fprint(w, `{"tokens":[{"id":"s1"},{"id":"s2"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == coreSessionsPath+"/current":
			rec.deleteCurrent++
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, coreSessionsPath+"/"):
			rec.deleteByID = append(rec.deleteByID, strings.TrimPrefix(r.URL.Path, coreSessionsPath+"/"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// seedTwoContexts records two login contexts pointing at two fake cores. The
// second (recB) is recorded with activate=true, so it is the *active* context
// — what a plain `logout` (no --all-contexts) targets.
func seedTwoContexts(t *testing.T) (recA, recB *coreRecorder) {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
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

// execLogout runs the real cobra logout command with --insecure-http-auth
// (the fake cores are http loopback) plus the given flags.
func execLogout(t *testing.T, flags ...string) {
	t.Helper()
	cmd := newLogoutCmd()
	cmd.SetArgs(append([]string{"--insecure-http-auth"}, flags...))
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("logout %v: %v (stderr=%q)", flags, err, errOut.String())
	}
}

// TestLogoutCommand_FlagMatrix pins all four quadrants of the --all-contexts/--everywhere
// matrix end-to-end through the cobra command, asserting which revoke shape each
// context's core actually received. Process-global env + keyring backend, so no
// t.Parallel(); subtests run sequentially, each with fresh state.
func TestLogoutCommand_FlagMatrix(t *testing.T) {
	t.Run("logout: active context, current session", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		execLogout(t)
		if l, c, b := recA.snapshot(); l+c+b != 0 {
			t.Errorf("inactive context A should be untouched, got list=%d current=%d byID=%d", l, c, b)
		}
		if l, c, b := recB.snapshot(); l != 0 || c != 1 || b != 0 {
			t.Errorf("active context B: want one current-session revoke, got list=%d current=%d byID=%d", l, c, b)
		}
	})

	t.Run("--everywhere: active context, all sessions", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		execLogout(t, "--everywhere")
		if l, c, b := recA.snapshot(); l+c+b != 0 {
			t.Errorf("inactive context A should be untouched, got list=%d current=%d byID=%d", l, c, b)
		}
		if l, c, b := recB.snapshot(); l != 1 || c != 0 || b != 2 {
			t.Errorf("active context B: want list + 2 by-id revokes, got list=%d current=%d byID=%d", l, c, b)
		}
	})

	t.Run("--all-contexts: every context, current session each", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		execLogout(t, "--all-contexts")
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			if l, c, b := rec.snapshot(); l != 0 || c != 1 || b != 0 {
				t.Errorf("context %s: want one current-session revoke, got list=%d current=%d byID=%d", name, l, c, b)
			}
		}
	})

	t.Run("--all-contexts --everywhere: every context, all sessions each", func(t *testing.T) {
		recA, recB := seedTwoContexts(t)
		execLogout(t, "--all-contexts", "--everywhere")
		for name, rec := range map[string]*coreRecorder{"A": recA, "B": recB} {
			if l, c, b := rec.snapshot(); l != 1 || c != 0 || b != 2 {
				t.Errorf("context %s: want list + 2 by-id revokes, got list=%d current=%d byID=%d", name, l, c, b)
			}
		}
	})
}
