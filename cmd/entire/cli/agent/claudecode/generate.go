package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// buildGenerateArgs assembles the claude CLI argv for a --print text-generation
// call.
//
// The subprocess must stay isolated from the user's project/local AND user
// settings: loading them would fire user-level hooks and, worse, honor
// user-level tool permissions (e.g. permissions.defaultMode=bypassPermissions),
// which would let prompt-injection in the untrusted dispatch data drive tool
// execution. So we pass --setting-sources "" (load nothing).
//
// The one thing we genuinely need from the user settings is auth. Users on API
// billing configure it with `apiKeyHelper` (a command that prints the key),
// which lives in user settings and is therefore dropped by --setting-sources "".
// Rather than load the whole settings file back (and re-inherit hooks and
// permissions), we extract only apiKeyHelper and re-inject it via a --settings
// file (settingsPath), so auth works while nothing else from the user's settings
// is loaded.
//
// The injected settings are passed as a file path, not an inline JSON string:
// apiKeyHelper can embed a literal key, and an inline value would land in the
// process argv (visible via ps / /proc/<pid>/cmdline / EDR tooling). The file is
// written 0600 (see writeAuthSettingsFile), matching settings.json's protection.
//
// Auth methods that do not live in user settings — an exported ANTHROPIC_API_KEY
// (survives StripGitEnv) and keychain/subscription credentials — keep working
// without any injection (settingsPath == "").
func buildGenerateArgs(model, settingsPath string) []string {
	args := []string{
		"--print", "--output-format", "json",
		"--model", model,
		"--setting-sources", "",
	}
	if settingsPath != "" {
		args = append(args, "--settings", settingsPath)
	}
	return args
}

// buildStreamingGenerateArgs is buildGenerateArgs for the stream-json path,
// with the same isolation and auth-injection contract (see buildGenerateArgs).
// --include-partial-messages enables the per-token stream_event envelopes
// that PhaseFirstToken and PhaseGenerating are dispatched from, and
// --verbose is required by the claude CLI for stream-json output.
func buildStreamingGenerateArgs(model, settingsPath string) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--model", model,
		"--setting-sources", "",
	}
	if settingsPath != "" {
		args = append(args, "--settings", settingsPath)
	}
	return args
}

// writeAuthSettingsFile writes a minimal claude settings file containing only
// the given apiKeyHelper and returns its path plus a cleanup func. The file is
// created 0600 so the (possibly key-bearing) helper is no more exposed than the
// user's own settings.json. Returns ("", nil, nil) when apiKeyHelper is empty.
func writeAuthSettingsFile(apiKeyHelper string) (string, func(), error) {
	if strings.TrimSpace(apiKeyHelper) == "" {
		return "", nil, nil
	}
	data, err := json.Marshal(map[string]string{"apiKeyHelper": apiKeyHelper})
	if err != nil {
		return "", nil, fmt.Errorf("marshal auth settings: %w", err)
	}
	f, err := os.CreateTemp("", "entire-claude-auth-*.json") // 0600 by default
	if err != nil {
		return "", nil, fmt.Errorf("create auth settings file: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write auth settings file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close auth settings file: %w", err)
	}
	return path, cleanup, nil
}

// userClaudeSettingsPath resolves the user's claude settings.json the same way
// the claude CLI does: $CLAUDE_CONFIG_DIR/settings.json when set, otherwise
// ~/.claude/settings.json.
func userClaudeSettingsPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// readUserAPIKeyHelper returns the apiKeyHelper field from the user's claude
// settings, or "" if absent. Best-effort: a missing file or malformed JSON
// yields "" so we fall back to env/keychain auth rather than failing.
func readUserAPIKeyHelper() string {
	path, err := userClaudeSettingsPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the user's own claude config, not attacker-controlled
	if err != nil {
		return ""
	}
	var settings struct {
		APIKeyHelper string `json:"apiKeyHelper"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return ""
	}
	return strings.TrimSpace(settings.APIKeyHelper)
}

// GenerateText sends a prompt to the Claude CLI and returns the raw text response.
// Implements the agent.TextGenerator interface.
// The model parameter hints which model to use (e.g., "haiku", "sonnet").
// If empty, defaults to "haiku" for fast, cheap generation.
//
// Unlike most agents, this implementation runs the subprocess directly rather
// than through agent.RunIsolatedTextGeneratorCLI. The shared helper collapses
// stderr + exit code into a single formatted error string, but Claude CLI
// returns operational errors (auth, rate limit, invalid model) as exit 0
// with is_error:true in the JSON envelope on stdout — so we need structured
// access to stdout, stderr and the exit code to produce the typed ClaudeError
// values that formatCheckpointSummaryError maps to actionable messages.
func (c *ClaudeCodeAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	claudePath := "claude"
	if model == "" {
		model = "haiku"
	}

	commandRunner := c.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}

	// Run isolated from all setting sources (see buildGenerateArgs), re-injecting
	// only the user's apiKeyHelper (via a 0600 file, never argv) so API-billing
	// auth keeps working without re-inheriting user hooks or tool permissions.
	// Best-effort: if extracting/writing the helper fails, fall back to running
	// without it (env/keychain auth still work) rather than failing the call.
	settingsPath, cleanup, err := writeAuthSettingsFile(readUserAPIKeyHelper())
	if err != nil {
		settingsPath = ""
	}
	if cleanup != nil {
		defer cleanup()
	}

	cmd := commandRunner(ctx, claudePath, buildGenerateArgs(model, settingsPath)...)

	agent.PrepareIsolatedCLICmd(cmd)
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Prefer a structured Claude error over bare ctx sentinels: if the CLI
		// already emitted an is_error envelope on stdout, surface that even when
		// the context happens to be done. Otherwise the user loses actionable
		// auth/rate-limit/config diagnostics whenever ctx and the subprocess
		// both fail at roughly the same time.
		if _, env, parseErr := parseGenerateTextResponse(stdout.Bytes()); parseErr == nil && env != nil && env.IsError {
			result := ""
			if env.Result != nil {
				result = *env.Result
			}
			exitCode := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
			return "", classifyEnvelopeError(result, env.APIErrorStatus, exitCode)
		}
		// No structured signal on stdout — ctx cancellation is next most
		// informative, since the rest is a guess. Wrap the sentinel in a
		// *agent.TextGenerationError (like the streaming path and every other
		// provider) so the explain timeout diagnostic still gets captured
		// stderr and the stdout byte count when this path is reached via the
		// old-CLI streaming fallback.
		if ctxErr := ctx.Err(); ctxErr != nil && (errors.Is(ctxErr, context.DeadlineExceeded) || errors.Is(ctxErr, context.Canceled)) {
			sentinel := context.Canceled
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				sentinel = context.DeadlineExceeded
			}
			return "", &agent.TextGenerationError{
				Err:         sentinel,
				Stderr:      strings.TrimSpace(stderr.String()),
				StdoutBytes: stdout.Len(),
			}
		}
		if isExecNotFound(err) {
			return "", &ClaudeError{Kind: ClaudeErrorCLIMissing, Cause: err}
		}
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return "", classifyStderrError(stderr.String(), exitCode)
	}

	// Exit 0: parse the response and check for is_error (Claude CLI returns
	// most operational errors as exit 0 with is_error:true in the envelope).
	result, env, err := parseGenerateTextResponse(stdout.Bytes())
	if err != nil {
		return "", &ClaudeError{Kind: ClaudeErrorUnknown, Message: fmt.Sprintf("failed to parse claude CLI response: %v", err), Cause: err}
	}
	if env != nil && env.IsError {
		return "", classifyEnvelopeError(result, env.APIErrorStatus, 0)
	}

	return result, nil
}

// isExecNotFound returns true only when err indicates the binary was not
// found on PATH or at the given absolute path. It intentionally excludes
// other *exec.Error causes (permission denied, invalid executable format),
// which should surface as a generic failure so operators aren't misdirected
// to a reinstall when the real problem is a broken/inaccessible binary.
func isExecNotFound(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return true
	}
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}
