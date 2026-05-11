package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// writePluginBinary writes a shell script that records argv to argFile.
// Skips the calling test on Windows.
func writePluginBinary(t *testing.T, dir, name, argFile string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == windowsGOOS {
		t.Skip("plugin shell-script harness only runs on Unix")
	}
	path := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit %d\n", argFile, exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write plugin %s: %v", path, err)
	}
	return path
}

func withPathDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "entire"}
	root.AddCommand(&cobra.Command{Use: "session", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(&cobra.Command{Use: "agent", Run: func(*cobra.Command, []string) {}})
	return root
}

func TestResolvePlugin_FoundOnPath(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	binPath := writePluginBinary(t, dir, "entire-pgr", filepath.Join(dir, "args.txt"), 0)
	withPathDir(t, dir)

	got, args, ok := resolvePlugin(newTestRoot(), []string{"pgr", "--flag", "value"})
	if !ok {
		t.Fatal("expected plugin to resolve")
	}
	if got != binPath {
		t.Errorf("binPath: got %q, want %q", got, binPath)
	}
	if want := []string{"--flag", "value"}; !equalStrings(args, want) {
		t.Errorf("plugin args: got %v, want %v", args, want)
	}
}

func TestResolvePlugin_BuiltinWins(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	writePluginBinary(t, dir, "entire-session", filepath.Join(dir, "args.txt"), 0)
	withPathDir(t, dir)

	if _, _, ok := resolvePlugin(newTestRoot(), []string{"session", "list"}); ok {
		t.Fatal("built-in 'session' must take precedence over entire-session plugin")
	}
}

func TestResolvePlugin_NotFound(t *testing.T) {
	t.Parallel()
	if _, _, ok := resolvePlugin(newTestRoot(), []string{"nope-no-such-plugin"}); ok {
		t.Fatal("missing plugin must not resolve")
	}
}

// Cobra registers `help` and `completion` lazily, inside Execute. The plugin
// resolver runs before Execute, so it must prime those commands before
// consulting Find — otherwise an entire-help / entire-completion binary on
// PATH would shadow the built-in, violating "built-ins always win."
func TestResolvePlugin_BuiltinHelpWins(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	writePluginBinary(t, dir, "entire-help", filepath.Join(dir, "args.txt"), 0)
	withPathDir(t, dir)

	// Use a Cobra-style root that mirrors NewRootCmd: SetHelpCommand only
	// stashes the help command on the struct — it is not in the tree until
	// InitDefaultHelpCmd runs.
	root := newTestRoot()
	root.SetHelpCommand(&cobra.Command{Use: "help"})

	if _, _, ok := resolvePlugin(root, []string{"help"}); ok {
		t.Fatal("built-in 'help' must take precedence over entire-help plugin")
	}
	if _, _, ok := resolvePlugin(root, []string{"help", "session"}); ok {
		t.Fatal("'help session' must route to built-in help, not entire-help plugin")
	}
}

func TestResolvePlugin_BuiltinCompletionWins(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	writePluginBinary(t, dir, "entire-completion", filepath.Join(dir, "args.txt"), 0)
	withPathDir(t, dir)

	if _, _, ok := resolvePlugin(newTestRoot(), []string{"completion", "bash"}); ok {
		t.Fatal("built-in 'completion' must take precedence over entire-completion plugin")
	}
}

func TestResolvePlugin_RejectsAgentPrefix(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	dir := t.TempDir()
	writePluginBinary(t, dir, "entire-agent-foo", filepath.Join(dir, "args.txt"), 0)
	withPathDir(t, dir)

	if _, _, ok := resolvePlugin(newTestRoot(), []string{"agent-foo"}); ok {
		t.Fatal("agent-foo must not resolve as a passthrough plugin")
	}
}

func TestIsAgentProtocolBinary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"/usr/local/bin/entire-agent-foo", true},
		{"/usr/local/bin/entire-agent-foo.exe", true},
		{"/usr/local/bin/entire-pgr", false},
		{"entire-pgr", false},
		{"entire-agent-bar.bat", true},
	}
	for _, tc := range cases {
		if got := isAgentProtocolBinary(tc.path); got != tc.want {
			t.Errorf("isAgentProtocolBinary(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestResolvePlugin_FlagAsFirstArg(t *testing.T) {
	t.Parallel()
	if _, _, ok := resolvePlugin(newTestRoot(), []string{"--help"}); ok {
		t.Fatal("flags must not trigger external-command lookup")
	}
}

func TestResolvePlugin_NonExecutableSurfacesAsLaunchError(t *testing.T) { //nolint:paralleltest // mutates PATH via t.Setenv
	if runtime.GOOS == windowsGOOS {
		t.Skip("executable bit semantics tested on Unix only")
	}
	dir := t.TempDir()
	// Same script body as writePluginBinary but mode 0o644 (not executable).
	path := filepath.Join(dir, "entire-bad")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withPathDir(t, dir)

	got, _, ok := resolvePlugin(newTestRoot(), []string{"bad"})
	if !ok {
		t.Fatal("non-executable plugin must surface as a launch failure, not a fall-through")
	}
	if got != path {
		t.Errorf("binPath: got %q, want %q", got, path)
	}
}

func TestResolvePlugin_PathTraversal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../evil", `..\evil`, "foo/bar"} {
		if _, _, ok := resolvePlugin(newTestRoot(), []string{name}); ok {
			t.Errorf("name %q must not resolve", name)
		}
	}
}

func TestRunPlugin_ExitCodePropagation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binPath := writePluginBinary(t, dir, "entire-exit42", filepath.Join(dir, "args.txt"), 42)

	code := runPlugin(context.Background(), "exit42", binPath, []string{"a", "b"})
	if code != 42 {
		t.Errorf("exit code: got %d, want 42", code)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("read argfile: %v", err)
	}
	if got := strings.TrimSpace(string(contents)); got != "a\nb" {
		t.Errorf("argv: got %q, want %q", got, "a\nb")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
