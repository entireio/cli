package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
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
	// probeMirror asks the control plane whether owner/repo is mirrored.
	probeMirror func(ctx context.Context, owner, repo string) (bool, error)
	// discoverImports reports per-agent local history discoverable for this
	// repo and how much of it has not been imported yet. Local-only.
	discoverImports func(ctx context.Context) ([]agentImportStatus, error)
}

// agentImportStatus summarizes one agent's importable history for this repo.
type agentImportStatus struct {
	Agent           string
	Sessions        int
	UnimportedTurns int
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

// probeRepoMirrored asks the active-context control plane whether owner/repo
// already has a mirror. Owner/repo arrive lowercased from the mirror rung.
func probeRepoMirrored(ctx context.Context, owner, repo string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, mirrorProbeTimeout)
	defer cancel()
	client, err := coreapi.New()
	if err != nil {
		return false, err
	}
	out, err := client.ListAvailableMirrors(ctx, coreapi.ListAvailableMirrorsParams{
		Owner: coreapi.NewOptString(owner),
	})
	if err != nil {
		return false, err
	}
	for _, m := range out.Available {
		if strings.EqualFold(m.Owner, owner) && strings.EqualFold(m.Repo, repo) {
			return m.Status == coreapi.AvailableMirrorStatusMirrored, nil
		}
	}
	return false, nil
}

// discoverAgentImports reports, per registered importer, how much local
// transcript history exists for this repo and how much of it has never been
// imported. Discovery is file globbing; only agents with discoverable
// sessions pay for the dry-run (which opens the checkpoint store to detect
// already-imported turns). Local-only, best-effort per agent.
func discoverAgentImports(ctx context.Context) ([]agentImportStatus, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	now := time.Now()
	var statuses []agentImportStatus
	for _, imp := range agentimport.All() {
		files, discoverErr := imp.Discover(repoRoot, "", now, nil)
		if discoverErr != nil || len(files) == 0 {
			continue
		}
		repo, openErr := openRepository(ctx)
		if openErr != nil {
			return nil, openErr
		}
		res, runErr := agentimport.Run(ctx, repo, imp, agentimport.Options{
			RepoRoot: repoRoot, Now: now, DryRun: true,
		})
		_ = repo.Close()
		if runErr != nil {
			continue
		}
		statuses = append(statuses, agentImportStatus{
			Agent:           imp.Name(),
			Sessions:        res.SessionsScanned,
			UnimportedTurns: res.TurnsImported,
		})
	}
	return statuses, nil
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
			if err != nil || forge != "gh" {
				return onboarding.Check{State: onboarding.StateNotApplicable, Detail: "no GitHub origin"}
			}
			// Server-persisted mirror slugs are lowercase (parseGitHubURL semantics).
			slug := "github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(repo)
			if !deps.authed(ctx) {
				return onboarding.Check{
					State:  onboarding.StateBlocked,
					Detail: "needs login",
					Hint:   "entire auth login",
				}
			}
			mirrored, err := deps.probeMirror(ctx, strings.ToLower(owner), strings.ToLower(repo))
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

func importRung(deps onboardingRungDeps) onboarding.Rung {
	return onboarding.Rung{
		Key:   onboarding.KeyImport,
		Title: "History",
		Check: func(ctx context.Context) onboarding.Check {
			statuses, err := deps.discoverImports(ctx)
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
			parts := make([]string, 0, len(unimported))
			unimportedSessions := 0
			for _, s := range unimported {
				parts = append(parts, fmt.Sprintf("%d %s", s.Sessions, s.Agent))
				unimportedSessions += s.Sessions
			}
			detail := strings.Join(parts, ", ") + " sessions found, not imported"
			if unimportedSessions == 1 {
				detail = strings.Join(parts, ", ") + " session found, not imported"
			}
			return onboarding.Check{
				State:  onboarding.StateMissing,
				Detail: detail,
				Hint:   "entire import " + unimported[0].Agent,
			}
		},
	}
}

func authRung(deps onboardingRungDeps) onboarding.Rung {
	return onboarding.Rung{
		Key:   onboarding.KeyAuth,
		Title: "Logged in",
		Check: func(context.Context) onboarding.Check {
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
				if token, tokenErr := deps.tokenForContext(c); tokenErr == nil && token != "" {
					return onboarding.Check{State: onboarding.StateDone, Detail: c.Handle}
				}
				break
			}
			return onboarding.Check{State: onboarding.StateMissing, Hint: "entire auth login"}
		},
	}
}
