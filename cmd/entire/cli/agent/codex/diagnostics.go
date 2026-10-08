package codex

import "context"

// HookDiagnostics keeps current-checkout ownership separate from the
// read-only file Codex discovers.
type HookDiagnostics struct {
	Discovery       HookDiscovery
	WorktreeHooks   WorktreeHooksPath
	Worktree        HookConfigInspection
	Discovered      HookConfigInspection
	Trust           HookTrustInspection
	WorktreePathErr error
}

// InspectHookDiagnostics collects the local and effective Codex hook state
// without creating, rewriting, or removing either file.
func InspectHookDiagnostics(ctx context.Context) HookDiagnostics {
	diagnostics := HookDiagnostics{Discovery: ResolveHookDiscovery(ctx)}
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	return finishHookDiagnostics(ctx, diagnostics, worktreeHooks, err, false)
}

// InspectHookDiagnosticsLightweight collects only the checks safe to run from
// Codex's SessionStart hook. It avoids freshness probes and platform-specific
// command checks, which can delay the agent startup path.
func InspectHookDiagnosticsLightweight(ctx context.Context) HookDiagnostics {
	diagnostics := HookDiagnostics{Discovery: ResolveHookDiscovery(ctx)}
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	return finishHookDiagnostics(ctx, diagnostics, worktreeHooks, err, true)
}

func inspectHookDiagnosticsAt(ctx context.Context, worktreeRoot string) HookDiagnostics {
	diagnostics := HookDiagnostics{Discovery: resolveHookDiscovery(worktreeRoot)}
	worktreeHooks, err := resolveWorktreeHooksPath(worktreeRoot)
	return finishHookDiagnostics(ctx, diagnostics, worktreeHooks, err, false)
}

func finishHookDiagnostics(
	ctx context.Context,
	diagnostics HookDiagnostics,
	worktreeHooks WorktreeHooksPath,
	err error,
	lightweight bool,
) HookDiagnostics {
	if err != nil {
		diagnostics.WorktreePathErr = err
		diagnostics.Worktree = HookConfigInspection{State: HookFileUnavailable, Err: err}
	} else {
		diagnostics.WorktreeHooks = worktreeHooks
		if lightweight {
			diagnostics.Worktree = inspectWorktreeHookConfigLightweight(worktreeHooks)
		} else {
			diagnostics.Worktree = inspectWorktreeHookConfig(ctx, worktreeHooks)
		}
	}

	if diagnostics.Discovery.State != HookDiscoveryResolved {
		diagnostics.Discovered = HookConfigInspection{
			State: HookFileUnavailable,
			Err:   diagnostics.Discovery.Diagnostic,
		}
		return diagnostics
	}

	switch {
	case diagnostics.WorktreeHooks.Path() == diagnostics.Discovery.DiscoveredHooks.Path() && diagnostics.WorktreeHooks.Path() != "":
		diagnostics.Discovered = diagnostics.Worktree
	case lightweight:
		diagnostics.Discovered = inspectDiscoveredHookConfigLightweight(diagnostics.Discovery.DiscoveredHooks)
	default:
		diagnostics.Discovered = inspectDiscoveredHookConfig(ctx, diagnostics.Discovery.DiscoveredHooks)
	}
	if diagnostics.Discovery.ProjectLayerExists() && diagnostics.Discovered.State == HookFileEntire {
		diagnostics.Trust = inspectHookTrustForDeclared(
			diagnostics.Discovery.DiscoveredHooks.Path(),
			diagnostics.Discovered.Declared,
		)
	}
	return diagnostics
}

// PathsDiffer reports whether current-checkout mutation and Codex discovery
// refer to different hook files.
func (d HookDiagnostics) PathsDiffer() bool {
	return d.WorktreeHooks.Path() != "" &&
		d.Discovery.DiscoveredHooks.Path() != "" &&
		d.WorktreeHooks.Path() != d.Discovery.DiscoveredHooks.Path()
}
