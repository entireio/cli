package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// runOnboardingLogin is the auth rung's offer: the same browser-first login
// `entire login` runs, with default flags (standard login server, no device
// mode unless the environment forces it).
func runOnboardingLogin(ctx context.Context, errW io.Writer) error {
	loginServer, err := parseLoginServer(api.DefaultAuthBaseURL)
	if err != nil {
		return fmt.Errorf("resolve login server: %w", err)
	}
	client := auth.NewClient(loginServer, nil, false)
	startBrowser := func(ctx context.Context) (browserAuthFlow, error) {
		return client.StartBrowserAuth(ctx)
	}
	return runLoginAuto(ctx, os.Stdout, errW, client, startBrowser, openBrowser, loginFlowFacts{
		canPrompt:  interactive.CanPromptInteractively(),
		sshSession: isSSHSession(),
	})
}

// runOnboardingMirrorCreate is the mirror rung's offer: register a mirror for
// the origin repo on the default cluster and return once placement is
// confirmed. The initial clone continues server-side (`--no-wait` semantics),
// so enable never blocks on a large repo's clone.
func runOnboardingMirrorCreate(ctx context.Context, errW io.Writer, deps onboardingRungDeps) error {
	forge, owner, repo, err := deps.resolveOrigin(ctx)
	if err != nil {
		return fmt.Errorf("resolve origin: %w", err)
	}
	if forge != "gh" {
		return errors.New("origin is not a GitHub repository")
	}
	// parseGitHubURL semantics: server-side slugs are lowercase.
	owner, repo, err = parseGitHubURL(fmt.Sprintf("github.com/%s/%s", owner, repo))
	if err != nil {
		return fmt.Errorf("parse origin slug: %w", err)
	}
	client, err := coreapi.NewForCluster(ctx, defaultClusterHost)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", defaultClusterHost, err)
	}
	spin := startSpinner(errW, fmt.Sprintf("Mirroring %s/%s to %s", owner, repo, defaultClusterHost))
	outcome, err := createAndAwaitMirror(ctx, client, owner, repo, defaultClusterHost,
		true /* noWait: clone continues in the background */, 0, nil, nil)
	spin(err == nil)
	if err != nil {
		return err
	}
	if outcome.created != nil && !outcome.created.Empty {
		fmt.Fprintln(errW, "  Mirror registered — the initial clone continues in the background.")
	}
	return nil
}
