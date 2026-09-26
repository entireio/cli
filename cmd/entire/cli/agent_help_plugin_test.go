package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readPluginArgv returns the argv a writePluginBinary stand-in recorded.
func readPluginArgv(t *testing.T, argFile string) string {
	t.Helper()
	data, err := os.ReadFile(argFile)
	if err != nil {
		t.Fatalf("plugin did not run (no argv recorded): %v", err)
	}
	return strings.TrimSpace(string(data))
}

// `agent-help <plugin> ...` runs `entire-<plugin> agent-help ...`, forwarding
// the remaining path and --json, which Cobra consumed as agent-help's own flag.
func TestAgentHelpDelegatesToPlugin(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	argFile := filepath.Join(dir, "args.txt")
	writePluginBinary(t, dir, "entire-pgr", argFile, 0)
	withPathDir(t, dir)

	for _, tc := range []struct {
		name   string
		args   []string
		asJSON bool
		want   string
	}{
		{"bare", []string{"pgr"}, false, "agent-help"},
		{"subcommand path", []string{"pgr", "sync", "now"}, false, "agent-help\nsync\nnow"},
		{"json", []string{"pgr", "sync"}, true, "agent-help\nsync\n--json"},
	} {
		t.Run(tc.name, func(t *testing.T) { // no t.Parallel: the parent set PATH
			handled, err := maybeDelegateAgentHelpToPlugin(context.Background(), newTestRoot(), tc.args, tc.asJSON)
			if !handled || err != nil {
				t.Fatalf("handled=%v err=%v, want handled with no error", handled, err)
			}
			if got := readPluginArgv(t, argFile); got != tc.want {
				t.Errorf("plugin argv = %q, want %q", got, tc.want)
			}
		})
	}
}

// A failing plugin fails agent-help, silently: the plugin's stderr already
// said why, so main must not print a second message.
func TestAgentHelpDelegation_PluginFailureIsSilentError(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	writePluginBinary(t, dir, "entire-pgr", filepath.Join(dir, "args.txt"), 3)
	withPathDir(t, dir)

	handled, err := maybeDelegateAgentHelpToPlugin(context.Background(), newTestRoot(), []string{"pgr"}, false)
	if !handled {
		t.Fatal("expected the plugin to handle the request")
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("err = %v (%T), want *SilentError", err, err)
	}
}

// Everything that is not a runnable external command falls through to
// runAgentHelp, and the stand-in must never run.
func TestAgentHelpDelegation_FallsThrough(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	argFile := filepath.Join(dir, "args.txt")
	// A plugin shadowing a built-in, and one using the agent-protocol prefix.
	writePluginBinary(t, dir, "entire-session", argFile, 0)
	writePluginBinary(t, dir, "entire-agent-foo", argFile, 0)
	withPathDir(t, dir)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"built-in wins", []string{"session", "list"}},
		{"another built-in", []string{"agent"}},
		{"reserved agent protocol name", []string{"agent-foo"}},
		{"not installed", []string{"no-such-plugin-anywhere"}},
		{"path traversal", []string{"../pgr"}},
	} {
		t.Run(tc.name, func(t *testing.T) { // no t.Parallel: the parent set PATH
			handled, err := maybeDelegateAgentHelpToPlugin(context.Background(), newTestRoot(), tc.args, false)
			if handled || err != nil {
				t.Errorf("handled=%v err=%v, want fall-through", handled, err)
			}
		})
	}
	if _, err := os.Stat(argFile); err == nil {
		t.Errorf("a stand-in ran; argv: %q", readPluginArgv(t, argFile))
	}
}

// A missing on-demand plugin is offered for installation by the dispatcher;
// a help lookup must not start a download, so it reads as unknown instead.
func TestAgentHelpDelegation_DoesNotOfferOnDemandInstall(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())

	handled, err := maybeDelegateAgentHelpToPlugin(context.Background(), newTestRoot(), []string{onDemandInstallPluginNames[0]}, false)
	if handled || err != nil {
		t.Errorf("handled=%v err=%v, want fall-through for a missing on-demand plugin", handled, err)
	}
}
