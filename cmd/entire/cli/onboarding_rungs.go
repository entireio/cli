package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/onboarding"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

// mirrorProbeTimeout bounds the control-plane round-trip when `entire status`
// or `entire enable` checks whether this repo is mirrored, so an offline
// terminal degrades to StateUnknown instead of hanging.
const mirrorProbeTimeout = 5 * time.Second

// onboardingRungDeps carries the ground-truth probes the setup rungs check.
// Production wiring lives in newOnboardingRungDeps; tests inject fakes so rung
// logic runs without keyring, network, or filesystem access.
type onboardingRungDeps struct {
	installedAgents func(ctx context.Context) []string
	envToken        func() string
	listContexts    func() ([]*contexts.Context, string, error)
	tokenForContext func(c *contexts.Context) (string, error)
	// resolveOrigin parses the origin remote into (forge, owner, repo);
	// forge is "gh" for github.com (gitremote.ResolveRemoteRepo semantics).
	resolveOrigin func(ctx context.Context) (forge, owner, repo string, err error)
	// authed reports whether a usable login exists, without prompting.
	authed func(ctx context.Context) bool
	// probeMirror reports whether owner/repo is mirrored on any reachable
	// core (production: cached, multi-core — see probeRepoMirrored).
	probeMirror func(ctx context.Context, owner, repo string) (bool, error)
	// discoverImports reports per-agent local history discoverable for this
	// repo and how much of it has not been imported yet. Local-only.
	discoverImports func(ctx context.Context) ([]agentImportStatus, error)
}

// agentImportStatus summarizes one agent's importable history for this repo.
type agentImportStatus struct {
	Agent string `json:"agent"`
	// Sessions is every discovered session, imported or not.
	Sessions        int `json:"sessions"`
	UnimportedTurns int `json:"unimported_turns"`
	ImportedTurns   int `json:"imported_turns"`
}

// newOnboardingRungDeps wires the rung probes to their real backends: agent
// hook detection, the stored auth context (offline — deliberately not
// RefreshedLoginToken, which can hit the network), origin parsing, the
// control-plane mirror list, and local agent-transcript discovery.
func newOnboardingRungDeps() onboardingRungDeps {
	deps := onboardingRungDeps{
		installedAgents: InstalledAgentDisplayNames,
		envToken:        func() string { return os.Getenv(auth.EnvTokenVar) },
		listContexts:    auth.Contexts,
		tokenForContext: auth.LoginTokenForContext,
		resolveOrigin: func(ctx context.Context) (string, string, string, error) {
			return gitremote.ResolveRemoteRepo(ctx, "origin")
		},
		probeMirror:     probeRepoMirrored,
		discoverImports: discoverAgentImports,
	}
	deps.authed = func(ctx context.Context) bool {
		return authRung(deps).Check(ctx).State == onboarding.StateDone
	}
	return deps
}

// onboardingLadder is the canonical setup ladder, in offer order.
func onboardingLadder(deps onboardingRungDeps) onboarding.Ladder {
	return onboarding.Ladder{hooksRung(deps), authRung(deps), mirrorRung(deps), importRung(deps)}
}

// githubSlug is the single encoding of the "server-side mirror slugs are
// lowercase" invariant (parseGitHubURL semantics) — used for probe queries,
// the probe-cache key, and the create write-through, which must never fork
// on case.
func githubSlug(owner, repo string) (slugOwner, slugRepo string) {
	return strings.ToLower(owner), strings.ToLower(repo)
}

// availableMirrorLister is the subset of *coreapi.Client's methods the mirror
// probe needs; a seam so probeMirrorAcross is testable without a control plane.
type availableMirrorLister interface {
	ListAvailableMirrors(ctx context.Context, params coreapi.ListAvailableMirrorsParams) (*coreapi.ListAvailableMirrorsOutputBody, error)
	CoreOrigin() string
}

// probeMirrorAcross asks each distinct core (deduped by origin) whether
// owner/repo is mirrored. A mirror found on any core counts: the active
// context's core and the default cluster the create offer targets can front
// different federations, and a mirror created on one is invisible to the
// other — probing only one would leave the rung re-offering creation
// forever. Errors matter only when every core is unreachable.
func probeMirrorAcross(ctx context.Context, clients []availableMirrorLister, owner, repo string) (bool, error) {
	seen := map[string]bool{}
	var lastErr error
	answered := false
	for _, client := range clients {
		if client == nil || seen[client.CoreOrigin()] {
			continue
		}
		seen[client.CoreOrigin()] = true
		out, err := client.ListAvailableMirrors(ctx, coreapi.ListAvailableMirrorsParams{
			Owner: coreapi.NewOptString(owner),
		})
		if err != nil {
			lastErr = err
			continue
		}
		answered = true
		for _, m := range out.Available {
			if strings.EqualFold(m.Owner, owner) && strings.EqualFold(m.Repo, repo) &&
				m.Status == coreapi.AvailableMirrorStatusMirrored {
				return true, nil
			}
		}
	}
	if !answered {
		if lastErr == nil {
			lastErr = errors.New("no control-plane core reachable")
		}
		return false, fmt.Errorf("probe mirrors: %w", lastErr)
	}
	return false, nil
}

// probeRepoMirrored checks whether owner/repo has a mirror on the active
// context's core or the default cluster's core, consulting a short-TTL
// per-user cache first so hot paths (`entire status`) don't pay a network
// round-trip on every run. Failures are cached briefly too, so an offline
// terminal doesn't hang on every invocation. Owner/repo arrive lowercased
// from the mirror rung.
func probeRepoMirrored(ctx context.Context, owner, repo string) (bool, error) {
	slug := owner + "/" + repo
	cache := defaultMirrorProbeCache()
	now := time.Now()
	if mirrored, unreachable, ok := cache.get(slug, now); ok {
		if unreachable {
			return false, errors.New("control plane unreachable (cached)")
		}
		return mirrored, nil
	}
	ctx, cancel := context.WithTimeout(ctx, mirrorProbeTimeout)
	defer cancel()
	var clients []availableMirrorLister
	if activeCore, err := coreapi.New(); err == nil {
		clients = append(clients, activeCore)
	}
	if clusterCore, err := coreapi.NewForCluster(ctx, defaultClusterHost); err == nil {
		clients = append(clients, clusterCore)
	}
	mirrored, err := probeMirrorAcross(ctx, clients, owner, repo)
	if err != nil {
		cache.putUnreachable(slug, now)
		return false, err
	}
	cache.put(slug, mirrored, now)
	return mirrored, nil
}

// discoverAgentImports reports, per registered importer, how much local
// transcript history exists for this repo and how much of it has never been
// imported. Discovery is file globbing; the expensive dry-run (checkpoint
// store open + transcript parsing) is memoized behind a fingerprint of the
// discovered files and the metadata-branch tip, so an unchanged repo pays
// only the glob on hot paths like `entire status`. Local-only, best-effort
// per agent.
func discoverAgentImports(ctx context.Context) ([]agentImportStatus, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	now := time.Now()
	type discovery struct {
		imp   agentimport.Importer
		files []agentimport.SessionFile
	}
	var discovered []discovery
	var inputs []importScanInput
	cacheable := true
	discoverFailures := 0
	// Scope to agents with hooks installed — the same set the import offer
	// acts on. Iterating every registered importer would report Missing for
	// history the offer will never import, an unresolvable checklist row.
	for _, ag := range installedImportAgents(ctx) {
		imp := importerForAgent(ag)
		if imp == nil {
			continue
		}
		files, discoverErr := imp.Discover(repoRoot, "", now, nil)
		if discoverErr != nil {
			// Best-effort per agent, but an error is not "nothing found":
			// don't memoize a result computed without this agent's files.
			logging.Warn(ctx, "onboarding: transcript discovery failed", "agent", imp.Name(), "error", discoverErr)
			discoverFailures++
			cacheable = false
			continue
		}
		if len(files) == 0 {
			continue
		}
		discovered = append(discovered, discovery{imp: imp, files: files})
		for _, f := range files {
			info, statErr := os.Stat(f.Path)
			if statErr != nil {
				// A file vanished between glob and stat — don't trust a
				// fingerprint built from a moving target.
				cacheable = false
				continue
			}
			inputs = append(inputs, importScanInput{Path: f.Path, ModTime: info.ModTime(), Size: info.Size()})
		}
	}
	if len(discovered) == 0 {
		if discoverFailures > 0 {
			// Every answer we have is an error — "no prior history found"
			// would be an affirmatively false settled state.
			return nil, fmt.Errorf("transcript discovery failed for %d agent(s)", discoverFailures)
		}
		return nil, nil
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return nil, err
	}
	defer repo.Close()

	// The offer's import runner refuses to write under a restrictive
	// checkpoint policy; the rung must agree, or it reports Missing forever
	// while every consented offer silently skips.
	if policyErr := ensureCheckpointPolicyAllowsCheckpointData(ctx, repo); policyErr != nil {
		return nil, fmt.Errorf("%w: %w", errImportsPolicyRestricted, policyErr)
	}

	fingerprint := importScanFingerprint(inputs, metadataBranchTip(repo))
	cache := defaultImportScanCache()
	if cacheable {
		if statuses, ok := cache.get(repoRoot, fingerprint); ok {
			return statuses, nil
		}
	}

	var statuses []agentImportStatus
	runFailures := 0
	for _, d := range discovered {
		res, runErr := agentimport.Run(ctx, repo, d.imp, agentimport.Options{
			RepoRoot: repoRoot, Now: now, DryRun: true,
		})
		if runErr != nil {
			// Best-effort per agent, but a partial result must not be
			// memoized — it would stay wrong until the fingerprint moves.
			logging.Warn(ctx, "onboarding: import dry-run failed", "agent", d.imp.Name(), "error", runErr)
			runFailures++
			cacheable = false
			continue
		}
		statuses = append(statuses, agentImportStatus{
			Agent:           d.imp.Name(),
			Sessions:        res.SessionsScanned,
			UnimportedTurns: res.TurnsImported,
			ImportedTurns:   res.TurnsSkipped,
		})
	}
	if runFailures == len(discovered) {
		// Every scan failed (typically one repo-level cause): report the
		// failure so the rung renders Unknown instead of a false
		// "no prior history found" / "N sessions imported".
		return nil, fmt.Errorf("import scan failed for all %d agent(s) with history", runFailures)
	}
	if cacheable {
		cache.put(repoRoot, fingerprint, statuses)
	}
	return statuses, nil
}

// metadataBranchTip identifies the local checkpoint-metadata state for the
// import-scan fingerprint: any new checkpoint or import moves the ref.
func metadataBranchTip(repo *git.Repository) string {
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		return "none"
	}
	return ref.Hash().String()
}

func hooksRung(deps onboardingRungDeps) onboarding.Rung {
	return onboarding.Rung{
		Key:   onboarding.KeyHooks,
		Title: "Agent hooks",
		Check: func(ctx context.Context) onboarding.Check {
			agents := deps.installedAgents(ctx)
			if len(agents) == 0 {
				return onboarding.Check{State: onboarding.StateMissing, Hint: "entire enable"}
			}
			return onboarding.Check{State: onboarding.StateDone, Detail: strings.Join(agents, ", ")}
		},
	}
}

func mirrorRung(deps onboardingRungDeps) onboarding.Rung {
	return onboarding.Rung{
		Key:   onboarding.KeyMirror,
		Title: "Repo mirrored",
		Check: func(ctx context.Context) onboarding.Check {
			forge, owner, repo, err := deps.resolveOrigin(ctx)
			if err != nil {
				// A resolution failure (git exec error, canceled context) is
				// not the same fact as "this repo isn't on GitHub".
				logging.Debug(ctx, "onboarding: origin resolution failed", "error", err)
				return onboarding.Check{State: onboarding.StateUnknown, Hint: "entire repo mirror list"}
			}
			if forge != "gh" {
				return onboarding.Check{State: onboarding.StateNotApplicable, Detail: "no GitHub origin"}
			}
			owner, repo = githubSlug(owner, repo)
			slug := "github.com/" + owner + "/" + repo
			if !deps.authed(ctx) {
				return onboarding.Check{
					State:  onboarding.StateBlocked,
					Detail: "needs login",
					Hint:   "entire auth login",
				}
			}
			mirrored, err := deps.probeMirror(ctx, owner, repo)
			if err != nil {
				return onboarding.Check{State: onboarding.StateUnknown, Hint: "entire repo mirror list"}
			}
			if mirrored {
				return onboarding.Check{State: onboarding.StateDone, Detail: slug}
			}
			return onboarding.Check{
				State:  onboarding.StateMissing,
				Detail: "commits won't appear in the web UI",
				Hint:   "entire repo mirror create " + slug,
			}
		},
	}
}

// errImportsPolicyRestricted marks a repo whose checkpoint policy forbids
// writing checkpoint data: importing is impossible here, not pending.
var errImportsPolicyRestricted = errors.New("checkpoint policy restricts imported history")

func importRung(deps onboardingRungDeps) onboarding.Rung {
	return onboarding.Rung{
		Key:   onboarding.KeyImport,
		Title: "History",
		Check: func(ctx context.Context) onboarding.Check {
			statuses, err := deps.discoverImports(ctx)
			if errors.Is(err, errImportsPolicyRestricted) {
				return onboarding.Check{
					State:  onboarding.StateNotApplicable,
					Detail: "imports restricted by checkpoint policy",
				}
			}
			if err != nil {
				return onboarding.Check{State: onboarding.StateUnknown, Hint: "entire import"}
			}
			totalSessions := 0
			var unimported []agentImportStatus
			for _, s := range statuses {
				totalSessions += s.Sessions
				if s.UnimportedTurns > 0 {
					unimported = append(unimported, s)
				}
			}
			if totalSessions == 0 {
				return onboarding.Check{State: onboarding.StateNotApplicable, Detail: "no prior history found"}
			}
			if len(unimported) == 0 {
				return onboarding.Check{
					State:  onboarding.StateDone,
					Detail: fmt.Sprintf("%d sessions imported", totalSessions),
				}
			}
			return onboarding.Check{
				State:  onboarding.StateMissing,
				Detail: unimportedDetail(unimported),
				Hint:   "entire import " + unimported[0].Agent,
			}
		},
	}
}

// unimportedDetail phrases the import rung's remaining work. When nothing was
// ever imported, session counts are accurate ("12 claude-code sessions found,
// not imported"). After a partial import, claiming every discovered session is
// unimported would overstate the work, so the wording switches to pending
// turns.
func unimportedDetail(unimported []agentImportStatus) string {
	agents := make([]string, 0, len(unimported))
	parts := make([]string, 0, len(unimported))
	sessions, pendingTurns, importedTurns := 0, 0, 0
	for _, s := range unimported {
		agents = append(agents, s.Agent)
		parts = append(parts, fmt.Sprintf("%d %s", s.Sessions, s.Agent))
		sessions += s.Sessions
		pendingTurns += s.UnimportedTurns
		importedTurns += s.ImportedTurns
	}
	if importedTurns > 0 {
		noun := "turns"
		if pendingTurns == 1 {
			noun = "turn"
		}
		return fmt.Sprintf("%s history partially imported (%d %s pending)",
			strings.Join(agents, ", "), pendingTurns, noun)
	}
	if sessions == 1 {
		return strings.Join(parts, ", ") + " session found, not imported"
	}
	return strings.Join(parts, ", ") + " sessions found, not imported"
}

func authRung(deps onboardingRungDeps) onboarding.Rung {
	return onboarding.Rung{
		Key:   onboarding.KeyAuth,
		Title: "Logged in",
		Check: func(ctx context.Context) onboarding.Check {
			if deps.envToken() != "" {
				return onboarding.Check{State: onboarding.StateDone, Detail: "using ENTIRE_TOKEN"}
			}
			ctxs, current, err := deps.listContexts()
			if err != nil {
				return onboarding.Check{State: onboarding.StateUnknown, Hint: "entire auth status"}
			}
			for _, c := range ctxs {
				if c.Name != current {
					continue
				}
				token, tokenErr := deps.tokenForContext(c)
				if tokenErr != nil {
					// A keyring/token-store failure is infrastructure, not
					// "not logged in" — offering a browser login would fail
					// against the same broken store.
					logging.Warn(ctx, "onboarding: token store unreadable", "context", c.Name, "error", tokenErr)
					return onboarding.Check{State: onboarding.StateUnknown, Hint: "entire auth status"}
				}
				if token != "" {
					return onboarding.Check{State: onboarding.StateDone, Detail: c.Handle}
				}
				break
			}
			return onboarding.Check{State: onboarding.StateMissing, Hint: "entire auth login"}
		},
	}
}
