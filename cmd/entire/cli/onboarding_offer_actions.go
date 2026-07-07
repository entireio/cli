package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

// runOnboardingImport is the import rung's offer, composing the enable-time
// import machinery from PR #1595: agent-scoped discovery, the optional
// per-agent picker, and best-effort per-agent imports (checkpoint policy and
// redaction config are enforced inside runSelectedImports). granular=true
// (step-by-step mode) shows the picker — Import/Skip confirm for one agent,
// multi-select for several, empty selection skips. granular=false imports
// everything eligible: the "set up everything" and --yes paths, where consent
// was already given.
func runOnboardingImport(ctx context.Context, w io.Writer, granular bool) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	eligible := sessionImportDiscover(ctx, installedImportAgents(ctx), repoRoot)
	if len(eligible) == 0 {
		return nil
	}
	selected := eligible
	if granular {
		selected, err = sessionImportPrompt(ctx, w, eligible)
		if err != nil {
			return fmt.Errorf("import selection: %w", err)
		}
		if len(selected) == 0 {
			return nil
		}
	}
	sessionImportRun(ctx, w, repoRoot, selected)
	return nil
}

// installedImportAgents resolves the agents whose hooks are installed — the
// ladder's import scope. (#1595 scoped to the just-selected agents; the
// ladder runs on resume paths too, where "installed" is the equivalent set.)
func installedImportAgents(ctx context.Context) []agent.Agent {
	names := GetAgentsWithHooksInstalled(ctx)
	out := make([]agent.Agent, 0, len(names))
	for _, name := range names {
		if ag, err := agent.Get(name); err == nil {
			out = append(out, ag)
		}
	}
	return out
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
