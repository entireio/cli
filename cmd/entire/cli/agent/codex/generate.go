package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

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
//
// Every other feature codex enables by default is classified, with the
// reason it stays on, in generateTextAcceptedFeatures
// (generate_features_test.go), where the residual tools above are listed. A
// codex release that enables a feature nobody has classified fails that test
// once its pinned feature list is refreshed, or against the installed codex
// when the real-agent tests run.
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
// out keeps the run tool-free. If the probe fails or outlives its own short
// budget (probeTimeout), every feature is passed and an unknown one fails the
// run instead.
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
	// Anchored at the start: clap rejects the argv before codex reads stdin,
	// so its error is the first thing on stderr, while the prompt (untrusted)
	// is echoed after it and may contain this text.
	if strings.HasPrefix(capturedStderr, "error: unexpected argument '"+flagIgnoreUserConfig+"'") {
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
//
// Output that does not list probeAnchorFeature is an error, not a short
// list: a codex that changed the format would otherwise parse as knowing none
// of generateTextDisabledFeatures, and filtering against that would drop
// every --disable instead of falling back to the full list.
//
// The probe runs with an empty CODEX_HOME. The generation run passes
// --ignore-user-config, which `features list` does not accept, and a
// config.toml codex cannot parse fails the probe outright. The names listed do
// not depend on the config (it changes only their enabled state), and the
// probe needs no sign-in, so nothing is lost by leaving the user's home out.
func (c *CodexAgent) knownFeatures(ctx context.Context) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout(ctx))
	defer cancel()
	codexHome, cleanup, err := agent.NewTextGenerationDir()
	if err != nil {
		return nil, fmt.Errorf("codex features list: %w", err)
	}
	defer cleanup()
	out, _, _, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "codex", "codex", []string{"features", "list"}, "", "CODEX_HOME="+codexHome)
	if err != nil {
		return nil, fmt.Errorf("codex features list: %w", err)
	}
	known := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			known[fields[0]] = true
		}
	}
	if !known[probeAnchorFeature] {
		return nil, errors.New("codex features list: output does not list " + probeAnchorFeature)
	}
	return known, nil
}

// probeTimeout bounds `codex features list` so it cannot spend the deadline
// the generation run shares with it (a configured summary timeout): at most
// maxProbeTimeout, and at most a tenth of whatever ctx has left. The probe
// normally takes milliseconds; one that runs out falls back to the full list.
func probeTimeout(ctx context.Context) time.Duration {
	d := maxProbeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		d = min(d, time.Until(deadline)/10)
	}
	return d
}

const maxProbeTimeout = 2 * time.Second

// probeAnchorFeature is a feature every codex Entire supports lists, so its
// absence means `codex features list` output was not parsed as intended.
const probeAnchorFeature = "shell_tool"

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
