package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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
//
// Model defaults to "haiku" for fast, cheap generation (the summarize package
// overrides to "sonnet" via ResolveModel for quality).
//
// Classification order:
//  1. A cleanly-parsed is_error:true envelope on stdout — checked first
//     because Claude's primary failure mode is exit 0 with is_error:true. This
//     wins over stderr and over ctx sentinels.
//  2. Context sentinels (ctx canceled/deadline) — passthrough, not typed.
//  3. CLIMissing — typed error for "install the binary" remediation.
//  4. Any other run error — stderr classified by HTTP status, then by auth
//     phrase. Reached even when stdout held non-JSON bytes: unparseable stdout
//     must not mask a real error on stderr (see classifyClaudeEnvelope).
//  5. Exit 0 with empty stdout — typed Unknown with "empty output" message.
//
// Exit 0 with unparseable stdout is handled inside step 1, which is the only
// caller that can distinguish it from a failed run.
func (c *ClaudeCodeAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	if model == "" {
		model = "haiku"
	}

	// Run isolated from all setting sources (see buildGenerateArgs), re-injecting
	// only the user's apiKeyHelper (via a 0600 file, never argv) so API-billing
	// auth keeps working without re-inheriting user hooks or tool permissions.
	// Best-effort: if extracting/writing the helper fails, fall back to running
	// without it (env/keychain auth still work) rather than failing the call.
	settingsPath, cleanup, settingsErr := writeAuthSettingsFile(readUserAPIKeyHelper())
	if settingsErr != nil {
		// Leave a breadcrumb: this is the one downgrade the error surface
		// cannot self-diagnose. An unwritable TMPDIR skips the injection, claude
		// then genuinely fails auth, and the user is correctly told to run
		// `claude login` — which will never help an API-billing user whose only
		// credential is apiKeyHelper. Without this line the real cause appears
		// nowhere, not even in .entire/logs/.
		logging.Warn(ctx, "could not inject claude apiKeyHelper; falling back to env/keychain auth",
			slog.String("error", settingsErr.Error()))
		settingsPath = ""
	}
	if cleanup != nil {
		defer cleanup()
	}

	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, c.CommandRunner, "claude", buildGenerateArgs(model, settingsPath), prompt)

	// withEvidence attaches the captured subprocess output the explain layer's
	// timeout diagnostic reads, matching agent.HandleTextGenResult. Classification
	// (*TextGenError) and evidence (*TextGenerationError) are complementary: both
	// have to survive, so the typed error is wrapped rather than replaced.
	withEvidence := func(err error) error {
		return &agent.TextGenerationError{
			Err:         err,
			Stderr:      agent.TruncateStderr(string(res.Stderr)),
			StdoutBytes: len(res.Stdout),
		}
	}

	if env := classifyClaudeEnvelope(res.Stdout, runErr); env != nil {
		env.ExitCode = res.ExitCode
		// Deliberately do NOT stamp a ctx sentinel into Cause. explain's
		// generateCheckpointAISummary branches on errors.Is(err,
		// DeadlineExceeded) BEFORE formatCheckpointSummaryError runs, and that
		// branch returns a bare "timed out" error — dropping the classification
		// entirely. Stamping the sentinel here would therefore turn a fully
		// diagnosed auth failure into "Timed out after 30s" whenever the
		// deadline fires during teardown, and no retry would ever fix it.
		//
		// This keeps main's documented invariant true: DeadlineExceeded is
		// present in the chain only when the timeout was actually the cause. It
		// also matches the streaming path, which sets no Cause at all.
		if !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
			env.Cause = runErr
		}
		return "", withEvidence(env)
	}

	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			return "", withEvidence(context.Canceled)
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			return "", withEvidence(context.DeadlineExceeded)
		}
		if agent.IsExecNotFoundErr(runErr) {
			return "", withEvidence(&agent.TextGenError{
				Kind:     agent.TextGenErrorCLIMissing,
				Provider: agent.AgentNameClaudeCode,
				Cause:    runErr,
			})
		}
		// Message: prefer stderr, fall back to stdout, then to the run error, so
		// it is never empty — a launch failure (permission denied, exec format
		// error) produces no output and only runErr describes it.
		stderr := strings.TrimSpace(string(res.Stderr))
		raw := stderr
		if raw == "" {
			raw = strings.TrimSpace(string(res.Stdout))
		}
		if raw == "" {
			raw = runErr.Error()
		}
		// Classification reads stderr ONLY, and the full stderr. Claude's stdout
		// here is a partial envelope — the model's draft summary of the user's
		// transcript — so classifying it would let a summary that merely
		// discusses an invalid API key report itself as an auth failure. Full
		// stderr because a status line or auth phrase can sit past the 500-byte
		// display cap.
		kind := agent.ClassifyStderrHTTPStatus(stderr)
		if kind == agent.TextGenErrorUnknown && containsAuthPhrase(stderr) {
			// Claude's CLI sometimes exits non-zero with auth failure text on
			// stderr before any envelope is produced (e.g. "Invalid API key"
			// with exit 2). Reuses containsAuthPhrase/envelopeAuthPhrases from
			// envelope_parser.go — one list, two call sites (envelope result
			// text and raw stderr).
			kind = agent.TextGenErrorAuth
		}
		return "", withEvidence(&agent.TextGenError{
			Kind:     kind,
			Provider: agent.AgentNameClaudeCode,
			Message:  agent.TruncateStderr(raw),
			ExitCode: res.ExitCode,
			Cause:    runErr,
		})
	}

	// Success path. Envelope was nil (stdout empty) or envelope.IsError was false.
	if len(res.Stdout) == 0 {
		return "", withEvidence(&agent.TextGenError{
			Kind:     agent.TextGenErrorUnknown,
			Provider: agent.AgentNameClaudeCode,
			Message:  "claude CLI returned empty output",
		})
	}
	// classifyClaudeEnvelope already parsed these same bytes successfully — with
	// runErr == nil it returns non-nil on any parse failure, so reaching here
	// means the parse succeeded. The previous defensive branch here was
	// unreachable and was the only failure return in this function not wrapped
	// in withEvidence; parseGenerateTextResponse is pure, so the second call
	// cannot disagree with the first.
	result, _, parseErr := parseGenerateTextResponse(res.Stdout)
	if parseErr != nil {
		return "", withEvidence(&agent.TextGenError{
			Kind:     agent.TextGenErrorUnknown,
			Provider: agent.AgentNameClaudeCode,
			Message:  agent.TruncateStderr(fmt.Sprintf("failed to parse claude CLI response: %v", parseErr)),
			Cause:    parseErr,
		})
	}
	return result, nil
}
