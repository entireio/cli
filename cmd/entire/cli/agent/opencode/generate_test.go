package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestOpenCodeSummaryCapability(t *testing.T) {
	t.Parallel()
	if _, ok := agent.AsTextGenerator(NewOpenCodeAgent()); !ok {
		t.Fatal("OpenCode must support text generation for summaries and runner setup")
	}
	if got := agent.SummaryCLIBinaryName(agent.AgentNameOpenCode); got != openCodeBinary {
		t.Fatalf("summary binary = %q, want opencode", got)
	}
}

func TestOpenCodeGenerateText(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"", "openai/gpt-5.6-sol"} {
		t.Run("model="+model, func(t *testing.T) {
			t.Parallel()
			var args []string
			var command *exec.Cmd
			a := &OpenCodeAgent{CommandRunner: func(ctx context.Context, binary string, argv ...string) *exec.Cmd {
				if binary != openCodeBinary {
					t.Fatalf("binary = %q", binary)
				}
				args = argv
				command = exec.CommandContext(ctx, "cat")
				return command
			}}
			prompt := `{"type":"text","part":{"type":"text","text":"hello","time":{"end":1}}}`
			got, err := a.GenerateText(t.Context(), prompt, model)
			if err != nil || got != "hello" {
				t.Fatalf("GenerateText = %q, %v", got, err)
			}
			if slices.Contains(args, prompt) {
				t.Fatal("prompt leaked into argv")
			}
			modelIndex := slices.Index(args, "--model")
			if model == "" && modelIndex != -1 {
				t.Fatal("default model must be left to OpenCode")
			}
			if model != "" && (modelIndex < 0 || args[modelIndex+1] != model) {
				t.Fatalf("model missing from %v", args)
			}
			dirIndex := slices.Index(args, "--dir")
			if dirIndex < 0 {
				t.Fatal("missing isolated directory")
			}
			if _, err := os.Stat(args[dirIndex+1]); !os.IsNotExist(err) {
				t.Fatalf("temp directory not removed: %v", err)
			}
			for _, entry := range command.Env {
				if strings.HasPrefix(entry, "GIT_") {
					t.Fatalf("git environment inherited: %s", entry)
				}
			}
		})
	}
}

func TestOpenCodeGenerateOutput(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, raw, want, err string }{
		{"text", `{"type":"step_start"}
{"type":"text","part":{"text":"hello"}}
{"type":"reasoning","part":{"text":"private reasoning"}}
{"type":"text","part":{"text":"world"}}
{"type":"step_finish"}`, "hello\nworld", ""},
		{"api error after text", `{"type":"text","part":{"text":"partial"}}
{"type":"error","error":{"name":"APIError","data":{"message":"API key is invalid."}}}`, "", "API key is invalid."},
		{"named error", `{"type":"error","error":{"name":"UnknownError"}}`, "", "UnknownError"},
		{"empty error", `{"type":"error"}`, "", "error"},
		{"missing text", `{"type":"step_finish"}`, "", "no text"},
		{"blank text", `{"type":"text","part":{"text":"  "}}`, "", "no text"},
		{"truncated", `{"type":"text","part":`, "", "decoding"},
		{"malformed after text", `{"type":"text","part":{"text":"partial"}}garbage`, "", "decoding"},
		{"long text", `{"type":"text","part":{"text":"` + strings.Repeat("x", 100000) + `"}}`, strings.Repeat("x", 100000), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(path, []byte(tt.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			a := &OpenCodeAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "cat", path)
			}}
			got, err := a.GenerateText(t.Context(), "synthetic prompt", "")
			if got != tt.want {
				t.Fatalf("text differs: got length %d, want length %d", len(got), len(tt.want))
			}
			if tt.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("error = %v, want %q", err, tt.err)
			}
			var generationErr *agent.TextGenerationError
			if !errors.As(err, &generationErr) || generationErr.StdoutBytes != len(strings.TrimSpace(tt.raw)) {
				t.Fatalf("missing output metadata: %v", err)
			}
		})
	}
}

func TestOpenCodeGenerateCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a := &OpenCodeAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "cat") }}
	_, err := a.GenerateText(ctx, "prompt", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestOpenCodeGenerationConfig(t *testing.T) {
	t.Parallel()
	raw, err := openCodeGenerationConfig(`{"model":"openai/custom","provider":{"openai":{"options":{"baseURL":"https://example.test"}}},"share":"auto","agent":{"build":{"permission":{"bash":"allow"}}}}`, "entire-test")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	if string(config["model"]) != `"openai/custom"` || string(config["provider"]) != `{"openai":{"options":{"baseURL":"https://example.test"}}}` {
		t.Fatalf("provider settings lost: %s", raw)
	}
	if string(config["share"]) != `"disabled"` {
		t.Fatalf("sharing not disabled: %s", raw)
	}
	var agents map[string]struct {
		Permission map[string]string `json:"permission"`
	}
	if err := json.Unmarshal(config["agent"], &agents); err != nil {
		t.Fatal(err)
	}
	if agents["entire-test"].Permission["*"] != "deny" || agents["build"].Permission["bash"] != "allow" {
		t.Fatalf("agent permissions: %s", raw)
	}
	for _, invalid := range []string{"{", `{"agent":1}`} {
		if _, err := openCodeGenerationConfig(invalid, "test"); err == nil {
			t.Fatalf("accepted invalid config %q", invalid)
		}
	}
}

func TestOpenCodeGenerationConfigJSONC(t *testing.T) {
	t.Parallel()
	raw, err := openCodeGenerationConfig(`{
 // Keep the user's provider selection.
 "model":"openai/custom",
 "agent":{"build":{"permission":{"bash":"allow",},},},
 }`, "entire-test")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	if string(config["model"]) != `"openai/custom"` {
		t.Fatalf("model lost: %s", raw)
	}
}
