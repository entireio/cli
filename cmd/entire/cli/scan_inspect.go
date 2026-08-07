package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"golang.org/x/sync/errgroup"
)

// repoScanEntry is one repository's row in a scan, and one element of the
// `--json` report. Field names are the JSON contract.
type repoScanEntry struct {
	Path                   string   `json:"path"`
	SetUp                  bool     `json:"set_up"`
	Enabled                bool     `json:"enabled"`
	GitHooksInstalled      bool     `json:"git_hooks_installed"`
	AgentsHooked           []string `json:"agents_hooked"`
	AgentsDetectedUnhooked []string `json:"agents_detected_unhooked"`
	HooksOutdated          []string `json:"hooks_outdated,omitempty"`
	CodexTrustGaps         []string `json:"codex_trust_gaps,omitempty"`
	LinkedWorktree         bool     `json:"linked_worktree,omitempty"`
	Error                  string   `json:"error,omitempty"`
}

// needsAttention reports whether the repo has something `entire scan --fix`
// could improve: an agent in use without hooks, stale hook config, or a repo
// that was set up and then disabled.
func (e repoScanEntry) needsAttention() bool {
	return len(e.AgentsDetectedUnhooked) > 0 ||
		len(e.HooksOutdated) > 0 ||
		(e.SetUp && !e.Enabled)
}

// scanRepoContext derives the per-repo context every inspection runs under.
//
// Both overrides are required and they are not redundant: paths.WithWorktreeRoot
// redirects the path helpers each agent uses to locate its config (which is what
// makes AreHooksInstalled/DetectPresence work against another repo without any
// per-agent change), while settings.WithWorktreeRoot additionally redirects the
// clone-local preferences layer that settings.Load merges from the git dir.
//
// Attach this to per-repo work only. Installing it on the command's root context
// would silently redirect every unrelated path lookup for the rest of the run.
func scanRepoContext(ctx context.Context, root string) context.Context {
	return settings.WithWorktreeRoot(paths.WithWorktreeRoot(ctx, root), root)
}

// inspectRepoForScan evaluates one repository's Entire enablement. It never
// returns an error: a repo that cannot be fully inspected is reported with its
// Error field set so one bad directory cannot abort a scan over ~/dev.
func inspectRepoForScan(ctx context.Context, cand scanCandidate) repoScanEntry {
	repoCtx := scanRepoContext(ctx, cand.Root)

	entry := repoScanEntry{
		Path:              cand.Root,
		SetUp:             settings.IsSetUpAny(repoCtx),
		Enabled:           settings.IsSetUpAndEnabled(repoCtx),
		GitHooksInstalled: strategy.IsGitHookInstalledInDir(repoCtx, cand.Root),
		LinkedWorktree:    cand.LinkedWorktree,
	}

	entry.AgentsHooked, entry.AgentsDetectedUnhooked, entry.Error = scanAgentStates(repoCtx)

	if claudecode.CheckHookConfig(repoCtx) == claudecode.HooksOutdated {
		entry.HooksOutdated = append(entry.HooksOutdated, string(agent.AgentNameClaudeCode))
	}
	entry.CodexTrustGaps = codex.HookTrustGaps(cand.Root)

	return entry
}

// scanAgentStates splits the registered agents into "hooks installed here" and
// "configured here but not hooked", returning the first detection error (if
// any) as a message.
//
// An agent only reaches the unhooked list if it is detectably in use in the
// repo. Note that Codex's DetectPresence is defined as AreHooksInstalled, so
// Codex can never appear as present-but-unhooked — a Codex repo without Entire
// hooks is indistinguishable, to us, from a repo that does not use Codex.
func scanAgentStates(repoCtx context.Context) (hooked, unhooked []string, errMsg string) {
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if hs, ok := agent.AsHookSupport(ag); ok && hs.AreHooksInstalled(repoCtx) {
			hooked = append(hooked, string(name))
			continue
		}
		present, err := ag.DetectPresence(repoCtx)
		if err != nil {
			if errMsg == "" {
				errMsg = "detecting " + string(name) + ": " + err.Error()
			}
			continue
		}
		if present {
			unhooked = append(unhooked, string(name))
		}
	}
	sort.Strings(hooked)
	sort.Strings(unhooked)
	return hooked, unhooked, errMsg
}

// inspectReposForScan inspects every candidate concurrently, preserving the
// caller's (sorted) order.
func inspectReposForScan(ctx context.Context, candidates []scanCandidate) ([]repoScanEntry, error) {
	entries := make([]repoScanEntry, len(candidates))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(scanConcurrencyLimit)
	for i, cand := range candidates {
		group.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err //nolint:wrapcheck // context error propagated verbatim to abort the scan
			}
			entries[i] = inspectRepoForScan(groupCtx, cand)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("inspecting repositories: %w", err)
	}
	return entries, nil
}
