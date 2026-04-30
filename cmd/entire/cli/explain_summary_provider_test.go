package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func rowsContain(rows []explainRow, label, value string) bool {
	for _, r := range rows {
		if r.Label == label && r.Value == value {
			return true
		}
	}
	return false
}

func TestSummaryProviderRows_PopulatesProviderAndModel(t *testing.T) {
	t.Parallel()
	p := &checkpointSummaryProvider{DisplayName: "claude-code", Model: "claude-sonnet-4-6"}
	rows := summaryProviderRows(p)
	if !rowsContain(rows, "provider", "claude-code") {
		t.Errorf("missing provider row: %+v", rows)
	}
	if !rowsContain(rows, "model", "claude-sonnet-4-6") {
		t.Errorf("missing model row: %+v", rows)
	}
}

func TestSummaryProviderRows_EmptyModelRendersDefault(t *testing.T) {
	t.Parallel()
	p := &checkpointSummaryProvider{DisplayName: "claude-code", Model: ""}
	rows := summaryProviderRows(p)
	if !rowsContain(rows, "model", "provider default") {
		t.Errorf("expected provider-default fallback: %+v", rows)
	}
}

func TestSummaryProviderRows_NilProviderReturnsNil(t *testing.T) {
	t.Parallel()
	if rows := summaryProviderRows(nil); rows != nil {
		t.Errorf("expected nil for nil provider, got %+v", rows)
	}
}

type stubTextAgent struct {
	name types.AgentName
	kind types.AgentType
}

func (s *stubTextAgent) Name() types.AgentName                        { return s.name }
func (s *stubTextAgent) Type() types.AgentType                        { return s.kind }
func (s *stubTextAgent) Description() string                          { return "stub" }
func (s *stubTextAgent) IsPreview() bool                              { return false }
func (s *stubTextAgent) DetectPresence(context.Context) (bool, error) { return true, nil }
func (s *stubTextAgent) ProtectedDirs() []string                      { return nil }
func (s *stubTextAgent) ReadTranscript(string) ([]byte, error)        { return nil, nil }
func (s *stubTextAgent) ChunkTranscript(context.Context, []byte, int) ([][]byte, error) {
	return nil, nil
}
func (s *stubTextAgent) ReassembleTranscript([][]byte) ([]byte, error) { return nil, nil }
func (s *stubTextAgent) GetSessionID(*agent.HookInput) string          { return "" }
func (s *stubTextAgent) GetSessionDir(string) (string, error)          { return "", nil }
func (s *stubTextAgent) ResolveSessionFile(string, string) string      { return "" }
func (s *stubTextAgent) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // test stub
}
func (s *stubTextAgent) WriteSession(context.Context, *agent.AgentSession) error { return nil }
func (s *stubTextAgent) FormatResumeCommand(string) string                       { return "" }
func (s *stubTextAgent) GenerateText(context.Context, string, string) (string, error) {
	return `{"intent":"Intent","outcome":"Outcome","learnings":{"repo":[],"code":[],"workflow":[]},"friction":[],"open_items":[]}`, nil
}

func TestResolveCheckpointSummaryProvider_UsesConfiguredProvider(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and package-level var stubs
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalCLI := isSummaryCLIAvailable
	originalDiscover := discoverSummaryProviders
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		isSummaryCLIAvailable = originalCLI
		discoverSummaryProviders = originalDiscover
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{
			Enabled: true,
			SummaryGeneration: &settings.SummaryGenerationSettings{
				Provider: string(agent.AgentNameClaudeCode),
				Model:    "haiku",
			},
		}, nil
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		return &stubTextAgent{
			name: name,
			kind: agent.AgentTypeClaudeCode,
		}, nil
	}
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }
	discoverSummaryProviders = func(context.Context) {
		t.Fatal("configured registered provider should not trigger external discovery")
	}

	provider, err := resolveCheckpointSummaryProvider(ctx, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveCheckpointSummaryProvider() error = %v", err)
	}

	if provider.Name != agent.AgentNameClaudeCode {
		t.Fatalf("provider.Name = %q, want %q", provider.Name, agent.AgentNameClaudeCode)
	}
	if provider.Model != "haiku" {
		t.Fatalf("provider.Model = %q, want %q", provider.Model, "haiku")
	}
}

func TestResolveCheckpointSummaryProvider_SavesSingleInstalledProvider(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and package-level var stubs
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalList := listRegisteredAgents
	originalCLI := isSummaryCLIAvailable
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		listRegisteredAgents = originalList
		isSummaryCLIAvailable = originalCLI
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{Enabled: true}, nil
	}
	listRegisteredAgents = func() []types.AgentName {
		return []types.AgentName{agent.AgentNameCodex}
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		return &stubTextAgent{name: name, kind: agent.AgentTypeCodex}, nil
	}
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }

	provider, err := resolveCheckpointSummaryProvider(ctx, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveCheckpointSummaryProvider() error = %v", err)
	}
	if provider.Name != agent.AgentNameCodex {
		t.Fatalf("provider.Name = %q, want %q", provider.Name, agent.AgentNameCodex)
	}

	// Auto-persist writes to settings.local.json (not tracked settings.json)
	// because provider selection is based on local PATH.
	localPath := filepath.Join(tmpDir, ".entire", "settings.local.json")
	s, err := settings.LoadFromFile(localPath)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if s.SummaryGeneration == nil {
		t.Fatal("expected summary_generation to be persisted in settings.local.json")
	}
	if s.SummaryGeneration.Provider != string(agent.AgentNameCodex) {
		t.Fatalf("persisted provider = %q, want %q", s.SummaryGeneration.Provider, agent.AgentNameCodex)
	}

	// Tracked settings.json must not be dirtied.
	projectPath := filepath.Join(tmpDir, ".entire", "settings.json")
	projectS, err := settings.LoadFromFile(projectPath)
	if err != nil {
		t.Fatalf("LoadFromFile(project) error = %v", err)
	}
	if projectS.SummaryGeneration != nil {
		t.Fatal("auto-persist should not write to tracked settings.json")
	}
}

func TestResolveCheckpointSummaryProvider_NoCandidatesReturnsError(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and package-level var stubs
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalList := listRegisteredAgents
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		listRegisteredAgents = originalList
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{Enabled: true}, nil
	}
	listRegisteredAgents = func() []types.AgentName {
		return nil // no agents registered
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		return &stubTextAgent{name: name, kind: agent.AgentTypeClaudeCode}, nil
	}

	_, err := resolveCheckpointSummaryProvider(ctx, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error when no summary-capable CLI is installed")
	}
	if !strings.Contains(err.Error(), "no summary-capable provider is available") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestResolveCheckpointSummaryProvider_NonInteractiveMultiCandidatePicksFirst(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir, t.Setenv, and package-level var stubs
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalList := listRegisteredAgents
	originalCLI := isSummaryCLIAvailable
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		listRegisteredAgents = originalList
		isSummaryCLIAvailable = originalCLI
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{Enabled: true}, nil
	}
	listRegisteredAgents = func() []types.AgentName {
		return []types.AgentName{agent.AgentNameCodex, agent.AgentNameGemini}
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		return &stubTextAgent{name: name, kind: agent.AgentTypeCodex}, nil
	}
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }

	provider, err := resolveCheckpointSummaryProvider(ctx, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveCheckpointSummaryProvider() error = %v", err)
	}
	if provider.Name != agent.AgentNameCodex {
		t.Fatalf("provider.Name = %q, want %q (first detected candidate, not Claude)", provider.Name, agent.AgentNameCodex)
	}
}

func TestResolveCheckpointSummaryProvider_ConfiguredProviderNotInstalledReturnsError(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and package-level var stubs
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalCLI := isSummaryCLIAvailable
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		isSummaryCLIAvailable = originalCLI
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{
			Enabled: true,
			SummaryGeneration: &settings.SummaryGenerationSettings{
				Provider: string(agent.AgentNameCodex),
			},
		}, nil
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		return &stubTextAgent{name: name, kind: agent.AgentTypeCodex}, nil
	}
	isSummaryCLIAvailable = func(types.AgentName) bool { return false }

	_, err := resolveCheckpointSummaryProvider(ctx, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error when configured provider's CLI is not on PATH")
	}
	if !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestResolveCheckpointSummaryProvider_ConfiguredExternalProvider(t *testing.T) {
	// Cannot use t.Parallel() because external agent discovery mutates the
	// package-level agent registry.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	const providerName = "external-summary-explain"
	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled":true,"external_agents":true,"summary_generation":{"provider":"`+providerName+`","model":"external-model"}}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	externalDir := t.TempDir()
	writeExternalSummaryAgentBinary(t, externalDir, providerName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider, err := resolveCheckpointSummaryProvider(ctx, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveCheckpointSummaryProvider() error = %v", err)
	}
	if provider.Name != types.AgentName(providerName) {
		t.Fatalf("provider.Name = %q, want %q", provider.Name, providerName)
	}
	if provider.Model != "external-model" {
		t.Fatalf("provider.Model = %q, want %q", provider.Model, "external-model")
	}

	summary, err := provider.Generator.Generate(ctx, summarize.Input{
		Transcript: []summarize.Entry{{Type: summarize.EntryTypeUser, Content: "summarize"}},
	})
	if err != nil {
		t.Fatalf("provider.Generator.Generate() error = %v", err)
	}
	if summary.Intent != "Intent" || summary.Outcome != "Outcome" {
		t.Fatalf("summary = %+v, want generated Intent/Outcome", summary)
	}
}

func TestPersistSummaryProviderSelection_ExternalFlipsFlagAndReturnsSignal(t *testing.T) {
	// Cannot use t.Parallel(): mutates the package-level agent registry via discovery.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	const providerName = "external-summary-persist"
	externalDir := t.TempDir()
	writeExternalSummaryAgentBinary(t, externalDir, providerName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Discover so getSummaryAgent returns a wrapped external (the type IsExternal recognizes).
	discoverSummaryProvidersAlways(ctx)

	flagFlipped, err := persistSummaryProviderSelection(ctx, types.AgentName(providerName), "")
	if err != nil {
		t.Fatalf("persistSummaryProviderSelection() error = %v", err)
	}
	if !flagFlipped {
		t.Fatal("expected flagFlipped=true when external_agents was off and provider is external")
	}

	s, err := settings.LoadFromFile(filepath.Join(tmpDir, ".entire", "settings.local.json"))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if !s.ExternalAgents {
		t.Fatal("external_agents should be true in settings.local.json after picking an external")
	}
	if s.SummaryGeneration == nil || s.SummaryGeneration.Provider != providerName {
		t.Fatalf("provider not persisted; got %+v", s.SummaryGeneration)
	}
}

func TestPersistSummaryProviderSelection_BuiltInDoesNotFlipFlag(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	flagFlipped, err := persistSummaryProviderSelection(ctx, agent.AgentNameClaudeCode, "")
	if err != nil {
		t.Fatalf("persistSummaryProviderSelection() error = %v", err)
	}
	if flagFlipped {
		t.Fatal("expected flagFlipped=false for a built-in provider")
	}

	s, err := settings.LoadFromFile(filepath.Join(tmpDir, ".entire", "settings.local.json"))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if s.ExternalAgents {
		t.Fatal("external_agents must not flip when picking a built-in provider")
	}
}

func TestPersistSummaryProviderSelection_ExternalAlreadyEnabledNoSignal(t *testing.T) {
	// Cannot use t.Parallel(): mutates the package-level agent registry via discovery.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.local.json"), []byte(`{"external_agents":true}`), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}

	const providerName = "external-summary-already"
	externalDir := t.TempDir()
	writeExternalSummaryAgentBinary(t, externalDir, providerName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	discoverSummaryProvidersAlways(ctx)

	flagFlipped, err := persistSummaryProviderSelection(ctx, types.AgentName(providerName), "")
	if err != nil {
		t.Fatalf("persistSummaryProviderSelection() error = %v", err)
	}
	if flagFlipped {
		t.Fatal("expected flagFlipped=false when external_agents was already enabled")
	}
}
