package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"

	"github.com/tailscale/hujson"
)

const openCodeBinary = "opencode"

var _ agent.TextGenerator = (*OpenCodeAgent)(nil)

// GenerateText runs OpenCode without repository context or tool access. An empty
// model preserves OpenCode's own default; explicit models use provider/model IDs.
func (a *OpenCodeAgent) GenerateText(ctx context.Context, prompt, model string) (string, error) {
	dir, err := os.MkdirTemp("", "entire-opencode-summary-")
	if err != nil {
		return "", fmt.Errorf("creating OpenCode generation directory: %w", err)
	}
	defer os.RemoveAll(dir)

	// A unique agent name prevents merging permissions from a user's named agent.
	name := filepath.Base(dir)
	config, err := openCodeGenerationConfig(os.Getenv("OPENCODE_CONFIG_CONTENT"), name)
	if err != nil {
		return "", err
	}
	args := []string{"run", "--format", "json", "--dir", dir, "--agent", name, "--title", "Entire summary generation"}
	if model != "" {
		args = append(args, "--model", model)
	}
	raw, stderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, openCodeBinary, openCodeBinary, args, prompt,
		"OPENCODE_CONFIG_CONTENT="+config, "OPENCODE_AUTO_SHARE=false")
	if err == nil {
		raw, err = parseOpenCodeGeneration(raw)
	}
	if err != nil {
		return "", &agent.TextGenerationError{Err: fmt.Errorf("opencode text generation failed: %w", err), Stderr: stderr, StdoutBytes: stdoutBytes}
	}
	return raw, nil
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
				Name string `json:"name"`
				Data struct {
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
			detail := event.Error.Data.Message
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
