package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cliapi "github.com/entireio/cli/cmd/entire/cli/api"
	cliauth "github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

type testExitError struct{ code int }

func (e testExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e testExitError) ExitCode() int { return e.code }

func TestGitIdentityFromEntireProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		profile     authProfile
		wantName    string
		wantEmail   string
		wantErrText string
	}{
		{
			name: "profile email",
			profile: authProfile{
				DisplayName:    " Octo Cat ",
				Handle:         "octo",
				Email:          " octo@example.com ",
				Provider:       "github",
				ProviderUserID: "42",
			},
			wantName:  "Octo Cat",
			wantEmail: "octo@example.com",
		},
		{
			name: "github private email from sparse foreign-region profile",
			profile: authProfile{
				Handle:         "  octo  ",
				Provider:       " github ",
				ProviderUserID: " 42 ",
				ForeignRegion:  true,
			},
			wantName:  "octo",
			wantEmail: "42+octo@users.noreply.github.com",
		},
		{
			name:        "insufficient verified profile",
			profile:     authProfile{Provider: "github", ProviderUserID: "42"},
			wantErrText: "entire profile does not contain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, email, err := gitIdentityFromEntireProfile(&tt.profile, "", "")
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("error = %v, want text %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("gitIdentityFromEntireProfile: %v", err)
			}
			if name != tt.wantName || email != tt.wantEmail {
				t.Fatalf("identity = %q <%s>, want %q <%s>", name, email, tt.wantName, tt.wantEmail)
			}
		})
	}
}

func TestEnsureGitIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		configuredName   string
		configuredEmail  string
		nameReadErr      error
		emailReadErr     error
		emailWriteErr    error
		profile          *authProfile
		wantResolverCall bool
		wantNameWrite    bool
		wantEmailWrite   bool
		wantErrText      string
	}{
		{
			name:            "complete identity is a local no-op",
			configuredName:  "Existing User",
			configuredEmail: "existing@example.com",
		},
		{
			name:             "both fields missing",
			nameReadErr:      testExitError{code: 1},
			emailReadErr:     testExitError{code: 1},
			wantResolverCall: true,
			wantNameWrite:    true,
			wantEmailWrite:   true,
		},
		{
			name:             "preserves configured name",
			configuredName:   "Existing User",
			emailReadErr:     testExitError{code: 1},
			wantResolverCall: true,
			wantEmailWrite:   true,
		},
		{
			name:             "preserves configured email",
			configuredEmail:  "existing@example.com",
			nameReadErr:      testExitError{code: 1},
			wantResolverCall: true,
			wantNameWrite:    true,
		},
		{
			name:        "operational read failure does not authenticate",
			nameReadErr: testExitError{code: 2},
			wantErrText: "read git config user.name",
		},
		{
			name:             "second write failure preserves the first local write",
			nameReadErr:      testExitError{code: 1},
			emailReadErr:     testExitError{code: 1},
			emailWriteErr:    errors.New("config locked"),
			wantResolverCall: true,
			wantNameWrite:    true,
			wantEmailWrite:   true,
			wantErrText:      "git config user.email",
		},
		{
			name:             "configured email only requires a profile name",
			configuredEmail:  "existing@example.com",
			nameReadErr:      testExitError{code: 1},
			profile:          &authProfile{DisplayName: "Entire User"},
			wantResolverCall: true,
			wantNameWrite:    true,
		},
		{
			name:             "configured name only requires a profile email",
			configuredName:   "Existing User",
			emailReadErr:     testExitError{code: 1},
			profile:          &authProfile{Email: "entire@example.com"},
			wantResolverCall: true,
			wantEmailWrite:   true,
		},
		{
			name:             "incomplete verified profile writes nothing",
			nameReadErr:      testExitError{code: 1},
			emailReadErr:     testExitError{code: 1},
			profile:          &authProfile{Handle: "entire-user"},
			wantResolverCall: true,
			wantErrText:      "entire profile does not contain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := newFakeRunner()
			runner.set("git", []string{"config", "--get", "user.name"}, tt.configuredName, tt.nameReadErr)
			runner.set("git", []string{"config", "--get", "user.email"}, tt.configuredEmail, tt.emailReadErr)
			if tt.wantNameWrite {
				runner.set("git", []string{"config", "user.name", "Entire User"}, "", nil)
			}
			if tt.wantEmailWrite {
				runner.set("git", []string{"config", "user.email", "entire@example.com"}, "", tt.emailWriteErr)
			}

			resolverCalls := 0
			resolve := func(context.Context) (*authProfile, error) {
				resolverCalls++
				if tt.profile != nil {
					return tt.profile, nil
				}
				return &authProfile{DisplayName: "Entire User", Email: "entire@example.com"}, nil
			}
			err := ensureGitIdentity(t.Context(), io.Discard, runner, t.TempDir(), resolve)
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("error = %v, want text %q", err, tt.wantErrText)
				}
			} else if err != nil {
				t.Fatalf("ensureGitIdentity: %v", err)
			}
			if got := resolverCalls; got != boolInt(tt.wantResolverCall) {
				t.Fatalf("resolver calls = %d, want %d", got, boolInt(tt.wantResolverCall))
			}
			if got := runner.hasCall(argsMatch("git", []string{"config", "user.name", "Entire User"})); got != tt.wantNameWrite {
				t.Errorf("user.name write = %v, want %v", got, tt.wantNameWrite)
			}
			if got := runner.hasCall(argsMatch("git", []string{"config", "user.email", "entire@example.com"})); got != tt.wantEmailWrite {
				t.Errorf("user.email write = %v, want %v", got, tt.wantEmailWrite)
			}
		})
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestResolveEntireIdentityProfile(t *testing.T) {
	t.Parallel()

	profile := &authProfile{DisplayName: "Entire User", Email: "entire@example.com"}
	ctxEntry := &contexts.Context{Name: "work", CoreURL: "https://core.example.test"}

	t.Run("stored context", func(t *testing.T) {
		t.Parallel()
		deps := identityProfileDependencies{
			lookupEnv:     func(string) (string, bool) { return "", false },
			activeContext: func() (*contexts.Context, bool, error) { return ctxEntry, true, nil },
			resolveLogin:  func(context.Context, *contexts.Context) (string, error) { return "stored-token", nil },
			fetchProfile: func(_ context.Context, coreURL, token string) (*authProfile, error) {
				if coreURL != ctxEntry.CoreURL || token != "stored-token" {
					t.Fatalf("profile target = %q token %q", coreURL, token)
				}
				return profile, nil
			},
		}
		got, err := resolveEntireIdentityProfile(t.Context(), deps)
		if err != nil || got.profile != profile || got.loginServer != ctxEntry.CoreURL {
			t.Fatalf("result = %+v, error = %v", got, err)
		}
	})

	t.Run("stored insecure context is rejected before profile fetch", func(t *testing.T) {
		t.Parallel()
		insecureContext := &contexts.Context{Name: "local", CoreURL: "http://127.0.0.1:8787"}
		fetchCalls := 0
		deps := identityProfileDependencies{
			lookupEnv:     func(string) (string, bool) { return "", false },
			activeContext: func() (*contexts.Context, bool, error) { return insecureContext, true, nil },
			resolveLogin:  func(context.Context, *contexts.Context) (string, error) { return "stored-token", nil },
			fetchProfile: func(context.Context, string, string) (*authProfile, error) {
				fetchCalls++
				return profile, nil
			},
		}
		_, err := resolveEntireIdentityProfile(t.Context(), deps)
		if !errors.Is(err, cliapi.ErrInsecureHTTP) {
			t.Fatalf("error = %v, want ErrInsecureHTTP", err)
		}
		if fetchCalls != 0 {
			t.Fatalf("profile fetch calls = %d, want 0", fetchCalls)
		}
	})

	t.Run("valid env token bypasses stored contexts", func(t *testing.T) {
		t.Parallel()
		raw := makeJWT(t, `{"alg":"RS256"}`, `{"aud":"https://env-core.example.test"}`)
		deps := identityProfileDependencies{
			lookupEnv: func(name string) (string, bool) {
				if name != cliauth.EnvTokenVar {
					t.Fatalf("lookup %q", name)
				}
				return raw, true
			},
			activeContext: func() (*contexts.Context, bool, error) {
				t.Fatal("stored contexts must not be read in ENTIRE_TOKEN mode")
				return nil, false, nil
			},
			fetchProfile: func(_ context.Context, coreURL, token string) (*authProfile, error) {
				if coreURL != "https://env-core.example.test" || token != raw {
					t.Fatalf("profile target = %q token %q", coreURL, token)
				}
				return profile, nil
			},
		}
		got, err := resolveEntireIdentityProfile(t.Context(), deps)
		if err != nil || got.profile != profile {
			t.Fatalf("result = %+v, error = %v", got, err)
		}
	})

	t.Run("refresh failure remains operational", func(t *testing.T) {
		t.Parallel()
		refreshErr := errors.New("credential store unavailable")
		deps := identityProfileDependencies{
			lookupEnv:     func(string) (string, bool) { return "", false },
			activeContext: func() (*contexts.Context, bool, error) { return ctxEntry, true, nil },
			resolveLogin:  func(context.Context, *contexts.Context) (string, error) { return "", refreshErr },
			fetchProfile: func(context.Context, string, string) (*authProfile, error) {
				t.Fatal("profile must not be fetched after refresh failure")
				return nil, errors.New("unexpected profile fetch")
			},
		}
		_, err := resolveEntireIdentityProfile(t.Context(), deps)
		if !errors.Is(err, refreshErr) || errors.Is(err, errEntireLoginRequired) {
			t.Fatalf("error = %v, want original operational failure", err)
		}
	})

	// cliauth.ActiveContext reports a context carrying no CoreURL as "none
	// acting" rather than returning an unusable pointer. Asserted here because
	// this path used to find the active context by name itself and had no such
	// guard: it would go on to mint a token against an empty core and surface
	// the resulting transport error instead of sending the user to log in.
	t.Run("active context without a core URL asks for login", func(t *testing.T) {
		t.Parallel()
		deps := identityProfileDependencies{
			lookupEnv:     func(string) (string, bool) { return "", false },
			activeContext: func() (*contexts.Context, bool, error) { return nil, false, nil },
			resolveLogin: func(context.Context, *contexts.Context) (string, error) {
				t.Fatal("must not resolve a token without an acting context")
				return "", nil
			},
			fetchProfile: func(context.Context, string, string) (*authProfile, error) {
				t.Fatal("must not fetch a profile without an acting context")
				return nil, errors.New("unexpected profile fetch")
			},
		}
		got, err := resolveEntireIdentityProfile(t.Context(), deps)
		if !errors.Is(err, errEntireLoginRequired) {
			t.Fatalf("error = %v, want errEntireLoginRequired", err)
		}
		if got.loginServer != cliapi.DefaultAuthBaseURL {
			t.Fatalf("loginServer = %q, want the default so the login prompt names a real server", got.loginServer)
		}
	})

	// The env-token branch has no RequireSecureURL call, which reads like a
	// missing TLS check next to the stored-context branch that has one. It is
	// not: the core URL comes from the token's own aud claim, and
	// CoreURLFromEnvToken -> validateCoreAudience hard-rejects any scheme but
	// https before this code ever sees it (auth/env_token.go). The stored
	// branch needs its own check because contexts.json is user-editable and may
	// legitimately hold an http dev core — which is what --insecure-http-auth
	// is for. No such escape exists for an env token, at any call site.
	//
	// Asserted here because the "missing check" reading has been reported
	// twice; if validateCoreAudience is ever relaxed, this fails rather than
	// the reviewers being right the third time.
	t.Run("http env token is refused before the bearer is sent", func(t *testing.T) {
		t.Parallel()
		raw := makeJWT(t, `{"alg":"RS256"}`, `{"aud":"http://insecure-core.example.test"}`)
		fetchCalls := 0
		deps := identityProfileDependencies{
			lookupEnv: func(string) (string, bool) { return raw, true },
			fetchProfile: func(context.Context, string, string) (*authProfile, error) {
				fetchCalls++
				return nil, errors.New("must not be reached")
			},
			// Even with the insecure opt-in: an http env token is never allowed.
			allowInsecure: true,
		}
		_, err := resolveEntireIdentityProfile(t.Context(), deps)
		if !errors.Is(err, errEntireEnvTokenRejected) {
			t.Fatalf("error = %v, want errEntireEnvTokenRejected", err)
		}
		if !strings.Contains(err.Error(), "must use https") {
			t.Fatalf("error = %v, want the https refusal from validateCoreAudience", err)
		}
		if fetchCalls != 0 {
			t.Fatalf("profile fetch calls = %d, want 0 — the bearer must never leave", fetchCalls)
		}
	})

	t.Run("rejected env token is distinguished from stored login expiry", func(t *testing.T) {
		t.Parallel()
		raw := makeJWT(t, `{"alg":"RS256"}`, `{"aud":"https://env-core.example.test"}`)
		deps := identityProfileDependencies{
			lookupEnv: func(string) (string, bool) { return raw, true },
			fetchProfile: func(context.Context, string, string) (*authProfile, error) {
				return nil, &cliapi.HTTPError{StatusCode: 401}
			},
		}
		_, err := resolveEntireIdentityProfile(t.Context(), deps)
		if !errors.Is(err, errEntireEnvTokenRejected) || errors.Is(err, errEntireLoginRequired) {
			t.Fatalf("error = %v, want env-token rejection", err)
		}
	})
}

func TestRecoverGitIdentity(t *testing.T) {
	t.Parallel()

	// Spelled out rather than referencing the constants: these are the exact
	// words a blocked user reads, so an accidental edit should fail a test
	// rather than pass one that compares a constant against itself.
	const gitConfigTail = "Or set the identity directly:\n" +
		"  git config --global user.name \"Your Name\"\n" +
		"  git config --global user.email \"you@example.com\""
	const unattendedGuidance = "Git identity is missing, and Entire authentication is required.\n" +
		"This environment has no interactive terminal, so sign-in cannot complete here.\n" +
		"Run `entire login` in an interactive shell, then rerun `entire enable`.\n" +
		"For unattended use, provide a valid user token in ENTIRE_TOKEN.\n" +
		gitConfigTail
	const envTokenGuidance = "ENTIRE_TOKEN could not authenticate an Entire user profile.\n" +
		"ENTIRE_TOKEN overrides stored logins, so automatic sign-in cannot repair this session.\n" +
		"Fix or unset ENTIRE_TOKEN, then rerun `entire enable`.\n" +
		gitConfigTail

	t.Run("login once then retry same target", func(t *testing.T) {
		t.Parallel()
		calls := 0
		loginCalls := 0
		profile := &authProfile{DisplayName: "Entire User", Email: "entire@example.com"}
		deps := identityRecoveryDependencies{
			resolve: func(context.Context) (identityProfileResult, error) {
				calls++
				if calls == 1 {
					return identityProfileResult{loginServer: "https://work.example.test"}, errEntireLoginRequired
				}
				return identityProfileResult{profile: profile, loginServer: "https://work.example.test"}, nil
			},
			login: func(_ context.Context, _, _ io.Writer, server string, insecure bool) error {
				loginCalls++
				if server != "https://work.example.test" || !insecure {
					t.Fatalf("login target = %q insecure=%v", server, insecure)
				}
				return nil
			},
			canPrompt: func() bool { return true },
		}
		got, err := recoverGitIdentity(t.Context(), io.Discard, io.Discard, true, deps)
		if err != nil || got != profile || calls != 2 || loginCalls != 1 {
			t.Fatalf("profile=%+v err=%v resolve=%d login=%d", got, err, calls, loginCalls)
		}
	})

	t.Run("no interactive terminal fails with exact guidance", func(t *testing.T) {
		t.Parallel()
		deps := identityRecoveryDependencies{
			resolve: func(context.Context) (identityProfileResult, error) {
				return identityProfileResult{}, errEntireLoginRequired
			},
			login: func(context.Context, io.Writer, io.Writer, string, bool) error {
				t.Fatal("login must not run without an interactive terminal")
				return nil
			},
			canPrompt: func() bool { return false },
		}
		_, err := recoverGitIdentity(t.Context(), io.Discard, io.Discard, false, deps)
		if err == nil || err.Error() != unattendedGuidance {
			t.Fatalf("error = %q, want %q", err, unattendedGuidance)
		}
	})

	// Regression: the gate used to be IsKnownUnattended, which is deliberately
	// permissive — CLAUDECODE is not on its list and Codex sets none of the
	// names on it. Every agent subprocess and every headless non-CI context
	// therefore reached deps.login, which with no terminal prints a device code
	// and blocks for up to 15 minutes on nobody.
	t.Run("agent subprocess with no terminal refuses instead of starting a login", func(t *testing.T) {
		t.Parallel()
		deps := identityRecoveryDependencies{
			resolve: func(context.Context) (identityProfileResult, error) {
				return identityProfileResult{}, errEntireLoginRequired
			},
			login: func(context.Context, io.Writer, io.Writer, string, bool) error {
				t.Fatal("login must not start where it cannot be completed")
				return nil
			},
			// What IsKnownUnattended reported for Claude Code and Codex.
			canPrompt: func() bool { return false },
		}
		_, err := recoverGitIdentity(t.Context(), io.Discard, io.Discard, false, deps)
		if err == nil || err.Error() != unattendedGuidance {
			t.Fatalf("error = %q, want %q", err, unattendedGuidance)
		}
	})

	// The guidance must keep offering the fix that needs no Entire account.
	t.Run("guidance names the direct git config fix", func(t *testing.T) {
		t.Parallel()
		for _, guidance := range []string{unattendedIdentityGuidance, envTokenIdentityGuidance} {
			if !strings.Contains(guidance, "git config --global user.name") ||
				!strings.Contains(guidance, "git config --global user.email") {
				t.Errorf("guidance omits the direct git config fix:\n%s", guidance)
			}
		}
	})

	t.Run("rejected env token fails with exact guidance", func(t *testing.T) {
		t.Parallel()
		deps := identityRecoveryDependencies{
			resolve: func(context.Context) (identityProfileResult, error) {
				return identityProfileResult{}, errEntireEnvTokenRejected
			},
			login: func(context.Context, io.Writer, io.Writer, string, bool) error {
				t.Fatal("login cannot repair ENTIRE_TOKEN")
				return nil
			},
			canPrompt: func() bool { return true },
		}
		_, err := recoverGitIdentity(t.Context(), io.Discard, io.Discard, false, deps)
		if err == nil || err.Error() != envTokenGuidance {
			t.Fatalf("error = %q, want %q", err, envTokenGuidance)
		}
	})

	t.Run("network error is preserved without login", func(t *testing.T) {
		t.Parallel()
		networkErr := errors.New("dial core: connection refused")
		deps := identityRecoveryDependencies{
			resolve: func(context.Context) (identityProfileResult, error) { return identityProfileResult{}, networkErr },
			login: func(context.Context, io.Writer, io.Writer, string, bool) error {
				t.Fatal("network errors must not start login")
				return nil
			},
			canPrompt: func() bool { return true },
		}
		_, err := recoverGitIdentity(t.Context(), io.Discard, io.Discard, false, deps)
		if !errors.Is(err, networkErr) {
			t.Fatalf("error = %v, want original network error", err)
		}
	})

	t.Run("login cancellation is preserved without retry", func(t *testing.T) {
		t.Parallel()
		loginErr := errors.New("login cancelled")
		resolveCalls := 0
		deps := identityRecoveryDependencies{
			resolve: func(context.Context) (identityProfileResult, error) {
				resolveCalls++
				return identityProfileResult{loginServer: "https://work.example.test"}, errEntireLoginRequired
			},
			login:     func(context.Context, io.Writer, io.Writer, string, bool) error { return loginErr },
			canPrompt: func() bool { return true },
		}
		_, err := recoverGitIdentity(t.Context(), io.Discard, io.Discard, false, deps)
		if !errors.Is(err, loginErr) || resolveCalls != 1 {
			t.Fatalf("error = %v, resolve calls = %d; want cancellation and no retry", err, resolveCalls)
		}
	})
}

// argsMatch builds a predicate over recorded fakeRunner calls. It lived in the
// bootstrap test file until that file's git-identity half moved here.
func argsMatch(name string, args []string) func(fakeCall) bool {
	return func(c fakeCall) bool {
		if c.name != name || len(c.args) < len(args) {
			return false
		}
		for i, a := range args {
			if c.args[i] != a {
				return false
			}
		}
		return true
	}
}

// The identity preflight has to cover the setup flow, not just `entire enable`:
// bare `entire` (root.go) and `entire agent` (runAgentMenu) both reach it for
// the same "existing repo, not set up yet" case, install hooks and settings,
// and would otherwise leave commits attributed to an unknown author.
func TestRunSetupFlow_PreflightRunsBeforeAnyWrite(t *testing.T) {
	repoDir := setupTestRepo(t)
	clearLocalGitIdentity(t, repoDir)

	preflightErr := errors.New("identity unavailable")
	called := 0
	err := runSetupFlowWithPreflight(t.Context(), io.Discard, EnableOptions{}, func() error {
		called++
		return preflightErr
	})
	if !errors.Is(err, preflightErr) {
		t.Fatalf("error = %v, want the preflight's error", err)
	}
	if called != 1 {
		t.Fatalf("preflight calls = %d, want 1", called)
	}
	// A failed preflight must leave the repo untouched — same guarantee
	// TestEnableCmd_IdentityFailureLeavesSetupAbsent makes for enable.
	if _, statErr := os.Stat(filepath.Join(repoDir, ".entire")); !os.IsNotExist(statErr) {
		t.Fatalf(".entire exists after a failed preflight (stat err = %v)", statErr)
	}
}

// The dependency structs above are injected in every other test in this file,
// which means nothing here exercises what production actually wires into them.
// That gap is not theoretical: reverting canPrompt to the old
// IsKnownUnattended-based gate — reintroducing the headless device-login hang —
// left the entire identity and enable test surface passing.
//
// Compare by function pointer rather than behaviour: the point is to pin which
// function is wired, and a behavioural check would pass for any function that
// happens to agree in the test environment (which IsKnownUnattended does).
func TestDefaultIdentityDependencies_WireTheRealFunctions(t *testing.T) {
	t.Parallel()

	recovery := defaultIdentityRecoveryDependencies(false)
	if got, want := funcPointer(recovery.canPrompt), funcPointer(interactive.CanPromptInteractively); got != want {
		t.Errorf("canPrompt is not interactive.CanPromptInteractively; a login must never start where it cannot be completed")
	}

	profile := defaultIdentityProfileDependencies(false)
	if got, want := funcPointer(profile.activeContext), funcPointer(cliauth.ActiveContext); got != want {
		t.Errorf("activeContext is not cliauth.ActiveContext; the CoreURL guard lives there")
	}
	if got, want := funcPointer(profile.resolveLogin), funcPointer(cliauth.RefreshedLoginToken); got != want {
		t.Errorf("resolveLogin is not cliauth.RefreshedLoginToken")
	}
}

func funcPointer(fn any) uintptr {
	return reflect.ValueOf(fn).Pointer()
}

// runSetupFlow must pass a real preflight, not nil. Injecting one (as
// TestRunSetupFlow_PreflightRunsBeforeAnyWrite does) cannot catch a
// runSetupFlow that stopped supplying it, which would silently restore the
// unknown-author bug for bare `entire` and `entire agent`.
//
// Drives the default wiring end to end without a network: under `go test`
// CanPromptInteractively is false, so recoverGitIdentity refuses with the
// guidance before any profile fetch or login is attempted.
func TestRunSetupFlow_UsesTheRealPreflight(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	repoDir := setupTestRepo(t)
	clearLocalGitIdentity(t, repoDir)

	err := runSetupFlow(t.Context(), io.Discard, EnableOptions{})
	if err == nil {
		t.Fatal("runSetupFlow succeeded with no git identity; the preflight did not run")
	}
	if !strings.Contains(err.Error(), "git config --global user.name") {
		t.Fatalf("error = %v, want the identity guidance", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, ".entire")); !os.IsNotExist(statErr) {
		t.Errorf(".entire exists after a refused preflight (stat err = %v)", statErr)
	}
}

// A configured identity must not reach the resolver at all — this is what keeps
// the preflight free on the overwhelmingly common path.
func TestDefaultIdentityPreflight_NoOpWhenIdentityConfigured(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	repoDir := setupTestRepo(t)
	testutil.RunGit(t, repoDir, "config", "--local", "user.name", "Configured User")
	testutil.RunGit(t, repoDir, "config", "--local", "user.email", "configured@example.com")

	if err := defaultIdentityPreflight(t.Context(), io.Discard)(); err != nil {
		t.Fatalf("preflight with a configured identity = %v, want nil", err)
	}
}
