package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestReadConfigDirReply(t *testing.T) {
	t.Parallel()
	abs := filepath.Join(t.TempDir(), "config")
	reply := func(requestID, styles string) string {
		return `{"type":"control_response","response":{"subtype":"success","request_id":"` + requestID +
			`","response":{"user_output_styles_dir":"` + filepath.ToSlash(styles) + `"}}}`
	}
	tests := []struct {
		name    string
		stream  string
		want    string
		wantErr string
	}{
		{
			name:   "skips hook progress and answers from the matching reply",
			stream: `{"type":"system","subtype":"hook_started"}` + "\nnot json\n" + reply("other", filepath.Join(t.TempDir(), "output-styles")) + "\n" + reply(configProbeRequestID, filepath.Join(abs, "output-styles")) + "\n",
			want:   abs,
		},
		{name: "older CLI without the field", stream: `{"type":"control_response","response":{"request_id":"` + configProbeRequestID + `","response":{}}}` + "\n", wantErr: "did not report"},
		{name: "relative path", stream: reply(configProbeRequestID, "rel/output-styles") + "\n", wantErr: "unexpected"},
		{name: "not the output-styles folder", stream: reply(configProbeRequestID, filepath.Join(abs, "styles")) + "\n", wantErr: "unexpected"},
		{name: "claude exits without answering", stream: `{"type":"system"}` + "\n", wantErr: "without answering"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := readConfigDirReply(strings.NewReader(tt.stream))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("readConfigDirReply() = %q, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != filepath.FromSlash(tt.want) {
				t.Errorf("readConfigDirReply() = %q, want %q", got, tt.want)
			}
		})
	}
}

// writeFakeClaude writes a claude stand-in that answers the initialize request
// with configDir and then lingers, as the real one does, until it is killed.
func writeFakeClaude(t *testing.T, configDir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nread -r _\n" +
		`printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"entire-config-dir","response":{"user_output_styles_dir":"` +
		filepath.Join(configDir, "output-styles") + `"}}}'` + "\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestProbeConfigDir_AsksClaude(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv(configProbeEnvVar, writeFakeClaude(t, configDir))

	got, err := probeConfigDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != configDir {
		t.Errorf("probeConfigDir() = %q, want %q", got, configDir)
	}
}

func TestProbeConfigDir_SkippedWhenOffOrUnderTest(t *testing.T) {
	for _, value := range []string{"off", ""} {
		t.Setenv(configProbeEnvVar, value)
		if _, err := probeConfigDir(context.Background()); !errors.Is(err, errConfigProbeSkipped) {
			t.Errorf("probeConfigDir() with %s=%q error = %v, want it skipped", configProbeEnvVar, value, err)
		}
	}
}

// resetConfigProbe clears the process-wide memo so a test can probe again.
func resetConfigProbe(t *testing.T) {
	t.Helper()
	reset := func() {
		configProbe.mu.Lock()
		configProbe.done, configProbe.dir = false, ""
		configProbe.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// Once a command enables home probes, claude's own answer outranks the
// environment: it is what claude resolves after applying its settings env, so
// it covers a CLAUDE_CONFIG_DIR the environment does not carry.
func TestResolveClaudeConfigDir_PrefersClaudesAnswer(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv(configProbeEnvVar, writeFakeClaude(t, configDir))
	t.Setenv(claudeConfigDirEnvVar, "")
	resetConfigProbe(t)
	agent.EnableHomeProbes()

	got, err := resolveClaudeConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != configDir {
		t.Errorf("resolveClaudeConfigDir() = %q, want claude's answer %q", got, configDir)
	}
}

func TestResolveClaudeConfigDir_FallsBackWhenTheProbeFails(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv(configProbeEnvVar, "off")
	t.Setenv(claudeConfigDirEnvVar, envDir)
	resetConfigProbe(t)
	agent.EnableHomeProbes()

	got, err := resolveClaudeConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != envDir {
		t.Errorf("resolveClaudeConfigDir() = %q, want the environment's %q", got, envDir)
	}
}
