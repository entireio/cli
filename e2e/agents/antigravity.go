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
	antigravityBinary             = "agy"
	antigravityDefaultModel       = "Gemini 3.5 Flash (Low)"
	antigravityDefaultConcurrency = 1
	antigravityADCEnvKey          = "USE_ADC"
	googleCredentialsEnvKey       = "GOOGLE_APPLICATION_CREDENTIALS"
	googleCloudProjectEnvKey      = "GOOGLE_CLOUD_PROJECT"
	antigravityProjectEnvKey      = "E2E_ANTIGRAVITY_PROJECT"
)

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
	if antigravityInCI(env) {
		return errors.New("antigravity E2E in CI requires GOOGLE_APPLICATION_CREDENTIALS; configure the ANTIGRAVITY_GOOGLE_APPLICATION_CREDENTIALS_JSON GitHub secret")
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
// will clear: an exhausted individual quota (multi-day reset), a gated/disabled
// Code Assist backend, or a missing agy login. Returns a human-actionable
// message and true when one is present. See reference_antigravity_auth_entitlement.
func antigravityFatalError(content string) (string, bool) {
	switch {
	case strings.Contains(content, "Individual quota reached"):
		return "agy individual quota exhausted (consumer tier resets on a multi-day window) — use an entitled account/ADC or wait for the reset shown in the error", true
	case strings.Contains(content, "SERVICE_DISABLED"), strings.Contains(content, "AUTH_PERMISSION_DENIED"):
		return "agy backend not provisioned for this project/identity (cloudcode-pa is gated) — needs a Gemini Code Assist subscription + seat, not just an enabled API", true
	case strings.Contains(content, "not logged into Antigravity"):
		return "agy is not logged in — authenticate with `agy` interactively or provide ADC credentials", true
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

func antigravityBrainDir(repoDir string) string {
	var homeDir string
	if antigravityHasADCCredentials(os.Environ()) {
		homeDir = antigravityTestHomeDir(repoDir)
	} else {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return ""
		}
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
// Antigravity's OAuth state lives under HOME/.gemini. If service-account ADC
// credentials are available, USE_ADC lets tests isolate HOME without opening a
// browser auth flow. Otherwise keep the developer's real HOME for local E2E.
func antigravityPromptEnvFrom(base []string, repoDir string) []string {
	if antigravityHasADCCredentials(base) {
		env := append(
			filterEnv(base, "ENTIRE_TEST_TTY", "ACCESSIBLE", "HOME", antigravityADCEnvKey, googleCloudProjectEnvKey),
			"ACCESSIBLE=1",
			antigravityADCEnvKey+"=1",
			"HOME="+antigravityTestHomeDir(repoDir),
		)
		if projectID := antigravityProjectID(base); projectID != "" {
			env = append(env, googleCloudProjectEnvKey+"="+projectID)
		}
		return env
	}
	return append(filterEnv(base, "ENTIRE_TEST_TTY", "ACCESSIBLE"), "ACCESSIBLE=1")
}

func antigravitySessionEnv(base []string, repoDir string) ([]string, []string) {
	envArgs := []string{"ACCESSIBLE=1"}
	unsetEnv := []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}
	if term, ok := antigravityEnvValue(base, "TERM"); ok && term != "" {
		envArgs = append(envArgs, "TERM="+term)
	}
	if antigravityHasADCCredentials(base) {
		envArgs = append(envArgs, antigravityADCEnvKey+"=1", "HOME="+antigravityTestHomeDir(repoDir))
		unsetEnv = append(unsetEnv, "HOME", antigravityADCEnvKey)
		if projectID := antigravityProjectID(base); projectID != "" {
			envArgs = append(envArgs, googleCloudProjectEnvKey+"="+projectID)
			unsetEnv = append(unsetEnv, googleCloudProjectEnvKey)
		}
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
