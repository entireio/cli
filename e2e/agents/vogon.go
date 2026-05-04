package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "vogon" {
		return
	}
	// Only register if the binary exists (built by the test runner).
	if _, err := exec.LookPath("vogon"); err != nil {
		return
	}
	Register(&Vogon{})
}

// Vogon implements the Agent interface using a deterministic binary
// that creates files and fires hooks without making any API calls.
// Named after the Vogons from The Hitchhiker's Guide to the Galaxy.
type Vogon struct{}

func (v *Vogon) Name() string               { return "vogon" }
func (v *Vogon) Binary() string             { return "vogon" }
func (v *Vogon) EntireAgent() string        { return "vogon" }
func (v *Vogon) PromptPattern() string      { return `>` }
func (v *Vogon) TimeoutMultiplier() float64 { return 0.5 } // Faster than real agents

func (v *Vogon) Bootstrap() error { return nil }

func (v *Vogon) IsTransientError(_ Output, _ error) bool { return false }

func (v *Vogon) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	args := []string{"-p", prompt}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt)}
	env := filterEnv(os.Environ(), "ENTIRE_TEST_TTY")
	homeDir, err := vogonHomeDir(dir)
	if err != nil {
		return Output{}, err
	}
	env = append(env, "HOME="+homeDir)
	return v.run(ctx, dir, args, displayArgs, env)
}

func (v *Vogon) StartSession(_ context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("vogon-test-%d", time.Now().UnixNano())
	homeDir, err := vogonHomeDir(dir)
	if err != nil {
		return nil, err
	}
	s, err := NewTmuxSession(name, dir, []string{"ENTIRE_TEST_TTY"}, "env", "HOME="+homeDir, v.Binary())
	if err != nil {
		return nil, err
	}

	// Wait for the interactive prompt.
	if _, err := s.WaitFor(`>`, 10*time.Second); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("waiting for startup prompt: %w", err)
	}
	s.stableAtSend = ""

	return s, nil
}

// WriteSessionTranscript creates a deterministic vogon session transcript
// without firing hooks or mutating the repository. Attach E2E tests use this
// to prepare a session that `entire attach` can import.
func (v *Vogon) WriteSessionTranscript(ctx context.Context, dir string, extraEnv []string, sessionID, userPrompt, assistantMessage string) (Output, error) {
	args := []string{
		"--write-session",
		"--session-id", sessionID,
		"--user-prompt", userPrompt,
		"--assistant-message", assistantMessage,
	}
	displayArgs := []string{
		"--write-session",
		"--session-id", sessionID,
		"--user-prompt", fmt.Sprintf("%q", userPrompt),
		"--assistant-message", fmt.Sprintf("%q", assistantMessage),
	}
	env := filterEnv(os.Environ(), "ENTIRE_TEST_TTY")
	env = append(env, extraEnv...)
	if !hasEnvVar(extraEnv, "HOME") {
		homeDir, err := vogonHomeDir(dir)
		if err != nil {
			return Output{}, err
		}
		env = append(env, "HOME="+homeDir)
	}
	return v.run(ctx, dir, args, displayArgs, env)
}

func (v *Vogon) run(ctx context.Context, dir string, args, displayArgs []string, env []string) (Output, error) {
	cmd := exec.CommandContext(ctx, v.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = env
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
	}

	return Output{
		Command:  v.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func vogonHomeDir(dir string) (string, error) {
	sum := sha256.Sum256([]byte(dir))
	homeDir := filepath.Join(os.TempDir(), "vogon-e2e-home-"+hex.EncodeToString(sum[:6]))
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return "", fmt.Errorf("create vogon home %s: %w", homeDir, err)
	}
	return homeDir, nil
}

func hasEnvVar(env []string, key string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return true
		}
	}
	return false
}
