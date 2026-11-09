package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// newTestTextGenerator isolates authentication discovery as well as execution.
// Replacing CommandRunner alone still prepares the real user's credentials.
func newTestTextGenerator(t *testing.T, runner agent.TextCommandRunner) *AntigravityAgent {
	t.Helper()
	auth := newTestTextGenerationAuth(t)
	return &AntigravityAgent{CommandRunner: runner, textGenerationAuth: &auth}
}

// agyReplying fakes agy's stream-json output: the stdout of script becomes the
// response of a successful result event. Newlines are escaped by awk; the
// scripts' output holds no quotes or backslashes.
func agyReplying(ctx context.Context, script string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", `out=$(`+script+`) || exit
printf '{"event":"init","init":{}}\n'
printf '{"event":"result","result":{"status":"SUCCESS","response":"%s"}}\n' "$(printf '%s' "$out" | awk 'BEGIN{ORS="\\n"}1')"`)
}

// The prompt carries condensed transcript content, so it must travel on stdin:
// argv is visible to other local users through the process table. agy 1.2.7
// ignores stdin in text print mode, so it goes as a stream-json message.
func TestGenerateText_PassesPromptOnStdinNotArgv(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to fake agy")
	}
	const prompt = "Summarize this \"transcript\".\nSecond line."
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	var gotBinary string
	var gotArgs []string
	a := newTestTextGenerator(t, func(ctx context.Context, binary string, argv ...string) *exec.Cmd {
		gotBinary, gotArgs = binary, slices.Clone(argv)
		return exec.CommandContext(ctx, "sh", "-c",
			`cat > "$1"; printf '{"event":"result","result":{"status":"SUCCESS","response":"PONG\\n"}}\n'`, "sh", stdinFile)
	})

	out, err := a.GenerateText(t.Context(), prompt, "gemini-3.8-flash-low")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if out != "PONG" {
		t.Fatalf("GenerateText output = %q, want the result event's response", out)
	}
	if gotBinary != "agy" {
		t.Fatalf("binary = %q, want agy", gotBinary)
	}
	want := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--model", "gemini-3.8-flash-low", "--print="}
	if !slices.Equal(gotArgs, want) {
		t.Fatalf("argv = %q, want %q", gotArgs, want)
	}

	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(stdin), "\n") != 1 || !strings.HasSuffix(string(stdin), "\n") {
		t.Fatalf("stdin = %q, want exactly one NDJSON line", stdin)
	}
	var msg struct {
		Event   string `json:"event"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(stdin, &msg); err != nil {
		t.Fatalf("stdin is not JSON: %v (%q)", err, stdin)
	}
	if msg.Event != "user" || msg.Message.Role != "user" || msg.Message.Content != prompt {
		t.Fatalf("stdin message = %+v, want a user message carrying the prompt", msg)
	}
}

func TestGenerateText_ReportsStreamResultFailures(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to fake agy")
	}
	for name, tc := range map[string]struct {
		stdout  string
		wantErr string
	}{
		"error status":    {`{"event":"result","result":{"status":"ERROR","response":"","error":"quota exhausted"}}`, "quota exhausted"},
		"no result event": {`{"event":"init","init":{}}`, "no stream-json result event"},
		"empty response":  {`{"event":"result","result":{"status":"SUCCESS","response":"\n"}}`, "empty response"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; printf '%s\n' "$1"`, "sh", tc.stdout)
			})
			_, err := a.GenerateText(t.Context(), "prompt", "")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("GenerateText error = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// A result-event failure arrives with exit 0; what agy printed to stderr must
// still reach TextGenerationError.Stderr, which `entire explain` shows.
func TestGenerateText_KeepsStderrForResultEventFailures(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to fake agy")
	}
	a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; echo "model overloaded" >&2
printf '{"event":"result","result":{"status":"ERROR","response":"","error":"turn failed"}}\n'`)
	})
	_, err := a.GenerateText(t.Context(), "prompt", "")
	var genErr *agent.TextGenerationError
	if !errors.As(err, &genErr) {
		t.Fatalf("GenerateText error = %v, want a *agent.TextGenerationError", err)
	}
	if genErr.Stderr != "model overloaded" {
		t.Fatalf("TextGenerationError.Stderr = %q, want agy's stderr", genErr.Stderr)
	}
}

// agy older than 1.1.15 has no --input-format; say so instead of falling back
// to an argv prompt.
func TestGenerateText_NamesTheMinimumVersionForAnUnknownFlag(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to fake agy")
	}
	a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `echo "flag provided but not defined: -input-format" >&2; exit 2`)
	})
	_, err := a.GenerateText(t.Context(), "prompt", "")
	if err == nil || !strings.Contains(err.Error(), "1.1.15") {
		t.Fatalf("GenerateText error = %v, want the minimum agy version", err)
	}
}

func TestGenerateText_RunsWithAnIsolatedHome(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's environment")
	}
	a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return agyReplying(ctx,
			`printf 'home=%s\ncwd=%s\nentries=%s\nxdg=%s\nadc=%s\n' "$HOME" "$(pwd -P)" "$(ls -A "$HOME" | tr '\n' ' ')" "$XDG_CONFIG_HOME" "$GOOGLE_APPLICATION_CREDENTIALS"`)
	})

	out, err := a.GenerateText(t.Context(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = strings.TrimSpace(v)
		}
	}
	if got["home"] == "" || got["home"] == a.textGenerationAuth.userHome {
		t.Fatalf("agy ran with the source home %q; want an isolated one", got["home"])
	}
	if strings.HasPrefix(got["cwd"], got["home"]) {
		t.Errorf("working directory %q is inside the isolated home %q; it must stay empty", got["cwd"], got["home"])
	}
	if !strings.HasPrefix(got["xdg"], got["home"]) {
		t.Errorf("XDG_CONFIG_HOME = %q, want it inside the isolated home %q", got["xdg"], got["home"])
	}
	if got["entries"] != "" {
		t.Errorf("isolated home holds %q, want no entries", got["entries"])
	}
	if got["adc"] != a.textGenerationAuth.adcCredentials {
		t.Errorf("ADC = %q, want only the injected fake source", got["adc"])
	}
	for _, dir := range []string{got["home"], got["cwd"]} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("temporary directory %q survives generation (stat error: %v)", dir, err)
		}
	}
}

func TestGenerateText_SignInFailureNamesTheIsolation(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to fake agy")
	}
	// Text print mode and stream-json mode word the failure differently on
	// stderr (observed live on agy 1.3.3), and stream-json also reports it in
	// the result event, which must be enough on its own.
	for name, script := range map[string]string{
		"text mode stderr":   `echo "Authentication required. Please visit the URL to log in:" >&2; exit 1`,
		"stream-json stderr": `echo "Error: authentication required. Run 'agy' to log in, then retry." >&2; exit 1`,
		"result event only":  `cat >/dev/null; printf '{"event":"result","result":{"status":"ERROR","response":"","error":"authentication failed or timed out"}}\n'`,
	} {
		a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", script)
		})
		_, err := a.GenerateText(t.Context(), "prompt", "")
		if err == nil {
			t.Fatalf("%s: GenerateText succeeded; want the sign-in failure", name)
		}
		if !strings.Contains(err.Error(), "isolated home") {
			t.Errorf("%s: error does not explain the isolated home: %v", name, err)
		}
	}
}

// Test the actual child environment, not just preparation's override list:
// an absent override would let os.Environ reintroduce an ambient ADC path.
func TestGenerateText_ClearsAmbientADCWhenNoSourceIsSelected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's environment")
	}
	for _, name := range []string{"empty sources", "missing default file"} {
		t.Run(name, func(t *testing.T) {
			// Setenv requires serial execution; the ambient pointer is fake.
			ambient := filepath.Join(t.TempDir(), "ambient-adc.json")
			t.Setenv(adcCredentialsEnvVar, ambient)
			a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return agyReplying(ctx, `printf 'adc=%s' "${GOOGLE_APPLICATION_CREDENTIALS-unset}"`)
			})
			if name == "empty sources" {
				a.textGenerationAuth = &textGenerationAuth{}
			} else {
				a.textGenerationAuth.adcCredentials = ""
			}
			out, err := a.GenerateText(t.Context(), "prompt", "")
			if err != nil || out != "adc=" {
				t.Fatalf("child environment = (%q, %v), want explicitly cleared ADC", out, err)
			}
		})
	}
}

// Exercise the production resolver too, but only after replacing every source
// it can inspect. Injected-source tests above can run in parallel without HOME
// or config overrides and cannot discover the developer's files.
func TestGenerateText_ResolvesAuthenticationFromFakeHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's credentials")
	}
	auth := newTestTextGenerationAuth(t)
	t.Setenv("HOME", auth.userHome)
	t.Setenv("USERPROFILE", auth.userHome)
	t.Setenv(configDirEnv, auth.configDir)
	t.Setenv(adcCredentialsEnvVar, auth.adcCredentials)
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	writeTextGenerationFixture(t, filepath.Join(auth.configDir, agyFileTokenName), "FAKE_TOKEN")
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return agyReplying(ctx, `cat "$HOME/.gemini/antigravity-cli/antigravity-oauth-token"`)
	}}
	out, err := a.GenerateText(t.Context(), "prompt", "")
	if err != nil || out != "FAKE_TOKEN" {
		t.Fatalf("GenerateText = (%q, %v), want only the fake token", out, err)
	}
}

// TestGenerateText_SmokeRealCLI checks the stream-json stdin transport and the
// isolated home against the real agy. It makes a real billable API call, so it
// is opt-in: run with ENTIRE_ANTIGRAVITY_SMOKE=1 on a machine where agy is
// installed and signed in.
func TestGenerateText_SmokeRealCLI(t *testing.T) {
	t.Parallel()

	if os.Getenv("ENTIRE_ANTIGRAVITY_SMOKE") == "" {
		t.Skip("set ENTIRE_ANTIGRAVITY_SMOKE=1 to run the real-CLI smoke test (makes a billable API call)")
	}

	got, err := (&AntigravityAgent{}).GenerateText(t.Context(), "Reply with the single word OK and nothing else.", "")
	if err != nil {
		t.Fatalf("text generation with the prompt on stdin failed: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("text generation with the prompt on stdin returned an empty response")
	}
}
