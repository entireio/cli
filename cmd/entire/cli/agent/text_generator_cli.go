package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// TextGenerationError carries captured subprocess output alongside a
// TextGenerator's error so the explain layer can build a meaningful
// timeout diagnostic ("provider produced no output" vs "was generating
// output when killed"). Wraps the original error so errors.As against
// the inner type (e.g. *TextGenError) keeps working.
type TextGenerationError struct {
	Err         error
	Stderr      string
	StdoutBytes int
}

func (e *TextGenerationError) Error() string { return e.Err.Error() }
func (e *TextGenerationError) Unwrap() error { return e.Err }

// TextCommandRunner matches exec.CommandContext and allows tests to inject a runner.
type TextCommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd

// summaryProviderBinaries maps agent names to the CLI binary that
// RunIsolatedTextGeneratorCLIRaw will exec. Used by IsSummaryCLIAvailable to
// check PATH instead of repo-level DetectPresence, because a repo can use
// one agent for development while a different agent generates summaries.
//
// This is the single source of truth for summary-capable provider binaries.
// Callers outside this package that need the binary name (e.g., the explain
// diagnostic's "run `claude` directly" suggestion) should use
// SummaryCLIBinaryName rather than duplicating the mapping.
var summaryProviderBinaries = map[types.AgentName]string{
	AgentNameClaudeCode: "claude",
	AgentNameCodex:      "codex",
	AgentNameCopilotCLI: "copilot",
	AgentNameCursor:     "agent",
	AgentNameGemini:     "gemini",
	AgentNamePi:         "pi",
}

// SummaryCLIBinaryName returns the CLI binary name for a summary-capable
// agent (e.g. "claude" for ClaudeCode, "agent" for Cursor). Returns "" for
// agents that are not summary-capable; callers should treat that as "we
// don't know" rather than guessing.
func SummaryCLIBinaryName(name types.AgentName) string {
	return summaryProviderBinaries[name]
}

// SummaryCapableAgents returns every agent with a registered summary CLI
// binary, sorted for determinism.
//
// Exported so the explain layer can assert its display-name and remediation
// tables cover the whole set. Those tables degrade silently when an agent is
// missing — displayNameFor falls back to the raw registry key
// ("copilot-cli authentication failed") and syntheticFallback yields no "try"
// row — so the only way to catch a forgotten registration is to enumerate.
func SummaryCapableAgents() []types.AgentName {
	names := make([]types.AgentName, 0, len(summaryProviderBinaries))
	for name := range summaryProviderBinaries {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
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

// RunIsolatedTextGeneratorCLIRaw executes a text-generation CLI in an isolated
// temp directory with all GIT_* environment variables removed, and returns
// stdout, stderr, and exit code as separate values so callers can classify
// based on the full subprocess signal set.
//
// Contract:
//   - Exit 0 returns (ExecResult, nil) with Stdout, Stderr, ExitCode populated.
//   - Non-zero exit returns (ExecResult, *exec.ExitError) with ExitCode set
//     from the ExitError.
//   - Binary-not-found returns (empty ExecResult, *exec.Error wrapping
//     exec.ErrNotFound). Callers use IsExecNotFoundErr to detect. Note a
//     path-containing argv skips LookPath and yields *fs.PathError/ENOENT
//     instead, which IsExecNotFoundErr also matches.
//   - Context cancellation returns (partial ExecResult, ctx.Err() in chain).
//     Stdout/Stderr reflect whatever was captured before the subprocess died.
func RunIsolatedTextGeneratorCLIRaw(ctx context.Context, runner TextCommandRunner, binary string, args []string, stdin string) (ExecResult, error) {
	if runner == nil {
		runner = exec.CommandContext
	}

	cmd := runner(ctx, binary, args...)
	cmd.Dir = os.TempDir()
	cmd.Env = StripGitEnv(os.Environ())
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := ExecResult{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
	}
	// ctx errors come through err already; preserve them so errors.Is works.
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && (errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded)) {
			return res, ctxErr //nolint:wrapcheck // preserve context sentinel for errors.Is
		}
		return res, err //nolint:wrapcheck // Classifier consumes raw *exec.Error / *exec.ExitError
	}
	return res, nil
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
