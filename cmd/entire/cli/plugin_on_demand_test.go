package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestMaybeRunPlugin_MissingGraphNonInteractive(t *testing.T) { //nolint:paralleltest // isolates PATH and terminal detection
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ENTIRE_TEST_TTY", "0")
	// The managed dir is consulted before the prompt (see installMissingPlugin),
	// and it is NOT covered by the testdirs fallback — pluginParentDir reads
	// $ENTIRE_PLUGIN_DIR/$XDG_DATA_HOME and the home dir itself. Without this
	// the test reads the developer's real plugins and passes only on a machine
	// that happens not to have graph installed.
	withPluginDir(t)
	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code, _ := MaybeRunPlugin(t.Context(), root, []string{"graph", "search", "hello"})
	if !handled || code != 1 {
		t.Fatalf("handled=%v code=%d, want true, 1", handled, code)
	}
	if !strings.Contains(stderr.String(), "entire plugin install graph") {
		t.Fatalf("missing installation hint: %q", stderr.String())
	}
}

func TestMaybeRunPlugin_InstallGraphAndRun(t *testing.T) { //nolint:paralleltest // isolates environment and installer seam
	for _, tc := range []struct {
		name          string
		answer        string
		installErr    error
		cancelInstall bool
		pluginCode    int
		wantCode      int
		wantInstall   bool
		wantRun       bool
	}{
		{name: "enter accepts default yes", answer: "\n", wantInstall: true, wantRun: true},
		{name: "explicit yes preserves exit code", answer: "y\n", pluginCode: 42, wantCode: 42, wantInstall: true, wantRun: true},
		{name: "cancelled install stays quiet", answer: "y\n", cancelInstall: true, installErr: context.Canceled, wantCode: ExitPluginSignalled, wantInstall: true},
		{name: "cancelled dependency confirmation does not run", answer: "y\n", cancelInstall: true, wantCode: ExitPluginSignalled, wantInstall: true},
		{name: "EOF declines", answer: "", wantCode: 1},
		{name: "no cancels", answer: "n\n", wantCode: 1},
		{name: "failed install does not run", answer: "\n", installErr: errors.New("download failed"), wantCode: 1, wantInstall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// dir first so entire-graph is unresolvable, but git still
			// reachable: the index clone shells out to it.
			withIsolatedPath(t)
			withPathDir(t, dir)
			t.Setenv("ENTIRE_PLUGIN_DIR", filepath.Join(dir, "managed"))
			t.Setenv("ENTIRE_TEST_TTY", "1")
			t.Setenv("ACCESSIBLE", "1")
			t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "1")
			// The prompt names the repository the binary comes from, so the
			// entry has to resolve before it is shown. A local index keeps
			// that off the network — without it these tests would consult the
			// real published catalog.
			withIndexCache(t)
			indexURL, _ := newIndexRepo(t, `{"version":1,"plugins":[{"name":"graph","repo_url":"https://github.com/entireio/entire-graph"}]}`)
			t.Setenv(pluginIndexEnvVar, indexURL)
			interceptVersionCheck(t)
			argFile := filepath.Join(dir, "args.txt")
			sourceDir := t.TempDir()
			source := writePluginBinary(t, sourceDir, "entire-graph", argFile, tc.pluginCode)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			installCalls := 0
			original := onDemandPluginInstall
			onDemandPluginInstall = func(_ context.Context, cmd *cobra.Command, src installSource, flags remoteInstallFlags) error {
				installCalls++
				if tc.cancelInstall {
					cancel()
				}
				if src.Kind != installFromIndex || src.Ref != "graph" || flags != (remoteInstallFlags{}) {
					t.Fatalf("unexpected install request: %+v %+v", src, flags)
				}
				// The entry the prompt named travels with the request, so the
				// install cannot re-resolve into a different repository after
				// the user agreed to this one.
				if src.Resolved == nil || src.Resolved.RepoURL != "https://github.com/entireio/entire-graph" {
					t.Fatalf("install was not bound to the repository shown: %+v", src.Resolved)
				}
				if tc.installErr != nil {
					return tc.installErr
				}
				_, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: source})
				fmt.Fprintln(cmd.OutOrStdout(), "Installed graph")
				return err
			}
			t.Cleanup(func() { onDemandPluginInstall = original })
			root := newTestRoot()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			originalInput := openPluginPromptTerminal
			openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
				return pluginPromptTerminal{in: io.NopCloser(strings.NewReader(tc.answer))}, nil
			}
			t.Cleanup(func() { openPluginPromptTerminal = originalInput })
			data := strings.NewReader("plugin data\n")
			root.SetIn(data)
			args := []string{"graph", "search", "two words", "--json", "--", "$(untouched)", ""}
			handled, code, _ := MaybeRunPlugin(ctx, root, args)
			if !handled || code != tc.wantCode {
				t.Fatalf("handled=%v code=%d, want true, %d; stderr=%s", handled, code, tc.wantCode, &stderr)
			}
			if (installCalls == 1) != tc.wantInstall {
				t.Errorf("install calls=%d, want install=%v", installCalls, tc.wantInstall)
			}
			// The prompt names its source: this is the only human checkpoint
			// before a remote binary is downloaded and executed, and an
			// index-listed install never prompts inside runRemoteInstall.
			if !strings.Contains(stderr.String(), "Install the entire-graph plugin from https://github.com/entireio/entire-graph?") ||
				!strings.Contains(stderr.String(), "[Y/n]") {
				t.Errorf("prompt must name the repository and default to Yes: %q", stderr.String())
			}
			if data.Len() != len("plugin data\n") {
				t.Error("confirmation consumed plugin stdin")
			}
			if stdout.Len() != 0 {
				t.Errorf("installation polluted stdout: %q", stdout.String())
			}
			got, err := os.ReadFile(argFile)
			if tc.wantRun {
				if err != nil || string(got) != strings.Join(args[1:], "\n")+"\n" {
					t.Errorf("forwarded args=%q err=%v", got, err)
				}
			} else if !os.IsNotExist(err) {
				t.Errorf("plugin unexpectedly ran: args=%q err=%v", got, err)
			}
			if tc.cancelInstall && strings.Contains(stderr.String(), "context canceled") {
				t.Errorf("raw cancellation: %s", &stderr)
			}
			if tc.installErr != nil && !tc.cancelInstall && !strings.Contains(stderr.String(), tc.installErr.Error()) {
				t.Errorf("missing install failure: %q", stderr.String())
			}
		})
	}
}

func TestMaybeRunPlugin_GraphInstalledSkipsPrompt(t *testing.T) { //nolint:paralleltest // isolates PATH and version check
	dir := t.TempDir()
	argFile := filepath.Join(dir, "args.txt")
	writePluginBinary(t, dir, "entire-graph", argFile, 0)
	t.Setenv("PATH", dir)
	interceptVersionCheck(t)
	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code, _ := MaybeRunPlugin(t.Context(), root, []string{"graph", "--help"})
	if !handled || code != 0 || stderr.Len() != 0 {
		t.Fatalf("handled=%v code=%d stderr=%q", handled, code, stderr.String())
	}
}

func TestResolvePlugin_OnDemandEligibility(t *testing.T) { //nolint:paralleltest // isolates PATH
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{nil, {"--help"}, {"other-plugin"}, {"Graph"}, {"agent-graph"}, {"session", "graph"}} {
		if _, _, ok := resolvePlugin(newTestRoot(), args); ok {
			t.Errorf("unexpected plugin resolution for %q", args)
		}
	}
	root := newTestRoot()
	root.AddCommand(&cobra.Command{Use: "graph"})
	if _, _, ok := resolvePlugin(root, []string{"graph", "search"}); ok {
		t.Fatal("built-in graph must take precedence over on-demand installation")
	}
}

// A managed entry PATH cannot reach is the case installMissingPlugin's return
// contract already promised: run it. Offering to install over it dead-ended,
// because an existing install needs --force and the on-demand path passes
// none — so the user answered Yes, waited for the index and metadata fetches,
// and got "already installed; use --force to replace".
func TestMaybeRunPlugin_GraphInManagedDirIsRunNotReinstalled(t *testing.T) { //nolint:paralleltest // isolates PATH and managed plugins
	withIsolatedPluginEnv(t)
	interceptVersionCheck(t)
	binDir, err := EnsurePluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	argFile := filepath.Join(t.TempDir(), "args.txt")
	writePluginBinary(t, binDir, "entire-graph", argFile, 0)
	// Deliberately NOT on PATH: this is the managed bin dir that could not be
	// prepended at startup.
	if _, lookErr := exec.LookPath("entire-graph"); lookErr == nil {
		t.Fatal("precondition: entire-graph must not resolve through PATH")
	}

	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")
	originalTerminal := openPluginPromptTerminal
	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		t.Error("an already-installed plugin must not prompt for installation")
		return pluginPromptTerminal{in: io.NopCloser(strings.NewReader("n\n"))}, nil
	}
	t.Cleanup(func() { openPluginPromptTerminal = originalTerminal })
	originalInstall := onDemandPluginInstall
	onDemandPluginInstall = func(context.Context, *cobra.Command, installSource, remoteInstallFlags) error {
		t.Error("an already-installed plugin must not be reinstalled")
		return nil
	}
	t.Cleanup(func() { onDemandPluginInstall = originalInstall })

	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code, _ := MaybeRunPlugin(t.Context(), root, []string{"graph", "search", "hello"})
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d, want true, 0; stderr=%s", handled, code, &stderr)
	}
	got, err := os.ReadFile(argFile)
	if err != nil || string(got) != "search\nhello\n" {
		t.Fatalf("managed entry did not run with the forwarded args: %q %v", got, err)
	}
	// The announcement names the binary and nothing else: the arguments are
	// the user's own command line, and echoing them back would carry whatever
	// they hold (a token, a newline, a terminal escape) into stderr.
	if !strings.Contains(stderr.String(), "Running entire-graph\n") {
		t.Errorf("plugin was not announced: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "hello") {
		t.Errorf("arguments were echoed back: %q", stderr.String())
	}
}

// Arguments never reach stderr, whatever they contain. A terminal escape in
// one could reposition the cursor or repaint the lines above it — the hazard
// hasTerminalControlChars guards for index entries — and a flag value could be
// a token that then lands in any log capturing stderr.
func TestMaybeRunPlugin_AnnouncementNeverEchoesArguments(t *testing.T) { //nolint:paralleltest // isolates PATH and managed plugins
	withIsolatedPluginEnv(t)
	interceptVersionCheck(t)
	binDir, err := EnsurePluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	writePluginBinary(t, binDir, "entire-graph", filepath.Join(t.TempDir(), "args.txt"), 0)

	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	args := []string{"graph", "--token", "s3cr3t", "\x1b[1A\x1b[2Kforged", "line\nbreak"}
	if handled, code, _ := MaybeRunPlugin(t.Context(), root, args); !handled || code != 0 {
		t.Fatalf("handled=%v code=%d; stderr=%s", handled, code, &stderr)
	}
	if !strings.Contains(stderr.String(), "Running entire-graph\n") {
		t.Errorf("plugin was not announced: %q", stderr.String())
	}
	for _, leaked := range []string{"s3cr3t", "\x1b", "forged", "line\nbreak"} {
		if strings.Contains(stderr.String(), leaked) {
			t.Errorf("argument content %q reached stderr: %q", leaked, stderr.String())
		}
	}
}

// A managed entry Lstat reports but exec cannot use — the local-dev symlink
// whose target moved — is neither run nor offered for installation: exec'ing
// it fails with a fork/exec ENOENT naming a path the user never chose, and
// prompting dead-ends on the already-installed guard. Both are replaced by a
// message that says what is broken and how to repair it.
func TestMaybeRunPlugin_BrokenManagedEntryReportsARemedy(t *testing.T) { //nolint:paralleltest // isolates PATH and managed plugins
	withIsolatedPluginEnv(t)
	interceptVersionCheck(t)
	binDir, err := EnsurePluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(binDir, "entire-graph")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone", "entire-graph"), entry); err != nil {
		t.Fatal(err)
	}
	if found, ferr := FindInstalledPlugin("graph"); ferr != nil || found == nil {
		t.Fatalf("precondition: a dangling entry must still be listed: %v %v", found, ferr)
	}

	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")
	originalTerminal := openPluginPromptTerminal
	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		t.Error("a broken entry must not be answered with an install prompt")
		return pluginPromptTerminal{in: io.NopCloser(strings.NewReader("n\n"))}, nil
	}
	t.Cleanup(func() { openPluginPromptTerminal = originalTerminal })
	originalInstall := onDemandPluginInstall
	onDemandPluginInstall = func(context.Context, *cobra.Command, installSource, remoteInstallFlags) error {
		t.Error("a broken entry must not be silently reinstalled over")
		return nil
	}
	t.Cleanup(func() { onDemandPluginInstall = originalInstall })

	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code, _ := MaybeRunPlugin(t.Context(), root, []string{"graph", "search"})
	if !handled || code != 1 {
		t.Fatalf("handled=%v code=%d, want true, 1; stderr=%s", handled, code, &stderr)
	}
	for _, want := range []string{
		entry,
		"cannot be run",
		"points at a file that no longer exists",
		"entire plugin install graph --force",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("missing %q in the diagnosis: %q", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "use --force to replace") {
		t.Errorf("fell through to the already-installed dead end: %q", stderr.String())
	}
}

// The remedy is offered only for conditions a reinstall repairs, so it cannot
// be hung off an error it would not resolve. Both identified conditions are
// repairable; the unidentified branch (a stat failure that is not ENOENT) is
// left to review, since staging one means breaking permissions on the managed
// directory, which breaks its discovery first and exercises the wrong path.
func TestCheckManagedPluginRunnable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runnable := filepath.Join(dir, "entire-ok")
	if err := os.WriteFile(runnable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "entire-dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	asDir := filepath.Join(dir, "entire-dir")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name             string
		path             string
		wantErr          string
		wantReinstallFix bool
	}{
		{name: "regular file", path: runnable},
		{name: "dangling symlink", path: dangling, wantErr: "points at a file that no longer exists", wantReinstallFix: true},
		{name: "directory", path: asDir, wantErr: "it is a directory", wantReinstallFix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reinstallFixes, err := checkManagedPluginRunnable(tc.path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err=%v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if reinstallFixes != tc.wantReinstallFix {
				t.Errorf("reinstallFixes=%v, want %v", reinstallFixes, tc.wantReinstallFix)
			}
		})
	}
}

// A name the index does not carry cannot be installed, so it must not be
// offered. Asking first and failing afterwards spends the user's Yes on a
// question that never had an answer.
func TestMaybeRunPlugin_UnlistedNameIsNotOffered(t *testing.T) { //nolint:paralleltest // isolates PATH, index cache and terminal detection
	withIsolatedPluginEnv(t)
	withIndexCache(t)
	indexURL, _ := newIndexRepo(t, `{"version":1,"plugins":[{"name":"other","repo_url":"https://example.invalid/entire-other"}]}`)
	t.Setenv(pluginIndexEnvVar, indexURL)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")
	originalTerminal := openPluginPromptTerminal
	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		t.Error("an unlisted plugin must not be offered for installation")
		return pluginPromptTerminal{in: io.NopCloser(strings.NewReader("y\n"))}, nil
	}
	t.Cleanup(func() { openPluginPromptTerminal = originalTerminal })

	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code, _ := MaybeRunPlugin(t.Context(), root, []string{"graph"})
	if !handled || code != 1 {
		t.Fatalf("handled=%v code=%d, want true, 1; stderr=%s", handled, code, &stderr)
	}
	if !strings.Contains(stderr.String(), "not listed in the plugin index") {
		t.Errorf("missing diagnosis: %q", stderr.String())
	}
}
