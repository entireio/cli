package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/orcarouter"
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

func TestIsSummaryProviderAvailable_OrcaRouterGatedOnAPIKey(t *testing.T) {
	// Cannot use t.Parallel(): mutates process environment.
	ag, err := agent.Get(orcarouter.AgentNameOrcaRouter)
	if err != nil {
		t.Fatalf("agent.Get(orcarouter): %v", err)
	}
	if _, ok := agent.AsTextGenerator(ag); !ok {
		t.Fatal("orcarouter agent must implement TextGenerator")
	}

	t.Run("unset key is unavailable", func(t *testing.T) {
		t.Setenv(orcarouter.EnvAPIKey, "")
		if isSummaryProviderAvailable(orcarouter.AgentNameOrcaRouter, ag) {
			t.Error("isSummaryProviderAvailable(orcarouter) = true with no ORCAROUTER_API_KEY, want false")
		}
	})

	t.Run("set key is available", func(t *testing.T) {
		t.Setenv(orcarouter.EnvAPIKey, "sk-orca-test")
		if !isSummaryProviderAvailable(orcarouter.AgentNameOrcaRouter, ag) {
			t.Error("isSummaryProviderAvailable(orcarouter) = false with ORCAROUTER_API_KEY set, want true")
		}
	})
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

type stubNonTextAgent struct {
	agent.Agent
}

func writeInfoSentinelExternalAgentBinary(t *testing.T, dir, name string) {
	t.Helper()

	script := `#!/bin/sh
if [ "$1" = "info" ]; then
  : > "$ENTIRE_TEST_UNRELATED_INFO_SENTINEL"
  exit 1
fi
echo '{}'
`
	if err := os.WriteFile(filepath.Join(dir, "entire-agent-"+name), []byte(script), 0o755); err != nil {
		t.Fatalf("write unrelated external agent binary: %v", err)
	}
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
	if provider.TextGenerator == nil {
		t.Fatal("provider.TextGenerator = nil, want configured provider's raw text generator")
	}
}

func TestResolveDispatchSummaryProvider_ExplicitCodexUsesDefaultModelWithoutPersistence(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	ctx := context.Background()
	codex := &stubTextAgent{name: agent.AgentNameCodex, kind: agent.AgentTypeCodex}

	originalLoad := loadSummarySettings
	originalLoadFile := loadSummarySettingsFromFile
	originalSave := saveLocalSummarySettings
	originalGet := getSummaryAgent
	originalCLI := isSummaryCLIAvailable
	originalDiscover := discoverNamedSummaryProvider
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		loadSummarySettingsFromFile = originalLoadFile
		saveLocalSummarySettings = originalSave
		getSummaryAgent = originalGet
		isSummaryCLIAvailable = originalCLI
		discoverNamedSummaryProvider = originalDiscover
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		t.Fatal("explicit dispatch provider must not load summary settings")
		return nil, errors.New("unexpected settings load")
	}
	loadSummarySettingsFromFile = func(string) (*settings.EntireSettings, error) {
		t.Fatal("explicit dispatch provider must not load settings for persistence")
		return nil, errors.New("unexpected settings load for persistence")
	}
	saveLocalSummarySettings = func(context.Context, *settings.EntireSettings) error {
		t.Fatal("explicit dispatch provider must not persist settings")
		return nil
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		if name != agent.AgentNameCodex {
			t.Fatalf("getSummaryAgent(%q), want %q", name, agent.AgentNameCodex)
		}
		return codex, nil
	}
	isSummaryCLIAvailable = func(name types.AgentName) bool {
		return name == agent.AgentNameCodex
	}
	discoverNamedSummaryProvider = func(context.Context, types.AgentName) error {
		t.Fatal("registered explicit provider should not trigger external discovery")
		return nil
	}

	provider, err := resolveDispatchSummaryProvider(ctx, &bytes.Buffer{}, "  codex  ")
	if err != nil {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v", err)
	}
	if provider.Name != agent.AgentNameCodex {
		t.Fatalf("provider.Name = %q, want %q", provider.Name, agent.AgentNameCodex)
	}
	if provider.Model != "" {
		t.Fatalf("provider.Model = %q, want provider CLI default", provider.Model)
	}
	if provider.TextGenerator != codex {
		t.Fatalf("provider.TextGenerator = %T %p, want raw generator %T %p", provider.TextGenerator, provider.TextGenerator, codex, codex)
	}
}

func TestResolveDispatchSummaryProvider_EmptyOverrideUsesConfiguredProviderAndModel(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	ctx := context.Background()
	configured := &stubTextAgent{name: agent.AgentNameGemini, kind: agent.AgentTypeGemini}

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalCLI := isSummaryCLIAvailable
	originalDiscover := discoverSummaryProvidersAlways
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		isSummaryCLIAvailable = originalCLI
		discoverSummaryProvidersAlways = originalDiscover
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{SummaryGeneration: &settings.SummaryGenerationSettings{
			Provider: string(agent.AgentNameGemini),
			Model:    "gemini-saved-model",
		}}, nil
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		if name != agent.AgentNameGemini {
			t.Fatalf("getSummaryAgent(%q), want %q", name, agent.AgentNameGemini)
		}
		return configured, nil
	}
	isSummaryCLIAvailable = func(name types.AgentName) bool {
		return name == agent.AgentNameGemini
	}
	discoverSummaryProvidersAlways = func(context.Context) {
		t.Fatal("configured registered provider should not trigger external discovery")
	}

	provider, err := resolveDispatchSummaryProvider(ctx, &bytes.Buffer{}, " \t\n")
	if err != nil {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v", err)
	}
	if provider.Name != agent.AgentNameGemini {
		t.Fatalf("provider.Name = %q, want %q", provider.Name, agent.AgentNameGemini)
	}
	if provider.Model != "gemini-saved-model" {
		t.Fatalf("provider.Model = %q, want configured model", provider.Model)
	}
	if provider.TextGenerator != configured {
		t.Fatalf("provider.TextGenerator = %T, want configured raw generator", provider.TextGenerator)
	}
}

func TestResolveDispatchSummaryProvider_ExplicitProviderIgnoresSavedProviderAndModel(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	ctx := context.Background()
	codex := &stubTextAgent{name: agent.AgentNameCodex, kind: agent.AgentTypeCodex}
	loadCalls := 0

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalCLI := isSummaryCLIAvailable
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		isSummaryCLIAvailable = originalCLI
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		loadCalls++
		return &settings.EntireSettings{SummaryGeneration: &settings.SummaryGenerationSettings{
			Provider: string(agent.AgentNameClaudeCode),
			Model:    "sonnet",
		}}, nil
	}
	getSummaryAgent = func(types.AgentName) (agent.Agent, error) { return codex, nil }
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }

	provider, err := resolveDispatchSummaryProvider(ctx, &bytes.Buffer{}, string(agent.AgentNameCodex))
	if err != nil {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("loadSummarySettings calls = %d, want 0 for explicit override", loadCalls)
	}
	if provider.Name != agent.AgentNameCodex || provider.Model != "" {
		t.Fatalf("provider = %+v, want explicit Codex with provider-default model", provider)
	}
}

func TestResolveDispatchSummaryProvider_ExplicitClaudeUsesSummaryDefaultModel(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	ctx := context.Background()
	claude := &stubTextAgent{name: agent.AgentNameClaudeCode, kind: agent.AgentTypeClaudeCode}
	loadCalls := 0

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalCLI := isSummaryCLIAvailable
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		isSummaryCLIAvailable = originalCLI
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		loadCalls++
		return &settings.EntireSettings{SummaryGeneration: &settings.SummaryGenerationSettings{
			Provider: string(agent.AgentNameClaudeCode),
			Model:    "opus",
		}}, nil
	}
	getSummaryAgent = func(types.AgentName) (agent.Agent, error) { return claude, nil }
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }

	provider, err := resolveDispatchSummaryProvider(ctx, &bytes.Buffer{}, string(agent.AgentNameClaudeCode))
	if err != nil {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("loadSummarySettings calls = %d, want 0 for explicit override", loadCalls)
	}
	if provider.Model != summarize.DefaultModel {
		t.Fatalf("provider.Model = %q, want summary default %q", provider.Model, summarize.DefaultModel)
	}
}

func TestResolveDispatchSummaryProvider_PropagatesDiscoveryDeadline(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	providerName := types.AgentName("external-discovery-deadline")

	originalGet := getSummaryAgent
	originalDiscover := discoverNamedSummaryProvider
	t.Cleanup(func() {
		getSummaryAgent = originalGet
		discoverNamedSummaryProvider = originalDiscover
	})

	getSummaryAgent = func(types.AgentName) (agent.Agent, error) {
		return nil, errors.New("not registered")
	}
	discoverNamedSummaryProvider = func(context.Context, types.AgentName) error {
		return fmt.Errorf("discovering external agent %q: %w", providerName, context.DeadlineExceeded)
	}

	_, err := resolveDispatchSummaryProvider(context.Background(), &bytes.Buffer{}, string(providerName))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v, want context deadline exceeded", err)
	}
	if strings.Contains(err.Error(), "unknown summary provider") {
		t.Fatalf("resolveDispatchSummaryProvider() error = %q, do not want unknown-provider rewrite", err)
	}
}

func TestResolveDispatchSummaryProvider_PropagatesDiscoveryCancellation(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	providerName := types.AgentName("external-discovery-canceled")

	originalGet := getSummaryAgent
	originalDiscover := discoverNamedSummaryProvider
	t.Cleanup(func() {
		getSummaryAgent = originalGet
		discoverNamedSummaryProvider = originalDiscover
	})

	getSummaryAgent = func(types.AgentName) (agent.Agent, error) {
		return nil, errors.New("not registered")
	}
	discoverNamedSummaryProvider = func(context.Context, types.AgentName) error {
		return fmt.Errorf("discovering external agent %q: %w", providerName, context.Canceled)
	}

	_, err := resolveDispatchSummaryProvider(context.Background(), &bytes.Buffer{}, string(providerName))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v, want context canceled", err)
	}
	if strings.Contains(err.Error(), "unknown summary provider") {
		t.Fatalf("resolveDispatchSummaryProvider() error = %q, do not want unknown-provider rewrite", err)
	}
}

func TestResolveDispatchSummaryProvider_PropagatesInvalidExternalInfo(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	providerName := types.AgentName("external-discovery-invalid-info")
	infoErr := errors.New("invalid helper info")

	originalGet := getSummaryAgent
	originalDiscover := discoverNamedSummaryProvider
	t.Cleanup(func() {
		getSummaryAgent = originalGet
		discoverNamedSummaryProvider = originalDiscover
	})

	getSummaryAgent = func(types.AgentName) (agent.Agent, error) {
		return nil, errors.New("not registered")
	}
	discoverNamedSummaryProvider = func(context.Context, types.AgentName) error {
		return fmt.Errorf("loading info for external agent %q: info: invalid JSON: %w", providerName, infoErr)
	}

	_, err := resolveDispatchSummaryProvider(context.Background(), &bytes.Buffer{}, string(providerName))
	if !errors.Is(err, infoErr) {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v, want invalid-info cause", err)
	}
	if !strings.Contains(err.Error(), string(providerName)) || !strings.Contains(err.Error(), "info: invalid JSON") {
		t.Fatalf("resolveDispatchSummaryProvider() error = %q, want provider and invalid-info context", err)
	}
	if strings.Contains(err.Error(), "unknown summary provider") {
		t.Fatalf("resolveDispatchSummaryProvider() error = %q, do not want unknown-provider rewrite", err)
	}
}

func TestResolveDispatchSummaryProvider_MissingExternalKeepsUnknownProviderError(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	providerName := types.AgentName("external-discovery-missing")

	originalGet := getSummaryAgent
	originalDiscover := discoverNamedSummaryProvider
	t.Cleanup(func() {
		getSummaryAgent = originalGet
		discoverNamedSummaryProvider = originalDiscover
	})

	getSummaryAgent = func(types.AgentName) (agent.Agent, error) {
		return nil, errors.New("not registered")
	}
	discoverNamedSummaryProvider = func(context.Context, types.AgentName) error { return nil }

	_, err := resolveDispatchSummaryProvider(context.Background(), &bytes.Buffer{}, string(providerName))
	if err == nil || !strings.Contains(err.Error(), "unknown summary provider") {
		t.Fatalf("resolveDispatchSummaryProvider() error = %v, want existing unknown-provider error", err)
	}
}

func TestResolveDispatchSummaryProvider_ExplicitValidationErrors(t *testing.T) {
	// Cannot use t.Parallel(): subtests mutate package-level resolution seams.
	tests := []struct {
		name          string
		override      string
		agent         agent.Agent
		getErr        error
		available     bool
		wantError     string
		unwantedError string
	}{
		{
			name:      "unknown provider",
			override:  "missing-provider",
			getErr:    errors.New("not registered"),
			available: true,
			wantError: "unknown summary provider",
		},
		{
			name:     "no text generator capability",
			override: "no-text",
			agent: &stubNonTextAgent{Agent: &stubTextAgent{
				name: "no-text",
				kind: agent.AgentTypeUnknown,
			}},
			available: true,
			wantError: "does not support summary generation",
		},
		{
			name:          "CLI unavailable",
			override:      string(agent.AgentNameCodex),
			agent:         &stubTextAgent{name: agent.AgentNameCodex, kind: agent.AgentTypeCodex},
			available:     false,
			wantError:     "install it or choose another provider",
			unwantedError: "configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalGet := getSummaryAgent
			originalCLI := isSummaryCLIAvailable
			originalDiscover := discoverNamedSummaryProvider
			t.Cleanup(func() {
				getSummaryAgent = originalGet
				isSummaryCLIAvailable = originalCLI
				discoverNamedSummaryProvider = originalDiscover
			})

			getSummaryAgent = func(types.AgentName) (agent.Agent, error) {
				if tt.getErr != nil {
					return nil, tt.getErr
				}
				return tt.agent, nil
			}
			isSummaryCLIAvailable = func(types.AgentName) bool { return tt.available }
			discoverNamedSummaryProvider = func(context.Context, types.AgentName) error { return nil }

			_, err := resolveDispatchSummaryProvider(context.Background(), &bytes.Buffer{}, tt.override)
			if err == nil {
				t.Fatalf("resolveDispatchSummaryProvider(%q) error = nil, want %q", tt.override, tt.wantError)
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("resolveDispatchSummaryProvider(%q) error = %q, want substring %q", tt.override, err, tt.wantError)
			}
			if tt.unwantedError != "" && strings.Contains(err.Error(), tt.unwantedError) {
				t.Fatalf("resolveDispatchSummaryProvider(%q) error = %q, do not want substring %q", tt.override, err, tt.unwantedError)
			}
		})
	}
}

func TestResolveDispatchSummaryProvider_ExplicitExternalProviderDoesNotWriteLocalSettings(t *testing.T) {
	// Cannot use t.Parallel(): subtests mutate cwd, PATH, and the agent registry.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	tests := []struct {
		name         string
		providerName string
		localContent string
	}{
		{name: "does not create settings.local.json", providerName: "external-dispatch-no-create"},
		{
			name:         "does not update settings.local.json",
			providerName: "external-dispatch-no-update",
			localContent: `{"external_agents":false,"summary_generation":{"provider":"codex","model":"saved-model"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tmpDir := t.TempDir()
			testutil.InitRepo(t, tmpDir)
			t.Chdir(tmpDir)

			if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
				t.Fatalf("mkdir .entire: %v", err)
			}
			if err := os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled":true,"external_agents":false}`), 0o644); err != nil {
				t.Fatalf("write settings.json: %v", err)
			}

			localPath := filepath.Join(tmpDir, ".entire", "settings.local.json")
			if tt.localContent != "" {
				if err := os.WriteFile(localPath, []byte(tt.localContent), 0o644); err != nil {
					t.Fatalf("write settings.local.json: %v", err)
				}
			}

			externalDir := t.TempDir()
			writeExternalSummaryAgentBinary(t, externalDir, tt.providerName)
			writeInfoSentinelExternalAgentBinary(t, externalDir, tt.providerName+"-unrelated")
			t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			unrelatedInfoSentinel := filepath.Join(t.TempDir(), "unrelated-info-called")
			t.Setenv("ENTIRE_TEST_UNRELATED_INFO_SENTINEL", unrelatedInfoSentinel)
			modelRecord := filepath.Join(t.TempDir(), "model-args")
			t.Setenv("ENTIRE_TEST_EXTERNAL_MODEL_RECORD", modelRecord)

			provider, err := resolveDispatchSummaryProvider(ctx, &bytes.Buffer{}, tt.providerName)
			if err != nil {
				t.Fatalf("resolveDispatchSummaryProvider() error = %v", err)
			}
			if provider.Name != types.AgentName(tt.providerName) {
				t.Fatalf("provider.Name = %q, want %q", provider.Name, tt.providerName)
			}
			if provider.Model != "" {
				t.Fatalf("provider.Model = %q, want external CLI default", provider.Model)
			}
			if provider.TextGenerator == nil {
				t.Fatal("provider.TextGenerator = nil, want external raw generator")
			}
			if _, err := os.Stat(unrelatedInfoSentinel); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unrelated plugin info was invoked: stat error = %v", err)
			}

			generated, err := provider.TextGenerator.GenerateText(ctx, "generate a summary", provider.Model)
			if err != nil {
				t.Fatalf("provider.TextGenerator.GenerateText() error = %v", err)
			}
			if !strings.Contains(generated, `"intent":"Intent"`) {
				t.Fatalf("provider.TextGenerator.GenerateText() = %q, want generated summary", generated)
			}
			modelArgs, err := os.ReadFile(modelRecord)
			if err != nil {
				t.Fatalf("read external model args: %v", err)
			}
			if string(modelArgs) != "--model\n\n" {
				t.Fatalf("external generate-text args = %q, want empty model argument", modelArgs)
			}

			got, err := os.ReadFile(localPath)
			switch {
			case tt.localContent == "" && !errors.Is(err, os.ErrNotExist):
				t.Fatalf("settings.local.json read error = %v, want file to remain absent (content %q)", err, got)
			case tt.localContent != "" && err != nil:
				t.Fatalf("read settings.local.json: %v", err)
			case tt.localContent != "" && string(got) != tt.localContent:
				t.Fatalf("settings.local.json changed:\n got: %s\nwant: %s", got, tt.localContent)
			}
		})
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

	flagFlipped, err := persistSummaryProviderSelection(ctx, types.AgentName(providerName), "", selectionByUser)
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

	flagFlipped, err := persistSummaryProviderSelection(ctx, agent.AgentNameClaudeCode, "", selectionByUser)
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

	flagFlipped, err := persistSummaryProviderSelection(ctx, types.AgentName(providerName), "", selectionByUser)
	if err != nil {
		t.Fatalf("persistSummaryProviderSelection() error = %v", err)
	}
	if flagFlipped {
		t.Fatal("expected flagFlipped=false when external_agents was already enabled")
	}
}

// TestResolveCheckpointSummaryProvider_ConfiguredProviderUsesNamedDiscovery
// pins that a configured-but-unregistered provider is resolved by NAME rather
// than by the ungated sweep.
//
// summary_generation.provider is honored from the committed
// .entire/settings.json, so a name that arrives there must not be able to
// trigger DiscoverAndRegisterAlways, which globs every absolute $PATH
// directory and executes every entire-agent-* binary's "info" subcommand. The
// named lookup returns immediately for a built-in and touches exactly one
// binary otherwise.
func TestResolveCheckpointSummaryProvider_ConfiguredProviderUsesNamedDiscovery(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	ctx := context.Background()
	const configuredName = types.AgentName("external-configured-provider")
	stub := &stubTextAgent{name: configuredName, kind: agent.AgentTypeClaudeCode}

	originalLoad := loadSummarySettings
	originalGet := getSummaryAgent
	originalCLI := isSummaryCLIAvailable
	originalSweep := discoverSummaryProvidersAlways
	originalNamed := discoverNamedSummaryProvider
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		isSummaryCLIAvailable = originalCLI
		discoverSummaryProvidersAlways = originalSweep
		discoverNamedSummaryProvider = originalNamed
	})

	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{SummaryGeneration: &settings.SummaryGenerationSettings{
			Provider: string(configuredName),
		}}, nil
	}
	// Unregistered until the named discovery runs, which is what makes
	// discoverSummaryProviderIfMissing reach for discovery at all.
	registered := false
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		if !registered {
			return nil, fmt.Errorf("agent %q not registered", name)
		}
		return stub, nil
	}
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }
	discoverSummaryProvidersAlways = func(context.Context) {
		t.Fatal("a configured provider name must not trigger the ungated $PATH sweep")
	}
	namedCalls := 0
	discoverNamedSummaryProvider = func(_ context.Context, name types.AgentName) error {
		namedCalls++
		if name != configuredName {
			t.Fatalf("named discovery for %q, want %q", name, configuredName)
		}
		registered = true
		return nil
	}

	provider, err := resolveCheckpointSummaryProvider(ctx, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveCheckpointSummaryProvider() error = %v", err)
	}
	if provider.Name != configuredName {
		t.Fatalf("provider.Name = %q, want %q", provider.Name, configuredName)
	}
	if namedCalls != 1 {
		t.Fatalf("named discovery called %d times, want exactly 1", namedCalls)
	}
}

// TestPersistSummaryProviderSelection_AutoSelectDoesNotGrantExternalAgents
// pins that the repo-wide external_agents grant needs a human.
//
// The non-interactive branches of resolveCheckpointSummaryProvider auto-select
// (single candidate, or first-of-many with no TTY) and used to persist the
// grant on the way through. That grant is not scoped to the chosen provider:
// it turns on the $PATH sweep that runs every entire-agent-* binary from then
// on, so it must not be minted by a code path where nobody chose anything. The
// chosen provider still resolves on later runs without it, through the named
// ungated lookup in discoverSummaryProviderIfMissing.
func TestPersistSummaryProviderSelection_AutoSelectDoesNotGrantExternalAgents(t *testing.T) {
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

	const providerName = "external-summary-autoselect"
	externalDir := t.TempDir()
	writeExternalSummaryAgentBinary(t, externalDir, providerName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	discoverSummaryProvidersAlways(ctx)

	flagFlipped, err := persistSummaryProviderSelection(ctx, types.AgentName(providerName), "", selectionAutomatic)
	if err != nil {
		t.Fatalf("persistSummaryProviderSelection() error = %v", err)
	}
	if flagFlipped {
		t.Error("an automatic selection must not flip external_agents")
	}

	s, err := settings.LoadFromFile(filepath.Join(tmpDir, ".entire", "settings.local.json"))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if s.ExternalAgents {
		t.Error("external_agents granted without a human choosing the provider")
	}
	// The provider choice itself is still worth persisting: it is what keeps
	// the next run from re-deciding, and it grants nothing on its own.
	if s.SummaryGeneration == nil || s.SummaryGeneration.Provider != providerName {
		t.Fatalf("provider not persisted; got %+v", s.SummaryGeneration)
	}
}
