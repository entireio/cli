package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/spf13/cobra"
)

func TestParseDispatchFlags_ServerReposAreAllowed(t *testing.T) {
	t.Parallel()

	opts, err := parseDispatchFlags(
		&cobra.Command{},
		false,
		"7d",
		"",
		false,
		[]string{"entireio/cli", "entireio/entire.io"},
		"",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(opts.RepoPaths); got != 2 {
		t.Fatalf("expected 2 repo slugs, got %d", got)
	}
	if opts.Mode != 0 {
		t.Fatalf("expected server mode, got %v", opts.Mode)
	}
	if opts.Branches != nil {
		t.Fatalf("expected nil branches for cloud default-branch mode, got %v", opts.Branches)
	}
	if opts.AllBranches {
		t.Fatal("did not expect all branches for cloud default-branch mode")
	}
}

func TestParseDispatchFlags_NormalizesRepoScopeValues(t *testing.T) {
	t.Parallel()

	opts, err := parseDispatchFlags(
		&cobra.Command{},
		false,
		"7d",
		"",
		false,
		[]string{" entireio/cli ", "", "entireio/cli", " otherco/service ", "   "},
		"",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opts.RepoPaths, ","); got != "entireio/cli,otherco/service" {
		t.Fatalf("expected normalized repo scope, got %q", got)
	}
	if opts.Branches != nil {
		t.Fatalf("expected nil branches for repo scope, got %v", opts.Branches)
	}
}

func TestParseDispatchFlags_LocalRejectsRepos(t *testing.T) {
	t.Parallel()

	_, err := parseDispatchFlags(
		&cobra.Command{},
		true,
		"7d",
		"",
		false,
		[]string{"entireio/cli"},
		"",
		false,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "--repos cannot be used with --local" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseDispatchFlags_CloudRejectsAllBranches(t *testing.T) {
	t.Parallel()

	_, err := parseDispatchFlags(
		&cobra.Command{},
		false,
		"7d",
		"",
		true,
		[]string{"entireio/cli"},
		"",
		false,
	)
	if err == nil {
		t.Fatal("expected error for --all-branches in cloud mode")
	}
	if !strings.Contains(err.Error(), "--all-branches only applies to --local") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseDispatchFlags_CloudCapsReposAtFive(t *testing.T) {
	t.Parallel()

	repos := []string{"a/b", "c/d", "e/f", "g/h", "i/j", "k/l"}
	_, err := parseDispatchFlags(
		&cobra.Command{},
		false,
		"7d",
		"",
		false,
		repos,
		"",
		false,
	)
	if err == nil {
		t.Fatal("expected error for too many repos")
	}
	if !strings.Contains(err.Error(), "supports at most 5") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseDispatchFlags_LocalAllBranchesFlag(t *testing.T) {
	t.Parallel()

	opts, err := parseDispatchFlags(
		&cobra.Command{},
		true,
		"7d",
		"",
		true,
		nil,
		"",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.AllBranches {
		t.Fatal("expected all branches to propagate")
	}
	if opts.Branches != nil {
		t.Fatalf("expected nil branches, got %v", opts.Branches)
	}
}

func TestParseDispatchFlags_InsecureHTTPAuthFlag(t *testing.T) {
	t.Parallel()

	opts, err := parseDispatchFlags(
		&cobra.Command{},
		false,
		"7d",
		"",
		false,
		[]string{"entireio/cli"},
		"",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.InsecureHTTPAuth {
		t.Fatal("expected InsecureHTTPAuth=true to propagate into Options")
	}
}

func TestNewDispatchCmd_InsecureHTTPAuthFlagIsHidden(t *testing.T) {
	t.Parallel()

	cmd := newDispatchCmd()
	flag := cmd.Flags().Lookup("insecure-http-auth")
	if flag == nil {
		t.Fatal("expected --insecure-http-auth flag to be registered")
	}
	if !flag.Hidden {
		t.Fatal("expected --insecure-http-auth flag to be hidden")
	}
}

func TestNewDispatchCmd_LocalHelpText(t *testing.T) {
	t.Parallel()

	cmd := newDispatchCmd()
	flag := cmd.Flags().Lookup("local")
	if flag == nil {
		t.Fatal("expected --local flag to be registered")
	}
	want := "generate via the locally-installed agent CLI instead of the Entire server"
	if flag.Usage != want {
		t.Fatalf("unexpected --local help text: %q", flag.Usage)
	}
}

func TestShouldRunDispatchWizard(t *testing.T) {
	t.Parallel()

	if !shouldRunDispatchWizard(0, true, true) {
		t.Fatal("expected wizard to run when stdin and stdout are terminals with no flags")
	}
	if shouldRunDispatchWizard(0, false, true) {
		t.Fatal("expected wizard not to run when stdin is piped")
	}
	if shouldRunDispatchWizard(1, true, true) {
		t.Fatal("expected wizard not to run when flags are provided")
	}
}

func TestNewDispatchCmd_NonTerminalPrintsPlainMarkdown(t *testing.T) {
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	oldMarkdown := renderDispatchMarkdown
	runDispatch = func(_ context.Context, _ dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		return &dispatchpkg.Dispatch{GeneratedText: "generated dispatch"}, nil
	}
	dispatchTerminalMode = func(_ io.Writer) bool { return false }
	renderDispatchMarkdown = func(dispatch *dispatchpkg.Dispatch) string {
		if dispatch.GeneratedText != "generated dispatch" {
			t.Fatalf("unexpected dispatch: %+v", dispatch)
		}
		return testDispatchGeneratedMarkdown
	}
	t.Cleanup(func() {
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
		renderDispatchMarkdown = oldMarkdown
	})

	cmd := newDispatchCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--repos", "entireio/cli"})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != testDispatchGeneratedMarkdown {
		t.Fatalf("unexpected stdout: %q", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("unexpected stderr: %q", got)
	}
}

func TestNewDispatchCmd_TerminalUsesInteractiveRenderer(t *testing.T) {
	oldTerminalMode := dispatchTerminalMode
	oldInteractive := runInteractiveDispatch
	oldGlow := renderTerminalMarkdown
	dispatchTerminalMode = func(_ io.Writer) bool { return true }
	runInteractiveDispatch = func(_ context.Context, _ io.Writer, _ dispatchpkg.Options) (string, error) {
		return testDispatchGeneratedMarkdown, nil
	}
	renderTerminalMarkdown = func(_ io.Writer, markdown string) (string, error) {
		if markdown != testDispatchGeneratedMarkdown {
			t.Fatalf("unexpected markdown: %q", markdown)
		}
		return "glow output\n", nil
	}
	t.Cleanup(func() {
		dispatchTerminalMode = oldTerminalMode
		runInteractiveDispatch = oldInteractive
		renderTerminalMarkdown = oldGlow
	})

	cmd := newDispatchCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--repos", "entireio/cli"})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "glow output\n" {
		t.Fatalf("unexpected stdout: %q", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("unexpected stderr: %q", got)
	}
}

func TestNewDispatchCmd_AccessibleModeSkipsInteractiveRenderer(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")

	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	oldInteractive := runInteractiveDispatch
	oldGlow := renderTerminalMarkdown
	oldMarkdown := renderDispatchMarkdown
	runDispatch = func(_ context.Context, _ dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		return &dispatchpkg.Dispatch{GeneratedText: "generated dispatch"}, nil
	}
	dispatchTerminalMode = func(_ io.Writer) bool { return true }
	runInteractiveDispatch = func(_ context.Context, _ io.Writer, _ dispatchpkg.Options) (string, error) {
		t.Fatal("did not expect interactive renderer in accessible mode")
		return "", nil
	}
	renderTerminalMarkdown = func(_ io.Writer, _ string) (string, error) {
		t.Fatal("did not expect terminal markdown renderer in accessible mode")
		return "", nil
	}
	renderDispatchMarkdown = func(dispatch *dispatchpkg.Dispatch) string {
		if dispatch.GeneratedText != "generated dispatch" {
			t.Fatalf("unexpected dispatch: %+v", dispatch)
		}
		return testDispatchGeneratedMarkdown
	}
	t.Cleanup(func() {
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
		runInteractiveDispatch = oldInteractive
		renderTerminalMarkdown = oldGlow
		renderDispatchMarkdown = oldMarkdown
	})

	cmd := newDispatchCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--repos", "entireio/cli"})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != testDispatchGeneratedMarkdown {
		t.Fatalf("unexpected stdout: %q", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("unexpected stderr: %q", got)
	}
}
