package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

func TestNewDispatchCmd_AgentFlagHelpText(t *testing.T) {
	t.Parallel()

	cmd := newDispatchCmd()
	flag := cmd.Flags().Lookup("agent")
	if flag == nil {
		t.Fatal("expected --agent flag to be registered")
	}
	want := "local text-generation agent (requires --local)"
	if flag.Usage != want {
		t.Fatalf("unexpected --agent help text: %q", flag.Usage)
	}
	if modelFlag := cmd.Flags().Lookup("model"); modelFlag != nil {
		t.Fatal("did not expect --model flag to be registered")
	}
}

func TestNewDispatchCmd_LongHelpIncludesLocalAgentExample(t *testing.T) {
	t.Parallel()

	cmd := newDispatchCmd()
	if !strings.Contains(cmd.Long, "entire dispatch --local --agent codex") {
		t.Fatalf("long help missing local-agent example:\n%s", cmd.Long)
	}
}

func TestDispatchPreflight_InvalidTimeBeforeProvider(t *testing.T) {
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	providerCalled := false
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		providerCalled = true
		return nil, errors.New("provider must not run")
	}
	runDispatch = func(context.Context, dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		t.Fatal("dispatch must not run after preflight fails")
		return nil, errors.New("dispatch must not run")
	}
	t.Cleanup(func() {
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
	})

	cmd := newDispatchCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--local", "--all-branches", "--since", "not-a-time"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unparseable time") {
		t.Fatalf("expected invalid time preflight error, got %v", err)
	}
	if providerCalled {
		t.Fatal("provider resolution ran before invalid time preflight returned")
	}
}

func TestDispatchPreflight_RepositoryFailureBeforeProvider(t *testing.T) {
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	providerCalled := false
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		providerCalled = true
		return nil, errors.New("provider must not run")
	}
	runDispatch = func(context.Context, dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		t.Fatal("dispatch must not run after preflight fails")
		return nil, errors.New("dispatch must not run")
	}
	t.Cleanup(func() {
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
	})
	t.Chdir(t.TempDir())

	cmd := newDispatchCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{
		"--local", "--all-branches",
		"--since", "2026-07-16T12:00:00Z",
		"--until", "2026-07-17T12:00:00Z",
	})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not in a git repository") {
		t.Fatalf("expected repository preflight error, got %v", err)
	}
	if providerCalled {
		t.Fatal("provider resolution ran before repository preflight returned")
	}
}

func TestDispatchProvider_LocalRunsAfterPreflightAndInjectsOptions(t *testing.T) {
	oldPrepare := prepareLocalDispatch
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	oldMarkdown := renderDispatchMarkdown
	var calls []string
	generator := &stubTextAgent{}
	prepareLocalDispatch = func(_ context.Context, opts dispatchpkg.Options) (dispatchpkg.Options, error) {
		calls = append(calls, "prepare")
		return opts, nil
	}
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		calls = append(calls, "provider")
		return &checkpointSummaryProvider{TextGenerator: generator, Model: "ordered-model"}, nil
	}
	runDispatch = func(_ context.Context, opts dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		calls = append(calls, "dispatch")
		if opts.TextGenerator != generator || opts.Model != "ordered-model" {
			t.Fatalf("provider options not passed to dispatch: generator=%T model=%q", opts.TextGenerator, opts.Model)
		}
		return &dispatchpkg.Dispatch{}, nil
	}
	dispatchTerminalMode = func(io.Writer) bool { return false }
	renderDispatchMarkdown = func(*dispatchpkg.Dispatch) string { return "" }
	t.Cleanup(func() {
		prepareLocalDispatch = oldPrepare
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
		renderDispatchMarkdown = oldMarkdown
	})

	cmd := newDispatchCmd()
	cmd.SetArgs([]string{"--local", "--all-branches"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "prepare,provider,dispatch" {
		t.Fatalf("call order = %q, want prepare,provider,dispatch", got)
	}
}

func TestDispatchPreflight_CloudSkipsLocalPreparationAndProvider(t *testing.T) {
	oldPrepare := prepareLocalDispatch
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	prepareLocalDispatch = func(context.Context, dispatchpkg.Options) (dispatchpkg.Options, error) {
		t.Fatal("cloud dispatch must not run local preflight")
		return dispatchpkg.Options{}, errors.New("local preflight must not run")
	}
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		t.Fatal("cloud dispatch must not resolve a local provider")
		return nil, errors.New("provider must not run")
	}
	runDispatch = func(_ context.Context, opts dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		if opts.Mode != dispatchpkg.ModeServer {
			t.Fatalf("mode = %v, want server", opts.Mode)
		}
		return &dispatchpkg.Dispatch{}, nil
	}
	dispatchTerminalMode = func(io.Writer) bool { return false }
	t.Cleanup(func() {
		prepareLocalDispatch = oldPrepare
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
	})

	cmd := newDispatchCmd()
	cmd.SetArgs([]string{"--repos", "entireio/cli"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestNewDispatchCmd_CloudAgentFailsBeforeProviderOrDispatch(t *testing.T) {
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	unexpectedCallErr := errors.New("unexpected command dependency call")
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		t.Fatal("local provider resolution must not run for cloud --agent validation")
		return nil, unexpectedCallErr
	}
	runDispatch = func(context.Context, dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		t.Fatal("dispatch must not run after invalid cloud --agent validation")
		return nil, unexpectedCallErr
	}
	t.Cleanup(func() {
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
	})

	cmd := newDispatchCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--agent", string(agent.AgentNameCodex)})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected cloud --agent validation error")
	}
	want := "--agent only applies to --local (cloud dispatch uses Entire's server-side generator)"
	if err.Error() != want {
		t.Fatalf("unexpected error: %q", err)
	}
}

func TestNewDispatchCmd_CloudExplicitEmptyAgentUsesLocalOnlyErrorPrecedence(t *testing.T) {
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	providerCalled := false
	dispatchCalled := false
	unexpectedCallErr := errors.New("unexpected command dependency call")
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		providerCalled = true
		return nil, unexpectedCallErr
	}
	runDispatch = func(context.Context, dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		dispatchCalled = true
		return nil, unexpectedCallErr
	}
	t.Cleanup(func() {
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
	})

	cmd := newDispatchCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--agent="})

	err := cmd.Execute()
	want := "--agent only applies to --local (cloud dispatch uses Entire's server-side generator)"
	if err == nil || err.Error() != want {
		t.Fatalf("unexpected error: %v", err)
	}
	if providerCalled {
		t.Fatal("local provider resolution must not run for cloud --agent validation")
	}
	if dispatchCalled {
		t.Fatal("dispatch must not run after invalid cloud --agent validation")
	}
}

func TestNewDispatchCmd_LocalExplicitEmptyAgentFailsBeforeProviderOrDispatch(t *testing.T) {
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	providerCalled := false
	dispatchCalled := false
	unexpectedCallErr := errors.New("unexpected command dependency call")
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		providerCalled = true
		return nil, unexpectedCallErr
	}
	runDispatch = func(context.Context, dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		dispatchCalled = true
		return nil, unexpectedCallErr
	}
	t.Cleanup(func() {
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
	})

	for _, args := range [][]string{
		{"--local", "--all-branches", "--agent="},
		{"--local", "--all-branches", "--agent", "   "},
	} {
		providerCalled = false
		dispatchCalled = false
		cmd := newDispatchCmd()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs(args)

		err := cmd.Execute()
		if err == nil || err.Error() != "--agent requires a non-empty value" {
			t.Fatalf("args %q: unexpected error: %v", args, err)
		}
		if providerCalled {
			t.Fatalf("args %q: provider resolution must not run", args)
		}
		if dispatchCalled {
			t.Fatalf("args %q: dispatch must not run", args)
		}
	}
}

func TestNewDispatchCmd_LocalAgentInjectsProviderAndKeepsOutputSeparated(t *testing.T) {
	oldPrepare := prepareLocalDispatch
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	oldMarkdown := renderDispatchMarkdown
	generator := &stubTextAgent{}
	prepareLocalDispatch = func(_ context.Context, opts dispatchpkg.Options) (dispatchpkg.Options, error) {
		return opts, nil
	}
	resolveDispatchProvider = func(_ context.Context, w io.Writer, override string) (*checkpointSummaryProvider, error) {
		if override != string(agent.AgentNameCodex) {
			t.Fatalf("provider override = %q, want codex", override)
		}
		if _, err := io.WriteString(w, "provider notice\n"); err != nil {
			t.Fatal(err)
		}
		return &checkpointSummaryProvider{TextGenerator: generator, Model: "exact-model"}, nil
	}
	runDispatch = func(_ context.Context, opts dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		if opts.Mode != dispatchpkg.ModeLocal {
			t.Fatalf("mode = %v, want local", opts.Mode)
		}
		if opts.TextGenerator != generator {
			t.Fatalf("TextGenerator = %T, want raw provider generator", opts.TextGenerator)
		}
		if opts.Model != "exact-model" {
			t.Fatalf("Model = %q, want exact-model", opts.Model)
		}
		return &dispatchpkg.Dispatch{GeneratedText: "generated dispatch"}, nil
	}
	dispatchTerminalMode = func(io.Writer) bool { return false }
	renderDispatchMarkdown = func(*dispatchpkg.Dispatch) string { return testDispatchGeneratedMarkdown }
	t.Cleanup(func() {
		prepareLocalDispatch = oldPrepare
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
		renderDispatchMarkdown = oldMarkdown
	})

	cmd := newDispatchCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--local", "--all-branches", "--agent", "  " + string(agent.AgentNameCodex) + "  "})

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != testDispatchGeneratedMarkdown {
		t.Fatalf("unexpected stdout: %q", got)
	}
	if got := stderr.String(); got != "provider notice\n" {
		t.Fatalf("unexpected stderr: %q", got)
	}
}

func TestNewDispatchCmd_LocalWithoutAgentResolvesConfiguredProvider(t *testing.T) {
	oldPrepare := prepareLocalDispatch
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	prepareLocalDispatch = func(_ context.Context, opts dispatchpkg.Options) (dispatchpkg.Options, error) {
		return opts, nil
	}
	resolveDispatchProvider = func(_ context.Context, _ io.Writer, override string) (*checkpointSummaryProvider, error) {
		if override != "" {
			t.Fatalf("provider override = %q, want empty", override)
		}
		return &checkpointSummaryProvider{TextGenerator: &stubTextAgent{}}, nil
	}
	runDispatch = func(context.Context, dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		return &dispatchpkg.Dispatch{}, nil
	}
	dispatchTerminalMode = func(io.Writer) bool { return false }
	t.Cleanup(func() {
		prepareLocalDispatch = oldPrepare
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
	})

	cmd := newDispatchCmd()
	cmd.SetArgs([]string{"--local", "--all-branches"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchWizard_LocalWithoutConfiguredAgentPromptsAndPersistsSelection(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	oldShouldRunWizard := shouldRunDispatchWizardForCommand
	oldRunWizard := runDispatchWizardForCommand
	oldLoad := loadSummarySettings
	oldLoadFile := loadSummarySettingsFromFile
	oldSave := saveLocalSummarySettings
	oldDiscover := discoverSummaryProvidersAlways
	oldList := listRegisteredAgents
	oldGet := getSummaryAgent
	oldAvailable := isSummaryCLIAvailable
	oldCanPrompt := canPromptForSummaryProvider
	oldPrompt := promptSummaryProvider
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	oldMarkdown := renderDispatchMarkdown
	t.Cleanup(func() {
		shouldRunDispatchWizardForCommand = oldShouldRunWizard
		runDispatchWizardForCommand = oldRunWizard
		loadSummarySettings = oldLoad
		loadSummarySettingsFromFile = oldLoadFile
		saveLocalSummarySettings = oldSave
		discoverSummaryProvidersAlways = oldDiscover
		listRegisteredAgents = oldList
		getSummaryAgent = oldGet
		isSummaryCLIAvailable = oldAvailable
		canPromptForSummaryProvider = oldCanPrompt
		promptSummaryProvider = oldPrompt
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
		renderDispatchMarkdown = oldMarkdown
	})

	var calls []string
	shouldRunDispatchWizardForCommand = func(int, bool, bool) bool { return true }
	runDispatchWizardForCommand = func(*cobra.Command) (dispatchpkg.Options, error) {
		calls = append(calls, "wizard")
		return dispatchpkg.Options{Mode: dispatchpkg.ModeLocal, Since: "7d", AllBranches: true}, nil
	}
	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{Enabled: true}, nil
	}
	loadSummarySettingsFromFile = func(string) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{}, nil
	}
	discoverSummaryProvidersAlways = func(context.Context) {}
	listRegisteredAgents = func() []types.AgentName {
		return []types.AgentName{agent.AgentNameCodex, agent.AgentNameGemini}
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		kind := agent.AgentTypeCodex
		if name == agent.AgentNameGemini {
			kind = agent.AgentTypeGemini
		}
		return &stubTextAgent{name: name, kind: kind}, nil
	}
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }
	canPromptForSummaryProvider = func() bool { return true }
	promptSummaryProvider = func(providers []checkpointSummaryProvider) (types.AgentName, error) {
		calls = append(calls, "picker")
		if len(providers) != 2 || providers[0].Name != agent.AgentNameCodex || providers[1].Name != agent.AgentNameGemini {
			t.Fatalf("picker providers = %+v, want enabled codex and gemini", providers)
		}
		return agent.AgentNameGemini, nil
	}
	var persistedProvider string
	saveLocalSummarySettings = func(_ context.Context, s *settings.EntireSettings) error {
		if s.SummaryGeneration != nil {
			persistedProvider = s.SummaryGeneration.Provider
		}
		return nil
	}
	runDispatch = func(_ context.Context, opts dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		calls = append(calls, "dispatch")
		selected, ok := opts.TextGenerator.(*stubTextAgent)
		if !ok || selected.name != agent.AgentNameGemini {
			t.Fatalf("dispatch generator = %#v, want selected gemini agent", opts.TextGenerator)
		}
		return &dispatchpkg.Dispatch{}, nil
	}
	dispatchTerminalMode = func(io.Writer) bool { return false }
	renderDispatchMarkdown = func(*dispatchpkg.Dispatch) string { return "" }

	cmd := newDispatchCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "wizard,picker,dispatch" {
		t.Fatalf("call order = %q, want wizard,picker,dispatch", got)
	}
	if persistedProvider != string(agent.AgentNameGemini) {
		t.Fatalf("persisted provider = %q, want %q", persistedProvider, agent.AgentNameGemini)
	}
}

func TestNewDispatchCmd_CloudDispatchDoesNotResolveLocalProvider(t *testing.T) {
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	unexpectedCallErr := errors.New("unexpected local provider resolution")
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		t.Fatal("normal cloud dispatch must not resolve a local provider")
		return nil, unexpectedCallErr
	}
	runDispatch = func(_ context.Context, opts dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		if opts.Mode != dispatchpkg.ModeServer {
			t.Fatalf("mode = %v, want server", opts.Mode)
		}
		return &dispatchpkg.Dispatch{}, nil
	}
	dispatchTerminalMode = func(io.Writer) bool { return false }
	t.Cleanup(func() {
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
	})

	cmd := newDispatchCmd()
	cmd.SetArgs([]string{"--repos", "entireio/cli"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestNewDispatchCmd_ProviderErrorUsesStderrAndSkipsDispatch(t *testing.T) {
	oldPrepare := prepareLocalDispatch
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	unexpectedCallErr := errors.New("unexpected dispatch call")
	prepareLocalDispatch = func(_ context.Context, opts dispatchpkg.Options) (dispatchpkg.Options, error) {
		return opts, nil
	}
	resolveDispatchProvider = func(_ context.Context, w io.Writer, override string) (*checkpointSummaryProvider, error) {
		if override != string(agent.AgentNameCodex) {
			t.Fatalf("provider override = %q, want codex", override)
		}
		if _, err := io.WriteString(w, "provider warning\n"); err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("provider failed")
	}
	runDispatch = func(context.Context, dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		t.Fatal("dispatch must not run after provider resolution fails")
		return nil, unexpectedCallErr
	}
	t.Cleanup(func() {
		prepareLocalDispatch = oldPrepare
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
	})

	cmd := newDispatchCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--local", "--all-branches", "--agent", string(agent.AgentNameCodex)})

	err := cmd.Execute()
	if err == nil || err.Error() != "provider failed" {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("unexpected stdout: %q", got)
	}
	if got := stderr.String(); got != "provider warning\n" {
		t.Fatalf("unexpected stderr: %q", got)
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

func TestNewDispatchCmd_JurisdictionFlagHelpText(t *testing.T) {
	t.Parallel()

	cmd := newDispatchCmd()
	flag := cmd.Flags().Lookup("jurisdiction")
	if flag == nil {
		t.Fatal("expected --jurisdiction flag")
	}
	if flag.Shorthand != "j" {
		t.Fatalf("expected -j shorthand to match `entire api`/`entire auth token`, got %q", flag.Shorthand)
	}
	if !strings.Contains(flag.Usage, "home jurisdiction") {
		t.Fatalf("expected usage to explain the home default, got %q", flag.Usage)
	}
	if !strings.Contains(cmd.Long, "--jurisdiction us") {
		t.Fatalf("expected a --jurisdiction example in long help, got %q", cmd.Long)
	}
}
