package antigravity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// agy 1.2.7 ignores stdin in print mode (`-p " "` fails with "empty prompt",
// `-p -` is answered as the literal message "-"), so the prompt must travel in
// argv. This pins the shape `entire dispatch --local --agent antigravity` and
// `explain --generate` depend on.
func TestGenerateText_PassesPromptInArgv(t *testing.T) {
	t.Parallel()
	var gotBinary string
	var gotArgs []string
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, binary string, argv ...string) *exec.Cmd {
		gotBinary, gotArgs = binary, argv
		return exec.CommandContext(ctx, "echo", "PONG")
	}}

	out, err := a.GenerateText(context.Background(), "Summarize this transcript.", "gemini-3.8-flash-low")
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

// agy reads its window-title command and its PreInvocation hooks from the
// user's home (~/.gemini/antigravity-cli/settings.json and
// ~/.gemini/config/hooks.json), and print mode runs both (verified live on agy
// 1.3.1). A summary prompt carries untrusted transcript content, so the run
// gets a home of its own: nothing from the user's agy configuration loads.
// On macOS the login keychain file, where agy keeps its sign-in, is linked in
// so authentication still works.
func TestGenerateText_RunsWithAnIsolatedHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's environment")
	}
	t.Setenv(configDirEnv, t.TempDir()) // no API-key mode to carry over
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			`printf 'home=%s\ncwd=%s\nentries=%s\nkeychains=%s\nxdg=%s\n' "$HOME" "$(pwd -P)" "$(ls -A "$HOME" | tr '\n' ' ')" "$(readlink "$HOME/Library/Keychains/login.keychain-db")" "$XDG_CONFIG_HOME"`)
	}}

	out, err := a.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = strings.TrimSpace(v)
		}
	}
	if got["home"] == "" || got["home"] == realHome {
		t.Fatalf("agy ran with the user's home %q; want an isolated one", got["home"])
	}
	if strings.HasPrefix(got["cwd"], got["home"]) {
		t.Errorf("working directory %q is inside the isolated home %q; the working directory must stay empty", got["cwd"], got["home"])
	}
	if !strings.HasPrefix(got["xdg"], got["home"]) {
		t.Errorf("XDG_CONFIG_HOME = %q, want it inside the isolated home %q", got["xdg"], got["home"])
	}
	wantEntries := ""
	if runtime.GOOS == "darwin" {
		// Only the keychain file, so no ".." walks back into the real home.
		want := ""
		if keychain := filepath.Join(realHome, "Library", "Keychains", "login.keychain-db"); fileExists(keychain) {
			want, wantEntries = keychain, "Library"
		}
		if got["keychains"] != want {
			t.Errorf("login.keychain-db links to %q, want %q", got["keychains"], want)
		}
	}
	if got["entries"] != wantEntries {
		t.Errorf("isolated home holds %q, want %q", got["entries"], wantEntries)
	}
}

func TestGenerateText_SignInFailureNamesTheIsolation(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to fake agy")
	}
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `echo "Authentication required. Please visit the URL to log in:" >&2; exit 1`)
	}}
	_, err := a.GenerateText(context.Background(), "prompt", "")
	if err == nil {
		t.Fatal("GenerateText succeeded; want the sign-in failure")
	}
	if !strings.Contains(err.Error(), "isolated home") {
		t.Errorf("error does not explain the isolated home: %v", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// agy's Gemini API-key mode is selected by {"modelProvider":"gemini"} in
// its settings.json, and without it agy refuses to start. The isolated home
// starts empty, so the user's choice has to be carried over, and only that:
// the same file holds the window-title command agy executes.
func TestGenerateText_CarriesTheAPIKeyProviderAndNothingElse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's settings")
	}
	userConfig := t.TempDir()
	t.Setenv(configDirEnv, userConfig)
	if err := os.WriteFile(filepath.Join(userConfig, agySettingsFileName), []byte(`{
  "modelProvider": "gemini",
  "title": {"type": "command", "command": "touch /tmp/should-not-run"},
  "trustedWorkspaces": ["/somewhere"]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `cat "$HOME/.gemini/antigravity-cli/settings.json"`)
	}}

	out, err := a.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if strings.TrimSpace(out) != `{"modelProvider":"gemini"}` {
		t.Fatalf("isolated settings.json = %q, want only the API-key provider", out)
	}
}

// Without API-key mode selected, the isolated home gets no settings.json, and
// a provider value agy does not document is not carried either.
func TestGenerateText_WritesNoSettingsWithoutAPIKeyMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's settings")
	}
	for name, content := range map[string]string{
		"no file":        "",
		"no provider":    `{"title": {"type": "command", "command": "x"}}`,
		"other provider": `{"modelProvider": "$(touch /tmp/x)"}`,
		"malformed":      `{"modelProvider":`,
		"symlinked":      "symlink",
	} {
		t.Run(name, func(t *testing.T) {
			userConfig := t.TempDir()
			t.Setenv(configDirEnv, userConfig)
			settingsPath := filepath.Join(userConfig, agySettingsFileName)
			switch content {
			case "":
			case "symlink":
				// A link to a file that does select API-key mode is still
				// refused, even one inside agy's own config directory.
				target := filepath.Join(userConfig, "elsewhere.json")
				if err := os.WriteFile(target, []byte(`{"modelProvider": "gemini"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				// Relative, so os.Root itself would follow it.
				if err := os.Symlink(filepath.Base(target), settingsPath); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(settingsPath, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", `test -e "$HOME/.gemini" && echo present || echo absent`)
			}}
			out, err := a.GenerateText(context.Background(), "prompt", "")
			if err != nil {
				t.Fatalf("GenerateText: %v", err)
			}
			if strings.TrimSpace(out) != "absent" {
				t.Fatalf("isolated home has a .gemini directory; want none")
			}
		})
	}
}

// Where no keyring is usable (agy detects an SSH session, for one), agy keeps
// its sign-in in antigravity-oauth-token in its config directory (observed on
// agy 1.3.1). That one file is linked into the isolated home; the settings
// file beside it, which names the window-title command, is not, and nor is
// anything from agy's hooks directory.
func TestGenerateText_CarriesTheFileTokenAndNothingElse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's home")
	}
	userConfig := t.TempDir()
	t.Setenv(configDirEnv, userConfig)
	token := filepath.Join(userConfig, agyFileTokenName)
	if err := os.WriteFile(token, []byte("TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userConfig, agySettingsFileName), []byte(`{"title": {"type": "command", "command": "touch /tmp/should-not-run"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			`d="$HOME/.gemini/antigravity-cli"; printf 'entries=%s\nlink=%s\ntoken=%s\nconfig=%s\n' "$(ls -A "$d" | tr '\n' ' ')" "$(readlink "$d/antigravity-oauth-token")" "$(cat "$d/antigravity-oauth-token")" "$(test -e "$HOME/.gemini/config" && echo present || echo absent)"`)
	}}

	out, err := a.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = strings.TrimSpace(v)
		}
	}
	if got["entries"] != agyFileTokenName {
		t.Errorf("isolated agy config dir holds %q, want only %q", got["entries"], agyFileTokenName)
	}
	if got["link"] != token {
		t.Errorf("file token links to %q, want %q", got["link"], token)
	}
	if got["token"] != "TOKEN" {
		t.Errorf("file token reads %q, want the user's token", got["token"])
	}
	if got["config"] != "absent" {
		t.Errorf("isolated home has agy's hooks directory; want none")
	}
}

// A symlinked file token is refused rather than followed, the way a
// symlinked settings.json is.
func TestGenerateText_RefusesASymlinkedFileToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh to report the child's home")
	}
	userConfig := t.TempDir()
	t.Setenv(configDirEnv, userConfig)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(userConfig, agyFileTokenName)); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `test -e "$HOME/.gemini" && echo present || echo absent`)
	}}
	out, err := a.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if strings.TrimSpace(out) != "absent" {
		t.Fatalf("a symlinked file token was carried into the isolated home")
	}
}

// agy's application-default-credentials mode (AGY_ADC_AUTH) honours
// GOOGLE_APPLICATION_CREDENTIALS before looking under $HOME (observed on agy
// 1.3.1), so the variable is pointed at the user's credentials file rather
// than linking anything into the isolated home. A value the user set is left
// alone.
func TestGenerateText_PointsADCAtTheUsersCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows finds ADC through %APPDATA%, which is inherited")
	}
	t.Setenv(configDirEnv, t.TempDir())
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	adc := filepath.Join(realHome, adcCredentialsFile)
	report := func(t *testing.T) string {
		t.Helper()
		a := &AntigravityAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `printf 'gac=%s' "$GOOGLE_APPLICATION_CREDENTIALS"`)
		}}
		out, err := a.GenerateText(context.Background(), "prompt", "")
		if err != nil {
			t.Fatalf("GenerateText: %v", err)
		}
		return strings.TrimPrefix(strings.TrimSpace(out), "gac=")
	}

	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	if got := report(t); got != "" {
		t.Errorf("without credentials, GOOGLE_APPLICATION_CREDENTIALS = %q, want unset", got)
	}
	if err := os.MkdirAll(filepath.Dir(adc), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adc, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := report(t); got != adc {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q, want the user's credentials %q", got, adc)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/custom/creds.json")
	if got := report(t); got != "/custom/creds.json" {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q, want the user's own value kept", got)
	}
}
