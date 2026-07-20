package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// TextGenerationError carries captured subprocess output alongside a
// TextGenerator's error so the explain layer can build a meaningful
// timeout diagnostic ("provider produced no output" vs "was generating
// output when killed"). Wraps the original error so errors.As against
// the inner type (e.g. *ClaudeError) keeps working.
type TextGenerationError struct {
	Err         error
	Stderr      string
	StdoutBytes int
	Provider    types.AgentName
	Kind        TextGenerationErrorKind
	Message     string
	ExitCode    int
}

func (e *TextGenerationError) Error() string {
	if e == nil {
		return "text generation failed"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "text generation failed"
}

func (e *TextGenerationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// TextGenerationErrorKind identifies actionable summary-provider failures.
type TextGenerationErrorKind int

const (
	TextGenerationErrorUnknown TextGenerationErrorKind = iota
	TextGenerationErrorAuth
	TextGenerationErrorRateLimit
	TextGenerationErrorConfig
	TextGenerationErrorCLIMissing
)

// TextCommandRunner matches exec.CommandContext and allows tests to inject a runner.
type TextCommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd

// RunIsolatedTextGeneratorCLI executes a text-generation CLI in an isolated temp
// directory with all GIT_* environment variables removed. This avoids recursive
// hook triggers and repo side effects while preserving provider-specific flags.
func RunIsolatedTextGeneratorCLI(
	ctx context.Context,
	runner TextCommandRunner,
	provider types.AgentName,
	args []string,
	stdin string,
) (string, error) {
	if runner == nil {
		runner = exec.CommandContext
	}
	binary := SummaryCLIBinaryName(provider)
	displayName := SummaryProviderErrorLabel(provider)

	cmd := runner(ctx, binary, args...)
	cmd.Dir = os.TempDir()
	cmd.Env = StripGitEnv(os.Environ())
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		capturedStderr := strings.TrimSpace(stderr.String())
		stdoutBytes := stdout.Len()
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			message := fmt.Sprintf("%s not found: %v", displayName, err)
			return "", newTextGenerationError(provider, TextGenerationErrorCLIMissing, message, err, capturedStderr, stdoutBytes, -1)
		}

		detail := capturedStderr
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		kind := classifyTextGenerationFailure(provider, detail)
		underlying := err
		if kind == TextGenerationErrorUnknown && ctx.Err() != nil {
			underlying = errors.Join(err, ctx.Err())
		}

		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			if detail == "" {
				detail = err.Error()
			}
			message := fmt.Sprintf("%s failed (exit %d): %s", displayName, exitCode, detail)
			return "", newTextGenerationError(provider, kind, message, underlying, capturedStderr, stdoutBytes, exitCode)
		}

		message := fmt.Sprintf("failed to run %s: %v", displayName, err)
		return "", newTextGenerationError(provider, kind, message, underlying, capturedStderr, stdoutBytes, exitCode)
	}

	result := strings.TrimSpace(stdout.String())
	if result == "" {
		capturedStderr := strings.TrimSpace(stderr.String())
		kind := classifyTextGenerationFailure(provider, capturedStderr)
		message := displayName + " returned empty output"
		if capturedStderr != "" {
			message += ": " + capturedStderr
		}
		underlying := errors.New(message)
		if kind == TextGenerationErrorUnknown && ctx.Err() != nil {
			underlying = errors.Join(underlying, ctx.Err())
		}
		return "", newTextGenerationError(provider, kind, message, underlying, capturedStderr, stdout.Len(), 0)
	}
	return result, nil
}

func newTextGenerationError(
	provider types.AgentName,
	kind TextGenerationErrorKind,
	message string,
	err error,
	stderr string,
	stdoutBytes int,
	exitCode int,
) *TextGenerationError {
	if err == nil {
		err = errors.New(message)
	}
	return &TextGenerationError{
		Err: err, Stderr: stderr, StdoutBytes: stdoutBytes,
		Provider: provider, Kind: kind, Message: message, ExitCode: exitCode,
	}
}

var contextualHTTPStatus = regexp.MustCompile(
	`(?i)\b(?:http(?:\s+status(?:\s+code)?|\s+response\s+status)?|(?:api|provider)\s+response\s+status|unexpected\s+status|status\s+code)\s*:?\s*(400|401|403|404|429)\b`,
)

func classifyTextGenerationFailure(provider types.AgentName, message string) TextGenerationErrorKind {
	if provider == AgentNameGemini && strings.Contains(strings.ToLower(message), "please set an auth method") {
		return TextGenerationErrorAuth
	}
	match := contextualHTTPStatus.FindStringSubmatch(message)
	if len(match) != 2 {
		return TextGenerationErrorUnknown
	}
	switch match[1] {
	case "401", "403":
		return TextGenerationErrorAuth
	case "429":
		return TextGenerationErrorRateLimit
	case "400", "404":
		return TextGenerationErrorConfig
	default:
		return TextGenerationErrorUnknown
	}
}

// summaryProviders maps agent names to summary CLI metadata. The binary is what
// RunIsolatedTextGeneratorCLI will exec. Used by IsSummaryCLIAvailable to
// check PATH instead of repo-level DetectPresence, because a repo can use
// one agent for development while a different agent generates summaries.
//
// This is the single source of truth for summary-capable provider binaries.
// Callers outside this package that need the binary name (e.g., the explain
// diagnostic's "run `claude` directly" suggestion) should use
// SummaryCLIBinaryName rather than duplicating the mapping.
type summaryProviderMetadata struct {
	binary string
	label  string
}

var summaryProviders = map[types.AgentName]summaryProviderMetadata{
	AgentNameClaudeCode: {binary: "claude", label: "Claude"},
	AgentNameCodex:      {binary: "codex", label: "Codex"},
	AgentNameCopilotCLI: {binary: "copilot", label: "Copilot CLI"},
	AgentNameCursor:     {binary: "agent", label: "Cursor"},
	AgentNameGemini:     {binary: "gemini", label: "Gemini"},
	AgentNamePi:         {binary: "pi", label: "Pi"},
}

// SummaryCLIBinaryName returns the CLI binary name for a summary-capable
// agent (e.g. "claude" for ClaudeCode, "agent" for Cursor). Returns "" for
// agents that are not summary-capable; callers should treat that as "we
// don't know" rather than guessing.
func SummaryCLIBinaryName(name types.AgentName) string {
	return summaryProviders[name].binary
}

// SummaryProviderErrorLabel returns a user-facing provider name.
func SummaryProviderErrorLabel(name types.AgentName) string {
	return summaryProviders[name].label
}

// IsSummaryCLIAvailable reports whether the CLI binary for a summary-capable
// agent is on PATH. This is distinct from DetectPresence, which checks
// repo-level agent configuration — a repo configured with Claude Code for
// development can still use Codex or Gemini for summary generation as long
// as the binary is installed.
func IsSummaryCLIAvailable(name types.AgentName) bool {
	binary := SummaryCLIBinaryName(name)
	if binary == "" {
		return false
	}
	_, err := exec.LookPath(binary)
	return err == nil
}

func StripGitEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "GIT_") {
			filtered = append(filtered, e)
		}
	}
	return filtered
}
