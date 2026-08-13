package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

func TestWarnIfImportNotSynced(t *testing.T) {
	// Mutates the package-level importLoggedIn seam, so it cannot run in
	// parallel with other tests that read it.
	orig := importLoggedIn
	t.Cleanup(func() { importLoggedIn = orig })

	cases := []struct {
		name       string
		loggedIn   bool
		imported   bool
		wantNotice bool
	}{
		{name: "logged out with imported history warns", loggedIn: false, imported: true, wantNotice: true},
		{name: "logged in does not warn", loggedIn: true, imported: true, wantNotice: false},
		{name: "nothing imported does not warn", loggedIn: false, imported: false, wantNotice: false},
		{name: "logged in and nothing imported does not warn", loggedIn: true, imported: false, wantNotice: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			importLoggedIn = func() bool { return tc.loggedIn }
			var buf bytes.Buffer
			warnIfImportNotSynced(&buf, tc.imported)
			got := buf.String()
			hasNotice := strings.Contains(got, "not logged in") && strings.Contains(got, "entire login")
			if hasNotice != tc.wantNotice {
				t.Fatalf("warnIfImportNotSynced(logged_in=%v, imported=%v): notice=%v, want %v; output=%q",
					tc.loggedIn, tc.imported, hasNotice, tc.wantNotice, got)
			}
		})
	}
}

// TestImportLoggedIn exercises the default login heuristic's branching via the
// local-read seams. The key case (#1773 review): a current context that exists
// but has no stored token must NOT count as logged in, so the sync notice still
// fires. Mutates package-level seams, so no t.Parallel.
func TestImportLoggedIn(t *testing.T) {
	origLogin, origToken := importCurrentLogin, importLoginToken
	t.Cleanup(func() { importCurrentLogin, importLoginToken = origLogin, origToken })
	// Ensure no env token leaks in from the environment for the context cases.
	t.Setenv(auth.EnvTokenVar, "")

	withCurrent := func() (*contexts.Context, error) {
		return &contexts.Context{Name: "current"}, nil
	}

	t.Run("current context with a stored token is logged in", func(t *testing.T) {
		importCurrentLogin = withCurrent
		importLoginToken = func(*contexts.Context) (string, error) { return "stored-token", nil }
		if !importLoggedIn() {
			t.Fatal("context with a token should count as logged in")
		}
	})

	t.Run("current context with a missing token is NOT logged in", func(t *testing.T) {
		importCurrentLogin = withCurrent
		importLoginToken = func(*contexts.Context) (string, error) {
			return "", errors.New("no token stored")
		}
		if importLoggedIn() {
			t.Fatal("context present but token missing must not count as logged in")
		}
	})

	t.Run("no current context is not logged in", func(t *testing.T) {
		importCurrentLogin = func() (*contexts.Context, error) { return nil, errors.New("no current login") }
		importLoginToken = func(*contexts.Context) (string, error) { return "stored-token", nil }
		if importLoggedIn() {
			t.Fatal("no current context should not count as logged in")
		}
	})

	t.Run("env token counts as logged in even with no context", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "env-token")
		importCurrentLogin = func() (*contexts.Context, error) { return nil, errors.New("no current login") }
		if !importLoggedIn() {
			t.Fatal("ENTIRE_TOKEN should count as logged in")
		}
	})
}
