package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

// A failing plugin's outcome travels back to main as a PluginExitError, so
// agent-help exits with the plugin's own code — or re-raises the signal that
// killed it — instead of a plain 1, and prints nothing over its stderr.
func TestAgentHelpDelegation_CarriesThePluginsOutcome(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	if runtime.GOOS == windowsGOOS {
		t.Skip("plugin shell-script harness only runs on Unix")
	}
	dir := t.TempDir()
	writeExecutableScript(t, filepath.Join(dir, "entire-failing"), "#!/bin/sh\nexit 3\n")
	writeExecutableScript(t, filepath.Join(dir, "entire-signaller"), "#!/bin/sh\ntrap - TERM\nkill -TERM $$\n")
	withPathDir(t, dir)

	for _, tc := range []struct {
		name     string
		plugin   string
		wantCode int
		wantSig  os.Signal
	}{
		{"exit code", "failing", 3, nil},
		{"killed by its own signal", "signaller", ExitPluginSignalled, syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) { // no t.Parallel: the parent set PATH
			handled, err := maybeDelegateAgentHelpToPlugin(context.Background(), newTestRoot(), []string{tc.plugin}, false)
			if !handled {
				t.Fatal("expected the plugin to handle the request")
			}
			var exitErr *PluginExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("err = %v (%T), want *PluginExitError", err, err)
			}
			if exitErr.Code != tc.wantCode || exitErr.KilledBy != tc.wantSig {
				t.Errorf("outcome = (code %d, signal %v), want (code %d, signal %v)", exitErr.Code, exitErr.KilledBy, tc.wantCode, tc.wantSig)
			}
			if !exitErr.AlreadyPrinted() {
				t.Error("the plugin's stderr is the message; main must not print another")
			}
		})
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

// main restores PATH before Cobra runs, so the managed bin dir is not on it
// when agent-help executes; plugins from `entire plugin install` must still
// be found, and PATH must be restored afterwards.
func TestAgentHelpDelegation_FindsManagedInstall(t *testing.T) { //nolint:paralleltest // mutates PATH and ENTIRE_PLUGIN_DIR via t.Setenv
	pluginDir := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", pluginDir)
	binDir := filepath.Join(pluginDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argFile := filepath.Join(t.TempDir(), "args.txt")
	writePluginBinary(t, binDir, "entire-pgr", argFile, 0)
	pathBefore := os.Getenv("PATH")

	handled, err := maybeDelegateAgentHelpToPlugin(context.Background(), newTestRoot(), []string{"pgr"}, false)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v, want the managed plugin to answer", handled, err)
	}
	if got := readPluginArgv(t, argFile); got != "agent-help" {
		t.Errorf("plugin argv = %q, want %q", got, "agent-help")
	}
	if got := os.Getenv("PATH"); got != pathBefore {
		t.Errorf("PATH not restored: got %q, want %q", got, pathBefore)
	}
}
