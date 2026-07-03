package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/internal/coreapi"
)

// runOnboardingLogin is the auth rung's offer: the same browser-first login
// `entire login` runs, with default flags (standard login server, no device
// mode unless the environment forces it). w is enable's output writer.
func runOnboardingLogin(ctx context.Context, w io.Writer) error {
	loginServer, err := parseLoginServer(api.DefaultAuthBaseURL)
	if err != nil {
		return fmt.Errorf("resolve login server: %w", err)
	}
	client := auth.NewClient(loginServer, nil, false)
	startBrowser := func(ctx context.Context) (browserAuthFlow, error) {
		return client.StartBrowserAuth(ctx)
	}
	return runLoginAuto(ctx, w, w, client, startBrowser, openBrowser, loginFlowFacts{
		canPrompt:  interactive.CanPromptInteractively(),
		sshSession: isSSHSession(),
	})
}

// runOnboardingImport is the import rung's offer: import discoverable agent
// history as read-only checkpoints, same flow as `entire import <agent>`
// (checkpoint policy honored, repo/user redaction config loaded before any
// write). Minimal by design: all agents, all sessions, no per-agent
// selection. PR #1595 explores per-agent selection for this offer; if
// adopted it must keep the ladder's re-offer-on-re-enable resume semantics.
func runOnboardingImport(ctx context.Context, w io.Writer) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()
	if err := ensureCheckpointPolicyAllowsCheckpointData(ctx, repo); err != nil {
		return err
	}
	strategy.EnsureRedactionConfigured()

	now := time.Now()
	for _, imp := range agentimport.All() {
		files, discoverErr := imp.Discover(repoRoot, "", now, nil)
		if discoverErr != nil || len(files) == 0 {
			continue
		}
		res, runErr := agentimport.Run(ctx, repo, imp, agentimport.Options{
			RepoRoot: repoRoot, Now: now,
		})
		if runErr != nil {
			return fmt.Errorf("import %s: %w", imp.Name(), runErr)
		}
		fmt.Fprintf(w, "  Imported %d turn(s) from %d %s session(s) (%d already imported).\n",
			res.TurnsImported, res.SessionsScanned, imp.Name(), res.TurnsSkipped)
	}
	return nil
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
	owner, repo = githubSlug(owner, repo)
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
	return finalizeMirrorOffer(errW, outcome, owner+"/"+repo, func(slug string) {
		// Write-through: the placement is registered, so the probe cache can
		// say "mirrored" immediately instead of waiting for the server-side
		// clone to surface in the available-mirrors list.
		defaultMirrorProbeCache().put(slug, true, time.Now())
	})
}

// finalizeMirrorOffer turns a nil-error createAndAwaitMirror outcome into the
// offer's result. A suspended placement returns nil error from the create
// path but will never serve — report it like `entire repo mirror create`
// does, return an error so the rung keeps its retry hint, and skip the
// cache write-through that would otherwise claim "mirrored" for the TTL.
func finalizeMirrorOffer(w io.Writer, outcome mirrorCreateOutcome, slug string, cachePut func(slug string)) error {
	created := outcome.created
	if created == nil {
		return errors.New("mirror placement not registered")
	}
	if created.Suspended {
		fmt.Fprintln(w, "  WARNING: this mirror has been suspended by an admin and won't be usable.")
		return errMirrorSuspended
	}
	if !created.Empty {
		fmt.Fprintln(w, "  Mirror registered — the initial clone continues in the background.")
	}
	cachePut(slug)
	return nil
}
