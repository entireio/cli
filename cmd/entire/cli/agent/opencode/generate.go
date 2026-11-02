package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/tailscale/hujson"
	"golang.org/x/mod/semver"
)

const openCodeBinary = "opencode"

// openCodeVersionTimeout bounds the `opencode --version` probe; it makes no
// model call, so a slow answer means something is wrong.
const openCodeVersionTimeout = 5 * time.Second

var _ agent.TextGenerator = (*OpenCodeAgent)(nil)

// GenerateText runs OpenCode without repository context or tool access. An empty
// model preserves OpenCode's own default; explicit models use provider/model IDs.
func (a *OpenCodeAgent) GenerateText(ctx context.Context, prompt, model string) (string, error) {
	// OpenCode runs in its working directory; OpenCode 2 removed --dir.
	dir, cleanup, err := agent.NewTextGenerationDir()
	if err != nil {
		return "", fmt.Errorf("opencode text generation failed: %w", err)
	}
	defer cleanup()

	// A unique agent name prevents merging permissions from a user's named agent.
	name := filepath.Base(dir)
	config, err := openCodeGenerationConfig(os.Getenv("OPENCODE_CONFIG_CONTENT"), name)
	if err != nil {
		return "", err
	}
	args := []string{"run"}
	standalone, err := a.openCodeNeedsStandalone(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("opencode text generation failed: %w", err)
	}
	if standalone {
		args = append(args, "--standalone")
	}
	args = append(args, "--format", "json", "--agent", name, "--title", "Entire summary generation")
	if model != "" {
		args = append(args, "--model", model)
	}
	raw, stderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLIIn(ctx, a.CommandRunner, dir, openCodeBinary, openCodeBinary, args, prompt,
		"OPENCODE_CONFIG_CONTENT="+config, "OPENCODE_AUTO_SHARE=false")
	if err == nil {
		raw, err = parseOpenCodeGeneration(raw)
	}
	if err != nil {
		return "", &agent.TextGenerationError{Err: fmt.Errorf("opencode text generation failed: %w", err), Stderr: stderr, StdoutBytes: stdoutBytes}
	}
	return raw, nil
}

// openCodeNeedsStandalone reports whether run needs --standalone. OpenCode 2
// otherwise attaches to a shared background service, which ignores
// OPENCODE_CONFIG_CONTENT (so the generation agent is missing) and, when none
// is running, is started from this call and outlives it. OpenCode 1 rejects the
// flag, so only a parsed 1.x version omits it.
func (a *OpenCodeAgent) openCodeNeedsStandalone(ctx context.Context, dir string) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, openCodeVersionTimeout)
	defer cancel()
	runner := a.CommandRunner
	if runner == nil {
		runner = exec.CommandContext
	}
	cmd := runner(probeCtx, openCodeBinary, "--version")
	cmd.Dir = dir
	// --version reads no config; keep inline config (which may hold credentials) out of the probe.
	cmd.Env = slices.DeleteFunc(agent.TextGenerationEnv(dir, os.Environ()), func(kv string) bool {
		return strings.HasPrefix(kv, "OPENCODE_CONFIG_CONTENT=")
	})
	// --version starts no service, so no child can hold the pipe past this.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("opencode version probe: %w", ctxErr)
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return false, fmt.Errorf("opencode CLI not found: %w", err)
	}
	version := openCodeVersion(string(out))
	if err != nil || version == "" {
		logging.Debug(logging.WithComponent(ctx, "opencode"), "opencode version probe inconclusive; using --standalone",
			slog.String("version", version), slog.Any("error", err))
		return true, nil
	}
	return semver.Major(version) != "v0" && semver.Major(version) != "v1", nil
}

// openCodeVersion returns the first full x.y.z token as canonical semver:
// OpenCode 1 prints "1.18.33" and OpenCode 2 "opencode v2.1.1".
func openCodeVersion(raw string) string {
	for _, field := range strings.Fields(raw) {
		v := "v" + strings.TrimPrefix(field, "v")
		if semver.Canonical(v) == v {
			return v
		}
	}
	return ""
}

// Preserve inline provider/auth/model settings while overriding only the
// generation agent and sharing. Global provider credentials remain available.
func openCodeGenerationConfig(inline, name string) (string, error) {
	config := make(map[string]json.RawMessage)
	if inline != "" {
		standard, err := hujson.Standardize([]byte(inline))
		if err != nil {
			return "", fmt.Errorf("reading OPENCODE_CONFIG_CONTENT: %w", err)
		}
		if err := json.Unmarshal(standard, &config); err != nil {
			return "", fmt.Errorf("reading OPENCODE_CONFIG_CONTENT: %w", err)
		}
	}
	if config == nil {
		config = make(map[string]json.RawMessage)
	}
	agents := make(map[string]json.RawMessage)
	if raw, ok := config["agent"]; ok {
		if err := json.Unmarshal(raw, &agents); err != nil {
			return "", fmt.Errorf("reading OpenCode agent configuration: %w", err)
		}
	}
	if agents == nil {
		agents = make(map[string]json.RawMessage)
	}
	agents[name] = json.RawMessage(`{"mode":"primary","description":"Entire text generation","prompt":"Generate the requested text from the supplied prompt. Do not use tools or inspect files.","permission":{"*":"deny"}}`)
	raw, err := json.Marshal(agents)
	if err != nil {
		return "", fmt.Errorf("encoding OpenCode agent configuration: %w", err)
	}
	config["agent"] = raw
	config["share"] = json.RawMessage(`"disabled"`)
	config["autoupdate"] = json.RawMessage(`false`)
	raw, err = json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encoding OpenCode generation configuration: %w", err)
	}
	return string(raw), nil
}

// OpenCode emits completed text as JSON events and can emit an API error even
// when the process exits successfully. Never return partial text after an error.
func parseOpenCodeGeneration(raw string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var parts []string
	for {
		var event struct {
			Type string `json:"type"`
			Part struct {
				Text string `json:"text"`
			} `json:"part"`
			Error struct {
				Name    string `json:"name"`
				Message string `json:"message"`
				Data    struct {
					Message string `json:"message"`
				} `json:"data"`
			} `json:"error"`
		}
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("decoding OpenCode output: %w", err)
		}
		switch event.Type {
		case "text":
			parts = append(parts, event.Part.Text)
		case "error":
			// OpenCode 2 reports error.message; OpenCode 1 error.data.message.
			detail := event.Error.Message
			if detail == "" {
				detail = event.Error.Data.Message
			}
			if detail == "" {
				detail = event.Error.Name
			}
			if detail == "" {
				detail = "unknown error"
			}
			return "", fmt.Errorf("OpenCode: %s", detail)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" {
		return "", errors.New("OpenCode returned no text")
	}
	return text, nil
}
