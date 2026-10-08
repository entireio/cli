package antigravity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// agy 1.2.7 ignores stdin in print mode (`-p " "` fails with "empty prompt",
// `-p -` is answered as the literal message "-"), so the prompt travels in argv.
func TestGenerateText_PassesPromptInArgv(t *testing.T) {
	t.Parallel()
	var gotBinary string
	var gotArgs []string
	a := newTestTextGenerator(t, func(ctx context.Context, binary string, argv ...string) *exec.Cmd {
		gotBinary, gotArgs = binary, argv
		return exec.CommandContext(ctx, "echo", "PONG")
	})

	out, err := a.GenerateText(t.Context(), "Summarize this transcript.", "gemini-3.8-flash-low")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if out != "PONG" {
		t.Fatalf("GenerateText output = %q, want the CLI's stdout", out)
	}
	if gotBinary != "agy" {
		t.Fatalf("binary = %q, want agy", gotBinary)
	}
	want := []string{"-p", "Summarize this transcript.", "--model", "gemini-3.8-flash-low"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %q, want %q", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %q, want %q", gotArgs, want)
		}
	}
}

func TestGenerateText_RunsWithAnIsolatedHome(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's environment")
	}
	a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
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
	a := newTestTextGenerator(t, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `echo "Authentication required. Please visit the URL to log in:" >&2; exit 1`)
	})
	_, err := a.GenerateText(t.Context(), "prompt", "")
	if err == nil {
		t.Fatal("GenerateText succeeded; want the sign-in failure")
	}
	if !strings.Contains(err.Error(), "isolated home") {
		t.Errorf("error does not explain the isolated home: %v", err)
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
				return exec.CommandContext(ctx, "sh", "-c", `printf 'adc=%s' "${GOOGLE_APPLICATION_CREDENTIALS-unset}"`)
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
		return exec.CommandContext(ctx, "sh", "-c", `cat "$HOME/.gemini/antigravity-cli/antigravity-oauth-token"`)
	}}
	out, err := a.GenerateText(t.Context(), "prompt", "")
	if err != nil || out != "FAKE_TOKEN" {
		t.Fatalf("GenerateText = (%q, %v), want only the fake token", out, err)
	}
}
