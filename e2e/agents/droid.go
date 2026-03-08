package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "factoryai-droid" {
		return
	}
	if _, err := exec.LookPath("droid"); err != nil {
		return
	}
	Register(&Droid{})
}

// Droid implements the Agent interface for Factory AI Droid.
type Droid struct{}

func (d *Droid) Name() string               { return "factoryai-droid" }
func (d *Droid) Binary() string             { return "droid" }
func (d *Droid) EntireAgent() string        { return "factoryai-droid" }
func (d *Droid) PromptPattern() string      { return `>` }
func (d *Droid) TimeoutMultiplier() float64 { return 2.0 }

func (d *Droid) IsTransientError(out Output, err error) bool {
	if err == nil {
		return false
	}
	combined := out.Stdout + out.Stderr
	transientPatterns := []string{
		"overloaded",
		"rate limit",
		"529",
		"503",
		"ECONNRESET",
		"ETIMEDOUT",
	}
	for _, p := range transientPatterns {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

// droidSettings represents the ~/.factory/settings.json structure used for
// BYOK (Bring Your Own Key) configuration.
type droidSettings struct {
	CustomModels []droidCustomModel `json:"customModels,omitempty"`
}

type droidCustomModel struct {
	DisplayName    string `json:"displayName"`
	Model          string `json:"model"`
	BaseURL        string `json:"baseUrl"`
	APIKey         string `json:"apiKey"`
	Provider       string `json:"provider"`
	MaxOutputToken int    `json:"maxOutputTokens"`
}

const (
	// Droid v0.63+ expects custom model selection using custom:<model-id>.
	// The displayName in settings is not accepted by --model.
	droidCustomModelDisplayName = "claude-haiku-custom"
	droidCustomModelBaseID      = "claude-haiku-4-5-20251001"
	defaultDroidModel           = "custom:" + droidCustomModelBaseID
)

// DefaultDroidModel returns the default model string for Droid BYOK configuration.
// Format is "custom:<baseModelID>" which the Droid API expects for custom model selection.
func DefaultDroidModel() string { return defaultDroidModel }

func (d *Droid) Bootstrap() error {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	dir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	settingsPath := filepath.Join(dir, "settings.json")

	// Read existing settings to merge (hooks may already be configured
	// in the repo-local .factory/settings.json, but the global config
	// at ~/.factory/settings.json might have other pre-existing entries).
	var settings droidSettings
	if data, readErr := os.ReadFile(settingsPath); readErr == nil {
		// Best-effort merge: ignore parse errors and start fresh
		_ = json.Unmarshal(data, &settings)
	}

	// Replace or add the BYOK model entry.
	byokModel := droidCustomModel{
		DisplayName:    droidCustomModelDisplayName,
		Model:          droidCustomModelBaseID,
		BaseURL:        "https://api.anthropic.com",
		APIKey:         apiKey,
		Provider:       "anthropic",
		MaxOutputToken: 8192,
	}

	found := false
	for i, m := range settings.CustomModels {
		if m.Model == byokModel.Model {
			settings.CustomModels[i] = byokModel
			found = true
			break
		}
	}
	if !found {
		settings.CustomModels = append(settings.CustomModels, byokModel)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	return os.WriteFile(settingsPath, data, 0o644)
}

func (d *Droid) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: defaultDroidModel}
	for _, o := range opts {
		o(cfg)
	}

	model := cfg.Model
	if model == "" {
		model = defaultDroidModel
	}

	args := []string{"exec", "--skip-permissions-unsafe", "--model", model, prompt}
	displayArgs := []string{"exec", "--skip-permissions-unsafe", "--model", model, fmt.Sprintf("%q", prompt)}

	cmd := exec.CommandContext(ctx, d.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = filterEnv(os.Environ(), "ENTIRE_TEST_TTY", "CI", "GITHUB_ACTIONS")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return Output{
		Command:  d.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (d *Droid) StartSession(ctx context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("droid-test-%d", time.Now().UnixNano())
	// Unset CI and GITHUB_ACTIONS so Droid doesn't enter single-turn/headless
	// mode — it checks these vars and skips interactive input after the first turn.
	s, err := NewTmuxSession(name, dir, []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}, d.Binary())
	if err != nil {
		return nil, err
	}

	// Wait for the interactive prompt indicator.
	if _, err := s.WaitFor(`>`, 30*time.Second); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("waiting for startup prompt: %w", err)
	}

	// Droid auto-generates a greeting on startup which fires a Stop hook.
	// Wait for the greeting turn to fully complete before accepting prompts.
	select {
	case <-ctx.Done():
		_ = s.Close()
		return nil, fmt.Errorf("context cancelled during startup wait: %w", ctx.Err())
	case <-time.After(2 * time.Second):
	}
	s.stableAtSend = ""

	return s, nil
}
