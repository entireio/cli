package codex

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// DiscoveredInstallCandidate reports the Codex-discovered hooks file to offer
// Entire's hooks to: one that belongs to a different project root than the
// current checkout and lacks Entire's current hooks. Malformed or unreadable
// files are not offered; the user has to fix those by hand.
func (d HookDiagnostics) DiscoveredInstallCandidate() (DiscoveredHooksPath, bool) {
	if d.Discovery.State != HookDiscoveryResolved || !d.PathsDiffer() {
		return DiscoveredHooksPath{}, false
	}
	switch d.Discovered.State {
	case HookFileAbsent, HookFileUserOnly:
		return d.Discovery.DiscoveredHooks, true
	case HookFileEntire:
		return d.Discovery.DiscoveredHooks, !d.Discovered.Current
	case HookFileMalformed, HookFileUnavailable:
		return DiscoveredHooksPath{}, false
	}
	return DiscoveredHooksPath{}, false
}

// InstallDiscoveredHooks merges Entire's Codex hooks into the hooks file Codex
// discovers for the current checkout, which in a linked worktree belongs to
// another checkout. Callers must have the user's explicit consent for
// consented, the path shown to them.
//
// Discovery is re-resolved here and must still name exactly consented, so a
// layout change between the prompt and the write cannot redirect it.
func InstallDiscoveredHooks(ctx context.Context, consented DiscoveredHooksPath) (int, error) {
	worktreeRoot, err := resolveWorktreeRoot(ctx)
	if err != nil {
		return 0, err
	}
	return installDiscoveredHooksFrom(ctx, worktreeRoot, consented)
}

func installDiscoveredHooksFrom(ctx context.Context, worktreeRoot string, consented DiscoveredHooksPath) (int, error) {
	discovery := resolveHookDiscovery(worktreeRoot)
	if discovery.State != HookDiscoveryResolved {
		if discovery.Diagnostic != nil {
			return 0, discovery.Diagnostic
		}
		return 0, errors.New("codex hook discovery is unresolved")
	}
	if consented.path == "" || discovery.DiscoveredHooks != consented {
		return 0, fmt.Errorf("codex hook discovery changed: now %q, consented to %q",
			discovery.DiscoveredHooks.Path(), consented.Path())
	}
	if err := validateMutableDiscoveredHookTarget(consented); err != nil {
		return 0, err
	}
	cfg, err := agent.OpenHookConfig(consented.root, (&CodexAgent{}).HookConfigRelPath())
	if err != nil {
		return 0, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}
	return installHooksInto(ctx, cfg, func() error { return validateMutableDiscoveredHookTarget(consented) }, false)
}
