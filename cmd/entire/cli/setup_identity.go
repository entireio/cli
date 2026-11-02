package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/entireio/auth-go/tokenmanager"
	"github.com/entireio/cli/cmd/entire/cli/api"
	cliauth "github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

var (
	errEntireLoginRequired    = errors.New("entire login required")
	errEntireEnvTokenRejected = errors.New("entire token rejected")
)

type identityGuidanceError string

func (e identityGuidanceError) Error() string { return string(e) }

// gitConfigIdentityGuidance names the fix that needs no Entire account at all.
// Every guidance string below offers it: the underlying problem is an unset
// local git config value, and a caller who cannot authenticate here can still
// set it directly. The implementation this replaced printed exactly this pair
// on its non-interactive path, and dropping it would leave a failed CI run
// telling the user to authenticate in order to set two local config values.
const gitConfigIdentityGuidance = "Or set the identity directly:\n" +
	"  git config --global user.name \"Your Name\"\n" +
	"  git config --global user.email \"you@example.com\""

const unattendedIdentityGuidance = "Git identity is missing, and Entire authentication is required.\n" +
	"This environment has no interactive terminal, so sign-in cannot complete here.\n" +
	"Run `entire login` in an interactive shell, then rerun `entire enable`.\n" +
	"For unattended use, provide a valid user token in ENTIRE_TOKEN.\n" +
	gitConfigIdentityGuidance

const envTokenIdentityGuidance = "ENTIRE_TOKEN could not authenticate an Entire user profile.\n" +
	"ENTIRE_TOKEN overrides stored logins, so automatic sign-in cannot repair this session.\n" +
	"Fix or unset ENTIRE_TOKEN, then rerun `entire enable`.\n" +
	gitConfigIdentityGuidance

type gitIdentityResolver func(context.Context) (*authProfile, error)
type identityResolverFactory func(io.Writer, io.Writer, bool) gitIdentityResolver

// activeContextProvider resolves the acting login context. It is the seam for
// cliauth.ActiveContext, which already applies the `--context`/$ENTIRE_CONTEXT
// selection and rejects a context carrying no CoreURL — so this path neither
// re-finds the active context by name nor re-derives that guard.
type activeContextProvider func() (*contexts.Context, bool, error)

type identityProfileDependencies struct {
	lookupEnv     func(string) (string, bool)
	activeContext activeContextProvider
	resolveLogin  loginTokenResolver
	fetchProfile  profileFetcher
	allowInsecure bool
}

type identityProfileResult struct {
	profile     *authProfile
	loginServer string
}

type identityRecoveryDependencies struct {
	resolve   func(context.Context) (identityProfileResult, error)
	login     func(context.Context, io.Writer, io.Writer, string, bool) error
	canPrompt func() bool
}

func defaultIdentityProfileDependencies(insecure bool) identityProfileDependencies {
	return identityProfileDependencies{
		lookupEnv:     os.LookupEnv,
		activeContext: cliauth.ActiveContext,
		resolveLogin:  cliauth.RefreshedLoginToken,
		fetchProfile:  defaultFetchProfile,
		allowInsecure: insecure,
	}
}

func defaultIdentityRecoveryDependencies(insecure bool) identityRecoveryDependencies {
	profileDeps := defaultIdentityProfileDependencies(insecure)
	return identityRecoveryDependencies{
		resolve: func(ctx context.Context) (identityProfileResult, error) {
			return resolveEntireIdentityProfile(ctx, profileDeps)
		},
		login: func(ctx context.Context, outW, errW io.Writer, server string, insecure bool) error {
			return runLoginCommand(ctx, outW, errW, server, insecure, false)
		},
		canPrompt: interactive.CanPromptInteractively,
	}
}

func newEntireGitIdentityResolver(outW, errW io.Writer, insecure bool) gitIdentityResolver {
	deps := defaultIdentityRecoveryDependencies(insecure)
	return func(ctx context.Context) (*authProfile, error) {
		applyInsecureHTTPAuth(insecure)
		return recoverGitIdentity(ctx, outW, errW, insecure, deps)
	}
}

func gitIdentityFromEntireProfile(profile *authProfile, existingName, existingEmail string) (string, string, error) {
	if profile == nil {
		return "", "", errors.New("entire profile does not contain a verified Git name and email")
	}

	name := strings.TrimSpace(existingName)
	handle := strings.TrimSpace(profile.Handle)
	if name == "" {
		name = strings.TrimSpace(profile.DisplayName)
		if name == "" {
			name = handle
		}
	}

	email := strings.TrimSpace(existingEmail)
	provider := strings.TrimSpace(profile.Provider)
	providerUserID := strings.TrimSpace(profile.ProviderUserID)
	if email == "" {
		email = strings.TrimSpace(profile.Email)
		if email == "" && provider == "github" && providerUserID != "" && handle != "" {
			email = fmt.Sprintf("%s+%s@users.noreply.github.com", providerUserID, handle)
		}
	}

	if name == "" || email == "" {
		return "", "", errors.New("entire profile does not contain a verified Git name and email")
	}
	return name, email, nil
}

func ensureGitIdentity(
	ctx context.Context,
	w io.Writer,
	runner bootstrapRunner,
	dir string,
	resolve gitIdentityResolver,
) error {
	existingName, nameSet, err := readGitIdentityField(ctx, runner, dir, "user.name")
	if err != nil {
		return err
	}
	existingEmail, emailSet, err := readGitIdentityField(ctx, runner, dir, "user.email")
	if err != nil {
		return err
	}
	if nameSet && emailSet {
		return nil
	}

	profile, err := resolve(ctx)
	if err != nil {
		return err
	}
	name, email, err := gitIdentityFromEntireProfile(profile, existingName, existingEmail)
	if err != nil {
		return err
	}

	if !nameSet {
		if _, err := runner.RunInDir(ctx, dir, "git", "config", "user.name", name); err != nil {
			return wrapExecError("git config user.name", err)
		}
	}
	if !emailSet {
		if _, err := runner.RunInDir(ctx, dir, "git", "config", "user.email", email); err != nil {
			return wrapExecError("git config user.email", err)
		}
	}
	fmt.Fprintf(w, "  Using git identity: %s <%s>\n", name, email)
	return nil
}

func readGitIdentityField(ctx context.Context, runner bootstrapRunner, dir, key string) (string, bool, error) {
	out, err := runner.RunInDir(ctx, dir, "git", "config", "--get", key)
	if err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, wrapExecError("read git config "+key, err)
	}
	value := strings.TrimSpace(out)
	return value, value != "", nil
}

func resolveEntireIdentityProfile(ctx context.Context, deps identityProfileDependencies) (identityProfileResult, error) {
	if raw, ok := deps.lookupEnv(cliauth.EnvTokenVar); ok {
		target, err := resolveEnvTokenStatusTarget(raw)
		if err != nil {
			return identityProfileResult{}, fmt.Errorf("%w: %w", errEntireEnvTokenRejected, err)
		}
		profile, err := deps.fetchProfile(ctx, target.coreURL, target.token)
		if err != nil {
			if isKeychainTokenRejected(err) {
				return identityProfileResult{loginServer: target.coreURL}, fmt.Errorf("%w: %w", errEntireEnvTokenRejected, err)
			}
			return identityProfileResult{loginServer: target.coreURL}, err
		}
		return identityProfileResult{profile: profile, loginServer: target.coreURL}, nil
	}

	active, ok, err := deps.activeContext()
	if err != nil {
		return identityProfileResult{}, err
	}
	result := identityProfileResult{loginServer: api.DefaultAuthBaseURL}
	if !ok {
		return result, errEntireLoginRequired
	}
	result.loginServer = active.CoreURL
	if !deps.allowInsecure {
		if err := api.RequireSecureURL(active.CoreURL); err != nil {
			return result, fmt.Errorf("context login server URL check: %w", err)
		}
	}
	token, err := deps.resolveLogin(ctx, active)
	if err != nil {
		if errors.Is(err, cliauth.ErrNotLoggedIn) || errors.Is(err, tokenmanager.ErrReauthRequired) {
			return result, fmt.Errorf("%w: %w", errEntireLoginRequired, err)
		}
		return result, err
	}
	if strings.TrimSpace(token) == "" {
		return result, errEntireLoginRequired
	}
	profile, err := deps.fetchProfile(ctx, active.CoreURL, token)
	if err != nil {
		if isKeychainTokenRejected(err) {
			return result, fmt.Errorf("%w: %w", errEntireLoginRequired, err)
		}
		return result, err
	}
	result.profile = profile
	return result, nil
}

func recoverGitIdentity(
	ctx context.Context,
	outW, errW io.Writer,
	insecure bool,
	deps identityRecoveryDependencies,
) (*authProfile, error) {
	result, err := deps.resolve(ctx)
	if err == nil {
		return result.profile, nil
	}
	if errors.Is(err, errEntireEnvTokenRejected) {
		return nil, identityGuidanceError(envTokenIdentityGuidance)
	}
	if !errors.Is(err, errEntireLoginRequired) {
		return nil, err
	}
	// Refuse rather than start a login nobody can finish. The gate is "can a
	// human answer here", not "is this known to be unattended": IsKnownUnattended
	// is deliberately permissive (CLAUDECODE is not on its list, and Codex sets
	// none of the names on it), so using it here let every agent subprocess and
	// every headless non-CI context — a `docker build` RUN step, say — fall
	// through to deps.login. With no terminal that takes the device-code flow,
	// which prints a code and then blocks in waitForApproval for up to
	// maxExpiresIn (15 minutes) on nothing. `entire enable` must not turn into
	// that; a headless human can run `entire login` deliberately, which is what
	// the guidance says.
	if !deps.canPrompt() {
		return nil, identityGuidanceError(unattendedIdentityGuidance)
	}
	if err := deps.login(ctx, outW, errW, result.loginServer, insecure); err != nil {
		return nil, err
	}
	result, err = deps.resolve(ctx)
	if err != nil {
		if errors.Is(err, errEntireEnvTokenRejected) {
			return nil, identityGuidanceError(envTokenIdentityGuidance)
		}
		return nil, fmt.Errorf("resolve Entire profile after login: %w", err)
	}
	return result.profile, nil
}
