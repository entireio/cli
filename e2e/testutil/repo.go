package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/agents"
	"github.com/entireio/cli/e2e/entire"
)

const droidRepoSettingsPath = ".factory/settings.json"

// RepoState holds the working state for a single test's cloned repository.
type RepoState struct {
	Agent            agents.Agent
	Dir              string
	ArtifactDir      string
	HeadBefore       string
	CheckpointBefore string
	ConsoleLog       *os.File
	session          agents.Session // interactive session, if started via StartSession
	skipArtifacts    bool           // suppresses artifact capture on scenario restart
}

// SetupRepo creates a fresh git repository in a temporary directory, seeds it
// with an initial commit, and runs `entire enable` for the given agent.
// Artifact capture is registered as a cleanup function.
//
// When E2E_KEEP_REPOS is set, the temporary directory is not cleaned up
// so it can be inspected after the test. A symlink in the artifact dir
// points to the preserved repo.
func SetupRepo(t *testing.T, agent agents.Agent) *RepoState {
	t.Helper()

	keepRepos := os.Getenv("E2E_KEEP_REPOS") != ""

	// Always use os.MkdirTemp instead of t.TempDir(). Go's t.TempDir()
	// creates nested subdirectories (TestName.../001/) whose structure
	// confuses some agents' (e.g. opencode) working-directory resolution.
	dir, err := os.MkdirTemp("", "e2e-repo-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	if keepRepos {
		t.Logf("E2E_KEEP_REPOS: repo will be preserved at %s", dir)
	} else {
		t.Cleanup(func() { os.RemoveAll(dir) })
	}

	// Resolve symlinks (macOS: /var -> /private/var) so paths match
	// what agent CLIs see when they resolve their own CWD.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	Git(t, dir, "init")
	Git(t, dir, "config", "user.name", "E2E Test")
	Git(t, dir, "config", "user.email", "e2e@test.local")
	Git(t, dir, "commit", "--allow-empty", "-m", "initial commit")

	entire.Enable(t, dir, agent.EntireAgent())
	if agent.Name() == "factoryai-droid" {
		if err := configureDroidRepoSettings(dir); err != nil {
			t.Fatalf("configure droid repo settings: %v", err)
		}
	}
	PatchSettings(t, dir, map[string]any{"log_level": "debug"})

	// OpenCode's non-interactive mode auto-rejects external_directory permission
	// since there's no user to prompt. Write a config to allow it.
	if agent.Name() == "opencode" {
		cfg := `{"$schema": "https://opencode.ai/config.json", "permission": {"external_directory": "allow"}}`
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			cfg = fmt.Sprintf(`{"$schema": "https://opencode.ai/config.json", "permission": {"external_directory": "allow"}, "provider": {"anthropic": {"options": {"apiKey": %q}}}}`, key)
		}
		if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(cfg+"\n"), 0o644); err != nil {
			t.Fatalf("write opencode.json: %v", err)
		}
	}

	// Create artifact dir eagerly so console.log is written to disk
	// incrementally. Even if the test is killed by a global timeout,
	// partial output survives.
	artDir := artifactDir(t)
	consoleLog, err := os.Create(filepath.Join(artDir, "console.log"))
	if err != nil {
		t.Fatalf("create console.log: %v", err)
	}

	state := &RepoState{
		Agent:            agent,
		Dir:              dir,
		ArtifactDir:      artDir,
		HeadBefore:       GitOutput(t, dir, "rev-parse", "HEAD"),
		CheckpointBefore: GitOutput(t, dir, "rev-parse", "entire/checkpoints/v1"),
		ConsoleLog:       consoleLog,
	}

	t.Cleanup(func() {
		_ = consoleLog.Close()
		if !state.skipArtifacts {
			CaptureArtifacts(t, state)
		}
	})

	return state
}

func configureDroidRepoSettings(repoDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	globalSettingsPath := filepath.Join(home, ".factory", "settings.json")
	repoSettingsPath := filepath.Join(repoDir, droidRepoSettingsPath)

	if err := mergeDroidCustomModels(globalSettingsPath, repoSettingsPath); err != nil {
		return err
	}
	if err := ensureGitInfoExcludeContains(repoDir, droidRepoSettingsPath); err != nil {
		return fmt.Errorf("exclude droid settings from git: %w", err)
	}
	return nil
}

func mergeDroidCustomModels(globalSettingsPath, repoSettingsPath string) error {
	globalSettings, err := loadJSONMap(globalSettingsPath, "global droid settings")
	if err != nil {
		return err
	}

	customModels, ok := globalSettings["customModels"]
	if !ok {
		return fmt.Errorf(
			"global droid settings at %s missing customModels; repo-local %s shadows global settings",
			globalSettingsPath,
			repoSettingsPath,
		)
	}

	var models []json.RawMessage
	if err := json.Unmarshal(customModels, &models); err != nil {
		return fmt.Errorf("parse customModels in %s: %w", globalSettingsPath, err)
	}
	if len(models) == 0 {
		return fmt.Errorf("global droid settings at %s has empty customModels", globalSettingsPath)
	}

	repoSettings, err := loadJSONMap(repoSettingsPath, "repo-local droid settings")
	if err != nil {
		return err
	}
	repoSettings["customModels"] = customModels

	out, err := json.MarshalIndent(repoSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal repo-local droid settings: %w", err)
	}
	out = append(out, '\n')

	if err := os.WriteFile(repoSettingsPath, out, 0o600); err != nil {
		return fmt.Errorf("write repo-local droid settings %s: %w", repoSettingsPath, err)
	}
	return nil
}

func loadJSONMap(path, description string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", description, path, err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse %s at %s: %w", description, path, err)
	}
	if parsed == nil {
		parsed = make(map[string]json.RawMessage)
	}
	return parsed, nil
}

func ensureGitInfoExcludeContains(repoDir, entry string) error {
	excludePath := filepath.Join(repoDir, ".git", "info", "exclude")

	data, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}

	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}

	var b strings.Builder
	if len(data) > 0 {
		b.Write(data)
		if data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	b.WriteString(entry)
	b.WriteByte('\n')

	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(excludePath), err)
	}
	if err := os.WriteFile(excludePath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", excludePath, err)
	}
	return nil
}

// ForEachAgent runs fn as a parallel subtest for every registered agent.
// It handles repo setup, concurrency gating, context timeout, and cleanup.
// The timeout is scaled by each agent's TimeoutMultiplier.
//
// If RunPrompt detects a transient API error (e.g. rate limit, token refresh
// failure), it panics with errScenarioRestart. ForEachAgent recovers from the
// panic and restarts the entire scenario with a fresh repository, up to
// maxScenarioRestarts times. This avoids stale CLI session state from the
// failed attempt poisoning the retry.
func ForEachAgent(t *testing.T, timeout time.Duration, fn func(t *testing.T, s *RepoState, ctx context.Context)) {
	t.Helper()
	t.Parallel()
	all := agents.All()
	if len(all) == 0 {
		t.Skip("no agents registered (check E2E_AGENT filter)")
	}
	for _, agent := range all {
		t.Run(agent.Name(), func(t *testing.T) {
			// Use the global test deadline for slot wait so we don't
			// skip prematurely — only bail if the whole binary is dying.
			slotCtx := context.Background()
			if deadline, ok := t.Deadline(); ok {
				var cancel context.CancelFunc
				slotCtx, cancel = context.WithDeadline(slotCtx, deadline)
				defer cancel()
			}
			if err := agents.AcquireSlot(slotCtx, agent); err != nil {
				t.Fatalf("timed out waiting for agent slot: %v", err)
			}
			defer agents.ReleaseSlot(agent)

			// Per-test timeout starts after slot is acquired, scaled
			// by the agent's multiplier (e.g. 2.5× for gemini).
			scaled := time.Duration(float64(timeout) * agent.TimeoutMultiplier())

			var prevState *RepoState
			for attempt := range maxScenarioRestarts + 1 {
				s := SetupRepo(t, agent)
				ctx, cancel := context.WithTimeout(context.Background(), scaled)

				// On restart, suppress artifact capture from the previous
				// failed attempt so it doesn't overwrite the final one.
				if prevState != nil {
					prevState.skipArtifacts = true
				}

				restarted := runScenario(t, s, ctx, fn)
				cancel()

				if !restarted {
					return
				}
				prevState = s
				if attempt >= maxScenarioRestarts {
					t.Fatalf("exhausted %d scenario attempts due to transient API errors", maxScenarioRestarts+1)
				}
				t.Logf("transient error, restarting scenario (attempt %d/%d)", attempt+2, maxScenarioRestarts+1)
			}
		})
	}
}

// runScenario runs the test function and recovers from errScenarioRestart
// panics triggered by transient API errors in RunPrompt. Returns true if
// the scenario should be restarted with a fresh repository.
func runScenario(t *testing.T, s *RepoState, ctx context.Context, fn func(t *testing.T, s *RepoState, ctx context.Context)) (restarted bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(errScenarioRestart); ok {
				restarted = true
				return
			}
			panic(r) // re-panic for unexpected panics
		}
	}()
	fn(t, s, ctx)
	return false
}

const maxScenarioRestarts = 2 // restart up to 2 times = 3 total attempts

// errScenarioRestart is panicked by RunPrompt when a transient API error is
// detected. ForEachAgent recovers from this panic and restarts the test
// scenario with a fresh repository, avoiding stale CLI state from the failed
// attempt.
type errScenarioRestart struct {
	msg string
}

// RunPrompt runs an agent prompt, logs the command and output to ConsoleLog,
// and returns the result. If the agent reports a transient API error, it
// panics with errScenarioRestart to trigger a full scenario restart in
// ForEachAgent (see runScenario).
func (s *RepoState) RunPrompt(t *testing.T, ctx context.Context, prompt string, opts ...agents.Option) (agents.Output, error) {
	t.Helper()
	out, err := s.Agent.RunPrompt(ctx, s.Dir, prompt, opts...)
	s.logPromptResult(out)

	if err != nil && s.Agent.IsTransientError(out, err) {
		errMsg := fmt.Sprintf("transient API error (stderr: %s)", strings.TrimSpace(out.Stderr))
		t.Logf("%s — restarting scenario", errMsg)
		fmt.Fprintf(s.ConsoleLog, "> [transient] %s — restarting scenario\n", errMsg)
		panic(errScenarioRestart{msg: errMsg})
	}

	return out, err
}

func (s *RepoState) logPromptResult(out agents.Output) {
	s.ConsoleLog.WriteString("> " + out.Command + "\n")
	s.ConsoleLog.WriteString("stdout:\n" + out.Stdout + "\n")
	s.ConsoleLog.WriteString("stderr:\n" + out.Stderr + "\n")
}

// Git runs a git command in the repo and logs it to ConsoleLog.
func (s *RepoState) Git(t *testing.T, args ...string) {
	t.Helper()
	s.ConsoleLog.WriteString("> git " + strings.Join(args, " ") + "\n")
	Git(t, s.Dir, args...)
}

// StartSession starts an interactive session and registers it for pane
// capture in artifacts. Returns nil if the agent does not support interactive
// mode. The session is closed automatically during test cleanup.
func (s *RepoState) StartSession(t *testing.T, ctx context.Context) agents.Session {
	t.Helper()
	session, err := s.Agent.StartSession(ctx, s.Dir)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if session == nil {
		return nil
	}
	s.session = session
	return session
}

// WaitFor waits for a pattern in the interactive session's pane and logs the
// pane content to ConsoleLog after the wait completes (success or failure).
func (s *RepoState) WaitFor(t *testing.T, session agents.Session, pattern string, timeout time.Duration) {
	t.Helper()
	content, err := session.WaitFor(pattern, timeout)
	fmt.Fprintf(s.ConsoleLog, "> pane after WaitFor(%q):\n%s\n", pattern, content)
	if err != nil {
		t.Fatalf("WaitFor(%q): %v", pattern, err)
	}
}

// Send sends input to an interactive session and logs it to ConsoleLog.
// Fails the test on error.
func (s *RepoState) Send(t *testing.T, session agents.Session, input string) {
	t.Helper()
	s.ConsoleLog.WriteString("> send: " + input + "\n")
	if err := session.Send(input); err != nil {
		t.Fatalf("send failed: %v", err)
	}
}

// PatchSettings merges extra keys into .entire/settings.json.
func PatchSettings(t *testing.T, dir string, extra map[string]any) {
	t.Helper()
	path := filepath.Join(dir, ".entire", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	for k, v := range extra {
		settings[k] = v
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// Git runs a git command in the given directory and fails the test if it
// returns a non-zero exit code.
func Git(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=0")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// GitOutput runs a git command in the given directory, returns its trimmed
// stdout, and fails the test on error.
func GitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}

	out, err := cmd.Output()
	if err != nil {
		var stderr string
		ee := &exec.ExitError{}
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, stderr)
	}

	return strings.TrimSpace(string(out))
}

// NewCheckpointCommits returns the SHAs of commits added to the
// entire/checkpoints/v1 branch since the test was set up, oldest first.
func NewCheckpointCommits(t *testing.T, s *RepoState) []string {
	t.Helper()

	log := GitOutput(t, s.Dir, "log", "--reverse", "--format=%H", s.CheckpointBefore+"..entire/checkpoints/v1")
	if log == "" {
		return nil
	}
	return strings.Split(log, "\n")
}

// CheckpointIDs lists all checkpoint IDs from the tree at the tip of the
// checkpoint branch. It parses the two-level directory structure
// ({prefix}/{suffix}/metadata.json) and returns the concatenated IDs.
func CheckpointIDs(t *testing.T, dir string) []string {
	t.Helper()
	out := gitOutputSafe(dir, "ls-tree", "-r", "--name-only", "entire/checkpoints/v1")
	if out == "" {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// Match {prefix}/{suffix}/metadata.json (top-level, not session-level)
		parts := strings.Split(line, "/")
		if len(parts) == 3 && parts[2] == "metadata.json" {
			id := parts[0] + parts[1]
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// ReadCheckpointMetadata reads the checkpoint-level metadata.json from the
// tip of the checkpoint branch for the given checkpoint ID.
func ReadCheckpointMetadata(t *testing.T, dir string, checkpointID string) CheckpointMetadata {
	t.Helper()

	path := CheckpointPath(checkpointID) + "/metadata.json"
	blob := "entire/checkpoints/v1:" + path

	raw := GitOutput(t, dir, "show", blob)

	var meta CheckpointMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("unmarshal checkpoint metadata from %s: %v", blob, err)
	}

	return meta
}

// ReadSessionMetadata reads a session's metadata.json from the tip of the
// checkpoint branch for the given checkpoint ID and session index.
func ReadSessionMetadata(t *testing.T, dir string, checkpointID string, sessionIndex int) SessionMetadata {
	t.Helper()

	path := fmt.Sprintf("%s/%d/metadata.json", CheckpointPath(checkpointID), sessionIndex)
	blob := "entire/checkpoints/v1:" + path

	raw := GitOutput(t, dir, "show", blob)

	var meta SessionMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("unmarshal session metadata from %s: %v", blob, err)
	}

	return meta
}

// WaitForSessionMetadata polls until session metadata exists on the checkpoint
// branch for the given checkpoint ID and session index, then returns it.
// This handles the race where the checkpoint branch advances before session
// metadata is fully committed.
func WaitForSessionMetadata(t *testing.T, dir string, checkpointID string, sessionIndex int, timeout time.Duration) SessionMetadata {
	t.Helper()

	path := fmt.Sprintf("%s/%d/metadata.json", CheckpointPath(checkpointID), sessionIndex)
	blob := "entire/checkpoints/v1:" + path

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw := gitOutputSafe(dir, "show", blob)
		if raw != "" {
			var meta SessionMetadata
			if err := json.Unmarshal([]byte(raw), &meta); err != nil {
				t.Fatalf("unmarshal session metadata from %s: %v", blob, err)
			}
			return meta
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("session metadata %s did not appear within %s", blob, timeout)
	return SessionMetadata{}
}

// SetupBareRemote creates a bare git repo, adds it as "origin", and pushes
// the initial commit. Returns the bare repo path.
func SetupBareRemote(t *testing.T, s *RepoState) string {
	t.Helper()

	var bareDir string
	if os.Getenv("E2E_KEEP_REPOS") != "" {
		var err error
		bareDir, err = os.MkdirTemp("", "e2e-bare-*")
		if err != nil {
			t.Fatalf("create bare remote dir: %v", err)
		}
		t.Logf("E2E_KEEP_REPOS: bare remote will be preserved at %s", bareDir)
	} else {
		bareDir = t.TempDir()
	}

	Git(t, bareDir, "init", "--bare")
	Git(t, s.Dir, "remote", "add", "origin", bareDir)
	Git(t, s.Dir, "push", "-u", "origin", "HEAD")
	return bareDir
}

// GitOutputErr runs a git command and returns (output, error) without
// failing the test. For commands expected to fail.
func GitOutputErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// GetCheckpointTrailer extracts the Entire-Checkpoint trailer value from a
// code commit. Returns the trimmed trailer value, or an empty string if the
// trailer is not present.
func GetCheckpointTrailer(t *testing.T, dir string, ref string) string {
	t.Helper()

	return GitOutput(t, dir, "log", "-1", "--format=%(trailers:key=Entire-Checkpoint,valueonly)", ref)
}
