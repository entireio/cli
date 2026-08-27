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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "antigravity" {
		return
	}
	if _, err := exec.LookPath(antigravityBinary); err != nil {
		return
	}
	Register(&Antigravity{})
	RegisterGate("antigravity", antigravityDefaultConcurrency)
}

const (
	antigravityBinary = "agy"
	// antigravityDefaultModel is the model slug agy lists in `agy models`
	// (gemini-3.5-flash-low ↔ "Gemini 3.5 Flash (Low)"). Slugs are the
	// documented --model form and resolve in every auth mode.
	antigravityDefaultModel       = "gemini-3.5-flash-low"
	antigravityDefaultConcurrency = 1
	antigravityADCEnvKey          = "USE_ADC"
	googleCredentialsEnvKey       = "GOOGLE_APPLICATION_CREDENTIALS"
	googleCloudProjectEnvKey      = "GOOGLE_CLOUD_PROJECT"
	antigravityProjectEnvKey      = "E2E_ANTIGRAVITY_PROJECT"
	// geminiAPIKeyEnvKey enables agy's API-key auth (agy >= 1.1.13): with
	// modelProvider "gemini" in the isolated HOME's settings.json, model
	// requests go straight to the Gemini API and no account session, keyring
	// or browser flow is involved. This is what lets antigravity run in the
	// default CI matrix alongside gemini-cli, which shares the same secret.
	geminiAPIKeyEnvKey = "GEMINI_API_KEY"
	// googleAPIKeyEnvKey is scrubbed from every spawned env: agy prefers it
	// over GEMINI_API_KEY when both are set ("Warning: Both GOOGLE_API_KEY and
	// GEMINI_API_KEY are set. Using GOOGLE_API_KEY."), so a stray developer
	// key must not silently replace the one the harness was told to use.
	googleAPIKeyEnvKey = "GOOGLE_API_KEY"
)

// antigravityAuthMode is how the spawned agy authenticates. Resolution order
// is deliberate: explicit ADC credentials are an antigravity-specific choice
// and win; GEMINI_API_KEY is ambient in CI (shared with gemini-cli) and is the
// default there; with neither, the developer's real HOME (interactive OAuth
// login) is used so local runs work without extra setup.
type antigravityAuthMode int

const (
	antigravityAuthOAuth antigravityAuthMode = iota
	antigravityAuthADC
	antigravityAuthAPIKey
)

func (m antigravityAuthMode) String() string {
	switch m {
	case antigravityAuthADC:
		return "adc"
	case antigravityAuthAPIKey:
		return "gemini-api-key"
	case antigravityAuthOAuth:
		return "oauth"
	}
	return "oauth"
}

func antigravityAuthModeFrom(env []string) antigravityAuthMode {
	if antigravityHasADCCredentials(env) {
		return antigravityAuthADC
	}
	if value, ok := antigravityEnvValue(env, geminiAPIKeyEnvKey); ok && strings.TrimSpace(value) != "" {
		return antigravityAuthAPIKey
	}
	return antigravityAuthOAuth
}

// antigravityUsesIsolatedHome reports whether agy runs with HOME redirected to
// the per-repo test home (every non-OAuth mode). Every path that peeks at agy's
// on-disk state must agree with this or it reads the wrong tree.
func antigravityUsesIsolatedHome(env []string) bool {
	return antigravityAuthModeFrom(env) != antigravityAuthOAuth
}

// Antigravity implements the Agent interface for the agy CLI (Antigravity 2.0,
// successor to Gemini CLI).
type Antigravity struct{}

func (a *Antigravity) Name() string        { return "antigravity" }
func (a *Antigravity) Binary() string      { return antigravityBinary }
func (a *Antigravity) EntireAgent() string { return "antigravity" }

// PromptPattern is a placeholder until the real interactive prompt pattern is
// observed against a working `agy` session. Tracked in the Deferred table.
func (a *Antigravity) PromptPattern() string      { return `>` }
func (a *Antigravity) TimeoutMultiplier() float64 { return 2.5 }

func antigravityModel() string {
	if model := os.Getenv("E2E_ANTIGRAVITY_MODEL"); model != "" {
		return model
	}
	return antigravityDefaultModel
}

func (a *Antigravity) Bootstrap() error {
	return antigravityBootstrap(os.Environ())
}

func antigravityBootstrap(env []string) error {
	credentialsPath, hasCredentials := antigravityEnvValue(env, googleCredentialsEnvKey)
	if hasCredentials && credentialsPath != "" {
		if _, err := os.Stat(credentialsPath); err != nil {
			return fmt.Errorf("antigravity E2E GOOGLE_APPLICATION_CREDENTIALS %q is not readable: %w", credentialsPath, err)
		}
		return nil
	}
	if antigravityAuthModeFrom(env) == antigravityAuthAPIKey {
		return nil
	}
	if antigravityInCI(env) {
		return errors.New("antigravity E2E in CI requires GEMINI_API_KEY (agy API-key mode) or GOOGLE_APPLICATION_CREDENTIALS (ADC via the ANTIGRAVITY_GOOGLE_APPLICATION_CREDENTIALS_JSON GitHub secret)")
	}
	return nil
}

func antigravityInCI(env []string) bool {
	return antigravityTruthyEnv(env, "CI") || antigravityTruthyEnv(env, "GITHUB_ACTIONS")
}

func antigravityTruthyEnv(env []string, key string) bool {
	value, ok := antigravityEnvValue(env, key)
	if !ok {
		return false
	}
	value = strings.TrimSpace(strings.ToLower(value))
	return value != "" && value != "0" && value != "false"
}

func (a *Antigravity) IsTransientError(out Output, err error) bool {
	combined := out.Stdout + out.Stderr
	if err != nil {
		combined += "\n" + err.Error()
	}
	// Fatal configuration/entitlement walls win over every transient signal:
	// agy wraps an exhausted individual quota in "overloaded"/"429", which the
	// transient patterns would otherwise match, sending the harness into a
	// futile scenario-restart loop against a wall that won't clear for days.
	if _, fatal := antigravityFatalError(combined); fatal {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return antigravityContainsTransient(combined) || antigravityRawToolCallOutput(combined)
}

// antigravityFatalError detects non-retryable walls that no scenario restart
// will clear: an exhausted individual quota, a gated/disabled Code Assist
// backend, or a missing agy login. Returns a human-actionable message and true
// when one is present. Also matches its own generated messages so a fatal
// finding folded into an error stays fatal on reclassification.
// See reference_antigravity_auth_entitlement.
func antigravityFatalError(content string) (string, bool) {
	switch {
	case strings.Contains(content, "Individual quota reached"),
		strings.Contains(content, "agy individual quota exhausted"):
		return "agy individual quota exhausted (consumer tier resets on a rolling window) — use an entitled account/ADC or wait for the reset shown in the agy log", true
	case strings.Contains(content, "SERVICE_DISABLED"),
		strings.Contains(content, "AUTH_PERMISSION_DENIED"),
		strings.Contains(content, "agy backend not provisioned"):
		return "agy backend not provisioned for this project/identity (cloudcode-pa is gated) — needs a Gemini Code Assist subscription + seat, not just an enabled API", true
	case strings.Contains(content, "GEMINI_API_KEY environment variable is not set"),
		strings.Contains(content, "agy Gemini API key mode misconfigured"):
		return "agy Gemini API key mode misconfigured: modelProvider is gemini but GEMINI_API_KEY is unset in the spawned env", true
	case strings.Contains(content, "API_KEY_INVALID"),
		strings.Contains(content, "API key not valid"),
		strings.Contains(content, "agy rejected the Gemini API key"):
		return "agy rejected the Gemini API key (API_KEY_INVALID) — check the GEMINI_API_KEY secret", true
	case strings.Contains(content, "not logged into Antigravity"),
		strings.Contains(content, "agy is not logged in"):
		return "agy is not logged in — authenticate with `agy` interactively, set GEMINI_API_KEY, or provide ADC credentials", true
	}
	return "", false
}

// antigravityFatalFromLogs scans agy CLI log files modified since `since` for
// fatal wall markers. Headless (-p) runs never surface the quota detail on
// stderr, and the transcript's ERROR_MESSAGE carries only the generic
// "overloaded" text — "Individual quota reached" lands solely in
// ~/.gemini/antigravity-cli/log/cli-*.log. Without this peek the harness
// classifies the quota wall as transient and retries into it.
// E2E_ANTIGRAVITY_LOG_DIR overrides the directory (tests).
func antigravityFatalFromLogs(since time.Time, repoDir string) (string, bool) {
	dir := os.Getenv("E2E_ANTIGRAVITY_LOG_DIR")
	if dir == "" {
		home := antigravityHomeDir(repoDir)
		if home == "" {
			return "", false
		}
		dir = filepath.Join(home, ".gemini", "antigravity-cli", "log")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "cli-") {
			continue
		}
		info, infoErr := e.Info()
		if infoErr != nil || info.ModTime().Before(since) {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			continue
		}
		if msg, fatal := antigravityFatalError(string(data)); fatal {
			return msg, true
		}
	}
	return "", false
}

func antigravityContainsTransient(content string) bool {
	transientPatterns := []string{
		"timed out waiting for response",
		"429",
		"RESOURCE_EXHAUSTED",
		"UNAVAILABLE",
		"DEADLINE_EXCEEDED",
		"INTERNAL",
		"503",
		"ECONNRESET",
		"ETIMEDOUT",
		"overloaded",
		"intermittent errors",
	}
	for _, p := range transientPatterns {
		if strings.Contains(content, p) {
			return true
		}
	}
	return false
}

func (a *Antigravity) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: antigravityModel()}
	for _, o := range opts {
		o(cfg)
	}

	timeout := 60 * time.Second
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}
	promptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startedAt := time.Now().Add(-2 * time.Second)

	headAtStart := ""
	if antigravityPromptRequestsCommit(prompt) {
		headAtStart = antigravityGitHead(dir)
	}

	out, err := a.runPromptOnce(promptCtx, dir, prompt, cfg.Model)
	if err == nil && antigravityShouldRetryMissingCommit(prompt, dir, headAtStart) {
		err = errors.New("antigravity did not create requested commit; HEAD unchanged")
	}

	if msg, ok := antigravityPromptTranscriptTransient(antigravityBrainDir(dir), prompt, startedAt); ok {
		if err == nil {
			err = errors.New(msg)
		} else {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}

	// Surface fatal config/entitlement walls as a clear, actionable error so the
	// test fails fast with a legible reason instead of a generic timeout/restart.
	if msg, fatal := antigravityFatalError(out.Stdout + out.Stderr); fatal {
		if err == nil {
			err = errors.New(msg)
		} else {
			err = fmt.Errorf("%s (%w)", msg, err)
		}
	} else if err != nil {
		// The stdout/stderr surface can be clean while the wall is only in
		// agy's CLI log (headless quota exhaustion). Peek before letting the
		// error be classified transient.
		if logMsg, fatal := antigravityFatalFromLogs(startedAt, dir); fatal {
			err = fmt.Errorf("%s (%w)", logMsg, err)
		}
	}

	return out, err
}

func (a *Antigravity) runPromptOnce(ctx context.Context, dir string, prompt string, model string) (Output, error) {
	// agy -p ignores cwd: without --add-dir it runs in
	// ~/.gemini/antigravity-cli/scratch/ instead of the test repo
	// (observed in PR #1287 validation — agy initialized a brand-new git
	// repo in scratch and committed the requested file there). --add-dir
	// pins agy to the workspace we actually want it to modify.
	args, displayArgs := antigravityPromptArgs(prompt, dir, model)

	if err := antigravityPrepareHome(os.Environ(), dir); err != nil {
		return Output{Command: a.Binary() + " " + strings.Join(displayArgs, " ")}, err
	}

	cmd := exec.CommandContext(ctx, a.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = antigravityPromptEnv(dir)
	setupProcessGroup(cmd)
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
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
		}
	}
	if err == nil && antigravityAuthenticationRequired(stdout.String()+"\n"+stderr.String()) {
		err = errors.New("antigravity authentication required or timed out")
	}
	if err == nil && antigravityRawToolCallOutput(stdout.String()) {
		err = errors.New("antigravity emitted raw tool call output")
	}

	return Output{
		Command:  a.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (a *Antigravity) StartSession(_ context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("antigravity-test-%d", time.Now().UnixNano())

	if err := antigravityPrepareHome(os.Environ(), dir); err != nil {
		return nil, err
	}
	envArgs, unsetEnv := antigravitySessionEnv(os.Environ(), dir)

	args := append([]string{"env"}, envArgs...)
	args = append(args, antigravitySessionCLIArgsFromEnv(dir, antigravityModel(), os.Environ())...)
	s, err := NewTmuxSession(name, dir, unsetEnv, args[0], args[1:]...)
	if err != nil {
		return nil, err
	}

	for range 10 {
		content, err := s.WaitFor(`(>|trust|Enter to select|Enter to confirm|Acknowledge)`, 30*time.Second)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for startup prompt: %w", err)
		}
		if antigravityNeedsStartupConfirmation(content) {
			_ = s.SendKeys("Enter")
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if antigravityReadyForPrompt(content) {
			break
		}
		_ = s.SendKeys("Enter")
		time.Sleep(500 * time.Millisecond)
	}
	s.stableAtSend = ""

	return &antigravityInteractiveSession{Session: s, dir: dir}, nil
}

type antigravityInteractiveSession struct {
	Session

	dir             string
	lastInput       string
	headAtSend      string
	commitRequested bool
	commitRetried   bool
}

func (s *antigravityInteractiveSession) Send(input string) error {
	s.lastInput = input
	s.commitRequested = antigravityPromptRequestsCommit(input)
	s.commitRetried = false
	s.headAtSend = ""
	if s.commitRequested {
		s.headAtSend = antigravityGitHead(s.dir)
	}
	return s.Session.Send(antigravityWorkspacePrompt(input, s.dir))
}

func (s *antigravityInteractiveSession) WaitFor(pattern string, timeout time.Duration) (string, error) {
	var content string
	var err error
	for range 3 {
		content, err = s.Session.WaitFor(pattern, timeout)
		if err != nil || s.lastInput == "" {
			return content, err
		}
		if antigravityRawToolCallOutput(content) {
			if sendErr := s.Session.Send(antigravityWorkspacePrompt(s.lastInput, s.dir)); sendErr != nil {
				return content, sendErr
			}
			continue
		}
		if s.shouldRetryMissingCommit() {
			s.commitRetried = true
			followup := antigravityMissingCommitPrompt(s.lastInput)
			if sendErr := s.Session.Send(antigravityWorkspacePrompt(followup, s.dir)); sendErr != nil {
				return content, sendErr
			}
			continue
		}
		return content, nil
	}
	// Retries exhausted. If the final response is still raw tool-call output,
	// returning it with a nil error would report a malformed turn as success
	// (and the test would fail later on an unrelated assertion). Surface it as
	// a failure — IsTransientError recognizes this message and restarts the
	// scenario.
	if antigravityRawToolCallOutput(content) {
		return content, errors.New("antigravity emitted raw tool call output after retries")
	}
	return content, err
}

func (s *antigravityInteractiveSession) shouldRetryMissingCommit() bool {
	return s.commitRequested && !s.commitRetried && s.headAtSend != "" && antigravityGitHead(s.dir) == s.headAtSend
}

func antigravityPromptRequestsCommit(input string) bool {
	lower := strings.ToLower(input)
	if strings.Contains(lower, "do not commit") || strings.Contains(lower, "don't commit") || strings.Contains(lower, "dont commit") {
		return false
	}
	return strings.Contains(lower, "commit")
}

func antigravityShouldRetryMissingCommit(prompt string, dir string, headAtStart string) bool {
	return antigravityPromptRequestsCommit(prompt) && headAtStart != "" && antigravityGitHead(dir) == headAtStart
}

func antigravityGitHead(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func antigravityMissingCommitPrompt(previous string) string {
	return "The previous request asked for a git commit, but HEAD has not changed. Complete the previous request now by staging the changed files and running git commit with an appropriate message. Do not modify repository file contents in this follow-up; only run git status, git add, and git commit as needed. Do not respond until the git commit command exits successfully.\n\nPrevious request:\n" + previous
}

func antigravitySessionCLIArgsFromEnv(dir, model string, env []string) []string {
	if model == "" {
		model = antigravityDefaultModel
	}
	args := []string{
		antigravityBinary,
		"--model", model,
		"--dangerously-skip-permissions",
		"--new-project",
		"--add-dir", dir,
	}
	if projectID := antigravityProjectID(env); projectID != "" {
		args = append(args, "--project", projectID)
	}
	return args
}

func antigravityPromptArgs(prompt, dir, model string) ([]string, []string) {
	return antigravityPromptArgsFromEnv(prompt, dir, model, os.Environ())
}

func antigravityPromptArgsFromEnv(prompt, dir, model string, env []string) ([]string, []string) {
	if model == "" {
		model = antigravityDefaultModel
	}
	workspacePrompt := antigravityWorkspacePrompt(prompt, dir)
	args := []string{
		"-p", workspacePrompt,
		"--model", model,
		"--dangerously-skip-permissions",
		"--new-project",
		"--add-dir", dir,
	}
	displayArgs := []string{
		"-p", fmt.Sprintf("%q", workspacePrompt),
		"--model", model,
		"--dangerously-skip-permissions",
		"--new-project",
		"--add-dir", dir,
	}
	if projectID := antigravityProjectID(env); projectID != "" {
		args = append(args, "--project", projectID)
		displayArgs = append(displayArgs, "--project", projectID)
	}
	return args, displayArgs
}

func antigravityWorkspacePrompt(prompt, dir string) string {
	return fmt.Sprintf("Use the workspace at %s. Resolve any relative file paths in the request relative to that workspace, and write files inside that workspace unless the request says otherwise. Complete every requested operation before responding, including every git command or commit mentioned in the request. If the request asks for a commit, run a shell command such as git add and git commit; editing files is not a completed commit. Do not claim a file was committed until the git command has completed successfully. Do not run verification commands such as list_dir or view_file unless requested. If the request has numbered steps, complete every numbered step in order. For multi-step requests with file contents and git commands, use shell commands when that is the most direct way to preserve order. Do not create artifacts for repository files; create or edit files in the workspace. After completing the requested change, do not run extra checks or commands; immediately respond with a short confirmation.\n\nRequest:\n%s", dir, prompt)
}

func antigravityTestHomeDir(repoDir string) string {
	return filepath.Join(filepath.Dir(repoDir), filepath.Base(repoDir)+"-antigravity-home")
}

// antigravityHomeDir resolves the HOME the agy subprocess actually ran with:
// the per-repo isolated test home in ADC and API-key mode
// (antigravityPromptEnvFrom sets HOME=antigravityTestHomeDir), the developer's
// real HOME otherwise. Every path that peeks at agy's on-disk state (brain,
// CLI logs) must go through this, or isolated-home runs read the wrong tree.
func antigravityHomeDir(repoDir string) string {
	if antigravityUsesIsolatedHome(os.Environ()) {
		return antigravityTestHomeDir(repoDir)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return homeDir
}

func antigravityBrainDir(repoDir string) string {
	homeDir := antigravityHomeDir(repoDir)
	if homeDir == "" {
		return ""
	}
	return filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain")
}

func antigravityPromptTranscriptTransient(brainDir, prompt string, since time.Time) (string, bool) {
	if brainDir == "" || prompt == "" {
		return "", false
	}
	for _, path := range antigravityTranscriptCandidates(brainDir) {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		if msg, ok := antigravityTranscriptFileTransient(path, prompt); ok {
			return msg, true
		}
	}
	return "", false
}

func antigravityTranscriptCandidates(brainDir string) []string {
	var candidates []string
	for _, name := range []string{"transcript_full.jsonl", "transcript.jsonl"} {
		matches, err := filepath.Glob(filepath.Join(brainDir, "*", ".system_generated", "logs", name))
		if err == nil {
			candidates = append(candidates, matches...)
		}
	}
	return candidates
}

func antigravityTranscriptFileTransient(path, prompt string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}

	var sawPrompt bool
	var transient string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var step struct {
			Type      string `json:"type"`
			Content   string `json:"content"`
			Error     string `json:"error"`
			ErrorCode int    `json:"error_code"`
		}
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			continue
		}
		if step.Type == "USER_INPUT" && strings.Contains(step.Content, prompt) {
			sawPrompt = true
		}
		if step.Type == "ERROR_MESSAGE" {
			msg := fmt.Sprintf("error_code=%d: %s", step.ErrorCode, step.Error)
			if step.ErrorCode == 429 || antigravityContainsTransient(msg) {
				transient = "antigravity transcript contains ERROR_MESSAGE " + msg
			}
		}
	}
	return transient, sawPrompt && transient != ""
}

func antigravityPromptEnv(repoDir string) []string {
	return antigravityPromptEnvFrom(os.Environ(), repoDir)
}

// antigravityPromptEnvFrom returns the env for spawning agy in print mode.
// Antigravity's OAuth state lives under HOME/.gemini, so both credential modes
// isolate HOME to the per-repo test home so no browser/keyring flow can start:
//   - ADC: USE_ADC=1 plus the ADC project; API keys are scrubbed so agy's
//     default backend is used.
//   - GEMINI_API_KEY: the key passes through (GOOGLE_GEMINI_BASE_URL too);
//     GOOGLE_API_KEY is scrubbed (agy would prefer it), and ADC-only knobs are
//     dropped. antigravityPrepareHome writes the modelProvider setting the
//     mode requires.
//
// With neither, the developer's real HOME is kept for local OAuth E2E.
func antigravityPromptEnvFrom(base []string, repoDir string) []string {
	switch antigravityAuthModeFrom(base) {
	case antigravityAuthADC:
		env := append(
			filterEnv(base, "ENTIRE_TEST_TTY", "ACCESSIBLE", "HOME", antigravityADCEnvKey, googleCloudProjectEnvKey,
				geminiAPIKeyEnvKey, googleAPIKeyEnvKey),
			"ACCESSIBLE=1",
			antigravityADCEnvKey+"=1",
			"HOME="+antigravityTestHomeDir(repoDir),
		)
		if projectID := antigravityProjectID(base); projectID != "" {
			env = append(env, googleCloudProjectEnvKey+"="+projectID)
		}
		return env
	case antigravityAuthAPIKey:
		return append(
			filterEnv(base, "ENTIRE_TEST_TTY", "ACCESSIBLE", "HOME", antigravityADCEnvKey, googleCloudProjectEnvKey, googleAPIKeyEnvKey),
			"ACCESSIBLE=1",
			"HOME="+antigravityTestHomeDir(repoDir),
		)
	case antigravityAuthOAuth:
		return append(filterEnv(base, "ENTIRE_TEST_TTY", "ACCESSIBLE"), "ACCESSIBLE=1")
	}
	return append(filterEnv(base, "ENTIRE_TEST_TTY", "ACCESSIBLE"), "ACCESSIBLE=1")
}

// antigravityPrepareHome makes the isolated test home ready for the resolved
// auth mode before agy is spawned. Only API-key mode needs on-disk state:
// agy reads modelProvider from HOME/.gemini/antigravity-cli/settings.json and
// refuses to start in API-key mode without it. No-op for ADC/OAuth.
func antigravityPrepareHome(env []string, repoDir string) error {
	if antigravityAuthModeFrom(env) != antigravityAuthAPIKey {
		return nil
	}
	return antigravityEnsureAPIKeyHome(repoDir)
}

// antigravityEnsureAPIKeyHome writes modelProvider "gemini" into the isolated
// home's agy settings.json, preserving any other keys already there (agy
// itself writes into this file once it runs). Idempotent.
func antigravityEnsureAPIKeyHome(repoDir string) error {
	dir := filepath.Join(antigravityTestHomeDir(repoDir), ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("antigravity E2E: create isolated agy home: %w", err)
	}
	settingsPath := filepath.Join(dir, "settings.json")
	settings := map[string]json.RawMessage{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &settings); err != nil {
				return fmt.Errorf("antigravity E2E: parse %s: %w", settingsPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("antigravity E2E: read %s: %w", settingsPath, err)
	}
	if string(settings["modelProvider"]) == `"gemini"` {
		return nil
	}
	settings["modelProvider"] = json.RawMessage(`"gemini"`)
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("antigravity E2E: encode settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("antigravity E2E: write %s: %w", settingsPath, err)
	}
	return nil
}

func antigravitySessionEnv(base []string, repoDir string) ([]string, []string) {
	envArgs := []string{"ACCESSIBLE=1"}
	unsetEnv := []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}
	if term, ok := antigravityEnvValue(base, "TERM"); ok && term != "" {
		envArgs = append(envArgs, "TERM="+term)
	}
	switch antigravityAuthModeFrom(base) {
	case antigravityAuthADC:
		envArgs = append(envArgs, antigravityADCEnvKey+"=1", "HOME="+antigravityTestHomeDir(repoDir))
		unsetEnv = append(unsetEnv, "HOME", antigravityADCEnvKey, geminiAPIKeyEnvKey, googleAPIKeyEnvKey)
		if projectID := antigravityProjectID(base); projectID != "" {
			envArgs = append(envArgs, googleCloudProjectEnvKey+"="+projectID)
			unsetEnv = append(unsetEnv, googleCloudProjectEnvKey)
		}
	case antigravityAuthAPIKey:
		envArgs = append(envArgs, "HOME="+antigravityTestHomeDir(repoDir))
		unsetEnv = append(unsetEnv, "HOME", antigravityADCEnvKey, googleCloudProjectEnvKey, googleAPIKeyEnvKey)
	case antigravityAuthOAuth:
		// Developer's real HOME and login; nothing to redirect.
	}
	return envArgs, unsetEnv
}

func antigravityNeedsStartupConfirmation(content string) bool {
	confirmationMarkers := []string{
		"Do you trust the contents of this project?",
		"Yes, I trust this folder",
		"Enter to select",
		"Enter to confirm",
		"Acknowledge",
	}
	for _, marker := range confirmationMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func antigravityReadyForPrompt(content string) bool {
	return strings.Contains(content, ">") && !antigravityNeedsStartupConfirmation(content)
}

func antigravityAuthenticationRequired(content string) bool {
	content = strings.ToLower(content)
	return strings.Contains(content, "authentication required") ||
		strings.Contains(content, "authentication timed out")
}

func antigravityRawToolCallOutput(content string) bool {
	hasRawChannel := strings.Contains(content, "<|start|>assistant") ||
		strings.Contains(content, "<|channel|>")
	hasToolPayload := strings.Contains(content, "to=functions.") ||
		strings.Contains(content, `"CommandLine"`) ||
		strings.Contains(content, `"toolAction"`)
	return hasRawChannel && strings.Contains(content, "<|message|>") && hasToolPayload
}

func antigravityHasADCCredentials(env []string) bool {
	value, ok := antigravityEnvValue(env, googleCredentialsEnvKey)
	return ok && value != ""
}

func antigravityProjectID(env []string) string {
	for _, key := range []string{antigravityProjectEnvKey, googleCloudProjectEnvKey} {
		if value, ok := antigravityEnvValue(env, key); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return antigravityProjectIDFromADC(env)
}

func antigravityProjectIDFromADC(env []string) string {
	credentialsPath, ok := antigravityEnvValue(env, googleCredentialsEnvKey)
	if !ok || credentialsPath == "" {
		return ""
	}
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		return ""
	}
	var credentials struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return ""
	}
	return strings.TrimSpace(credentials.ProjectID)
}

func antigravityEnvValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix), true
		}
	}
	return "", false
}
