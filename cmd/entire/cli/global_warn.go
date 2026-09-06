package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// globalWarnMarkerName marks that the current enabled generation has been
// announced. Generations are observational (hand-edits bypass every writer):
// observed-off deletes the marker; the next observed-enabled command warns.
const globalWarnMarkerName = "global_warn_ack"

func globalWarnMarkerPath() string {
	return filepath.Join(userdirs.Config(), globalWarnMarkerName)
}

// globalPostRun is the root PersistentPostRun hook for the global tier: the
// one-time detection warning plus user-hook reconciliation, sharing a single
// read of the user settings file. Unreadable settings stay silent — doctor is
// that failure's surface, and a warn here would fire on every command forever.
// The installer (curl-bash-post-install) calls it explicitly because hidden
// commands skip the root post-run; `entire trust` is experimental and so
// hidden in stable builds — it does not trigger the post-run there, which is
// harmless (the next visible command does).
func globalPostRun(ctx context.Context, errW io.Writer) {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		return
	}
	maybeWarnGlobalTracking(ctx, us, errW)
	reconcileUserHooks(ctx, us, errW)
}

// maybeWarnGlobalTracking is the foreground detection warn. The marker holds
// the announced generation — "enabled" or "enabled+trust_all" — so flipping
// trust_all on while already enabled re-warns with the wider "captured AND
// synced" copy instead of staying silent.
func maybeWarnGlobalTracking(ctx context.Context, us *settings.UserSettings, errW io.Writer) {
	acked, statErr := os.ReadFile(globalWarnMarkerPath())
	markerPresent := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		// Treated as marker-absent: can only over-warn, never suppress.
		logging.Debug(ctx, "global warn marker unreadable; treating as absent", slog.String("error", statErr.Error()))
	}
	generation := globalWarnGeneration(us)
	switch {
	case us.GlobalEnabled() && (!markerPresent || strings.TrimSpace(string(acked)) != generation):
		fmt.Fprintln(errW, globalTrackingWarnText(us))
		ackGlobalWarnMarker(ctx, generation)
	case !us.GlobalEnabled() && markerPresent:
		// Off-detection: a hand-edited disable still owes the held-data note.
		if err := os.Remove(globalWarnMarkerPath()); err != nil {
			return // marker survived; retry (and print) on a later command
		}
		fmt.Fprintln(errW, "Global tracking is off; locally captured checkpoints in untrusted repos will not sync.")
	}
}

// globalWarnGeneration names the enabled state the warning describes.
func globalWarnGeneration(us *settings.UserSettings) string {
	if us.Global != nil && us.Global.TrustAll {
		return "enabled+trust_all"
	}
	return "enabled"
}

// ackGlobalWarnMarker records which enabled generation the detection warning
// announced. Best-effort: a failed write only re-warns.
func ackGlobalWarnMarker(ctx context.Context, generation string) {
	if err := os.MkdirAll(userdirs.Config(), 0o700); err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
		return
	}
	if err := os.WriteFile(globalWarnMarkerPath(), []byte(generation+"\n"), 0o600); err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
	}
}

// globalTrackingWarnText picks the warn copy: under trust_all the per-repo
// "sync only after `entire trust`" sentence would lie, so warn capture+sync.
func globalTrackingWarnText(us *settings.UserSettings) string {
	file := settings.UserSettingsPath()
	if us.Global != nil && us.Global.TrustAll {
		return fmt.Sprintf("Warning: global tracking is enabled (%s) — agent sessions in every repo on this machine are captured AND synced (trust_all is enabled). See `entire status` for this repo.", file)
	}
	return fmt.Sprintf("Warning: global tracking is enabled (%s) — agent sessions in every repo on this machine are now captured locally. Checkpoints sync per repo only after `entire trust`. See `entire status` for this repo.", file)
}

// reconcileUserHooks keeps user-level agent hooks in step with the global
// tier: installed while global tracking is enabled (for agents that are
// present on this machine or already carry an Entire entry), removed when
// the tier is configured but disabled. This is how a hand edit of the user
// settings file takes effect without a dedicated command — the next
// foreground `entire` invocation wires or unwires the hooks and says so.
// Unconfigured tier: nothing (zero cost for users who never opted in).
// Hook processes never reach this: the root post-run skips hidden commands
// (`hooks`, `hooks git`); userHookMutationSuppressed is defense in depth.
func reconcileUserHooks(ctx context.Context, us *settings.UserSettings, errW io.Writer) {
	if us == nil || !us.GlobalConfigured() || userHookMutationSuppressed() {
		return
	}
	supports, _ := agent.UserHookSupports()
	for _, candidate := range supports {
		installed, err := candidate.Support.AreUserHooksInstalled(ctx)
		if err != nil {
			continue // doctor reports unreadable agent configs
		}
		switch {
		case us.GlobalEnabled() && !installed && userHookAgentPresent(candidate.Name):
			if _, err := candidate.Support.InstallUserHooks(ctx); err != nil {
				fmt.Fprintf(errW, "Note: could not install %s user-level hooks for global tracking: %v\n", candidate.Name, err)
				continue
			}
			fmt.Fprintf(errW, "entire: installed user-level %s hooks (global tracking is on)\n", candidate.Name)
		case !us.GlobalEnabled() && installed:
			if err := candidate.Support.UninstallUserHooks(ctx); err != nil {
				fmt.Fprintf(errW, "Note: could not remove %s user-level hooks: %v\n", candidate.Name, err)
				continue
			}
			fmt.Fprintf(errW, "entire: removed user-level %s hooks (global tracking is off)\n", candidate.Name)
		}
	}
}

// userHookMutationSuppressed reports that this process is an agent hook: the
// hidden-command walk in root.go already keeps globalPostRun out of hook
// processes, so this is defense in depth for any future direct caller.
func userHookMutationSuppressed() bool {
	return currentHookAgentName != ""
}

// userHookLookPath is exec.LookPath, swappable so tests can pretend an agent
// binary is (or is not) installed.
var userHookLookPath = exec.LookPath

// userHookAgentPresent: the agent binary is on PATH, or its user config
// already contains an Entire entry (a previous install we should keep current).
func userHookAgentPresent(name types.AgentName) bool {
	binaries := map[types.AgentName]string{agent.AgentNameClaudeCode: "claude", agent.AgentNameGemini: "gemini"}
	if bin, ok := binaries[name]; ok {
		if _, err := userHookLookPath(bin); err == nil {
			return true
		}
	}
	return userHookConfigContainsEntire(name)
}

func userHookConfigContainsEntire(name types.AgentName) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	var path string
	switch name {
	case agent.AgentNameClaudeCode:
		path = filepath.Join(home, ".claude", "settings.json")
	case agent.AgentNameGemini:
		path = filepath.Join(home, ".gemini", "settings.json")
	default:
		return false
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed per-user agent settings location
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte("entire hooks")) || bytes.Contains(data, []byte("entire-dev"))
}
