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
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "codex" {
		return
	}
	Register(&Codex{})
	RegisterGate("codex", 2)
}

// Codex implements the E2E Agent interface for OpenAI's Codex CLI.
type Codex struct{}

type CodexSession struct {
	*TmuxSession

	home string
}

func (s *CodexSession) Home() string { return s.home }

func (c *Codex) Name() string               { return "codex" }
func (c *Codex) Binary() string             { return "codex" }
func (c *Codex) EntireAgent() string        { return "codex" }
func (c *Codex) PromptPattern() string      { return `›` }
func (c *Codex) TimeoutMultiplier() float64 { return 1.5 }

func (c *Codex) Bootstrap() error {
	if os.Getenv("CI") != "" && os.Getenv("OPENAI_API_KEY") == "" {
		return errors.New("OPENAI_API_KEY must be set on CI for Codex E2E tests")
	}
	return nil
}

func (c *Codex) IsTransientError(out Output, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	combined := out.Stdout + out.Stderr
	for _, p := range []string{"overloaded", "rate limit", "rate_limit", "503", "529", "ECONNRESET", "ETIMEDOUT"} {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

// codexHome creates an isolated CODEX_HOME for a test run.
// Auth still works via OPENAI_API_KEY env var or symlinked auth.json.
//
// The directory lives under the user's home (not the system temp dir) because
// recent Codex versions refuse to install PATH helper binaries when CODEX_HOME
// sits under /tmp, which breaks subsequent tool calls.
func codexHome() (string, func(), error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", nil, fmt.Errorf("resolve user cache dir: %w", err)
	}
	base := filepath.Join(cache, "entire-e2e")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, fmt.Errorf("create codex home base %q: %w", base, err)
	}
	dir, err := os.MkdirTemp(base, "codex-home-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary codex home under %q: %w", base, err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func (c *Codex) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	timeout, err := promptTimeout(60*time.Second, cfg)
	if err != nil {
		return Output{}, err
	}
	promptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	home, cleanup, err := codexHome()
	if err != nil {
		return Output{}, fmt.Errorf("create codex home: %w", err)
	}
	defer cleanup()

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	if err := seedCodexHome(home, absDir); err != nil {
		return Output{}, fmt.Errorf("seed codex home: %w", err)
	}

	args := []string{"exec", "--dangerously-bypass-approvals-and-sandbox"}
	if cfg.Model != "" {
		args = append(args, "-m", cfg.Model)
	}
	args = append(args, prompt)

	env := append(filterEnv(os.Environ(), "ENTIRE_TEST_TTY", "CODEX_HOME"),
		"CODEX_HOME="+home,
	)

	cmd := exec.CommandContext(promptCtx, c.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = env
	setupProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode, err := runCapture(cmd, promptCtx)

	return Output{
		Command:  c.Binary() + " " + strings.Join(args[:len(args)-1], " ") + " " + fmt.Sprintf("%q", prompt),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (c *Codex) StartSession(ctx context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("codex-test-%d", time.Now().UnixNano())

	home, cleanup, err := codexHome()
	if err != nil {
		return nil, fmt.Errorf("create codex home: %w", err)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	if err := seedCodexHome(home, absDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("seed codex home: %w", err)
	}

	s, err := c.startTmuxSession(name, dir, home, "codex", "--dangerously-bypass-approvals-and-sandbox")
	if err != nil {
		cleanup()
		return nil, err
	}
	s.OnClose(cleanup)

	if err := c.dismissStartupDialogs(s, "startup"); err != nil {
		_ = s.Close()
		return nil, err
	}

	return &CodexSession{TmuxSession: s, home: home}, nil
}

func (c *Codex) ResumeSession(ctx context.Context, dir, home, sessionID string) (Session, error) {
	_ = ctx
	name := fmt.Sprintf("codex-resume-%d", time.Now().UnixNano())

	s, err := c.startTmuxSession(name, dir, home, "codex", "--dangerously-bypass-approvals-and-sandbox", "resume", sessionID)
	if err != nil {
		return nil, err
	}

	if err := c.dismissStartupDialogs(s, "resume"); err != nil {
		_ = s.Close()
		return nil, err
	}

	return &CodexSession{TmuxSession: s, home: home}, nil
}

// codexComposerPlaceholder is what Codex renders in an empty input box. It is
// the only positive evidence that the session accepts a prompt: the dialogs
// Codex draws over the composer at startup render PromptPattern's "›" on their
// selected row too, so a "›" match is not readiness. Typing a prompt into a
// dialog loses it — the text is swallowed and its trailing Enter answers the
// dialog — and the test then waits out its timeout on an agent that never saw
// the work. A session that already has history shows the placeholder too, so
// this holds for resumed sessions.
const codexComposerPlaceholder = "Ask Codex to do anything"

// codexUpgradeOption and codexExistingModelOption are the model-migration
// dialog's two rows. Codex 0.156.x announces a successor for whichever model
// is configured and selects the upgrade by default, so confirming blind would
// move the run off the model seeded into config.toml — onto a costlier tier
// where the successor is not the same tier (gpt-5.6-terra upgrades to
// gpt-6-sol). The opt-out row is only drawn while the configured model is
// still one of Codex's presets, so its absence is the retired-model case.
const (
	codexUpgradeOption       = "Try new model"
	codexExistingModelOption = "Use existing model"
)

// codexDialogRedraw is how long a keystroke sent to a startup dialog gets to
// show up on the pane. A keystroke with no visible effect is a failure, not
// something to send again: the pane may simply be behind, and the repeat then
// lands on whatever dialog has replaced the one being answered.
const codexDialogRedraw = 2 * time.Second

// dismissStartupDialogs answers Codex's startup dialogs until the input
// composer is showing, and reports what it was looking at when it gave up.
// The wording of each dialog is release-dependent, so the loop recognises the
// composer rather than the dialogs: an unknown dialog is answered by its
// default, and a screen that never becomes the composer is an error rather
// than a prompt sent into the dark.
func (c *Codex) dismissStartupDialogs(s *TmuxSession, phase string) error {
	for range 5 {
		content, err := s.WaitFor(c.PromptPattern(), 15*time.Second)
		if err != nil {
			return fmt.Errorf("waiting for codex %s prompt: %w", phase, err)
		}
		if codexStartupReady(content) {
			return nil
		}
		// Only the migration dialog gets the walk; other first-run dialogs are
		// answered by their default. Bail rather than confirm blind once
		// committed to it: Enter on the wrong row takes the upgrade silently.
		if codexStartupOffersUpgrade(content) {
			if !codexStartupOffersExistingModel(content) {
				return fmt.Errorf("codex %s: migration dialog offers no %q row, so every answer changes the model — the configured model is no longer one of Codex's presets; point E2E_CODEX_MODEL at a current one\n%s", phase, codexExistingModelOption, content)
			}
			for range 3 {
				if codexStartupSelectionIsExistingModel(content) {
					break
				}
				if err := s.SendKeys("Down"); err != nil {
					return fmt.Errorf("codex %s dialog: %w", phase, err)
				}
				// Read the moved selection, not the pre-keystroke pane: a slow
				// redraw would otherwise walk past the row being aimed for.
				// A pane that never moves means the capture below would be the
				// pre-keystroke screen, so the walk would spend its remaining
				// steps on a stale reading of where the selection is.
				if !s.paneChangedFrom(content, codexDialogRedraw) {
					return fmt.Errorf("codex %s dialog: pane did not react to Down within %s while walking to %q\n%s", phase, codexDialogRedraw, codexExistingModelOption, s.Capture())
				}
				content = s.Capture()
			}
			if !codexStartupSelectionIsExistingModel(content) {
				return fmt.Errorf("codex %s dialog: %q not reachable, wording may have changed\n%s", phase, codexExistingModelOption, content)
			}
		}
		if err := s.SendKeys("Enter"); err != nil {
			return fmt.Errorf("codex %s dialog: %w", phase, err)
		}
		// Same reason: WaitFor does not require the pane to change during a
		// startup wait, so a dialog that has not repainted yet reads as
		// unanswered on the next pass and collects a second Enter — which by
		// then lands on its successor and takes that dialog's default, the
		// blind confirmation the walk above exists to avoid. An Enter with no
		// visible effect is therefore reported, not repeated.
		if !s.paneChangedFrom(content, codexDialogRedraw) {
			return fmt.Errorf("codex %s dialog: pane did not react to Enter within %s; answering again could confirm the next dialog blind\n%s", phase, codexDialogRedraw, s.Capture())
		}
	}
	return fmt.Errorf("codex %s: input composer (%q) never appeared after answering 5 dialogs\n%s", phase, codexComposerPlaceholder, s.Capture())
}

// codexStartupReady reports whether the pane shows the input composer rather
// than a dialog drawn over it.
func codexStartupReady(content string) bool {
	return strings.Contains(content, codexComposerPlaceholder)
}

// codexStartupOffersUpgrade reports whether the dialog on screen is the model
// migration, the one dialog whose default answer is wrong.
func codexStartupOffersUpgrade(content string) bool {
	return strings.Contains(content, codexUpgradeOption)
}

// codexStartupOffersExistingModel reports whether that dialog can be answered
// without changing the model.
func codexStartupOffersExistingModel(content string) bool {
	return strings.Contains(content, codexExistingModelOption)
}

// codexStartupSelectionIsExistingModel reports whether the highlighted row is
// the keep-the-configured-model option. The dialog renders below whatever it
// covers, so the last "›" line in the pane is its selection; an earlier one
// belongs to prior content.
func codexStartupSelectionIsExistingModel(content string) bool {
	selected := ""
	for line := range strings.SplitSeq(content, "\n") {
		if strings.Contains(line, "›") {
			selected = line
		}
	}
	return strings.Contains(selected, codexExistingModelOption)
}

func (c *Codex) startTmuxSession(name, dir, home string, args ...string) (*TmuxSession, error) {
	tmuxArgs := append([]string{"CODEX_HOME=" + home, "HOME=" + os.Getenv("HOME"), "TERM=" + os.Getenv("TERM")}, args...)
	return NewTmuxSession(name, dir, []string{"CODEX_HOME", "ENTIRE_TEST_TTY"}, "env", tmuxArgs...)
}

// seedCodexHome writes trust + feature flag config and links auth credentials
// so Codex loads the project's .codex/ layer and can authenticate.
func seedCodexHome(home, projectDir string) error {
	if err := os.MkdirAll(home, 0o750); err != nil {
		return err
	}

	// Write config with trust, feature flag, and the model to run on. Pinning
	// the model does not suppress upgrade dialogs: Codex announces a successor
	// for the configured model, so the pin selects which dialog appears rather
	// than whether one does. dismissStartupDialogs answers it.
	model := os.Getenv("E2E_CODEX_MODEL")
	if model == "" {
		model = "gpt-5.6-terra"
	}
	config := fmt.Sprintf("model = %q\n\n[features]\nhooks = true\n\n[projects.%q]\ntrust_level = \"trusted\"\n", model, projectDir)

	// Codex 0.129+ refuses to run unmanaged hooks until each one has a
	// trusted_hash entry in the user's config. Compute the hashes the same
	// way Codex does and pre-trust them — without this, every hook in
	// .codex/hooks.json sits as Untrusted and our hooks never fire under e2e.
	trustState, err := codexHookTrustState(projectDir)
	if err != nil {
		return fmt.Errorf("compute hook trust state: %w", err)
	}
	if trustState != "" {
		config += "\n" + trustState
	}

	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		return err
	}

	if apiKey := os.Getenv("OPENAI_API_KEY"); apiKey != "" {
		return writeCodexAPIKeyAuth(home, apiKey)
	}

	// Symlink auth.json from the real ~/.codex/ so API credentials are available.
	if realHome, err := os.UserHomeDir(); err == nil {
		src := filepath.Join(realHome, ".codex", "auth.json")
		if _, err := os.Stat(src); err == nil {
			_ = os.Symlink(src, filepath.Join(home, "auth.json"))
		}
	}

	return nil
}

func writeCodexAPIKeyAuth(home, apiKey string) error {
	auth := struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	}{
		AuthMode:     "apikey",
		OpenAIAPIKey: apiKey,
	}

	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal codex auth: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		return fmt.Errorf("write codex auth: %w", err)
	}

	return nil
}
