package plugins

import (
	"context"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Process-wide plugin registry. Entire hook invocations are short-lived,
// single-purpose processes (one git hook or one lifecycle event per exec), so a
// lazily-built per-process registry is the right lifetime: it is constructed on
// the first FireHook call and reused for any subsequent fires in the same
// process. Package-level state is acceptable here because the registry models a
// process-global resource; access is guarded by globalMu.
var (
	globalMu   sync.Mutex
	globalReg  *Registry
	globalInit bool
)

// FireHook dispatches an observer hook to every enabled plugin. It lazily
// builds the process registry on first use from settings and the current
// worktree root. When no plugin is enabled it is a fast no-op, so seams can
// call it unconditionally without measurable cost in the common case.
//
// Firing is best-effort: plugin failures are logged and never propagated, so a
// misbehaving observer cannot break a lifecycle event or git hook.
func FireHook(ctx context.Context, hook string, payload map[string]any) {
	globalRegistry(ctx).FireObserver(ctx, hook, payload)
}

// Enabled reports whether any plugin is loaded in this process. Seams can use
// it to skip building an expensive payload when there are no subscribers.
func Enabled(ctx context.Context) bool {
	return globalRegistry(ctx).Len() > 0
}

// FireCommitMsg dispatches the prepare_commit_msg mutating hook and returns the
// trailer lines contributed by capable plugins (empty when none). See
// Registry.FireCommitMsg for semantics.
func FireCommitMsg(ctx context.Context, payload map[string]any) []string {
	return globalRegistry(ctx).FireCommitMsg(ctx, payload)
}

// FirePrePush dispatches the pre_push hook (observer + veto). Returns a non-nil
// error when a capable plugin vetoes the push. See Registry.FirePrePush.
func FirePrePush(ctx context.Context, payload map[string]any) error {
	return globalRegistry(ctx).FirePrePush(ctx, payload)
}

// RunCommand runs a plugin-contributed command by name, returning its exit code
// and whether a command was found. See Registry.RunCommand.
func RunCommand(ctx context.Context, name string, args []string) (exitCode int, found bool) {
	return globalRegistry(ctx).RunCommand(ctx, name, args)
}

// ProcessCommands returns the plugin-contributed commands available in this
// process, for listing/help.
func ProcessCommands(ctx context.Context) []CommandInfo {
	return globalRegistry(ctx).Commands()
}

func globalRegistry(ctx context.Context) *Registry {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalInit {
		return globalReg
	}
	globalInit = true
	globalReg = buildProcessRegistry(ctx)
	return globalReg
}

// buildProcessRegistry loads settings and discovers plugins for the current
// worktree. Any failure yields an empty (no-op) registry rather than an error:
// plugins are an optional, best-effort layer.
func buildProcessRegistry(ctx context.Context) *Registry {
	s, err := settings.Load(ctx)
	if err != nil || !hasEnabledPlugin(s) {
		return &Registry{}
	}
	worktreeRoot, _ := paths.WorktreeRoot(ctx) //nolint:errcheck // outside a repo only user plugins are considered; empty root is fine
	// localGrants comes from .entire/settings.local.json only; it is the sole
	// authority for enabling repo-local plugins so a committed team
	// settings.json cannot auto-run repo-shipped plugin code. On error we fail
	// closed (nil grants → repo-local plugins stay inert); user-global plugins
	// still resolve from the merged settings s.
	localGrants, lgErr := settings.LocalPluginGrants(ctx)
	if lgErr != nil {
		localGrants = nil
	}
	return Discover(ctx, worktreeRoot, s, localGrants)
}

// ResetProcessRegistryForTest clears the process registry so a test can rebuild
// it. Not for production use.
func ResetProcessRegistryForTest() {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalReg != nil {
		globalReg.Close()
	}
	globalReg = nil
	globalInit = false
}
