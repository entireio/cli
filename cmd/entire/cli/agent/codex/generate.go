package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// generateTextDisabledFeatures are codex features that give the model a
// tool. Summary generation needs none (the transcript is already in the
// prompt), and its prompt carries untrusted transcript content. codex's
// default read-only sandbox blocks writes and network but not reads, so with
// shell_tool or unified_exec enabled an injected instruction could read an
// arbitrary file into the summary; disabling only those two still left a read
// through the code-mode JS host (verified live on codex-cli 0.156.1).
//
// This is a denylist and it is NOT complete: codex exec offers no "no tools"
// switch, withholding sandbox read permissions (sandbox_permissions=[]) does
// not stop reads, and codex keeps adding tool-bearing features. On 0.156.1
// the model still reports collaboration (sub-agent) tools, apply_patch, web
// search, and goal tools with this set disabled; none of them read the
// canary in live probes, but that is observed behavior, not a guarantee.
// view_image, multi_agent, and image_generation are listed because they are
// stable, on by default, and reach files or spawn work. `codex features list`
// shows candidates.
var generateTextDisabledFeatures = []string{
	"shell_tool",
	"unified_exec",
	"code_mode_host",
	"apps",
	"plugins",
	"browser_use",
	"browser_use_external",
	"computer_use",
	"in_app_browser",
	"view_image",
	"multi_agent",
	"image_generation",
}

// GenerateText sends a prompt to the Codex CLI and returns the raw text response.
//
// An older codex exits with "Unknown feature flag: <name>" when asked to
// disable a feature added after it shipped, so the features passed are first
// narrowed to the ones the installed codex knows (knownFeatures). That is
// decided by a separate, prompt-free `codex features list`, never by the
// generation run's own output: a run whose prompt is untrusted must not be
// able to talk Entire into dropping a --disable and retrying. A feature the
// installed codex does not know cannot give the model a tool, so leaving it
// out keeps the run tool-free. If the probe fails, every feature is passed and
// an unknown one fails the run instead.
func (c *CodexAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	disabled := generateTextDisabledFeatures
	if known, err := c.knownFeatures(ctx); err == nil {
		disabled = slices.DeleteFunc(slices.Clone(disabled), func(f string) bool { return !known[f] })
	} else {
		logging.Debug(ctx, "codex features probe failed; disabling the full feature list",
			slog.String("error", err.Error()))
	}

	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "codex", "codex", generateTextArgs(disabled, model), prompt)
	if err == nil {
		return result, nil
	}
	if strings.Contains(capturedStderr, "'"+flagIgnoreUserConfig+"'") {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("codex text generation failed: this codex does not support %s, which Entire needs to generate summaries without the user's MCP servers and hooks; update codex (0.122 or newer): %w", flagIgnoreUserConfig, err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return "", &agent.TextGenerationError{
		Err:         fmt.Errorf("codex text generation failed: %w", err),
		Stderr:      capturedStderr,
		StdoutBytes: stdoutBytes,
	}
}

// knownFeatures returns the feature names the installed codex lists, from
// `codex features list` (one "<name> <stage> <enabled>" row per feature).
func (c *CodexAgent) knownFeatures(ctx context.Context) (map[string]bool, error) {
	out, _, _, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "codex", "codex", []string{"features", "list"}, "")
	if err != nil {
		return nil, fmt.Errorf("codex features list: %w", err)
	}
	known := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			known[fields[0]] = true
		}
	}
	if len(known) == 0 {
		return nil, errors.New("codex features list: no features")
	}
	return known, nil
}

// generateTextArgs builds the codex argv for one text-generation call.
//
// --ignore-user-config skips ~/.codex/config.toml. The features below are
// flags, but MCP servers, a web_search setting, and hooks come from that file,
// and `-c mcp_servers={}` does not remove configured servers (codex merges
// -c overrides into the table). Ignoring the file keeps all three out; login
// lives in auth.json and still works. A custom model provider or profile set
// in config.toml is not used for summaries as a result.
func generateTextArgs(disabled []string, model string) []string {
	args := []string{"exec", "--skip-git-repo-check", flagIgnoreUserConfig}
	for _, feature := range disabled {
		args = append(args, "--disable", feature)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "-")
}

// flagIgnoreUserConfig is present in codex-cli since at least 0.122.0.
const flagIgnoreUserConfig = "--ignore-user-config"
