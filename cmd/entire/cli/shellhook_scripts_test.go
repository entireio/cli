package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellhookScriptContracts are the invariants every emitted script must hold,
// regardless of shell. Each is a property the hot path depends on: no output
// in a non-interactive shell, an honored kill switch, a dedupe variable so
// moving around inside one repo is free, the settings-file short circuit, and
// the single fork that is allowed to happen.
var shellhookScriptContracts = []struct {
	name string
	want string
}{
	{"kill switch", "ENTIRE_NO_SHELL_HOOK"},
	{"repo dedupe", "last_root"},
	{"settings short circuit", ".entire/settings.json"},
	{"local settings short circuit", ".entire/settings.local.json"},
	{"check invocation", "command entire shellhook check --root"},
}

func TestShellhookScript_Contracts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		shell            string
		interactiveGuard string
	}{
		{"zsh", "[[ -o interactive ]] || return"},
		{"bash", "case $- in *i*)"},
		{"fish", "status is-interactive; or exit"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			t.Parallel()

			script, err := shellhookScript(tc.shell)
			if err != nil {
				t.Fatalf("shellhookScript(%q) error = %v", tc.shell, err)
			}
			if !strings.Contains(script, tc.interactiveGuard) {
				t.Errorf("script missing interactivity guard %q", tc.interactiveGuard)
			}
			for _, contract := range shellhookScriptContracts {
				if !strings.Contains(strings.ToLower(script), strings.ToLower(contract.want)) {
					t.Errorf("script missing %s (%q)", contract.name, contract.want)
				}
			}
		})
	}
}

func TestShellhookScript_UnsupportedShell(t *testing.T) {
	t.Parallel()

	if _, err := shellhookScript("nushell"); !errors.Is(err, errUnsupportedShell) {
		t.Errorf("shellhookScript(nushell) error = %v, want errUnsupportedShell", err)
	}
}

func TestShellhookRCLines_CoverEveryShell(t *testing.T) {
	t.Parallel()

	for _, kind := range []shellKind{shellZsh, shellBash, shellFish} {
		line, ok := shellhookRCLines[kind]
		if !ok {
			t.Errorf("no rc line for %s", kind)
			continue
		}
		if !strings.Contains(line, "entire shellhook init "+string(kind)) {
			t.Errorf("%s rc line %q does not load its own init script", kind, line)
		}
	}
}

// TestShellhookScript_Syntax validates the emitted scripts with each shell's
// own parser, when that shell is installed. Skipped otherwise, so the suite
// still runs on a bare CI image.
func TestShellhookScript_Syntax(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		shell string
		args  []string
	}{
		{"bash", []string{"-n"}},
		{"zsh", []string{"-n"}},
		{"fish", []string{"--no-execute"}},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			t.Parallel()

			bin, err := exec.LookPath(tc.shell)
			if err != nil {
				t.Skipf("%s not installed", tc.shell)
			}
			script, err := shellhookScript(tc.shell)
			if err != nil {
				t.Fatalf("shellhookScript(%q) error = %v", tc.shell, err)
			}
			// `return` outside a function is only legal in a sourced file, so
			// write the script to disk and parse it as one.
			path := filepath.Join(t.TempDir(), "hook."+tc.shell)
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			out, err := exec.CommandContext(t.Context(), bin, append(tc.args, path)...).CombinedOutput()
			if err != nil {
				t.Errorf("%s syntax check failed: %v\n%s", tc.shell, err, out)
			}
		})
	}
}
